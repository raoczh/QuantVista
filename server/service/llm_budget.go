package service

// P0-9 模块化输出预算与截断语义（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.1）：
// 每个业务 LLM 模块的输出 token 预算、repair 次数上限与 repair 回灌字符上限由本表
// 统一声明，全部收口既有 capModuleTokens（ai_client.go）——不再有模块「直接使用用户
// 全局 MaxTokens、隐式决定 repair 次数」。
//
// 语义边界：
//   - MaxTokens 是模块输出的**上限声明**（capModuleTokens 与用户配置取小）：用户配置
//     更小以用户为准；用户未配置（0）时直接用模块预算。目的不是压缩正常输出，而是
//     ①防用户全局 max_tokens 过大把单次生成拖出上游 60s 窗口，②让每个模块的预算
//     可归因（manifest/审计里 length 截断可对照声明值定位）。数值为按现网输出分布
//     校准的初值，调整须掂量：过小会触发 finish_reason=length 拒收（契约开启时
//     fail-closed，见下），过大失去预算意义。
//   - RepairAttempts 是首轮之后允许的额外修复次数：全局默认 llmDefaultRepairAttempts=1，
//     模块显式覆盖须在本表登记（analysis/trade_plan 保留历史值 2——共用
//     AnalysisService.callWithRepair 的既有行为，不做静默变更）。达到上限后必须拒答
//     （RefusalLLMOutputInvalid）或走确定性降级，禁止隐式追加。
//   - RepairFeedChars 是 repair 轮回灌上一轮坏输出的字符上限（完整回灌会把大段废文本
//     重新塞进上下文，拖慢下一轮生成、更易撞上游 60s 超时——推荐 600/日报 800 先例）。
//   - 截断不静默当成功：模块预算触发的 finish_reason=length/max_tokens 截断由 P0-1
//     完整性门禁拒收（llm_response_incomplete）；`llm_accuracy_contract` 关闭时回退
//     旧兼容路径（截断如实记 finish_state，不粉饰）。空响应归 llm_response_incomplete、
//     repair 打满仍无合法输出归 llm_output_invalid（llm_contract.go 统一码表）。
//   - llm.go 探针（module=test）不进本表：probe 请求 max_tokens 固定 16/32，与业务预算无关。

// llmDefaultRepairAttempts repair 次数全局默认：首轮之后最多 1 次额外修复。
const llmDefaultRepairAttempts = 1

// llmDefaultRepairFeedChars repair 回灌坏输出的默认字符上限。
const llmDefaultRepairFeedChars = 800

// llmModuleBudget 一个模块的输出预算声明。
type llmModuleBudget struct {
	// MaxTokens 模块输出 token 预算（0=不钳制，仅用户配置说了算——不应出现在业务模块上）。
	MaxTokens int
	// RepairAttempts 首轮之后允许的额外修复次数（自由文本模块无结构化校验为 0）。
	RepairAttempts int
	// RepairFeedChars repair 回灌上一轮坏输出的字符上限（RepairAttempts=0 时无意义）。
	RepairFeedChars int
}

// llmModuleBudgets 模块预算表：key 与 llm_call_logs.module / llmRun.Module 同名同值。
// 新增 chatCompletion* 调用模块必须先在此登记预算（与 §2.5 调用矩阵登记同一纪律）。
var llmModuleBudgets = map[string]llmModuleBudget{
	// 分析主调（标准/panel 同 module 不同 schema）：结构化字段多（要点/风险/机会/反方/失效
	// 条件等），预算给足；repair 2 次为既有显式覆盖（callWithRepair 历史行为）。
	"analysis": {MaxTokens: 4000, RepairAttempts: 2, RepairFeedChars: 800},
	// 交易计划：小 JSON（价位区间/止损/止盈/checklist）；与 analysis 共用 callWithRepair，
	// repair 2 次保留既有行为。
	"trade_plan": {MaxTokens: 1500, RepairAttempts: 2, RepairFeedChars: 800},
	// 分析复核员：单条 verdict JSON。
	"analysis_review": {MaxTokens: 1000, RepairAttempts: 1, RepairFeedChars: 800},
	// 推荐主调/复核/反方：2026-07-14 异步任务化批定标（压单次生成进上游 60s 窗口）。
	"recommendation": {MaxTokens: 2500, RepairAttempts: 1, RepairFeedChars: 600},
	"rec_review":     {MaxTokens: 1500, RepairAttempts: 1, RepairFeedChars: 600},
	"rec_bear":       {MaxTokens: 1500, RepairAttempts: 1, RepairFeedChars: 600},
	// 日报复盘：同上批定标；callReview 为固定「首轮+1 次 repair」结构，与本表声明一致。
	"daily_report": {MaxTokens: 1500, RepairAttempts: 1, RepairFeedChars: 800},
	// 问答：自由文本对话，预算宽（长解释合法），无结构化 repair。
	"qa": {MaxTokens: 8000, RepairAttempts: 0},
	// 对比短评：自由文本，无 repair。
	"compare": {MaxTokens: 2000, RepairAttempts: 0},
	// 新闻情绪批标注（8 条/批 JSON）：解析失败降级规则路径，不 repair。
	"news": {MaxTokens: 3000, RepairAttempts: 0},
	// 白话建策略：条件树 JSON。
	"screener_parse": {MaxTokens: 2000, RepairAttempts: 1, RepairFeedChars: 800},
}

// moduleBudget 取模块预算；未登记模块回默认（不钳 token、repair 默认 1）——
// 但业务模块都应显式登记，未登记属接线遗漏。
func moduleBudget(module string) llmModuleBudget {
	if b, ok := llmModuleBudgets[module]; ok {
		return b
	}
	return llmModuleBudget{MaxTokens: 0, RepairAttempts: llmDefaultRepairAttempts, RepairFeedChars: llmDefaultRepairFeedChars}
}

// moduleTokenCap 模块预算与用户配置合成实际请求 max_tokens（统一收口 capModuleTokens）。
func moduleTokenCap(module string, userMax int) int {
	return capModuleTokens(userMax, moduleBudget(module).MaxTokens)
}

// moduleRepairAttempts 模块 repair 次数上限（首轮之后的额外次数）。
func moduleRepairAttempts(module string) int {
	return moduleBudget(module).RepairAttempts
}

// moduleRepairFeed 按模块上限截断 repair 回灌的坏输出。
func moduleRepairFeed(module, content string) string {
	limit := moduleBudget(module).RepairFeedChars
	if limit <= 0 {
		limit = llmDefaultRepairFeedChars
	}
	return truncateRunes(content, limit)
}
