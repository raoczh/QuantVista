package service

import (
	"encoding/json"
	"strings"
	"unicode"
)

// 推理/思考侧适配（对标 new-api relaykit 的请求转换与响应归一实践）：
//   - 请求侧不主动注入 reasoning_effort/thinking_budget 等思考控制参数——new-api 作为
//     中继同样只透传客户端显式携带的字段，客户端不发即用网关/模型默认行为；本项目
//     有意保持该默认（思考控制交给网关侧模型名约定，如 -thinking/-nothinking 后缀）。
//   - 已知按 max_completion_tokens 计数的模型（o 系列/GPT-5）主动切换字段，并由
//     requestTokenBudget 为隐藏 reasoning token 预留空间（new-api 是原值搬移，正文
//     预算会被思考挤占——这里显式补偿）。
//   - 响应侧归一 reasoning_content/<think> 思考文本与 usage 明细（reasoning/cached
//     token），审计据此把 finish_state=length 拆分为「思考吃光预算/正文写满」两类归因。

// inputTokenDetails / outputTokenDetails 同时服务 Chat 与 Responses。兼容网关可能同时
// 返回两套字段名，归一时取较大值而不相加，避免重复记账。
type inputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type outputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// UnmarshalJSON 把 usage 的标准嵌套明细归一到 chatUsage 平铺字段。别名字段用于兼容
// DeepSeek/new-api 等常见网关，不改变上游的 prompt/completion/total 主计数。
func (u *chatUsage) UnmarshalJSON(data []byte) error {
	type rawUsage chatUsage
	var base rawUsage
	if err := json.Unmarshal(data, &base); err != nil {
		return err
	}
	var aliases struct {
		InputTokensDetails   inputTokenDetails  `json:"input_tokens_details"`
		OutputTokensDetails  outputTokenDetails `json:"output_tokens_details"`
		PromptCacheHitTokens int                `json:"prompt_cache_hit_tokens"`
		CachedTokens         int                `json:"cached_tokens"`
	}
	if err := json.Unmarshal(data, &aliases); err != nil {
		return err
	}
	*u = chatUsage(base)
	u.ReasoningTokens = maxInt(u.CompletionTokensDetails.ReasoningTokens, aliases.OutputTokensDetails.ReasoningTokens)
	u.CachedTokens = maxInt(u.PromptTokensDetails.CachedTokens, aliases.InputTokensDetails.CachedTokens,
		aliases.PromptCacheHitTokens, aliases.CachedTokens)
	return nil
}

func maxInt(values ...int) int {
	max := 0
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}

// hasReportedUsage 判断上游是否提供了任何真实用量。reasoning/cached 是
// completion/prompt 的分量，也足以证明 usage 并非缺失。
func (u chatUsage) hasReportedUsage() bool {
	return u.TotalTokens > 0 || u.PromptTokens > 0 || u.CompletionTokens > 0 ||
		u.ReasoningTokens > 0 || u.CachedTokens > 0
}

// fillMissingUsageTotal 只补上游缺失的 total。cached/reasoning 分别是 prompt/completion
// 的子分量：父分量存在时以父分量为准，父分量缺失时才用对应子分量补已知下界。
func (u *chatUsage) fillMissingUsageTotal() {
	if u == nil || u.TotalTokens > 0 {
		return
	}
	promptTokens := u.PromptTokens
	if promptTokens == 0 {
		promptTokens = u.CachedTokens
	}
	completionTokens := u.CompletionTokens
	if completionTokens == 0 {
		completionTokens = u.ReasoningTokens
	}
	u.TotalTokens = promptTokens + completionTokens
}

// mergeChatUsage 合并流式分块里可能拆开的 usage。各块常是累计快照或分别携带部分
// 字段，因此逐字段取大而不累计。上游非零 total 是真实事实，合并阶段只在真实
// total 之间取大；等所有块收齐后再由 fillMissingUsageTotal 补“始终为零”的 total。
func mergeChatUsage(dst *chatUsage, src chatUsage) {
	if dst == nil {
		return
	}
	dst.PromptTokens = maxInt(dst.PromptTokens, src.PromptTokens)
	dst.CompletionTokens = maxInt(dst.CompletionTokens, src.CompletionTokens)
	dst.ReasoningTokens = maxInt(dst.ReasoningTokens, src.ReasoningTokens)
	dst.CachedTokens = maxInt(dst.CachedTokens, src.CachedTokens)
	dst.TotalTokens = maxInt(dst.TotalTokens, src.TotalTokens)
}

// usageOrEstimate 统一 Chat/Responses、流式/非流式的成功收尾语义：有任何上游
// 真值就逐项保留并只补 total；确实完全没有 usage 时才做字符估算。
func usageOrEstimate(messages []chatMessage, content string, usage chatUsage) chatUsage {
	if !usage.hasReportedUsage() {
		return estimateUsage(messages, content)
	}
	usage.fillMissingUsageTotal()
	return usage
}

