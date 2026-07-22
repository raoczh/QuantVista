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
	aiCallTimeout       = 90 * time.Second
	aiRetryBackoff      = 800 * time.Millisecond
	aiResponseBodyLimit = 1 << 20
)

// 包级复用两个 client（allowPrivate 两态），避免每次调用重建 Transport——
// repair/panel 会对同一上游连发多次请求，复用连接池省去反复 TLS 握手。
var (
	aiClientPublic  = common.SafeHTTPClient(aiCallTimeout, false)
	aiClientPrivate = common.SafeHTTPClient(aiCallTimeout, true)
	// 流式专用 client：流式总时长由内容长度决定，client.Timeout（读完 body 才算结束）
	// 会切断长回答，故不设整体超时；改设 ResponseHeaderTimeout 防连接挂死，
	// 读取阶段由调用方 context（或 ensureDeadline 兜底）控制。
	aiStreamClientPublic  = newAIStreamClient(false)
	aiStreamClientPrivate = newAIStreamClient(true)
)

func newAIStreamClient(allowPrivate bool) *http.Client {
	c := common.SafeHTTPClient(0, allowPrivate)
	if tr, ok := c.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = aiCallTimeout
	}
	return c
}

func aiHTTPClient(allowPrivate bool) *http.Client {
	if allowPrivate {
		return aiClientPrivate
	}
	return aiClientPublic
}

func aiStreamHTTPClient(allowPrivate bool) *http.Client {
	if allowPrivate {
		return aiStreamClientPrivate
	}
	return aiStreamClientPublic
}

// aiStreamMaxDuration 流式调用无 client.Timeout 兜底，若 ctx 也没带 deadline，
// 由 ensureDeadline 补这个上限，防止对端停止发数据后连接无限悬挂。
const aiStreamMaxDuration = 10 * time.Minute

// ensureDeadline ctx 无 deadline 时补一个流式上限（有 deadline 则原样返回）。
func ensureDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, aiStreamMaxDuration)
}

// readLLMResponseBody 对整包成功响应做有界、可验证读取。旧实现忽略 ReadAll 错误，
// 当 Content-Length 未读满但前缀恰好仍是合法 JSON 时会误收半截；limit+1 用于区分
// “恰好 1MB”与“已超过 1MB 被本地截断”。契约关闭时保持旧的截断/忽略读错语义。
func readLLMResponseBody(r io.Reader, contractEnabled bool) ([]byte, error) {
	raw, readErr := io.ReadAll(io.LimitReader(r, aiResponseBodyLimit+1))
	if len(raw) > aiResponseBodyLimit {
		raw = raw[:aiResponseBodyLimit]
		if rerr := responseBodyIntegrityReject(contractEnabled, "超过 1MB 上限"); rerr != nil {
			return raw, rerr
		}
	}
	if readErr != nil {
		if rerr := responseBodyIntegrityReject(contractEnabled, readErr.Error()); rerr != nil {
			return raw, rerr
		}
	}
	return raw, nil
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
	// Repair 本次请求是否为 repair 轮（首轮之后的结构修复请求）：契约开启时温度固定 0
	//（llm_contract.go）。由各模块 repair 循环在 attempt>0 时置 true；次数上限仍归模块管。
	Repair bool
	// accuracyContract 是公开出口捕获的本次调用开关快照；nil 仅用于绕过出口的内部测试。
	// 不进入上游 payload，也不写入业务 prompt。
	accuracyContract *bool
	// effectiveJSONMode 审计观测：本次上游请求最终是否携带 JSON 结构化约束
	//（response_format/text.format）。公开出口按 JSONMode 初始化；端点不支持而回落
	// 时由内部四处回落点置 false——writeLLMCallLog 据此记录实际生效的 structured_method。
	effectiveJSONMode *bool
	// omitTemperature / omitMaxTokens 参数省略观测（P0-5 修复批）：声明化路由或运行时
	// fallback 判定上游不接受该参数时置位，四条请求路径构造 payload 时据此省略字段
	//（此前 temperature/max_tokens 无条件发送，能力维度只是结构字段）。指针共享：
	// 公开出口初始化，内部 fallback 点置位后重试与审计均可见。
	omitTemperature *bool
	omitMaxTokens   *bool
	// finalRequestBody 最终实际发送的请求体 JSON（审计观测）：每次真正发出 HTTP 请求前
	// 由各路径写入（fallback 重试覆盖为最终形态），writeLLMCallLog 落 RequestBody——
	// 审计保存上游真实收到的完整 payload（含最终有效 temperature、实际 token 上限与
	// 结构化参数）。API key 走 Authorization 头、绝不进 body，此处天然无敏感凭据。
	finalRequestBody *string
	Meta             chatMeta
}

