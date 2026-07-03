package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"quantvista/common"
)

// AI 调用客户端：OpenAI 兼容 /chat/completions。与 llm.go 的测试连接同源（复用 SafeHTTPClient 防 SSRF），
// 但这里是真正的分析调用——带 usage token 统计、JSON mode 优先且不支持时优雅 fallback。
// 2026-07-03 参照 new-api 的中继实践加固：连接池复用、瞬时错误单次重试（429/500/502/503 与
// 未达上游的网络错误；504 视为真超时不重试）、状态码分类提示、usage 缺失时字符粗估兜底。

const (
	aiCallTimeout  = 90 * time.Second
	aiRetryBackoff = 800 * time.Millisecond
)

// 包级复用两个 client（allowPrivate 两态），避免每次调用重建 Transport——
// repair/panel 会对同一上游连发多次请求，复用连接池省去反复 TLS 握手。
var (
	aiClientPublic  = common.SafeHTTPClient(aiCallTimeout, false)
	aiClientPrivate = common.SafeHTTPClient(aiCallTimeout, true)
)

func aiHTTPClient(allowPrivate bool) *http.Client {
	if allowPrivate {
		return aiClientPrivate
	}
	return aiClientPublic
}

// chatMessage 一条对话消息。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatParams 一次补全调用的参数。
type chatParams struct {
	BaseURL      string
	APIKey       string
	Model        string
	Temperature  float64
	MaxTokens    int
	Messages     []chatMessage
	JSONMode     bool // 请求 response_format=json_object（不支持则由调用逻辑 fallback）
	AllowPrivate bool // 管理员可放行内网自建模型
}

// chatUsage token 统计。
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatResult 一次补全调用的结果。
type chatResult struct {
	Content   string
	Usage     chatUsage
	LatencyMs int64
}

// chatCompletion 发起一次补全。JSONMode=true 时先带 response_format 请求；
// 若服务端因不支持该字段返回 4xx，则去掉 response_format 重试一次（fallback，靠 prompt 约束 JSON）。
func chatCompletion(ctx context.Context, p chatParams) (*chatResult, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("Base URL 非法（仅支持 http/https）")
	}

	res, status, raw, latency, err := doChat(ctx, p, p.JSONMode)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && p.JSONMode && looksLikeUnsupportedJSONMode(status, raw) {
		// 该服务端不支持 response_format：去掉后重试。
		res, status, raw, latency, err = doChat(ctx, p, false)
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("LLM 返回 HTTP %d%s：%s", status, statusHint(status), extractErr(raw))
	}
	if strings.TrimSpace(res.Content) == "" {
		return nil, errors.New("LLM 返回空内容")
	}
	// 部分兼容端点不回 usage：按字符粗估兜底（≈2 字符/token），仅作用量审计、不影响次数配额。
	if res.Usage.TotalTokens == 0 {
		res.Usage = estimateUsage(p.Messages, res.Content)
	}
	res.LatencyMs = latency
	return res, nil
}

// doChat 执行单次 HTTP 调用，返回解析后的结果、HTTP 状态码、原始响应体、耗时。
func doChat(ctx context.Context, p chatParams, jsonMode bool) (*chatResult, int, []byte, int64, error) {
	endpoint := strings.TrimRight(p.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}

	payload := map[string]any{
		"model":       p.Model,
		"messages":    p.Messages,
		"temperature": p.Temperature,
		"stream":      false,
	}
	if p.MaxTokens > 0 {
		payload["max_tokens"] = p.MaxTokens
	}
	if jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, 0, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := aiHTTPClient(p.AllowPrivate)
	send := func() (*http.Response, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header = req.Header.Clone()
		return client.Do(r)
	}

	start := time.Now()
	resp, err := send()
	if err != nil && transientNetErr(ctx, err) {
		// 未达上游的瞬时网络错误（连接被拒/复位等）重试一次；超时/取消不重试。
		time.Sleep(aiRetryBackoff)
		resp, err = send()
	}
	if err == nil && retryableStatus(resp.StatusCode) && ctx.Err() == nil {
		// 429/500/502/503 多为上游瞬时抖动，重试一次；504 视为真超时不重试（new-api 同款经验）。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		time.Sleep(aiRetryBackoff)
		if r2, e2 := send(); e2 == nil {
			resp = r2
		}
	}
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return nil, 0, nil, latency, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	// 限制响应体大小，防止异常超大响应打爆内存。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, raw, latency, nil
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, resp.StatusCode, raw, latency, fmt.Errorf("解析 LLM 响应失败: %w", err)
	}
	content := ""
	if len(parsed.Choices) > 0 {
		content = parsed.Choices[0].Message.Content
	}
	return &chatResult{Content: content, Usage: parsed.Usage}, resp.StatusCode, raw, latency, nil
}

// looksLikeUnsupportedJSONMode 粗略判断 4xx 是否因不支持 response_format 引起。
func looksLikeUnsupportedJSONMode(status int, raw []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	msg := strings.ToLower(string(raw))
	return strings.Contains(msg, "response_format") ||
		strings.Contains(msg, "json_object") ||
		strings.Contains(msg, "json mode") ||
		strings.Contains(msg, "not support")
}

// transientNetErr 是否为值得重试的瞬时网络错误：排除调用方取消、
// 整体超时（client.Timeout / context deadline）——这类重试只会白等。
func transientNetErr(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return false
	}
	// SafeHTTPClient 的 SSRF 拦截（dialer Control 拒绝内网地址）是确定性失败，重试无意义。
	if strings.Contains(err.Error(), "目标地址不允许") {
		return false
	}
	return true
}

// retryableStatus 上游瞬时抖动状态码（504 除外：网关超时重试多半再等一整轮）。
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusInternalServerError ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable
}

// statusHint 给常见状态码一个中文归类，用户不用翻上游文档。
func statusHint(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "（API Key 无效或无权限）"
	case status == http.StatusNotFound:
		return "（接口路径或模型名不存在，请检查 Base URL 与模型）"
	case status == http.StatusTooManyRequests:
		return "（上游限流或额度不足）"
	case status >= 500:
		return "（上游服务异常）"
	default:
		return ""
	}
}

// estimateUsage usage 缺失时的字符粗估（中英混合 ≈2 字符/token）。仅审计展示用。
func estimateUsage(messages []chatMessage, content string) chatUsage {
	promptChars := 0
	for _, m := range messages {
		promptChars += utf8.RuneCountInString(m.Content)
	}
	u := chatUsage{
		PromptTokens:     (promptChars + 1) / 2,
		CompletionTokens: (utf8.RuneCountInString(content) + 1) / 2,
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}
