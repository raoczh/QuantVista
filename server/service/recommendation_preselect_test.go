package service

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

func preselectionTestTable() *FactorTable {
	t := &FactorTable{TradeDate: "2026-09-11", Symbols: []string{"600101", "600102", "600103"}, Names: []string{"支撑附近", "大成交延伸", "流动性低"}, LastDates: []string{"2026-09-11", "2026-09-11", "2026-09-11"}, cols: map[string][]float64{}}
	for _, def := range factorDefs {
		t.cols[def.Key] = []float64{math.NaN(), math.NaN(), math.NaN()}
	}
	for key, value := range map[string]float64{"close": 10, "amount_yi": 1, "above_ma20": 1, "above_ma60": 1, "bull_align": 1, "chg_5d": 3, "chg_20d": 10, "vol_boost": 1.5, "volatility_20": 2, "drawdown_20": 5, "atr_pct": 2, "bias_20": 0.5, "vol_5v20": 0.8} {
		t.cols[key] = []float64{value, value, value}
	}
	t.cols["amount_yi"] = []float64{1, 20, 0.1}
	t.cols["bias_20"] = []float64{0.5, 15, 0}
	return t
}

func TestRecommendationPreselectionBeforeAmountTruncation(t *testing.T) {
	table := preselectionTestTable()
	for _, profile := range []string{"momentum", "pullback", "value", "leader", "balanced"} {
		idxs := []int{1, 2, 0}
		facts := rankRecommendationScan(table, idxs, profile, 1)
		if idxs[0] != 0 || idxs[2] != 2 {
			t.Fatalf("%s 应保留入场距离合理者并后置薄成交样本: %v", profile, idxs)
		}
		if facts[0].Matched != 3 || facts[0].Retained != 1 || facts[0].Truncated != 2 || facts[0].Rank != 1 {
			t.Fatalf("应记录截断前机会集: %+v", facts[0])
		}
		other := []int{0, 1, 2}
		rankRecommendationScan(table, other, profile, 1)
		if !reflect.DeepEqual(idxs, other) {
			t.Fatal("扫描输入顺序不得决定预选")
		}
	}
	// 缺失量能可追溯，不能流出 NaN/Inf 到持久化快照。
	table.cols["vol_boost"][0] = math.Inf(1)
	fact := recommendationWidePreselection(table, 0, "momentum")
	if fact.Status != "partial" || len(fact.Missing) == 0 || !finiteRecNumber(*fact.Score) {
		t.Fatalf("缺口应显式记录: %+v", fact)
	}
}

func TestCandidateBudgetCoverageAndStableRefill(t *testing.T) {
	var base []candidate
	for _, source := range []string{"watchlist", "daily_discovery", "strategy_signal", "gainer"} {
		for i := 0; i < 200; i++ {
			base = append(base, candidate{Symbol: fmt.Sprintf("%s%03d", source, i), Sources: []string{source}})
		}
	}
	for _, intake := range []bool{true, false} {
		limit := maxScanCandidates
		if intake {
			limit = maxPoolIntake
		}
		first := append([]candidate(nil), base...)
		second := append([]candidate(nil), base...)
		for i, j := 0, len(second)-1; i < j; i, j = i+1, j-1 {
			second[i], second[j] = second[j], second[i]
		}
		allocateCandidateBudget(first, limit, intake, "watchlist")
		allocateCandidateBudget(second, limit, intake, "watchlist")
		slots := func(pool []candidate) map[string]int {
			got := map[string]int{}
			for _, c := range pool {
				if c.Excluded == "" {
					budget := c.ScanBudget
					if intake {
						budget = c.IntakeBudget
					}
					got[c.Symbol] = budget.Order
				}
			}
			return got
		}
		if !reflect.DeepEqual(slots(first), slots(second)) {
			t.Fatal("来源预算不得依赖输入排列")
		}
		counts := map[string]int{}
		for _, c := range first {
			if c.Excluded == "" {
				counts[c.Sources[0]]++
			}
		}
		if len(slots(first)) != limit || len(counts) != 4 || counts["watchlist"] == limit {
			t.Fatalf("大来源不能垄断: %+v", counts)
		}
		if !intake {
			refill := sortRecommendationRefill(second)
			if len(refill) == 0 || second[refill[0]].ScanBudget.Order != limit+1 {
				t.Fatal("补位应沿用已冻结的预算顺序")
			}
		}
	}
}