// markJSONModeDropped JSON mode 回落点统一记录（nil 安全：绕过公开出口的路径无观测指针）。
// 同时回流到 run 级观测（Meta.StructuredDropped）供业务 manifest 记录最终实际生效形态。
func (p chatParams) markJSONModeDropped() {
	if p.effectiveJSONMode != nil {
		*p.effectiveJSONMode = false
	}
	if p.Meta.StructuredDropped != nil {
		*p.Meta.StructuredDropped = true
	}
}

// markTemperatureOmitted / temperatureOmitted 温度参数省略观测（nil 安全）。
func (p chatParams) markTemperatureOmitted() {
	if p.omitTemperature != nil {
		*p.omitTemperature = true
	}
}

func (p chatParams) temperatureOmitted() bool {
	return p.omitTemperature != nil && *p.omitTemperature
}

// markMaxTokensOmitted / maxTokensOmitted token 上限参数省略观测（nil 安全）。
func (p chatParams) markMaxTokensOmitted() {
	if p.omitMaxTokens != nil {
		*p.omitMaxTokens = true
	}
}

func (p chatParams) maxTokensOmitted() bool {
	return p.omitMaxTokens != nil && *p.omitMaxTokens
}

// noteFinalRequestBody 记录最终实际发送的请求体（nil 安全；fallback 重试覆盖）。
func (p chatParams) noteFinalRequestBody(body []byte) {
	if p.finalRequestBody != nil {
		*p.finalRequestBody = string(body)
	}
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
	// FirstChunkMs 流式路径首个 data 块到达耗时（非流式恒 0）。
	// ≈LatencyMs 说明上游忽略 stream 整包返回（假流式），是区分
	// 「模型生成慢」与「网关不透传流」的关键观测。
	FirstChunkMs int64
	// FinishReason 上游报告的完成状态原始值：chat=finish_reason（stop/length/…），
	// responses=status（completed/incomplete/…）。完整性门禁（llm_contract.go）据此拒收
	// 截断/拦截响应；空串=上游未报告。
	FinishReason string
}

// capModuleTokens 模块级输出预算：上游普遍存在 60s 级整包超时，单次生成时长
// 主要由输出 token 数决定——内部 JSON 任务（日报复盘/推荐/复核）按模块钳制
// 输出上限，避免用户全局 max_tokens 过大拖死单次调用。用户配置更小时以用户
// 配置为准；未配置（0）时用模块上限。
func capModuleTokens(userMax, moduleCap int) int {
	if moduleCap <= 0 {
		return userMax
	}
	if userMax > 0 && userMax < moduleCap {
		return userMax
	}
	return moduleCap
}

