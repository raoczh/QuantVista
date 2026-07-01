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

	"quantvista/common"
)

// AI 调用客户端：OpenAI 兼容 /chat/completions。与 llm.go 的测试连接同源（复用 SafeHTTPClient 防 SSRF），
// 但这里是真正的分析调用——带 usage token 统计、JSON mode 优先且不支持时优雅 fallback。

const aiCallTimeout = 90 * time.Second

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
		return nil, fmt.Errorf("LLM 返回 HTTP %d：%s", status, extractErr(raw))
	}
	if strings.TrimSpace(res.Content) == "" {
		return nil, errors.New("LLM 返回空内容")
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

	client := common.SafeHTTPClient(aiCallTimeout, p.AllowPrivate)
	start := time.Now()
	resp, err := client.Do(req)
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