func addChatUsage(dst *chatUsage, src chatUsage) {
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.ReasoningTokens += src.ReasoningTokens
	dst.CachedTokens += src.CachedTokens
	dst.TotalTokens += src.TotalTokens
}

func chatResultContent(res *chatResult) string {
	if res == nil {
		return ""
	}
	return res.Content
}

// applyReasoningTokenField 对齐 new-api 的 o 系列/GPT-5 字段挪移（max_tokens →
// max_completion_tokens），payload 层由 requestTokenBudget 为 reasoning token 额外预留。
func applyReasoningTokenField(p chatParams) chatParams {
	if p.MaxTokens > 0 && !p.isResponsesEndpoint() && requiresMaxCompletionTokens(p.Model) {
		p.markMaxCompletionTokens()
	}
	return p
}

func requiresMaxCompletionTokens(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	return strings.HasPrefix(name, "gpt-5") || strings.HasPrefix(name, "o1") ||
		strings.HasPrefix(name, "o3") || strings.HasPrefix(name, "o4")
}

func (p chatParams) promptCacheKey() string {
	if (p.Meta.Module != "recommendation" && p.Meta.Module != "analysis") ||
		strings.TrimSpace(p.Meta.PromptVersion) == "" {
		return ""
	}
	return p.Meta.Module + ":" + strings.TrimSpace(p.Meta.PromptVersion)
}

// addPromptCacheField system prompt 稳定的模块携带 prompt_cache_key（OpenAI 兼容缓存
// 亲和提示；上游 4xx 拒绝该参数时由调用路径回落重试并去掉）。
func (p chatParams) addPromptCacheField(payload map[string]any, include bool) {
	if !include {
		return
	}
	if key := p.promptCacheKey(); key != "" {
		payload["prompt_cache_key"] = key
	}
}

func joinReasoningContent(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonEmpty = append(nonEmpty, strings.TrimSpace(part))
		}
	}
	return strings.Join(nonEmpty, "\n")
}

// thinkStreamFilter 只分离首个非空白位置开始、且完整闭合的 provider think 块。
// 在确认闭合前必须暂存原文：未闭合时 Flush 将整段按 visible 原样返回；一旦确认
// 开头不是 think 块，后续 chunk 全部直通，正文或 JSON 中的字面标签不会被误删。
type thinkStreamFilter struct {
	pending  string
	resolved bool
}

func (f *thinkStreamFilter) Push(chunk string) (visible, reasoning string) {
	if f.resolved {
		return chunk, ""
	}
	f.pending += chunk
	trimmed := strings.TrimLeftFunc(f.pending, unicode.IsSpace)
	if trimmed == "" {
		return "", ""
	}

	const openTag = "<think>"
	if len(trimmed) < len(openTag) {
		if strings.EqualFold(trimmed, openTag[:len(trimmed)]) {
			return "", ""
		}
		return f.resolveVisible(), ""
	}
	if !strings.EqualFold(trimmed[:len(openTag)], openTag) {
		return f.resolveVisible(), ""
	}

	const closeTag = "</think>"
	body := trimmed[len(openTag):]
	closeAt := indexASCIIFold(body, closeTag)
	if closeAt < 0 {
		return "", ""
	}
	leadingLen := len(f.pending) - len(trimmed)
	visible = f.pending[:leadingLen] + body[closeAt+len(closeTag):]
	reasoning = body[:closeAt]
	f.pending = ""
	f.resolved = true
	return visible, reasoning
}

func (f *thinkStreamFilter) Flush() (visible, reasoning string) {
	if !f.resolved {
		visible = f.resolveVisible()
	}
	return visible, ""
}

func (f *thinkStreamFilter) resolveVisible() string {
	visible := f.pending
	f.pending = ""
	f.resolved = true
	return visible
}

// indexASCIIFold 返回 ASCII 标签的大小写不敏感字节位置。不能先 ToLower 整段文本再
// 用索引切原文：Unicode 大小写映射可能改变 UTF-8 字节长度，导致切片位置漂移。
func indexASCIIFold(value, tag string) int {
	for offset := 0; offset < len(value); {
		next := strings.IndexByte(value[offset:], tag[0])
		if next < 0 {
			return -1
		}
		next += offset
		if len(value)-next >= len(tag) && strings.EqualFold(value[next:next+len(tag)], tag) {
			return next
		}
		offset = next + 1
	}
	return -1
}

func splitThinkContent(content string) (visible, reasoning string) {
	filter := thinkStreamFilter{}
	visible, reasoning = filter.Push(content)
	lastVisible, lastReasoning := filter.Flush()
	return visible + lastVisible, reasoning + lastReasoning
}
