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
	// capMaxTokens max_tokens（chat）/ max_output_tokens（responses）参数。
	capMaxTokens llmCapability = "max_tokens"
	// capEndpointChat / capEndpointResponses 端点可用性（smoke 观察；业务不自动改写
	// 用户显式选择的端点类型，仅作可观察声明）。
	capEndpointChat      llmCapability = "endpoint_chat_completions"
	capEndpointResponses llmCapability = "endpoint_responses"
)

// llmModelCapabilities 一个 (provider, model, endpoint) 目标的能力声明快照
//（内置声明与运行时观察合并后的视图）。
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

// llmCapabilityTarget 观察 key 的目标标识。configID>0 用配置身份（配置改模型后 key
// 变化天然失效）；无配置身份（TestByInput 表单探测）退回 URL+模型。
func llmCapabilityTarget(configID int64, baseURL, modelName, endpointType string) string {
	ep := endpointType
	if ep == "" {
		ep = model.LLMEndpointChat
	}
	if configID > 0 {
		return fmt.Sprintf("cfg:%d|%s|%s", configID, modelName, ep)
	}
	return fmt.Sprintf("url:%s|%s|%s", strings.TrimRight(strings.TrimSpace(baseURL), "/"), modelName, ep)
}

// capabilityTargetOf 从一次调用参数派生观察 key。
func capabilityTargetOf(p chatParams) string {
	return llmCapabilityTarget(p.Meta.ConfigID, p.BaseURL, p.Model, p.EndpointType)
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
//（观察来自该目标的真实响应，比按 provider 名的静态假设可信）。
func capabilitiesFor(provider string, target string) llmModelCapabilities {
	caps, ok := builtinProviderCapabilities[strings.ToLower(strings.TrimSpace(provider))]
	if !ok {
		caps = defaultProviderCapabilities
	}
	if obs, ok := lookupLLMCapability(target, capJSONObject); ok {
		caps.JSONObject = obs.State
	}
	return caps
}

// applyCapabilityRouting 公开出口的声明化路由：结构化调用（JSONMode）在能力矩阵已声明
// json_object 不支持时，直接按 free_text 请求——省掉一次注定失败的请求与隐式回落，
// 审计 structured_method 如实记 free_text（markJSONModeDropped）。
// 必须在 effectiveJSONMode 观测指针初始化之后调用；flag 关闭时原样返回（保留旧隐式回落路径）。
func applyCapabilityRouting(p chatParams) chatParams {
	if !p.JSONMode || !setting.LLMCapabilityRouting() {
		return p
	}
	target := capabilityTargetOf(p)
	if capabilitiesFor(p.Meta.Provider, target).JSONObject == capUnsupported {
		p.JSONMode = false
		p.markJSONModeDropped()
	}
	return p
}

// noteJSONModeUnsupported 四处 JSON mode 隐式回落点（chat/responses × 流式/非流式）的
// 统一挂钩：审计观测置 free_text（markJSONModeDropped 原语义）+ 写入能力观察，让下一次
// 调用走声明化路由。新增回落分支必须改调本方法而非裸调 markJSONModeDropped。
func (p chatParams) noteJSONModeUnsupported(reason string) {
	p.markJSONModeDropped()
	observeLLMCapability(capabilityTargetOf(p), capJSONObject, capUnsupported, reason)
}
