package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"quantvista/datasource"
	"quantvista/model"
)

// 交易员阶段（M3c）：个股标准分析成功后追加一次 LLM 调用，产出可执行的交易计划
//（买入区间/目标价/止损/持有周期/操作清单），并用纯 Go 公式给出量化仓位建议。
// 纪律：止损必须低于现价（校验不过触发 repair）；盈亏比 <2:1 时仓位建议减半（服务端后处理）。
// 计划为 best-effort：LLM 调用失败/始终校验不过时结果不带 trade_plan，不影响主分析。

// tradePlan 交易计划。前半段由 LLM 输出，服务端字段（rr_ratio/position/discipline_notes）
// 只能由服务端回填——parseTradePlan 解析前剥除，防模型伪造「纪律校验通过」。
type tradePlan struct {
	NoPlan       bool     `json:"no_plan,omitempty"`        // 评级偏空/风险闸门/依据不足时不给计划
	NoPlanReason string   `json:"no_plan_reason,omitempty"` // 不给计划的原因
	BuyLow       float64  `json:"buy_low,omitempty"`        // 买入区间下沿
	BuyHigh      float64  `json:"buy_high,omitempty"`       // 买入区间上沿
	TargetPrice  float64  `json:"target_price,omitempty"`   // 目标价
	StopPrice    float64  `json:"stop_price,omitempty"`     // 止损价（硬纪律：必须低于现价）
	HorizonDays  FlexInt  `json:"horizon_days,omitempty"`   // 预期持有周期（交易日）
	PlanNote     string   `json:"plan_note,omitempty"`      // 计划思路一句话
	Checklist    []string `json:"checklist,omitempty"`      // 买入前逐项核对的操作清单

	// --- 服务端回填（非 LLM 输出）---
	RRRatio    float64         `json:"rr_ratio,omitempty"`         // 盈亏比 =(目标-区间中值)/(区间中值-止损)
	Position   *positionAdvice `json:"position,omitempty"`         // 量化仓位建议（纯 Go 公式）
	Discipline []string        `json:"discipline_notes,omitempty"` // 自洽纪律校验说明（降仓等）
}

// positionAdvice 量化仓位建议：仓位% = 100 × clip(2.5/20日波动率, 0.3, 1.0) × 择时系数，
// 择时系数 = 0.6 + (涨家占比 - 0.5) × 1.2，夹 [0.3, 1.2]。全部确定性计算，可复现。
type positionAdvice struct {
	PositionPct  float64 `json:"position_pct"`            // 建议仓位（占计划投入资金的百分比）
	Vol20        float64 `json:"vol_20d"`                 // 近 20 个交易日日收益率样本标准差（%）
	VolCoef      float64 `json:"vol_coef"`                // clip(2.5/vol20, 0.3, 1.0)
	TimingCoef   float64 `json:"timing_coef"`             // 择时系数
	AdvanceRatio float64 `json:"advance_ratio,omitempty"` // 市场涨家占比（0~1；0=不可得）
	Note         string  `json:"note,omitempty"`          // 数据缺失等如实声明
}

const tradePlanSystem = `你是一名严谨的交易计划员，基于另一位研究员的分析结论与数据快照，给出可执行的交易计划。你的输出仅供研究参考，不构成投资建议。
规则：
1. 价位必须锚定数据快照：买入区间、目标价、止损价须依据现价、MA5/MA10/MA20、区间高低点、涨跌停价等快照中的具体数值推导，禁止凭空给价、禁止使用你记忆中的历史价位。若快照含 org_view.target_price（机构目标价统计），把它当对照锚而非依据：你的目标价应独立从技术面推导，若与机构中位数（median）偏离很大，在 plan_note 中用一句话说明差异方向（注意卖方目标价普遍乐观，不得直接抄用）。
2. 硬纪律（违反会被程序拒绝）：止损价必须低于现价；止损价必须低于买入区间下沿；目标价必须高于买入区间上沿与现价；买入区间下沿不高于上沿。
3. horizon_days 为预期持有周期（交易日，1~120）：短线思路 3~20，波段 20~60，依据分析结论选择。
4. checklist 为买入前逐项核对的操作清单（3~6 条，每条一个可观察、可执行的核对项，如「竞价高开超 5% 放弃按区间挂单」「跌破买入区间下沿当日不接刀」）。
5. 若分析评级为偏空(bearish)、风险闸门标注禁止买入、或数据不足以定出可靠价位，只输出 {"no_plan": true, "no_plan_reason": "一句话原因"}。
只输出一个 JSON 对象，不要任何解释或代码块标记，字段：no_plan(可省)、no_plan_reason(可省)、buy_low、buy_high、target_price、stop_price、horizon_days(整数)、plan_note(一句话计划思路)、checklist(字符串数组)。`

