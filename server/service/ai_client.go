package service

import (
	"bufio"
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
	"quantvista/model"
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
	EndpointType string // model.LLMEndpointChat（默认，空值同）/ model.LLMEndpointResponses
	Temperature  float64
	MaxTokens    int
	Messages     []chatMessage
	JSONMode     bool // 请求 response_format=json_object（不支持则由调用逻辑 fallback）
	AllowPrivate bool // 管理员可放行内网自建模型
}

// isResponsesEndpoint 该次调用是否走 /v1/responses（空值按 chat/completions）。
func (p chatParams) isResponsesEndpoint() bool {
	return p.EndpointType == model.LLMEndpointResponses
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
// EndpointType=responses 时分流到 /v1/responses 适配（ai_client_responses.go），返回语义一致。
func chatCompletion(ctx context.Context, p chatParams) (*chatResult, error) {
	if p.isResponsesEndpoint() {
		return responsesCompletion(ctx, p)
	}
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
	endpoint := chatCompletionsURL(p.BaseURL)

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
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if looksLikeHTML(raw) {
			// 典型场景：Base URL 指向 new-api/one-api 等站点的非 API 路径，命中前端 SPA fallback
			// 返回 200 + index.html。裸报 invalid character '<' 用户无从下手，这里直接指出端点。
			return nil, resp.StatusCode, raw, latency, fmt.Errorf(
				"LLM 返回了网页而非 JSON（HTTP %d）：%s 不是有效的 API 端点，请检查 Base URL——填服务根地址（自动补 /v1/chat/completions）或以 /v1 结尾的地址均可", resp.StatusCode, endpoint)
		}
		return nil, resp.StatusCode, raw, latency, fmt.Errorf("解析 LLM 响应失败: %w（响应开头: %s）", err, bodySnippet(raw))
	}
	content := ""
	if len(parsed.Choices) > 0 {
		content = parsed.Choices[0].Message.Content
		// 上游安全策略拦截（finish_reason=content_filter）且无内容：给明确文案而非笼统"空内容"。
		if content == "" && parsed.Choices[0].FinishReason == "content_filter" {
			return nil, resp.StatusCode, raw, latency, errors.New("内容被上游安全策略拦截（finish_reason=content_filter）")
		}
	}
	return &chatResult{Content: content, Usage: parsed.Usage}, resp.StatusCode, raw, latency, nil
}

