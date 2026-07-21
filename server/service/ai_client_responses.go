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
)

// OpenAI Responses API（/v1/responses）适配：请求/响应/流式与 chat/completions 的
// 字段映射按 new-api service/relayconvert 的转换口径实现——
//   messages(system) → instructions（多条 \n\n 连接）、messages(user/assistant) → input、
//   max_tokens → max_output_tokens、response_format:{type:json_object} → text:{format:{type:json_object}}、
//   usage 的 input_tokens/output_tokens → prompt/completion_tokens。
// 本项目只用纯文本对话（无 tool call/多模态），转换取 new-api 的文本子集。

// responsesURL 由 Base URL 构造 /responses 端点，归一化规则与 chatCompletionsURL 一致：
// 根地址补 /v1/responses；以版本段结尾（/v1、/api/v3）只补 /responses；已是完整端点原样。
func responsesURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	if endsWithVersionSegment(base) {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

// buildResponsesPayload chat 语义 → responses 请求体。
func buildResponsesPayload(p chatParams, jsonMode, stream bool) map[string]any {
	var instructions []string
	input := make([]map[string]any, 0, len(p.Messages))
	for _, m := range p.Messages {
		switch m.Role {
		case "system", "developer":
			// system 不进 input，抽出并入 instructions（new-api 同款处理）。
			if m.Content != "" {
				instructions = append(instructions, m.Content)
			}
		default:
			input = append(input, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	payload := map[string]any{
		"model":       p.Model,
		"input":       input,
		"temperature": p.Temperature,
		"stream":      stream,
	}
	if len(instructions) > 0 {
		payload["instructions"] = strings.Join(instructions, "\n\n")
	}
	if p.MaxTokens > 0 {
		payload["max_output_tokens"] = p.MaxTokens
	}
	if jsonMode {
		payload["text"] = map[string]any{"format": map[string]string{"type": "json_object"}}
	}
	return payload
}

// responses 响应体（文本子集）。usage 字段名与 chat 不同（input/output_tokens）。
type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func (u responsesUsage) toChatUsage() chatUsage {
	return chatUsage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
}

type responsesOutputContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesOutputItem struct {
	Type    string                   `json:"type"`
	Role    string                   `json:"role"`
	Content []responsesOutputContent `json:"content"`
}

type responsesResponse struct {
	Status            string                `json:"status"` // completed / incomplete / failed …
	Error             json.RawMessage       `json:"error"`
	Output            []responsesOutputItem `json:"output"`
	Usage             *responsesUsage       `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// extractResponsesText 从 output 数组提取正文：type=="message" 且 role 为空或 assistant
// 的项，拼接其 content 中 type=="output_text" 的 text（new-api ExtractOutputTextFromResponses 口径）。
func extractResponsesText(output []responsesOutputItem) string {
	var sb strings.Builder
	for _, out := range output {
		if out.Type != "message" {
			continue
		}
		if out.Role != "" && out.Role != "assistant" {
			continue
		}
		for _, c := range out.Content {
			if c.Type == "output_text" && c.Text != "" {
				sb.WriteString(c.Text)
			}
		}
	}
	return sb.String()
}

// extractResponsesRefusal 提取标准 Responses 结构化拒答内容。bool 表示是否出现
// type=refusal；即使上游漏了文案也不能把该形态误报为空响应或当成功。
func extractResponsesRefusal(output []responsesOutputItem) (string, bool) {
	for _, out := range output {
		if out.Type != "message" || (out.Role != "" && out.Role != "assistant") {
			continue
		}
		for _, c := range out.Content {
			if c.Type == "refusal" {
				return strings.TrimSpace(c.Refusal), true
			}
		}
	}
	return "", false
}

// responsesCompletion 非流式补全（与 chatCompletion 同语义同返回）。JSONMode 不被支持时
// 去掉 text.format 重试一次，fallback 逻辑与 chat 端一致。
func responsesCompletion(ctx context.Context, p chatParams) (*chatResult, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("Base URL 非法（仅支持 http/https）")
	}

	res, status, raw, latency, err := doResponses(ctx, p, p.JSONMode)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && p.JSONMode && looksLikeUnsupportedJSONMode(status, raw) {
		p.markJSONModeDropped()
		res, status, raw, latency, err = doResponses(ctx, p, false)
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
	if res.Usage.TotalTokens == 0 {
		res.Usage = estimateUsage(p.Messages, res.Content)
	}
	res.LatencyMs = latency
	return res, nil
}

// doResponses 单次 /responses HTTP 调用；重试策略与 doChat 一致（瞬时网络错误 +
// 429/500/502/503 各重试一次，504 不重试）。
func doResponses(ctx context.Context, p chatParams, jsonMode bool) (*chatResult, int, []byte, int64, error) {
	endpoint := responsesURL(p.BaseURL)
	body, _ := json.Marshal(buildResponsesPayload(p, jsonMode, false))

	client := aiHTTPClient(p.AllowPrivate)
	send := func() (*http.Response, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		return client.Do(req)
	}

	start := time.Now()
	resp, err := send()
	if err != nil && transientNetErr(ctx, err) {
		time.Sleep(aiRetryBackoff)
		resp, err = send()
	}
	if err == nil && retryableStatus(resp.StatusCode) && ctx.Err() == nil {
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
	contractEnabled := p.accuracyContractEnabled()
	raw, readErr := readLLMResponseBody(resp.Body, contractEnabled)

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, raw, latency, nil
	}
	if readErr != nil {
		return nil, resp.StatusCode, raw, latency, readErr
	}

	res, perr := parseResponsesBody(raw, resp.StatusCode, endpoint, contractEnabled)
	if perr != nil {
		return nil, resp.StatusCode, raw, latency, perr
	}
	return res, resp.StatusCode, raw, latency, nil
}

// parseResponsesBody 解析 /responses 的 200 响应体（非流式整包形态）。
func parseResponsesBody(raw []byte, status int, endpoint string, contractEnabled bool) (*chatResult, error) {
	var parsed responsesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if looksLikeHTML(raw) {
			return nil, fmt.Errorf(
				"LLM 返回了网页而非 JSON（HTTP %d）：%s 不是有效的 API 端点，请检查 Base URL——填服务根地址（自动补 /v1/responses）或以 /v1 结尾的地址均可", status, endpoint)
		}
		return nil, fmt.Errorf("解析 LLM 响应失败: %w（响应开头: %s）", err, bodySnippet(raw))
	}
	// 200 但响应体带 error 对象（部分网关形态）：按错误处理。error 非 null 即失败，
	// 不能只在带 message 时拒绝；原始 code 还用于 content_filter 机读分类。
	if contractEnabled {
		if uerr := upstreamLLMError(parsed.Error); uerr != nil {
			return nil, uerr
		}
	} else if msg := errorMessageFromRaw(parsed.Error); msg != "" {
		// flag 关闭时保留旧路径：只拒绝带 message 的 error；code-only error 与
		// output 并存的非标准网关形态继续按旧逻辑读取 output。
		return nil, fmt.Errorf("LLM 返回错误：%s", msg)
	}
	if refusal, ok := extractResponsesRefusal(parsed.Output); contractEnabled && ok {
		if refusal == "" {
			refusal = "上游未提供拒答原因"
		}
		return nil, refusalErr(RefusalLLMContentFiltered, "模型拒绝生成该内容："+refusal)
	}
	content := extractResponsesText(parsed.Output)
	incompleteReason := ""
	if parsed.IncompleteDetails != nil {
		incompleteReason = parsed.IncompleteDetails.Reason
	}
	// 完整性门禁（契约开启时）：仅 status=completed 算成功，incomplete/failed/空一律拒收
	//（incomplete 带部分内容也是半截，responses 是显式端点选择、标准实现必回 status）。
	if rerr := responsesStatusReject(contractEnabled, parsed.Status, incompleteReason); rerr != nil {
		return nil, rerr
	}
	if parsed.Status == "incomplete" && strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("LLM 响应未完成（%s）且无内容，可尝试调大 max_tokens", incompleteReason)
	}
	res := &chatResult{Content: content, FinishReason: parsed.Status}
	if parsed.Usage != nil {
		res.Usage = parsed.Usage.toChatUsage()
	}
	return res, nil
}

// errorMessageFromRaw 从 error 字段（对象 {message:...} 或裸字符串）里抽 message。
func errorMessageFromRaw(rawErr json.RawMessage) string {
	if len(rawErr) == 0 || string(rawErr) == "null" {
		return ""
	}
	var eo struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(rawErr, &eo) == nil && eo.Message != "" {
		return eo.Message
	}
	var es string
	if json.Unmarshal(rawErr, &es) == nil && es != "" {
		return es
	}
	return ""
}

// upstreamLLMError 把 HTTP 200/SSE 中的非 null error 对象转成错误。raw 中的
// content_filter 不能在只提取 message 后丢失，否则中央分类会误报 llm_call_failed。
func upstreamLLMError(rawErr json.RawMessage) error {
	rawText := strings.TrimSpace(string(rawErr))
	if rawText == "" || rawText == "null" {
		return nil
	}
	msg := errorMessageFromRaw(rawErr)
	if msg == "" {
		msg = rawText
	}
	if strings.Contains(strings.ToLower(rawText), "content_filter") {
		return refusalErr(RefusalLLMContentFiltered, "内容被上游安全策略拦截（content_filter）："+msg)
	}
	return refusalErr(RefusalLLMCallFailed, "LLM 返回错误："+msg)
}

// responsesCompletionStream 流式补全：SSE data 行按事件 type 分派——
// response.output_text.delta 取 delta 追加、response.completed/incomplete 取最终 usage、
// response.failed/error 判失败。流中断即失败不落半截，与 chat 端纪律一致。
// JSONMode 带 text.format，遇不支持的 4xx 去掉重试一次；上游忽略 stream 返回整包 JSON 时按非流式解析兼容。
func responsesCompletionStream(ctx context.Context, p chatParams, onDelta func(string)) (*chatResult, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("Base URL 非法（仅支持 http/https）")
	}
	endpoint := responsesURL(p.BaseURL)
	jsonOn := p.JSONMode
	body, _ := json.Marshal(buildResponsesPayload(p, jsonOn, true))

	client := aiStreamHTTPClient(p.AllowPrivate)
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
	if retryableStatus(resp.StatusCode) && ctx.Err() == nil {
		// 建流前的瞬时抖动状态码重试一次（尚无内容，与非流式重试纪律一致）。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		time.Sleep(aiRetryBackoff)
		if r2, e2 := send(); e2 == nil {
			resp = r2
		}
	}
	if jsonOn && resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// 不支持 text.format 的端点：去掉后重试一次（与非流式 fallback 同款）。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if looksLikeUnsupportedJSONMode(resp.StatusCode, raw) && ctx.Err() == nil {
			jsonOn = false
			p.markJSONModeDropped()
			body, _ = json.Marshal(buildResponsesPayload(p, false, true))
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
	br := bufio.NewReader(resp.Body)
	if !isSSEResponse(resp, br) {
		// 上游忽略 stream 参数直接返回整包 JSON：按非流式解析。
		contractEnabled := p.accuracyContractEnabled()
		raw, readErr := readLLMResponseBody(br, contractEnabled)
		if readErr != nil {
			return nil, readErr
		}
		res, perr := parseResponsesBody(raw, resp.StatusCode, endpoint, contractEnabled)
		if perr != nil {
			return nil, perr
		}
		return finishStreamResult(p, res.Content, res.Usage, res.FinishReason, start, onDelta)
	}

	var sb strings.Builder
	var usage chatUsage
	var firstChunkMs int64
	doneStatus := ""       // 终态事件 response 对象实际携带的 status；不得由事件名推断
	terminalEvent := ""    // response.completed/done/incomplete 或传输层 [DONE]
	incompleteReason := "" // incomplete 事件的截断原因（如 max_output_tokens）
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	done := false
	contractEnabled := p.accuracyContractEnabled()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue // 空行/注释/event 行（类型信息在 data JSON 的 type 字段里重复携带）
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		if firstChunkMs == 0 {
			if firstChunkMs = time.Since(start).Milliseconds(); firstChunkMs == 0 {
				firstChunkMs = 1
			}
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			terminalEvent = "[DONE]"
			done = true
			break
		}
		var ev struct {
			Type     string             `json:"type"`
			Delta    string             `json:"delta"`
			Code     string             `json:"code"`
			Message  string             `json:"message"`
			Refusal  string             `json:"refusal"`
			Response *responsesResponse `json:"response"`
			Error    json.RawMessage    `json:"error"`
		}
		if jerr := json.Unmarshal([]byte(data), &ev); jerr != nil {
			if rerr := streamProtocolReject(contractEnabled, "Responses SSE JSON 解析失败"); rerr != nil {
				return nil, rerr
			}
			continue // 关闭契约时保留旧兼容路径
		}
		if contractEnabled {
			if uerr := upstreamLLMError(ev.Error); uerr != nil {
				return nil, uerr
			}
		}
		if strings.TrimSpace(ev.Type) == "" {
			if rerr := streamProtocolReject(contractEnabled, "Responses SSE 事件缺少 type"); rerr != nil {
				return nil, rerr
			}
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				sb.WriteString(ev.Delta)
				if onDelta != nil {
					onDelta(ev.Delta)
				}
			}
		case "response.refusal.delta", "response.refusal.done":
			if contractEnabled {
				msg := strings.TrimSpace(ev.Delta)
				if msg == "" {
					msg = strings.TrimSpace(ev.Refusal)
				}
				if msg == "" {
					msg = "上游未提供拒答原因"
				}
				return nil, refusalErr(RefusalLLMContentFiltered, "模型拒绝生成该内容："+msg)
			}
		case "response.completed", "response.done":
			terminalEvent = ev.Type
			if ev.Response != nil {
				if contractEnabled {
					if uerr := upstreamLLMError(ev.Response.Error); uerr != nil {
						return nil, uerr
					}
				}
				if ev.Response.IncompleteDetails != nil {
					if rerr := responsesStatusReject(contractEnabled, "incomplete", ev.Response.IncompleteDetails.Reason); rerr != nil {
						return nil, rerr
					}
				}
				if refusal, ok := extractResponsesRefusal(ev.Response.Output); contractEnabled && ok {
					if refusal == "" {
						refusal = "上游未提供拒答原因"
					}
					return nil, refusalErr(RefusalLLMContentFiltered, "模型拒绝生成该内容："+refusal)
				}
				doneStatus = ev.Response.Status
				if ev.Response.Usage != nil {
					usage = ev.Response.Usage.toChatUsage()
				}
			}
			if !contractEnabled {
				// 兼容旧路径：历史实现仅凭事件名即视为 completed。
				doneStatus = "completed"
			}
			done = true
		case "response.incomplete":
			terminalEvent = ev.Type
			if ev.Response != nil {
				if contractEnabled {
					if uerr := upstreamLLMError(ev.Response.Error); uerr != nil {
						return nil, uerr
					}
					if refusal, ok := extractResponsesRefusal(ev.Response.Output); ok {
						if refusal == "" {
							refusal = "上游未提供拒答原因"
						}
						return nil, refusalErr(RefusalLLMContentFiltered, "模型拒绝生成该内容："+refusal)
					}
				}
				doneStatus = ev.Response.Status
				if ev.Response.Usage != nil {
					usage = ev.Response.Usage.toChatUsage()
				}
				if ev.Response.IncompleteDetails != nil {
					incompleteReason = ev.Response.IncompleteDetails.Reason
				}
			}
			if !contractEnabled && doneStatus == "" {
				doneStatus = "incomplete"
			}
			done = true
		case "response.failed", "response.error", "error":
			if uerr := upstreamLLMError(ev.Error); uerr != nil {
				return nil, uerr
			}
			if strings.Contains(strings.ToLower(ev.Code), "content_filter") {
				return nil, refusalErr(RefusalLLMContentFiltered, "内容被上游安全策略拦截（content_filter）："+ev.Message)
			}
			msg := strings.TrimSpace(ev.Message)
			if ev.Response != nil {
				if uerr := upstreamLLMError(ev.Response.Error); uerr != nil {
					return nil, uerr
				}
			}
			if msg == "" {
				msg = extractErr([]byte(data))
			}
			return nil, fmt.Errorf("LLM 流式返回错误：%s", msg)
		}
		if done {
			break
		}
	}
	if err := sc.Err(); err != nil && !done {
		return nil, fmt.Errorf("流式响应中断: %w", err)
	}
	if !done {
		// 正常 EOF 但从未收到完成事件/[DONE]：契约开启时拒收（eof_without_marker）。
		if rerr := streamEOFReject(contractEnabled); rerr != nil {
			return nil, rerr
		}
	}
	// Responses 的 [DONE] 只是传输层哨兵，不能证明模型完成。契约开启时必须同时看到
	// completed/done 终态事件和其 response.status=completed；空状态、冲突状态均拒收。
	if rerr := responsesStreamStatusReject(contractEnabled, terminalEvent, doneStatus, incompleteReason); rerr != nil {
		return nil, rerr
	}
	content := sb.String()
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("LLM 返回空内容")
	}
	if usage.TotalTokens == 0 {
		usage = estimateUsage(p.Messages, content)
	}
	return &chatResult{Content: content, Usage: usage, LatencyMs: time.Since(start).Milliseconds(), FirstChunkMs: firstChunkMs, FinishReason: doneStatus}, nil
}