// chatCompletion 发起一次补全。JSONMode=true 时先带 response_format 请求；
// 若服务端因不支持该字段返回 4xx，则去掉 response_format 重试一次（fallback，靠 prompt 约束 JSON）。
// EndpointType=responses 时分流到 /v1/responses 适配（ai_client_responses.go），返回语义一致。
func chatCompletion(ctx context.Context, p chatParams) (res *chatResult, err error) {
	p = applyAccuracyContract(p) // ac1 契约注入+温度钳制在审计之前——RequestBody 记录上游真实收到的形态
	p = initCallObservers(p)
	p = applyCapabilityRouting(p) // P0-5 声明化路由：已知不支持的结构化/参数维度直接省略
	started := time.Now()
	streamed := true // 默认先走流式；回落非流式时置 false——审计必须记录实际请求形态而非入口意图
	defer func() {
		err = classifyLLMError(err)
		writeLLMCallLog(p, streamed, res, err, time.Since(started))
	}()
	return chatCompletionInner(ctx, p, &streamed)
}

// initCallObservers 公开出口初始化本次调用的观测指针（structured/temperature/max_tokens
// 的实际生效形态 + 最终请求体），供内部 fallback 点回写、审计层读取。
func initCallObservers(p chatParams) chatParams {
	effJSON := p.JSONMode
	p.effectiveJSONMode = &effJSON
	omitTemp, omitMaxTok := false, false
	p.omitTemperature, p.omitMaxTokens = &omitTemp, &omitMaxTok
	finalBody := ""
	p.finalRequestBody = &finalBody
	return p
}

func chatCompletionInner(ctx context.Context, p chatParams, streamed *bool) (*chatResult, error) {
	// 流式优先（2026-07-10）：非流式要等模型全部生成完才返回首字节，生成一旦超过
	// 上游网关的整包超时（部分中转站为 60s 且不可调）连接就被掐断；流式期间 chunk
	// 持续到达可绕开该限制。这里 onDelta=nil 仅在本端聚合，对调用方语义不变。
	// 注意：流式只解决「空闲/整包超时」，上游若是绝对总时长限制则只能靠输出预算
	//（capModuleTokens）把单次生成压进窗口内。
	res, err := chatCompletionStreamInner(ctx, p, nil)
	if err == nil {
		return res, nil
	}
	if !looksLikeStreamUnsupported(err) {
		// audit outcome：完整性拒收等错误可能带出真实上游结果（正文/usage/原始终态），
		// 随错误原样上抛供审计保留，调用方按错误判失败。
		return res, err
	}
	// 少数端点不支持 stream：回落非流式一次（此时仍受上游整包超时约束，无能为力）。
	if streamed != nil {
		*streamed = false
	}
	return chatCompletionPlain(ctx, p)
}

// looksLikeStreamUnsupported 流式请求的失败是否因上游不支持 stream（此时值得回落
// 非流式）。仅匹配 4xx 错误文案中的 stream 字样；"流式响应中断"等中途失败不回落
// ——半截内容不可续，且非流式重发会让调用方等两轮。
func looksLikeStreamUnsupported(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 4") && strings.Contains(msg, "stream")
}