// looksLikeUnsupportedJSONMode 粗略判断 4xx 是否因不支持 response_format / text.format 引起。
func looksLikeUnsupportedJSONMode(status int, raw []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	msg := strings.ToLower(string(raw))
	return strings.Contains(msg, "response_format") ||
		strings.Contains(msg, "text.format") ||
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

// chatCompletionsURL 由 Base URL 构造 /chat/completions 端点，对齐 new-api / OpenAI SDK 的填写惯例：
// 根地址（https://xxx.com）自动补 /v1/chat/completions；以版本段结尾（/v1、/api/v3 等）只补 /chat/completions；
// 已是完整端点则原样使用。这样按 new-api 习惯填根地址、按 SDK 习惯填 /v1、直接填完整端点三种写法都能打对路径。
func chatCompletionsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if endsWithVersionSegment(base) {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// endsWithVersionSegment 路径最后一段是否形如 v1/v2/v3（火山方舟 /api/v3 这类也算）。
func endsWithVersionSegment(base string) bool {
	i := strings.LastIndexByte(base, '/')
	if i < 0 || i == len(base)-1 {
		return false
	}
	seg := base[i+1:]
	if len(seg) < 2 || (seg[0] != 'v' && seg[0] != 'V') {
		return false
	}
	for _, c := range seg[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// looksLikeHTML 响应体形似网页而非 JSON——SPA fallback、网关登录页、CDN 拦截页的典型形态
//（合法 JSON 不会以 '<' 开头）。
func looksLikeHTML(raw []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "<")
}

// bodySnippet 取响应体开头一小段用于报错展示（压平空白，按 rune 截断避免切碎中文）。
func bodySnippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if s == "" {
		return "(空)"
	}
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
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

// --- 流式补全（S1）---

// chatCompletionStream 发起一次流式补全（SSE）：逐行剥 "data: " 前缀解析 delta.content，
// 每个增量经 onDelta 回调吐出（调用方负责推给前端）；[DONE] 或 finish_reason 终止。
// 返回聚合后的完整内容与 usage（请求带 stream_options.include_usage 让上游在末 chunk 回
// usage——new-api 同款；不支持该字段的 4xx 会去掉重试一次，仍缺失则字符粗估）。
// 不做 JSON mode（流式只用于自由文本模块），不做状态码重试（流一旦建立，中断即失败——
// 半截内容重试会让用户看到重复文本）；建立连接前的错误分类与非流式一致。
// EndpointType=responses 时分流到 /v1/responses 适配。
func chatCompletionStream(ctx context.Context, p chatParams, onDelta func(string)) (*chatResult, error) {
	if p.isResponsesEndpoint() {
		return responsesCompletionStream(ctx, p, onDelta)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("Base URL 非法（仅支持 http/https）")
	}
	endpoint := chatCompletionsURL(p.BaseURL)

	buildBody := func(withUsageOpt bool) []byte {
		payload := map[string]any{
			"model":       p.Model,
			"messages":    p.Messages,
			"temperature": p.Temperature,
			"stream":      true,
		}
		if p.MaxTokens > 0 {
			payload["max_tokens"] = p.MaxTokens
		}
		if withUsageOpt {
			payload["stream_options"] = map[string]bool{"include_usage": true}
		}
		b, _ := json.Marshal(payload)
		return b
	}
	body := buildBody(true)

	client := aiHTTPClient(p.AllowPrivate)
	send := func() (*http.Response, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		req.Header.Set("Accept", "text/event-stream")
		return client.Do(req)
	}

	start := time.Now()
	resp, err := send()
	if err != nil && transientNetErr(ctx, err) {
		time.Sleep(aiRetryBackoff)
		resp, err = send()
	}
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// 部分兼容端点不认识 stream_options：识别后去掉该字段重试一次（照 JSON mode fallback 模式）。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if strings.Contains(strings.ToLower(string(raw)), "stream_options") && ctx.Err() == nil {
			body = buildBody(false)
			if r2, e2 := send(); e2 == nil {
				resp = r2
			} else {
				return nil, fmt.Errorf("请求失败: %w", e2)
			}
		} else {
			return nil, fmt.Errorf("LLM 返回 HTTP %d%s：%s", resp.StatusCode, statusHint(resp.StatusCode), extractErr(raw))
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("LLM 返回 HTTP %d%s：%s", resp.StatusCode, statusHint(resp.StatusCode), extractErr(raw))
	}

	// SSE 逐行状态机：每行形如 "data: {...}"；空行为事件分隔；"data: [DONE]" 结束。
	var sb strings.Builder
	var usage chatUsage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // 单行上限 1MB（长 delta/大 usage 容错）
	done := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") { // 空行/注释行
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *chatUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue // 单个坏 chunk 容错跳过
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			usage = *chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				sb.WriteString(c.Delta.Content)
				if onDelta != nil {
					onDelta(c.Delta.Content)
				}
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				done = true
			}
		}
		if done {
			break
		}
	}
	if err := sc.Err(); err != nil && !done {
		// 流中断且未正常收尾：已有内容也判失败（半截回答不落库，让调用方决定重试）。
		return nil, fmt.Errorf("流式响应中断: %w", err)
	}
	content := sb.String()
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("LLM 返回空内容")
	}
	if usage.TotalTokens == 0 {
		usage = estimateUsage(p.Messages, content)
	}
	return &chatResult{Content: content, Usage: usage, LatencyMs: time.Since(start).Milliseconds()}, nil
}
