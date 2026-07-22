package service

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// P0-5 capability matrix（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.1）：
// 每个 provider/model 的结构化输出、温度、token 参数与端点能力由「内置声明 + 运行时观察」
// 两层合并声明化，JSON mode 回落从「每次调用先发一个注定失败的请求再隐式重试」升级为
// 声明化路由——已知不支持 json_object 的 (配置,模型,端点) 直接按 free_text 请求，
// 回落原因可观察（审计 structured_method + 系统日志 + 观察存储）。
//
// 设计边界：
//   - 本项目对接的是任意 OpenAI 兼容网关（provider 是用户自由填写的字符串），静态表
//     无法穷举真实能力，因此以 capUnknown 为缺省、运行时观察为主：四处 JSON mode 回落点
//     （chat/responses × 流式/非流式）与 llm.go 的 provider smoke 都会写入观察存储。
//   - 观察带 TTL：单次 4xx 误判不应把一个配置永久降级为 free_text，到期自动恢复
//     unknown 重新在线探测（隐式回落路径仍在，作为 unknown 状态的兜底）。
//   - flag `llm_capability_routing` 缺省开（!= "false"），关闭只回退「声明化路由」——
//     观察仍照常记录，四处隐式回落点行为不变（P0-1 之前就存在的兼容路径）。
//   - llm.go 探针（module=test）保持独立：capability smoke 只发最小探测请求、不注入
//     任何业务 prompt，探针通过（含 json_object supported）≠ 业务推理可用。

// llmCapState 能力三态。unknown 表示未声明也未观察——调用按乐观路径尝试，
// 失败由既有隐式回落兜底并转化为观察。
type llmCapState string

const (
	capSupported   llmCapState = "supported"
	capUnsupported llmCapState = "unsupported"
	capUnknown     llmCapState = "unknown"
)

// llmCapability 能力维度枚举。
type llmCapability string

const (
	// capJSONObject response_format:{type:json_object}（chat）/ text.format（responses）。
	capJSONObject llmCapability = "json_object"
	// capFreeText 纯文本输出——OpenAI 兼容口径恒支持，声明位仅为矩阵完整性。
	capFreeText llmCapability = "free_text"
	// capTemperature temperature 参数。
	capTemperature llmCapability = "temperature"
	// capMaxTokens 记录 Chat 端旧字段 max_tokens 是否被拒。unsupported 表示应改用
	// 等价的 max_completion_tokens，而不是省略输出预算。target 含 endpoint，Responses
	// 的 max_output_tokens 不复用这项回落。
	capMaxTokens llmCapability = "max_tokens"
	// capEndpointChat / capEndpointResponses 端点可用性（smoke 观察；业务不自动改写
	// 用户显式选择的端点类型，仅作可观察声明）。
	capEndpointChat      llmCapability = "endpoint_chat_completions"
	capEndpointResponses llmCapability = "endpoint_responses"
)

// llmModelCapabilities 一个 (provider, model, endpoint) 目标的能力声明快照
// （内置声明与运行时观察合并后的视图）。
type llmModelCapabilities struct {
	JSONObject  llmCapState
	FreeText    llmCapState
	Temperature llmCapState
	MaxTokens   llmCapState
	Endpoint    llmCapState // 本次调用所选端点的可用性
}

// builtinProviderCapabilities 内置 provider 声明（key 为小写 provider 名）。
// openai 官方口径 json_object/温度/token 参数全支持；其余任意兼容中转（含空 provider）
// 结构化支持情况不可静态断言，json_object 记 unknown 由运行时观察补齐。
var builtinProviderCapabilities = map[string]llmModelCapabilities{
	"openai": {JSONObject: capSupported, FreeText: capSupported, Temperature: capSupported, MaxTokens: capSupported, Endpoint: capSupported},
}

// defaultProviderCapabilities 未登记 provider 的缺省声明。
var defaultProviderCapabilities = llmModelCapabilities{
	JSONObject: capUnknown, FreeText: capSupported, Temperature: capSupported, MaxTokens: capSupported, Endpoint: capUnknown,
}

// llmCapObservationTTL 运行时观察的有效期：期内声明化路由生效，到期恢复 unknown
// 重新在线探测。不宜过长——上游网关升级支持 json_object 后应能自动恢复结构化请求；
// 也不宜过短——每次过期都要付一次「注定失败的请求 + 隐式回落」的探测成本。
const llmCapObservationTTL = 12 * time.Hour

// llmCapObservation 一条运行时能力观察。
type llmCapObservation struct {
	State      llmCapState
	Reason     string
	ObservedAt time.Time
}

// llmCapabilityStore 进程内观察存储：key = llmCapabilityKey(目标) + "#" + 能力维度。
// 个人自用单实例，进程内即可；重启清零无害（回到 unknown 乐观路径）。
var llmCapabilityStore sync.Map // string -> llmCapObservation

// llmCapabilityTarget 观察 key 的目标标识。必须包含配置身份（configID）、规范化
// provider、规范化 BaseURL、model 与 endpoint 全部维度——configID 不变但用户改配置
// 换上游（改 BaseURL/provider 而保持 model/endpoint）时，key 随之变化，旧上游的观察
// 天然失效，不会被新上游继承（P0-5 修复批：此前 configID 形态缺 BaseURL/provider 维度）。
// 无配置身份（TestByInput 表单探测）时 configID=0，凭 URL 形态区分。
func llmCapabilityTarget(configID int64, provider, baseURL, modelName, endpointType string) string {
	ep := endpointType
	if ep == "" {
		ep = model.LLMEndpointChat
	}
	base := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	prov := strings.ToLower(strings.TrimSpace(provider))
	return fmt.Sprintf("cfg:%d|%s|%s|%s|%s", configID, prov, base, modelName, ep)
}

