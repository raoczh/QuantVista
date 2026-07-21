package service

import (
	"errors"
	"fmt"

	"quantvista/model"
	"quantvista/setting"
)

// P0-4 跨模块 semantic validator（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §5.3/§7.1）：
// rating/action/position/target/stop/buy zone/RR/risk gate 的跨字段一致性统一收口。
//
// 定位与边界（不可漂移）：
//   - 这是「统一收口」不是「替换」：各模块既有专属校验（validateTradePlan 四价关系、
//     shortPlanPricesValid 悬空计划、parseAnalysisResult 枚举归一等）原样保留且不受
//     flag 控制；本文件只承载此前分散/缺失的跨字段规则，并作为跨模块规则的唯一落点。
//   - 失败处理与既有 degraded/refusal 语义一致，不静默修正：
//       * 分析/panel：校验错误在 parse 回调返回 → 触发 repair（错误文案即修复反馈）→
//         repair 后仍不过走既有 degraded 路径（原文保留，不落「成功」结构化结果）；
//       * 交易计划：校验错误 → repair → 仍不过走既有 best-effort（不带 trade_plan，
//         run 记 llm_output_invalid），主分析不受影响；
//       * 推荐 pick：盈亏比纪律沿 shortPlanPricesValid 既有「透明降级」先例——降为
//         watch 并追加 risks 注记（prompt 已声明「盈亏比不足时降为 watch」，这里是把
//         该纪律程序化，模型遵守与否都成立），不是无痕修改。
//   - flag `llm_semantic_validator`（缺省开，§10.1）：只回退本文件新增的跨字段规则；
//     关闭不影响任何既有模块校验。
//
// 各模块适用性（如实）：dailyreport 复盘输出为纯文本段落（无 rating/action/价位字段），
// qa/compare 为自由文本——本批无可执行的跨字段规则，其结论级一致性属 P1-2 claims 契约。

// semanticValidatorOn P0-4 开关快照（每次校验点读取一次）。
func semanticValidatorOn() bool { return setting.LLMSemanticValidator() }

// validateAnalysisSemantics 个股/持仓等分析结果的跨字段一致性（parse 回调内调用，
// 错误触发 repair）。规则：风险闸门含 block 级标志（ST/退市风险警示）时 rating 不得为
// bullish——此前只有 prompt 软约束（riskgate 提示文本），模型不遵守时会把「禁止买入」
// 标的以偏多评级落库展示。
func validateAnalysisSemantics(r *AnalysisResult, snapshot map[string]any) error {
	if r == nil || !semanticValidatorOn() {
		return nil
	}
	if r.Rating == model.AnalysisRatingBullish && hasBlockRiskFlag(snapshot) {
		return errors.New("语义校验失败：风险闸门为禁止买入级（ST/退市风险警示），rating 不得为 bullish；请改为 neutral 或 bearish，并在 risks 中说明该风险")
	}
	return nil
}

// validatePanelSemantics 多角色观点的跨字段一致性（parse 回调内调用）。规则：风险闸门
// block 时多数评级不得为 bullish（单个角色可以偏多——contrarian/risk 角色的分歧是 panel
// 的价值——但落库展示的多数结论不得与「禁止买入」相悖）。
func validatePanelSemantics(p *PanelResult, snapshot map[string]any) error {
	if p == nil || !semanticValidatorOn() {
		return nil
	}
	if panelMajorityRating(p.Roles) == model.AnalysisRatingBullish && hasBlockRiskFlag(snapshot) {
		return errors.New("语义校验失败：风险闸门为禁止买入级（ST/退市风险警示），多数角色评级不得为 bullish；请让至少与该风险相关的角色（risk/contrarian）给出 neutral/bearish 评级并说明")
	}
	return nil
}

// validateTradePlanSemantics 交易计划的统一校验入口（attachTradePlan 的 parse 回调调用）：
// 先跑既有专属校验 validateTradePlan（四价关系/止损低于现价/horizon/checklist，恒开），
// 再叠加跨字段上下文规则（flag 控）——计划与快照/评级上下文的一致性反证：
//   - 风险闸门 block 时不得给出计划（attachTradePlan 前置已确定性 NoPlan，此处是同一
//     契约在校验层的收口，防未来调用顺序重构后旧防线旁路）；
//   - 分析评级偏空时不得给出买入计划（同上，前置已拦，此处收口）。
func validateTradePlanSemantics(px float64, p *tradePlan, rating string, snapshot map[string]any) error {
	if err := validateTradePlan(px, p); err != nil {
		return err
	}
	if !semanticValidatorOn() {
		return nil
	}
	if hasBlockRiskFlag(snapshot) {
		return errors.New("语义校验失败：风险闸门为禁止买入级，必须输出 {\"no_plan\":true,...} 而非交易计划")
	}
	if rating == model.AnalysisRatingBearish {
		return errors.New("语义校验失败：分析评级为偏空(bearish)，必须输出 {\"no_plan\":true,...} 而非买入计划")
	}
	return nil
}

// recPickMinRR 短线推荐 buy 条目的最低盈亏比纪律（与 shortTermSpec prompt 中
// 「盈亏比至少 1.5，不足时降为 watch 并说明」同一数值；此前只有 prompt 约束）。
const recPickMinRR = 1.5

// recPickRRRatio 短线计划盈亏比 =(止盈-区间中值)/(区间中值-止损)。价位不全或分母非正
// 返回 0（价位合法性由 shortPlanPricesValid 把关，这里只算比值）。
func recPickRRRatio(p recPick) float64 {
	if p.BuyZoneLow <= 0 || p.BuyZoneHigh <= 0 || p.TakeProfit <= 0 || p.StopLoss <= 0 {
		return 0
	}
	entry := (p.BuyZoneLow + p.BuyZoneHigh) / 2
	risk := entry - p.StopLoss
	if risk <= 0 {
		return 0
	}
	return round2((p.TakeProfit - entry) / risk)
}

// applyRecPickSemantics 推荐 pick 的跨字段纪律（normalizePick 尾部调用，价位已归一）：
// 短线 buy 且价位齐全时盈亏比 <1.5 → 降为 watch 并追加注记（透明降级，沿
// shortPlanPricesValid 先例；价位关系本身合法故保留价位供追踪展示）。flag 控。
func applyRecPickSemantics(p recPick) recPick {
	if !semanticValidatorOn() {
		return p
	}
	if p.Action != model.RecActionBuy {
		return p
	}
	rr := recPickRRRatio(p)
	if rr > 0 && rr < recPickMinRR {
		p.Action = model.RecActionWatch
		p.Risks = append(p.Risks, fmt.Sprintf("盈亏比 %.2f 低于 %.1f 纪律线（止盈到止损的赔率不足），已降级为观察", rr, recPickMinRR))
	}
	return p
}
