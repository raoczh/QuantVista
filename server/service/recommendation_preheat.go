package service

import (
	"context"
	"sort"
	"time"

	"quantvista/common"
	"quantvista/model"
)

const (
	recEnrichmentAvailable = "available"
	recEnrichmentMissing   = "missing"
)

// recPreheatCandidate 只携带补拉前已经确定的 A 类/PIT 基础事实。
// NeedsFetch 仅决定是否进入对应域的规划池，不参与候选之间的优先级比较。
type recPreheatCandidate struct {
	Idx        int
	Symbol     string
	BaseScore  float64
	NeedsFetch bool
}

// planRecPreheat 按基础分降序、symbol 升序选定本域实际请求集合。
// reserve 在同一域冷却锁内占用请求槽；冷却命中返回 false，不消耗预算，规划继续扫描。
func planRecPreheat(candidates []recPreheatCandidate, budget int, reserve func(recPreheatCandidate) bool) []recPreheatCandidate {
	if budget <= 0 || len(candidates) == 0 {
		return nil
	}
	ordered := append([]recPreheatCandidate(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].BaseScore != ordered[j].BaseScore {
			return ordered[i].BaseScore > ordered[j].BaseScore
		}
		return ordered[i].Symbol < ordered[j].Symbol
	})
	selected := make([]recPreheatCandidate, 0, min(budget, len(ordered)))
	for _, c := range ordered {
		if !c.NeedsFetch || !reserve(c) {
			continue
		}
		selected = append(selected, c)
		if len(selected) >= budget {
			break
		}
	}
	return selected
}

type recRoundEnrichment struct {
	Flows            map[int][]model.FundFlowDaily
	Finance          map[int]*candFin
	FlowAvailable    map[int]bool
	FinanceAvailable map[int]bool
	FlowFetchSymbols []string
	FinFetchSymbols  []string
}

// preheatRecommendationRound 先完整读取本轮全部候选的两个本地缓存状态，再分别规划
// 财务与资金流的真实请求集合。两个集合都冻结后才执行 I/O；全部 I/O 结束后统一解析
// 为本轮不可变结果，调用方随后才能写 Factors/Fin 并计算最终分数。
func (s *RecommendationService) preheatRecommendationRound(
	ctx context.Context,
	recType string,
	pool []candidate,
	bases []recPreheatCandidate,
	finBudget, flowBudget *int,
	now time.Time,
) recRoundEnrichment {
	result := recRoundEnrichment{
		Flows:            make(map[int][]model.FundFlowDaily, len(bases)),
		Finance:          make(map[int]*candFin, len(bases)),
		FlowAvailable:    make(map[int]bool, len(bases)),
		FinanceAvailable: make(map[int]bool, len(bases)),
	}
	flowProbes := make(map[int]stockFundFlowProbe, len(bases))
	finProbes := make(map[int]financeFactorProbe, len(bases))
	flowPlans := make([]recPreheatCandidate, 0, len(bases))
	finPlans := make([]recPreheatCandidate, 0, len(bases))
	asOf := now.In(time.Local).Format("2006-01-02")

	// 只读阶段：任何候选开始补拉之前，先冻结整轮所有候选的本地状态。
	for _, base := range bases {
		flowProbe := inspectStockFundFlow(pool[base.Idx].Market, base.Symbol, now)
		flowProbes[base.Idx] = flowProbe
		flowPlan := base
		flowPlan.NeedsFetch = common.DB != nil && flowProbe.Market == "cn" && !flowProbe.Fresh
		flowPlans = append(flowPlans, flowPlan)

		if recType == model.RecTypeLongTerm {
			finProbe := inspectFinanceFactor(base.Symbol, asOf)
			finProbes[base.Idx] = finProbe
			finPlan := base
			finPlan.NeedsFetch = finProbe.RefreshNeeded
			finPlans = append(finPlans, finPlan)
		}
	}

	flowLimit := 0
	if flowBudget != nil {
		flowLimit = *flowBudget
	}
	flowSelected := planRecPreheat(flowPlans, flowLimit, func(c recPreheatCandidate) bool {
		p := flowProbes[c.Idx]
		return fflowTryAllowed(p.Market + ":" + p.Symbol)
	})
	if flowBudget != nil {
		*flowBudget -= len(flowSelected)
	}

	finLimit := 0
	if finBudget != nil {
		finLimit = *finBudget
	}
	finSelected := planRecPreheat(finPlans, finLimit, func(c recPreheatCandidate) bool {
		return finTryAllowed("ind:" + c.Symbol)
	})
	if finBudget != nil {
		*finBudget -= len(finSelected)
	}

	// 请求阶段：失败也已消耗预算；不得根据结果继续补选，避免结果反向影响集合。
	for _, selected := range flowSelected {
		p := fetchStockFundFlowReserved(ctx, s.em, flowProbes[selected.Idx], now)
		flowProbes[selected.Idx] = p
		result.FlowFetchSymbols = append(result.FlowFetchSymbols, selected.Symbol)
	}
	finFetched := make(map[int]bool, len(finSelected))
	for _, selected := range finSelected {
		fetchFinanceIndicators(ctx, selected.Symbol)
		finFetched[selected.Idx] = true
		result.FinFetchSymbols = append(result.FinFetchSymbols, selected.Symbol)
	}

	// 冻结阶段：先生成完整结果 map，调用方不得在此之前逐只写回或计算最终分数。
	for _, base := range bases {
		flowProbe := flowProbes[base.Idx]
		if flowProbe.Fresh && len(flowProbe.Rows) > 0 {
			result.Flows[base.Idx] = flowProbe.Rows
			result.FlowAvailable[base.Idx] = true
		} else {
			result.FlowAvailable[base.Idx] = false
		}
		if recType == model.RecTypeLongTerm {
			fin := resolveFinanceFactor(finProbes[base.Idx], finFetched[base.Idx])
			result.Finance[base.Idx] = fin
			result.FinanceAvailable[base.Idx] = fin != nil
		}
	}
	return result
}