// chatCompletionPlain 非流式补全（流式不可用时的回落路径，原 chatCompletionInner 主体）。
func chatCompletionPlain(ctx context.Context, p chatParams) (*chatResult, error) {
	if p.isResponsesEndpoint() {
		return responsesCompletion(ctx, p)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("Base URL 非法（仅支持 http/https）")
	}

	res, status, raw, latency, err := doChat(ctx, p, p.JSONMode)
	if err != nil {
		return res, err
	}
	// 参数兼容性 fallback（P0-5 修复批）：4xx 错误明确指向 response_format/temperature/
	// max_tokens 参数时，去掉命中参数重试（至多 2 轮，覆盖上游逐个报参数错的形态）。
	// 能力观察只在「去参后的请求确认成功」时统一提交——fallback 失败说明错误另有原因，
	// 提交观察会让一次误判污染该目标 12h 的能力状态。
	jsonOn := p.JSONMode
	var capConfirms []func()
	for retry := 0; retry < 2 && status >= 400 && status < 500 && ctx.Err() == nil; retry++ {
		changed := false
		switch {
		case jsonOn && looksLikeUnsupportedJSONMode(status, raw):
			jsonOn = false
			p.markJSONModeDropped()
			reason := fmt.Sprintf("chat 非流式 HTTP %d 拒绝 response_format", status)
			capConfirms = append(capConfirms, func() { p.observeJSONModeUnsupported(reason) })
			changed = true
		case !p.temperatureOmitted() && looksLikeUnsupportedTemperature(status, raw):
			p.markTemperatureOmitted()
			reason := fmt.Sprintf("chat 非流式 HTTP %d 拒绝 temperature", status)
			capConfirms = append(capConfirms, func() { p.observeTemperatureUnsupported(reason) })
			changed = true
		case p.MaxTokens > 0 && !p.maxTokensOmitted() && looksLikeUnsupportedMaxTokens(status, raw):
			p.markMaxTokensOmitted()
			reason := fmt.Sprintf("chat 非流式 HTTP %d 拒绝 max_tokens", status)
			capConfirms = append(capConfirms, func() { p.observeMaxTokensUnsupported(reason) })
			changed = true
		}
		if !changed {
			break
		}
		res, status, raw, latency, err = doChat(ctx, p, jsonOn)
		if err != nil {
			return res, err
		}
	}
	if status == http.StatusOK {
		for _, confirm := range capConfirms {
			confirm()
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("LLM 返回 HTTP %d%s：%s", status, statusHint(status), extractErr(raw))
	}
	// 完整性门禁（契约开启时）：截断/拦截响应即使带部分内容也拒收——但审计结果（res：
	// 上游真实正文/usage/原始终态）随错误一并返回，writeLLMCallLog 与 run.record 据此
	// 保留真实审计（P0-8 修复批 audit outcome）；调用方按 err 判定失败，不消费 res 内容。
	if rerr := chatFinishReject(p.accuracyContractEnabled(), res.FinishReason, false); rerr != nil {
		res.LatencyMs = latency
		return res, rerr
	}
	if strings.TrimSpace(res.Content) == "" {
		res.LatencyMs = latency
		return res, errors.New("LLM 返回空内容")
	}
	// 部分兼容端点不回 usage：按字符粗估兜底（≈2 字符/token），仅作用量审计、不影响次数配额。
	if res.Usage.TotalTokens == 0 {
		res.Usage = estimateUsage(p.Messages, res.Content)
	}
	res.LatencyMs = latency
	return res, nil
}

// doChat 执行单次 HTTP 调用，返回解析后的结果、HTTP 状态码、原始响应体、耗时。
// 解析/门禁类错误发生时结果仍尽量带出（audit outcome：正文/usage/原始终态供审计保留）。
func doChat(ctx context.Context, p chatParams, jsonMode bool) (*chatResult, int, []byte, int64, error) {
	endpoint := chatCompletionsURL(p.BaseURL)

	payload := map[string]any{
		"model":    p.Model,
		"messages": p.Messages,
		"stream":   false,
	}
	if !p.temperatureOmitted() {
		payload["temperature"] = p.Temperature
	}
	if p.MaxTokens > 0 && !p.maxTokensOmitted() {
		payload["max_tokens"] = p.MaxTokens
	}
	if jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, _ := json.Marshal(payload)
	p.noteFinalRequestBody(body)

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
	// 限制响应体大小并拒收传输中断/本地截断的成功体。
	contractEnabled := p.accuracyContractEnabled()
	raw, readErr := readLLMResponseBody(resp.Body, contractEnabled)

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, raw, latency, nil
	}
	if readErr != nil {
		return nil, resp.StatusCode, raw, latency, readErr
	}

	res, perr := parseChatResponse(raw, resp.StatusCode, endpoint, contractEnabled)
	if perr != nil {
		// audit outcome：拒收/解析错误时 res 可能仍带真实正文与 usage，原样带出供审计。
		return res, resp.StatusCode, raw, latency, perr
	}
	return res, resp.StatusCode, raw, latency, nil
}