const tradePlanRepairHint = `请只输出一个合法 JSON 对象：{"buy_low":数字,"buy_high":数字,"target_price":数字,"stop_price":数字,"horizon_days":整数,"plan_note":"...","checklist":["..."]}，且满足 止损价<现价、止损价<buy_low、target_price>buy_high；或 {"no_plan":true,"no_plan_reason":"..."}。不要任何解释或代码块标记。`

// attachTradePlan 为一份成功的个股标准分析追加交易计划（就地写 result.TradePlan），
// 返回本次消耗的 token 与计划 run 元数据（确定性拒绝零调用时为 nil）。仅 stock+标准模式+
// 非回溯（as_of 的历史现价上做「当下计划」既无意义又易被误读为回测结论）。
// 评级偏空与风险闸门 block 两种确定性拒绝不烧 token。
func (s *AnalysisService) attachTradePlan(ctx context.Context, userID int64, cfg *model.LLMConfig, apiKey string, allowPrivate bool, req AnalyzeRequest, snapshot map[string]any, result *AnalysisResult, traceID, parentRunID string) (chatUsage, *llmRun) {
	if req.Module != model.AnalysisModuleStock || req.Mode != model.AnalysisModeStandard || req.AsOf != "" {
		return chatUsage{}, nil
	}
	if result.Rating == model.AnalysisRatingBearish {
		result.TradePlan = &tradePlan{NoPlan: true, NoPlanReason: "分析评级偏空，不生成买入计划"}
		return chatUsage{}, nil
	}
	if hasBlockRiskFlag(snapshot) {
		result.TradePlan = &tradePlan{NoPlan: true, NoPlanReason: "风险闸门禁止买入（ST/退市风险警示），不生成交易计划"}
		return chatUsage{}, nil
	}
	// 行情时效确定性拒绝（零 token）：stale/unknown 行情上给出的买入区间/目标价/止损价/
	// 仓位是对着旧盘面定价——分析正文可作「截至 quote_as_of 的历史解释」，但精确交易
	// 计划必须建立在当前有效行情上。freshness_status 由 buildStockSnapshot 统一判定。
	if fs, _ := snapshot["freshness_status"].(string); fs != freshStatusFresh {
		result.TradePlan = &tradePlan{NoPlan: true, NoPlanReason: "行情数据非当前有效口径（过期或无法确认时效），不生成精确交易计划；分析正文仅为截至行情时间的历史解释"}
		return chatUsage{}, nil
	}
	if _, hasNote := snapshot["freshness_note"]; hasNote {
		result.TradePlan = &tradePlan{NoPlan: true, NoPlanReason: "行情仅更新至历史时点（全部数据源均未取到更新数据），不生成精确交易计划"}
		return chatUsage{}, nil
	}
	px := quotePriceFromSnapshot(snapshot)
	if px <= 0 {
		result.TradePlan = &tradePlan{NoPlan: true, NoPlanReason: "现价不可得，无法定价位"}
		return chatUsage{}, nil
	}

	// 涨跌家数与 LLM 调用无依赖：并发预取，别让 GetOverview（缓存未命中时 1~3s）
	// 串行叠在计划 LLM 调用（数十秒）之后。缓冲 1，早退路径不泄漏 goroutine。
	breadthCh := make(chan *datasource.Breadth, 1)
	go func() { breadthCh <- s.marketBreadth(ctx, req.Market) }()

	snapJSON, _ := json.Marshal(snapshot)
	resJSON, _ := json.Marshal(result)
	messages := []chatMessage{
		{Role: "system", Content: tradePlanSystem},
		{Role: "user", Content: fmt.Sprintf("现价：%.2f\n\n【数据快照】（JSON）：\n%s\n\n【分析结论】（JSON）：\n%s",
			px, string(snapJSON), string(resJSON))},
	}
	run := newLLMRun(traceID, parentRunID, "trade_plan", "trade_plan.v1", analysisPromptVersion)
	run.hashData(string(snapJSON))

	var plan *tradePlan
	parse := func(content string) error {
		p, perr := parseTradePlan(content)
		if perr != nil {
			return perr
		}
		if !p.NoPlan {
			if verr := validateTradePlan(px, p); verr != nil {
				return verr
			}
		}
		plan = p
		return nil
	}
	_, usage, _, callErr := s.callWithRepair(ctx, userID, run, cfg, apiKey, allowPrivate, messages, parse, tradePlanRepairHint)
	if callErr != nil || plan == nil {
		// best-effort：失败/始终校验不过 → 不带 trade_plan，不影响主分析。
		if callErr == nil {
			run.DegradedReason = "llm_output_invalid"
		}
		return usage, run
	}

	if plan.NoPlan {
		plan.NoPlanReason = truncateRunes(orDefaultStr(plan.NoPlanReason, "依据不足，不生成交易计划"), 200)
		// 重建只含 no_plan 字段的干净结构：丢弃模型可能一并输出的残留价位/清单，防止前端误渲染。
		result.TradePlan = &tradePlan{NoPlan: true, NoPlanReason: plan.NoPlanReason}
		return usage, run
	}

	plan.BuyLow = round2(plan.BuyLow)
	plan.BuyHigh = round2(plan.BuyHigh)
	plan.TargetPrice = round2(plan.TargetPrice)
	plan.StopPrice = round2(plan.StopPrice)
	plan.PlanNote = truncateRunes(strings.TrimSpace(plan.PlanNote), 300)
	if len(plan.Checklist) > 8 {
		plan.Checklist = plan.Checklist[:8]
	}
	for i, c := range plan.Checklist {
		plan.Checklist[i] = truncateRunes(strings.TrimSpace(c), 120)
	}

	pos := computePositionAdvice(closesFromSnapshot(snapshot), <-breadthCh)
	applyPlanDiscipline(plan, pos)
	plan.Position = pos
	result.TradePlan = plan
	return usage, run
}

