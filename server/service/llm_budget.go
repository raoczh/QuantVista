package service

import "math"

// P0-9 模块化输出预算与截断语义（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.1）：
// 每个业务 LLM 模块的输出 token 预算、repair 次数上限与 repair 回灌字符上限由本表
// 统一声明，全部收口既有 capModuleTokens（ai_client.go）——不再有模块「直接使用用户
// 全局 MaxTokens、隐式决定 repair 次数」。
//
// 语义边界：
//   - MaxTokens 是用户未配置 max_tokens（0）时的模块**默认正文预算**，按各模块真实
//     输出分布与结构复杂度定标；用户明确配置正数时以用户值为准，模块默认值不得再
//     暗中把它截小。流式读取没有整包 60s 超时，旧版“压进上游 60s 窗口”的定标前提
//     已不成立。逐请求最终字段和值以 llm_call_logs.request_body 为事实来源。
//   - RepairAttempts 是首轮之后允许的额外修复次数：全局默认 llmDefaultRepairAttempts=1，
//     模块显式覆盖须在本表登记。上游单次请求有绝对 60s 窗口时，多打一轮并不能挽救
//     已超时的调用；结构化模块统一至多 repair 1 次。达到上限后必须拒答
//     （RefusalLLMOutputInvalid）或走确定性降级，禁止隐式追加。
//   - RepairTokenMultiplier 仅在上一轮因 length/max_tokens 截断时放大下一轮预算；这类
//     半截输出不回灌。RepairFeedChars 只用于“请求完整但结构校验失败”的 repair。
//   - 截断不静默当成功：模块预算触发的 finish_reason=length/max_tokens 截断由 P0-1
//     完整性门禁拒收（llm_response_incomplete）；`llm_accuracy_contract` 关闭时回退
//     旧兼容路径（截断如实记 finish_state，不粉饰）。空响应归 llm_response_incomplete、
//     repair 打满仍无合法输出归 llm_output_invalid（llm_contract.go 统一码表）。
//   - llm.go 探针（module=test）不进本表：probe 请求 max_tokens 固定 16/32，与业务预算无关。

// llmDefaultRepairAttempts repair 次数全局默认：首轮之后最多 1 次额外修复。
const llmDefaultRepairAttempts = 1

// llmDefaultRepairFeedChars repair 回灌坏输出的默认字符上限。
const llmDefaultRepairFeedChars = 600

// llmDefaultRepairTokenMultiplier 截断后的 repair 输出预算倍率。
const llmDefaultRepairTokenMultiplier = 1.5

// llmGlobalHardCap 仅防整数放大溢出，取 new-api 请求校验同量级的 MaxInt32/2。
// 这是协议实现保护，不是业务输出限额；正常请求以用户配置为准。
const llmGlobalHardCap = 1_073_741_823

// llmModuleBudget 一个模块的输出预算声明。
type llmModuleBudget struct {
	// MaxTokens 用户未配置时的模块默认正文预算。
	MaxTokens int
	// RepairAttempts 首轮之后允许的额外修复次数（自由文本模块无结构化校验为 0）。
	RepairAttempts int
	// RepairFeedChars repair 回灌上一轮坏输出的字符上限（RepairAttempts=0 时无意义）。
	RepairFeedChars int
	// RepairTokenMultiplier 上一轮因 token 上限截断时，本轮预算放大倍率。
	RepairTokenMultiplier float64
}