// parseChatResponse 解析 chat/completions 的 200 响应体（非流式整包形态）。
// 返回错误时结果仍尽量非 nil（audit outcome：已解析出的正文/usage/finish_reason 供审计
// 保留真实上游结果——调用方以错误为准判失败，不消费结果内容）。
func parseChatResponse(raw []byte, status int, endpoint string, contractEnabled bool) (*chatResult, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage chatUsage       `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if looksLikeHTML(raw) {
			// 典型场景：Base URL 指向 new-api/one-api 等站点的非 API 路径，命中前端 SPA fallback
			// 返回 200 + index.html。裸报 invalid character '<' 用户无从下手，这里直接指出端点。
			return nil, fmt.Errorf(
				"LLM 返回了网页而非 JSON（HTTP %d）：%s 不是有效的 API 端点，请检查 Base URL——填服务根地址（自动补 /v1/chat/completions）或以 /v1 结尾的地址均可", status, endpoint)
		}
		return nil, fmt.Errorf("解析 LLM 响应失败: %w（响应开头: %s）", err, bodySnippet(raw))
	}
	content := ""
	finish := ""
	if len(parsed.Choices) > 0 {
		content = parsed.Choices[0].Message.Content
		finish = parsed.Choices[0].FinishReason
	}
	res := &chatResult{Content: content, Usage: parsed.Usage, FinishReason: finish}
	// 部分兼容网关把错误包在 HTTP 200 中；error 与 choices 同时出现也必须以 error 为准，
	// 否则可能把上游明确失败后的占位/半截 choices 当成功。
	if contractEnabled {
		if uerr := upstreamLLMError(parsed.Error); uerr != nil {
			return res, uerr
		}
	}
	if len(parsed.Choices) > 0 {
		if contractEnabled && strings.TrimSpace(parsed.Choices[0].Message.Refusal) != "" {
			return res, refusalErr(RefusalLLMContentFiltered, "模型拒绝生成该内容："+parsed.Choices[0].Message.Refusal)
		}
		// 上游安全策略拦截（finish_reason=content_filter）且无内容：给明确文案而非笼统"空内容"。
		if content == "" && finish == "content_filter" {
			return res, errors.New("内容被上游安全策略拦截（finish_reason=content_filter）")
		}
	}
	return res, nil
}

// looksLikeUnsupportedJSONMode 判断 4xx 是否因不支持 response_format / text.format 引起。
// P0-5 修复批收紧：错误文案必须**明确指向结构化字段**（response_format/text.format/
// json_object/json mode）才判定——此前裸匹配 "not support" 会把「model not supported」
// 「stream not supported」「temperature not supported」等任意 4xx 全部误判成 JSON mode
// 不支持，触发无意义的降级重试并污染能力观察。
func looksLikeUnsupportedJSONMode(status int, raw []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	msg := strings.ToLower(string(raw))
	return strings.Contains(msg, "response_format") ||
		strings.Contains(msg, "text.format") ||
		strings.Contains(msg, "json_object") ||
		strings.Contains(msg, "json mode") ||
		strings.Contains(msg, "json_mode")
}

// unsupportedParamHints 参数类 4xx 的「不被接受」措辞集合：字段名之外还须命中其一，
// 防把「值超限」（max_tokens is too large）、字段回显等误判成参数不支持。
var unsupportedParamHints = []string{
	"not support", "unsupported", "unknown parameter", "unrecognized", "unexpected", "invalid parameter", "extra_forbidden",
}

func matchUnsupportedParam(msg string, fieldHints ...string) bool {
	hitField := false
	for _, f := range fieldHints {
		if strings.Contains(msg, f) {
			hitField = true
			break
		}
	}
	if !hitField {
		return false
	}
	for _, h := range unsupportedParamHints {
		if strings.Contains(msg, h) {
			return true
		}
	}
	return false
}

// looksLikeUnsupportedTemperature 判断 4xx 是否因上游不接受 temperature 参数
// （如 OpenAI o 系列仅允许默认温度、部分 reasoning 模型直接拒绝该字段）。
func looksLikeUnsupportedTemperature(status int, raw []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	return matchUnsupportedParam(strings.ToLower(string(raw)), "temperature")
}

// looksLikeUnsupportedMaxTokens 判断 4xx 是否因上游不接受 max_tokens/max_output_tokens
// 参数本身（如要求改用 max_completion_tokens 的模型）。「值超限」类错误不算——去掉
// 参数解决不了值超限，反而丢失输出预算。
func looksLikeUnsupportedMaxTokens(status int, raw []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	msg := strings.ToLower(string(raw))
	if strings.Contains(msg, "too large") || strings.Contains(msg, "maximum value") || strings.Contains(msg, "exceed") {
		return false
	}
	return matchUnsupportedParam(msg, "max_tokens", "max_output_tokens")
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
// （合法 JSON 不会以 '<' 开头）。
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
// JSONMode 同非流式：带 response_format，遇不支持的 4xx 去掉重试一次靠 prompt 约束。
// 不做状态码重试（流一旦建立，中断即失败——半截内容重试会让用户看到重复文本）；
// 建立连接前的错误分类与非流式一致。上游忽略 stream 参数返回整包 JSON 时按非流式解析兼容。
// EndpointType=responses 时分流到 /v1/responses 适配。
func chatCompletionStream(ctx context.Context, p chatParams, onDelta func(string)) (res *chatResult, err error) {
	p = applyAccuracyContract(p)
	p = initCallObservers(p)
	p = applyCapabilityRouting(p) // P0-5 声明化路由：已知不支持的结构化/参数维度直接省略
	started := time.Now()
	defer func() {
		err = classifyLLMError(err)
		writeLLMCallLog(p, true, res, err, time.Since(started))
	}()
	return chatCompletionStreamInner(ctx, p, onDelta)
}

func chatCompletionStreamInner(ctx context.Context, p chatParams, onDelta func(string)) (*chatResult, error) {
	ctx, cancel := ensureDeadline(ctx)
	defer cancel()
	if p.isResponsesEndpoint() {
		return responsesCompletionStream(ctx, p, onDelta)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("Base URL 非法（仅支持 http/https）")
	}
	endpoint := chatCompletionsURL(p.BaseURL)

	buildBody := func(withUsageOpt, withJSON bool) []byte {
		payload := map[string]any{
			"model":    p.Model,
			"messages": p.Messages,
			"stream":   true,
		}
		if !p.temperatureOmitted() {
			payload["temperature"] = p.Temperature
		}
		if p.MaxTokens > 0 && !p.maxTokensOmitted() {
			payload["max_tokens"] = p.MaxTokens
		}
		if withUsageOpt {
			payload["stream_options"] = map[string]bool{"include_usage": true}
		}
		if withJSON {
			payload["response_format"] = map[string]string{"type": "json_object"}
		}
		b, _ := json.Marshal(payload)
		p.noteFinalRequestBody(b)
		return b
	}
	usageOpt, jsonOn := true, p.JSONMode
	body := buildBody(usageOpt, jsonOn)

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
		// 建流前的瞬时抖动状态码（429/500/502/503）重试一次——此时尚无任何内容，
		// 无重复文本之虞，与非流式的重试纪律一致。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		time.Sleep(aiRetryBackoff)
		if r2, e2 := send(); e2 == nil {
			resp = r2
		}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// 部分兼容端点不认识 stream_options / response_format / temperature / max_tokens：
		// 识别后去掉命中字段重试一次；能力观察只在重试建流成功（HTTP 200）后提交
		//（fallback 失败说明错误另有原因，不得据 4xx 字样污染能力状态——P0-5 修复批）。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		dropUsage := strings.Contains(strings.ToLower(string(raw)), "stream_options")
		dropJSON := jsonOn && looksLikeUnsupportedJSONMode(resp.StatusCode, raw)
		dropTemp := !p.temperatureOmitted() && looksLikeUnsupportedTemperature(resp.StatusCode, raw)
		dropMaxTok := p.MaxTokens > 0 && !p.maxTokensOmitted() && looksLikeUnsupportedMaxTokens(resp.StatusCode, raw)
		if (dropUsage || dropJSON || dropTemp || dropMaxTok) && ctx.Err() == nil {
			usageOpt = usageOpt && !dropUsage
			jsonOn = jsonOn && !dropJSON
			var capConfirms []func()
			if dropJSON {
				p.markJSONModeDropped()
				reason := fmt.Sprintf("chat 流式 HTTP %d 拒绝 response_format", resp.StatusCode)
				capConfirms = append(capConfirms, func() { p.observeJSONModeUnsupported(reason) })
			}
			if dropTemp {
				p.markTemperatureOmitted()
				reason := fmt.Sprintf("chat 流式 HTTP %d 拒绝 temperature", resp.StatusCode)
				capConfirms = append(capConfirms, func() { p.observeTemperatureUnsupported(reason) })
			}
			if dropMaxTok {
				p.markMaxTokensOmitted()
				reason := fmt.Sprintf("chat 流式 HTTP %d 拒绝 max_tokens", resp.StatusCode)
				capConfirms = append(capConfirms, func() { p.observeMaxTokensUnsupported(reason) })
			}
			body = buildBody(usageOpt, jsonOn)
			if r2, e2 := send(); e2 == nil {
				resp = r2
				if resp.StatusCode == http.StatusOK {
					for _, confirm := range capConfirms {
						confirm()
					}
				}
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
		// 上游忽略 stream 参数直接返回整包 JSON（部分网关形态）：按非流式解析。
		contractEnabled := p.accuracyContractEnabled()
		raw, readErr := readLLMResponseBody(br, contractEnabled)
		if readErr != nil {
			return nil, readErr
		}
		res, perr := parseChatResponse(raw, resp.StatusCode, endpoint, contractEnabled)
		if perr != nil {
			return res, perr // audit outcome：已解析出的正文/usage 随错误带出
		}
		// 整包回落同样过完整性门禁（截断的整包 JSON 若碰巧仍合法解析，靠 finish_reason 拦）。
		if rerr := chatFinishReject(contractEnabled, res.FinishReason, false); rerr != nil {
			return res, rerr
		}
		return finishStreamResult(p, res.Content, res.Usage, res.FinishReason, start, onDelta)
	}

	// SSE 逐行状态机：每行形如 "data: {...}"；空行为事件分隔；"data: [DONE]" 结束。
	var sb strings.Builder
	var usage chatUsage
	var firstChunkMs int64
	finishReason := "" // 最后一个非空 finish_reason（stop/length/content_filter/…）
	sawDoneMarker := false
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // 单行上限 1MB（长 delta/大 usage 容错）
	done := false
	contractEnabled := p.accuracyContractEnabled()
	// partialResult 拒收/中断路径的审计结果（audit outcome）：把已聚合的正文、上游报告的
	// usage 与原始终态随错误一并带出——writeLLMCallLog 与 run.record 据此保留真实上游
	// 结果，业务调用方按错误判失败、不消费内容。usage 不做粗估（失败路径只记上游真值）。
	partialResult := func() *chatResult {
		return &chatResult{Content: sb.String(), Usage: usage,
			LatencyMs: time.Since(start).Milliseconds(), FirstChunkMs: firstChunkMs, FinishReason: finishReason}
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") { // 空行/注释行
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		if firstChunkMs == 0 {
			// 首个 data 行到达时刻（含 role 前导块）：诊断上游首 token 延迟的观测锚点。
			if firstChunkMs = time.Since(start).Milliseconds(); firstChunkMs == 0 {
				firstChunkMs = 1 // 亚毫秒（本地假服务）与「未记录」区分
			}
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			sawDoneMarker = true
			done = true
			break
		}
		var chunk struct {
			Choices *[]struct {
				Delta struct {
					Content string `json:"content"`
					Refusal string `json:"refusal"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *chatUsage      `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
			if rerr := streamProtocolReject(contractEnabled, "Chat SSE JSON 解析失败"); rerr != nil {
				return partialResult(), rerr
			}
			continue // 关闭契约时保留旧兼容路径
		}
		if contractEnabled {
			if uerr := upstreamLLMError(chunk.Error); uerr != nil {
				return partialResult(), uerr
			}
		}
		if chunk.Choices == nil {
			if rerr := streamProtocolReject(contractEnabled, "Chat SSE 事件缺少 choices"); rerr != nil {
				return partialResult(), rerr
			}
			continue
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			usage = *chunk.Usage
		}
		for _, c := range *chunk.Choices {
			if contractEnabled && strings.TrimSpace(c.Delta.Refusal) != "" {
				return partialResult(), refusalErr(RefusalLLMContentFiltered, "模型拒绝生成该内容："+c.Delta.Refusal)
			}
			if c.Delta.Content != "" {
				sb.WriteString(c.Delta.Content)
				if onDelta != nil {
					onDelta(c.Delta.Content)
				}
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finishReason = *c.FinishReason
				done = true
			}
		}
		if done {
			break
		}
	}
	if err := sc.Err(); err != nil && !done {
		// 流中断且未正常收尾：已有内容也判失败（半截回答不落库，让调用方决定重试）。
		return partialResult(), fmt.Errorf("流式响应中断: %w", err)
	}
	if !done {
		// 正常 EOF 但从未收到 [DONE]/finish_reason（网关在上游超时后干净关连接的典型形态）：
		// 契约开启时拒收（eof_without_marker），关闭时保持旧行为当成功。
		if rerr := streamEOFReject(contractEnabled); rerr != nil {
			return partialResult(), rerr
		}
	}
	if rerr := chatFinishReject(contractEnabled, finishReason, sawDoneMarker); rerr != nil {
		return partialResult(), rerr
	}
	content := sb.String()
	if strings.TrimSpace(content) == "" {
		return partialResult(), errors.New("LLM 返回空内容")
	}
	if usage.TotalTokens == 0 {
		usage = estimateUsage(p.Messages, content)
	}
	return &chatResult{Content: content, Usage: usage, LatencyMs: time.Since(start).Milliseconds(), FirstChunkMs: firstChunkMs, FinishReason: finishReason}, nil
}

// finishStreamResult 流式路径拿到整包内容（上游忽略 stream）时的统一收尾：
// 空内容校验、usage 粗估兜底、onDelta 一次性吐出全文（保持流式调用方能看到内容）。
// FirstChunkMs 记为整包到达时刻（≈总耗时）——审计里两值几乎相等即可识别假流式网关。
func finishStreamResult(p chatParams, content string, usage chatUsage, finishReason string, start time.Time, onDelta func(string)) (*chatResult, error) {
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("LLM 返回空内容")
	}
	if usage.TotalTokens == 0 {
		usage = estimateUsage(p.Messages, content)
	}
	if onDelta != nil {
		onDelta(content)
	}
	ms := time.Since(start).Milliseconds()
	if ms == 0 {
		ms = 1
	}
	return &chatResult{Content: content, Usage: usage, LatencyMs: ms, FirstChunkMs: ms, FinishReason: finishReason}, nil
}

// isSSEResponse 判断 200 响应是否为 SSE 流：优先看 Content-Type；部分网关不回标准
// Content-Type，再 peek 响应体开头——SSE 以 "data:"/"event:"/":" 行开头，整包 JSON 以 '{' 开头。
func isSSEResponse(resp *http.Response, br *bufio.Reader) bool {
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return true
	}
	b, _ := br.Peek(64)
	s := strings.TrimLeft(string(b), " \t\r\n")
	return strings.HasPrefix(s, "data:") || strings.HasPrefix(s, "event:") || strings.HasPrefix(s, ":")
}