// marketBreadth 取市场涨跌家数（择时系数用）。best-effort：仅 A 股有涨跌家数源，
// 失败/缺失返回 nil，由 computePositionAdvice 按中性处理并如实声明。
func (s *AnalysisService) marketBreadth(ctx context.Context, market string) *datasource.Breadth {
	if normalizeMarketOnly(market) != "cn" || s.market == nil {
		return nil
	}
	ov := s.market.GetOverview(ctx, "cn")
	if ov == nil {
		return nil
	}
	return ov.Breadth
}

// parseTradePlan 解析交易计划输出。服务端回填字段（rr_ratio/position/discipline_notes）
// 剥除，不可由模型伪造（同 parseAnalysisResult 剥信任层字段前例）。
func parseTradePlan(content string) (*tradePlan, error) {
	jsonStr := extractJSONObject(content)
	if jsonStr == "" {
		return nil, errors.New("未找到 JSON 对象")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	for _, k := range []string{"rr_ratio", "position", "discipline_notes"} {
		delete(raw, k)
	}
	cleaned, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	var p tradePlan
	if err := json.Unmarshal(cleaned, &p); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	return &p, nil
}

// validateTradePlan 自洽纪律校验（返回 error 触发 repair）。核心硬纪律：止损必须低于现价。
func validateTradePlan(px float64, p *tradePlan) error {
	for _, v := range []struct {
		name string
		val  float64
	}{
		{"buy_low", p.BuyLow}, {"buy_high", p.BuyHigh},
		{"target_price", p.TargetPrice}, {"stop_price", p.StopPrice},
	} {
		if v.val <= 0 || math.IsNaN(v.val) || math.IsInf(v.val, 0) {
			return fmt.Errorf("%s 必须为正数", v.name)
		}
	}
	if p.BuyLow > p.BuyHigh {
		return errors.New("buy_low 不得高于 buy_high")
	}
	if p.StopPrice >= px {
		return fmt.Errorf("止损价 %.2f 必须低于现价 %.2f（硬纪律）", p.StopPrice, px)
	}
	if p.StopPrice >= p.BuyLow {
		return errors.New("止损价必须低于买入区间下沿")
	}
	if p.TargetPrice <= p.BuyHigh || p.TargetPrice <= px {
		return errors.New("目标价必须高于买入区间上沿与现价")
	}
	if p.HorizonDays < 1 || p.HorizonDays > 120 {
		return errors.New("horizon_days 须为 1~120 的整数（交易日）")
	}
	if len(p.Checklist) == 0 {
		return errors.New("checklist 至少给 1 条操作核对项")
	}
	return nil
}

// applyPlanDiscipline 服务端后处理纪律：盈亏比 =(目标-区间中值)/(区间中值-止损)，
// <2:1 时仓位建议减半并记入 discipline_notes（宁可少赚不做赔率不足的重仓）。
func applyPlanDiscipline(p *tradePlan, pos *positionAdvice) {
	entry := (p.BuyLow + p.BuyHigh) / 2
	risk := entry - p.StopPrice
	if risk <= 0 {
		return // validateTradePlan 已保证不会走到；防御性兜底
	}
	rr := (p.TargetPrice - entry) / risk
	p.RRRatio = round2(rr)
	if rr < 2 {
		if pos != nil && pos.PositionPct > 0 {
			pos.PositionPct = round2(pos.PositionPct * 0.5)
		}
		p.Discipline = append(p.Discipline,
			fmt.Sprintf("盈亏比 %.2f 低于 2:1 纪律线，仓位建议已减半", rr))
	}
}

// computePositionAdvice 量化仓位公式（纯函数）：
// 仓位% = 100 × clip(2.5/vol20, 0.3, 1.0) × timing，timing = clip(0.6+(涨家占比-0.5)×1.2, 0.3, 1.2)。
// closes 为按日升序的收盘价（快照 recent_bars 提取）；不足 21 根时波动率不可得，如实不给仓位数字。
// breadth 缺失时择时系数按 0.6（与涨跌均衡的中性市场同待遇，偏保守）并声明。
func computePositionAdvice(closes []float64, breadth *datasource.Breadth) *positionAdvice {
	pos := &positionAdvice{}
	if len(closes) < 21 {
		pos.Note = "日线不足 21 根，20 日波动率不可得，仓位建议缺席"
		return pos
	}
	tail := closes[len(closes)-21:]
	rets := make([]float64, 0, 20)
	for i := 1; i < len(tail); i++ {
		if tail[i-1] <= 0 {
			pos.Note = "收盘价序列异常（含非正值），仓位建议缺席"
			return pos
		}
		rets = append(rets, (tail[i]-tail[i-1])/tail[i-1]*100)
	}
	vol := stddev(rets)
	pos.Vol20 = round2(vol)
	if vol < 0.05 {
		// 近乎零波动（长期一字/数据异常）：2.5/vol 发散，按上限 1.0 处理。
		pos.VolCoef = 1.0
	} else {
		pos.VolCoef = round2(clipf(2.5/vol, 0.3, 1.0))
	}

	timing := 0.6
	if breadth != nil {
		total := breadth.Advances + breadth.Declines + breadth.Unchanged
		if total > 0 {
			adv := float64(breadth.Advances) / float64(total)
			pos.AdvanceRatio = round2(adv)
			timing = clipf(0.6+(adv-0.5)*1.2, 0.3, 1.2)
		} else {
			pos.Note = "市场涨跌家数不可得，择时系数按中性 0.6"
		}
	} else {
		pos.Note = "市场涨跌家数不可得，择时系数按中性 0.6"
	}
	pos.TimingCoef = round2(timing)
	pos.PositionPct = round2(clipf(100*pos.VolCoef*timing, 0, 100))
	return pos
}

func clipf(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// quotePriceFromSnapshot 从个股快照取现价（quote.price）。
func quotePriceFromSnapshot(snapshot map[string]any) float64 {
	q, ok := snapshot["quote"].(map[string]any)
	if !ok {
		return 0
	}
	return toF64(q["price"])
}

// closesFromSnapshot 从快照 recent_bars（compactBars 产物，升序）提取收盘价序列。
// 兼容 Go 原生 map（[]map[string]any）与 JSON 回灌（[]any）两形态。
func closesFromSnapshot(snapshot map[string]any) []float64 {
	var out []float64
	appendClose := func(m map[string]any) {
		if c := toF64(m["c"]); c > 0 {
			out = append(out, c)
		}
	}
	switch rows := snapshot["recent_bars"].(type) {
	case []map[string]any:
		for _, m := range rows {
			appendClose(m)
		}
	case []any:
		for _, v := range rows {
			if m, ok := v.(map[string]any); ok {
				appendClose(m)
			}
		}
	}
	return out
}

// hasBlockRiskFlag 快照 risk_gate 是否含 level=block 的条目（兼容两形态，同 riskGateTexts）。
func hasBlockRiskFlag(snapshot map[string]any) bool {
	blk, ok := snapshot["risk_gate"].(map[string]any)
	if !ok {
		return false
	}
	switch fl := blk["flags"].(type) {
	case []riskFlag:
		for _, f := range fl {
			if f.Level == "block" {
				return true
			}
		}
	case []any:
		for _, v := range fl {
			if m, ok := v.(map[string]any); ok {
				if lv, _ := m["level"].(string); lv == "block" {
					return true
				}
			}
		}
	}
	return false
}

func toF64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
