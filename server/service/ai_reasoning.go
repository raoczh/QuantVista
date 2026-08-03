package service

import (
	"encoding/json"
	"strings"
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

// thinkStreamFilter 能跨 SSE chunk 分离 <think>...</think>。未闭合标签后的剩余文本在
// Flush 时仍归 reasoning，不能误当业务正文交给 extractJSONObject 或前端。
type thinkStreamFilter struct {
	pending string
	inThink bool
}

func (f *thinkStreamFilter) Push(chunk string) (visible, reasoning string) {
	f.pending += chunk
	var visibleBuilder, reasoningBuilder strings.Builder
	for f.pending != "" {
		tag := "<think>"
		if f.inThink {
			tag = "</think>"
		}
		lower := strings.ToLower(f.pending)
		if idx := strings.Index(lower, tag); idx >= 0 {
			f.writePart(&visibleBuilder, &reasoningBuilder, f.pending[:idx])
			f.pending = f.pending[idx+len(tag):]
			f.inThink = !f.inThink
			continue
		}
		keep := longestTagPrefixSuffix(lower, tag)
		emitLen := len(f.pending) - keep
		f.writePart(&visibleBuilder, &reasoningBuilder, f.pending[:emitLen])
		f.pending = f.pending[emitLen:]
		break
	}
	return visibleBuilder.String(), reasoningBuilder.String()
}

func (f *thinkStreamFilter) Flush() (visible, reasoning string) {
	if f.inThink {
		reasoning = f.pending
	} else {
		visible = f.pending
	}
	f.pending = ""
	return visible, reasoning
}

func (f *thinkStreamFilter) writePart(visible, reasoning *strings.Builder, part string) {
	if f.inThink {
		reasoning.WriteString(part)
	} else {
		visible.WriteString(part)
	}
}

func longestTagPrefixSuffix(value, tag string) int {
	max := len(tag) - 1
	if len(value) < max {
		max = len(value)
	}
	for size := max; size > 0; size-- {
		if strings.HasSuffix(value, tag[:size]) {
			return size
		}
	}
	return 0
}

func splitThinkContent(content string) (visible, reasoning string) {
	filter := thinkStreamFilter{}
	visible, reasoning = filter.Push(content)
	lastVisible, lastReasoning := filter.Flush()
	return visible + lastVisible, reasoning + lastReasoning
}