// capabilityTargetOf 从一次调用参数派生观察 key。
func capabilityTargetOf(p chatParams) string {
	return llmCapabilityTarget(p.Meta.ConfigID, p.Meta.Provider, p.BaseURL, p.Model, p.EndpointType)
}

// observeLLMCapability 写入一条运行时观察。状态变化时打系统日志（错误路由可观察）；
// 同状态刷新只延长 TTL 不刷屏。
func observeLLMCapability(target string, dim llmCapability, state llmCapState, reason string) {
	key := target + "#" + string(dim)
	prev, had := llmCapabilityStore.Load(key)
	llmCapabilityStore.Store(key, llmCapObservation{State: state, Reason: reason, ObservedAt: time.Now()})
	if !had || prev.(llmCapObservation).State != state {
		common.SysLog("LLM 能力观察更新：%s %s=%s（%s）", target, dim, state, reason)
	}
}

// lookupLLMCapability 读取运行时观察（过期视为不存在）。
func lookupLLMCapability(target string, dim llmCapability) (llmCapObservation, bool) {
	key := target + "#" + string(dim)
	v, ok := llmCapabilityStore.Load(key)
	if !ok {
		return llmCapObservation{}, false
	}
	obs := v.(llmCapObservation)
	if time.Since(obs.ObservedAt) > llmCapObservationTTL {
		llmCapabilityStore.Delete(key)
		return llmCapObservation{}, false
	}
	return obs, true
}

// resetLLMCapabilityStore 清空观察存储（测试用；生产无调用方）。
func resetLLMCapabilityStore() {
	llmCapabilityStore.Range(func(k, _ any) bool {
		llmCapabilityStore.Delete(k)
		return true
	})
}

// capabilitiesFor 合并「内置 provider 声明 + 运行时观察」输出能力快照：观察优先于声明
// （观察来自该目标的真实响应，比按 provider 名的静态假设可信）。json_object 之外，
// temperature/max_tokens 参数能力同样由观察覆盖（P0-5 修复批）。其中 max_tokens 的
// unsupported 在 Chat 端表示改用 max_completion_tokens，模块预算始终保留。
func capabilitiesFor(provider string, target string) llmModelCapabilities {
	caps, ok := builtinProviderCapabilities[strings.ToLower(strings.TrimSpace(provider))]
	if !ok {
		caps = defaultProviderCapabilities
	}
	if obs, ok := lookupLLMCapability(target, capJSONObject); ok {
		caps.JSONObject = obs.State
	}
	if obs, ok := lookupLLMCapability(target, capTemperature); ok {
		caps.Temperature = obs.State
	}
	if obs, ok := lookupLLMCapability(target, capMaxTokens); ok {
		caps.MaxTokens = obs.State
	}
	return caps
}

// applyCapabilityRouting 公开出口的声明化路由（P0-5）：
//   - 结构化调用（JSONMode）在能力矩阵已声明 json_object 不支持时，直接按 free_text 请求
//     ——省掉一次注定失败的请求与隐式回落，审计 structured_method 如实记 free_text；
//   - temperature 已声明不支持时省略；Chat 的 max_tokens 已知被拒时改用
//     max_completion_tokens。Responses 的 max_output_tokens 没有等价替代，不做省略回落。
//
// 必须在 effectiveJSONMode 观测指针初始化之后调用；flag 关闭时原样返回（保留旧隐式回落路径）。
func applyCapabilityRouting(p chatParams) chatParams {
	if !setting.LLMCapabilityRouting() {
		return p
	}
	caps := capabilitiesFor(p.Meta.Provider, capabilityTargetOf(p))
	if p.JSONMode && caps.JSONObject == capUnsupported {
		p.JSONMode = false
		p.markJSONModeDropped()
	}
	if caps.Temperature == capUnsupported {
		p.markTemperatureOmitted()
	}
	if p.MaxTokens > 0 && !p.isResponsesEndpoint() && caps.MaxTokens == capUnsupported {
		p.markMaxCompletionTokens()
	}
	return p
}

// observeJSONModeUnsupported JSON mode 能力观察提交（P0-5 修复批：与审计观测拆分）。
// 只允许在「去掉结构化参数后的 fallback 请求确认成功」之后调用——4xx 里的字样只是猜测，
// fallback 成功才证明失败确实源于结构化参数；fallback 也失败（错误另有原因，如模型名
// 不存在）时不得提交，否则一次误判污染该目标 12h 的能力状态。
// 审计侧的 markJSONModeDropped（最终请求形态=free_text）仍应在重试发出前置位，两者职责不同。
func (p chatParams) observeJSONModeUnsupported(reason string) {
	observeLLMCapability(capabilityTargetOf(p), capJSONObject, capUnsupported, reason)
}

// observeTemperatureUnsupported / observeMaxTokensUnsupported 参数能力观察提交。
// 后者只在 Chat 改用 max_completion_tokens 后确认成功时调用。
func (p chatParams) observeTemperatureUnsupported(reason string) {
	observeLLMCapability(capabilityTargetOf(p), capTemperature, capUnsupported, reason)
}

func (p chatParams) observeMaxTokensUnsupported(reason string) {
	observeLLMCapability(capabilityTargetOf(p), capMaxTokens, capUnsupported, reason)
}