// llmModuleBudgets 模块预算表：key 与 llm_call_logs.module / llmRun.Module 同名同值。
// 新增 chatCompletion* 调用模块必须先在此登记预算（与 §2.5 调用矩阵登记同一纪律）。
var llmModuleBudgets = map[string]llmModuleBudget{
	// 分析主调（标准/panel 同 module 不同 schema）：字段较多。
	"analysis": {MaxTokens: 5000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	// 交易计划：小 JSON（价位区间/止损/止盈/checklist），一次 repair 足够。
	"trade_plan": {MaxTokens: 2000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	// 分析复核员：单条 verdict JSON。
	"analysis_review": {MaxTokens: 1500, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	// 推荐主调与实验恒等，保证单变量对照纪律。
	"recommendation": {MaxTokens: 6000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	"rec_review":     {MaxTokens: 3000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	"rec_bear":       {MaxTokens: 2500, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	"daily_report":   {MaxTokens: 3000, RepairAttempts: 1, RepairFeedChars: 800, RepairTokenMultiplier: 1.5},
	// 问答：自由文本对话，无结构化 repair。
	"qa": {MaxTokens: 4000, RepairAttempts: 0},
	// 对比点评：自由文本，无 repair。
	"compare": {MaxTokens: 1500, RepairAttempts: 0},
	// 新闻情绪批标注（8 条/批 JSON）：解析失败降级规则路径，不 repair。
	"news": {MaxTokens: 3000, RepairAttempts: 0},
	// 白话建策略：条件树 JSON。
	"screener_parse": {MaxTokens: 2500, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	// P1-3 条件式辩论三角色（analysis_debate.go）：bull/bear 各输出 ≤4 条带证据引用的
	// claims、judge 输出裁决 JSON；rebuttal 是单条反驳小 JSON。条件触发非默认路径，
	// 预算按「结构紧凑的观点 JSON」定标。
	"debate_bull":     {MaxTokens: 2000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	"debate_bear":     {MaxTokens: 2000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	"debate_rebuttal": {MaxTokens: 1200, RepairAttempts: 1, RepairFeedChars: 400, RepairTokenMultiplier: 1.5},
	"debate_judge":    {MaxTokens: 2000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	// P1-5 反思记忆生成（recreflect.go）：批量 ≤5 条成熟推荐的教训 JSON（每条 2-4 句）。
	"reflection": {MaxTokens: 2500, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	// P2-1 challenger 影子采样（llm_experiment.go）：与推荐主调同预算（公平对照），
	// **无 repair**——影子对照要测的就是 challenger「一把过」的结构化质量。
	"experiment": {MaxTokens: 6000, RepairAttempts: 0},
	// P2-6 发布审计员（llm_release_gate.go）：verdict+findings 小 JSON；手动管理员动作。
	"release_audit": {MaxTokens: 2500, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
	// D17 持仓卖出建议（positionadvice.go）：逐笔 {verdict, reason, invalidation} 小 JSON，
	// 单次至多 positionAdviceMaxPositions=20 笔——按每笔约 100 token 的理由+失效条件定标。
	"position_advice": {MaxTokens: 4000, RepairAttempts: 1, RepairFeedChars: 600, RepairTokenMultiplier: 1.5},
}

// moduleBudget 取模块预算；未登记模块回默认（不钳 token、repair 默认 1）——
// 但业务模块都应显式登记，未登记属接线遗漏。
func moduleBudget(module string) llmModuleBudget {
	if b, ok := llmModuleBudgets[module]; ok {
		return b
	}
	return llmModuleBudget{MaxTokens: 0, RepairAttempts: llmDefaultRepairAttempts, RepairFeedChars: llmDefaultRepairFeedChars, RepairTokenMultiplier: llmDefaultRepairTokenMultiplier}
}

// moduleTokenCap 模块预算与用户配置合成实际请求 max_tokens（统一收口 capModuleTokens）。
func moduleTokenCap(module string, userMax int) int {
	return capModuleTokens(userMax, moduleBudget(module).MaxTokens)
}

// moduleRepairAttempts 模块 repair 次数上限（首轮之后的额外次数）。
func moduleRepairAttempts(module string) int {
	return moduleBudget(module).RepairAttempts
}

// moduleRepairTokenCap 放大一次 token 截断后的 repair 预算，并只做整型上限保护。
func moduleRepairTokenCap(module string, current int) int {
	if current <= 0 {
		return current
	}
	multiplier := moduleBudget(module).RepairTokenMultiplier
	if multiplier <= 1 {
		multiplier = llmDefaultRepairTokenMultiplier
	}
	if current >= llmGlobalHardCap {
		return llmGlobalHardCap
	}
	scaled := float64(current) * multiplier
	if scaled >= float64(llmGlobalHardCap) {
		return llmGlobalHardCap
	}
	return int(math.Ceil(scaled))
}

func isTokenLimitFinishState(state string) bool {
	return state == "length" || state == "max_tokens"
}

// appendModuleRepairMessages 只在完整响应的结构校验失败时回灌 assistant 内容；token
// 截断的半截 JSON 没有修复价值，直接追加重新生成指令。
func appendModuleRepairMessages(convo []chatMessage, module, content, finishState, instruction string) []chatMessage {
	if !isTokenLimitFinishState(finishState) {
		convo = append(convo, chatMessage{Role: "assistant", Content: moduleRepairFeed(module, content)})
	}
	return append(convo, chatMessage{Role: "user", Content: instruction})
}

// moduleRepairFeed 按模块上限截断 repair 回灌的坏输出。
func moduleRepairFeed(module, content string) string {
	limit := moduleBudget(module).RepairFeedChars
	if limit <= 0 {
		limit = llmDefaultRepairFeedChars
	}
	return truncateRunes(content, limit)
}
