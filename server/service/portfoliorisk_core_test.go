package service

import (
	"math"
	"testing"
	"time"

	"quantvista/model"
)

func closeEnough(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func TestPortfolioRiskPureFunctions(t *testing.T) {
	points := []EquityPoint{
		{TradeDate: "2026-01-01", Assets: 100},
		{TradeDate: "2026-01-02", Assets: 110},
		{TradeDate: "2026-01-05", Assets: 110, CashFlow: 10},
		{TradeDate: "2026-01-06", Assets: 90},
	}
	if got := ComputeTWR(points); got.Status != RiskStatusAvailable || !closeEnough(got.Value, -18.1818181818, 1e-8) {
		t.Fatalf("TWR 错误: %+v", got)
	}
	returns, withReturns := DailyReturns(points)
	if len(returns) != 3 || withReturns[2].Return == nil || !closeEnough(*withReturns[2].Return, -9.090909, 1e-5) {
		t.Fatalf("日收益错误: %+v %+v", returns, withReturns)
	}
	if got := AnnualizedVolatility([]float64{0.01, -0.01, 0.02}, 252); got.Status != RiskStatusAvailable || got.Value <= 0 {
		t.Fatalf("年化波动错误: %+v", got)
	}
	if got := DownsideVolatility([]float64{0.01, -0.02, 0.03}, 252); got.Status != RiskStatusAvailable || got.Value <= 0 {
		t.Fatalf("下行波动错误: %+v", got)
	}
	if got := SharpeRatio([]float64{0.01, -0.01, 0.02}, 252, 0); got.Status != RiskStatusAvailable {
		t.Fatalf("Sharpe 错误: %+v", got)
	}
	if got := SortinoRatio([]float64{0.01, -0.01, 0.02}, 252, 0); got.Status != RiskStatusAvailable {
		t.Fatalf("Sortino 错误: %+v", got)
	}
	dd, curve := MaxDrawdown([]EquityPoint{{TradeDate: "d1", Assets: 100}, {TradeDate: "d2", Assets: 120}, {TradeDate: "d3", Assets: 90}, {TradeDate: "d4", Assets: 121}})
	if dd.PeakDate != "d2" || dd.TroughDate != "d3" || dd.RecoveryDate != "d4" || !closeEnough(dd.Metric.Value, -25, 1e-9) || curve[2].DrawdownPct == nil {
		t.Fatalf("最大回撤错误: %+v %+v", dd, curve)
	}
	beta, alpha := BetaAlpha([]float64{0.01, 0.02, -0.01}, []float64{0.005, 0.01, -0.005}, 252, 0)
	if !closeEnough(beta.Value, 2, 1e-9) || !closeEnough(alpha.Value, 0, 1e-9) {
		t.Fatalf("Beta/Alpha 错误: beta=%+v alpha=%+v", beta, alpha)
	}
}

func TestPortfolioRiskUnavailableCases(t *testing.T) {
	if got := ComputeTWR([]EquityPoint{{Assets: 100}}); got.Status != RiskStatusUnavailable || got.Reason == "" {
		t.Fatalf("样本不足不得返回 0: %+v", got)
	}
	partial := []EquityPoint{{TradeDate: "d1", Assets: 100}, {TradeDate: "d2", Assets: 110, Partial: true}}
	if got := ComputeTWR(partial); got.Status != RiskStatusUnavailable {
		t.Fatalf("partial 不得参与完整指标: %+v", got)
	}
	if beta, alpha := BetaAlpha([]float64{0.1}, nil, 252, 0); beta.Status != RiskStatusUnavailable || alpha.Status != RiskStatusUnavailable {
		t.Fatalf("基准缺失必须 unavailable: %+v %+v", beta, alpha)
	}
	returns, _ := DailyReturns([]EquityPoint{{TradeDate: "2026-01-01", Assets: 100}, {TradeDate: "2026-01-03", Assets: 102}})
	if len(returns) != 1 {
		t.Fatalf("缺失交易日不得插值，收益点=%d", len(returns))
	}
}

func TestCashFlowBetweenSnapshotsUsesNextInterval(t *testing.T) {
	points := []EquityPoint{
		{TradeDate: "2026-01-01", Assets: 100},
		{TradeDate: "2026-01-03", Assets: 210, CashFlow: 100},
	}
	got := ComputeTWR(points)
	if got.Status != RiskStatusAvailable || !closeEnough(got.Value, 10, 1e-9) {
		t.Fatalf("快照间入金应从下一段收益剔除: %+v", got)
	}
}

func TestRiskContributions(t *testing.T) {
	series := map[string]map[string]float64{
		"A": {"d1": 0.01, "d2": 0.02, "d3": -0.01},
		"B": {"d1": 0.01, "d2": 0.02, "d3": -0.01},
	}
	got := ComputeRiskContributions(series, map[string]float64{"A": 0.5, "B": 0.5}, 252, 30, "d3")
	if got.PredictedVolatility.Status != RiskStatusAvailable || got.SampleCount != 3 || len(got.Items) != 2 {
		t.Fatalf("风险贡献计算失败: %+v", got)
	}
	if !closeEnough(got.Items[0].RiskContributionPct, 50, 1e-9) || !closeEnough(got.Items[1].RiskContributionPct, 50, 1e-9) {
		t.Fatalf("等权同收益的风险贡献应各为 50%%: %+v", got.Items)
	}
	missing := ComputeRiskContributions(map[string]map[string]float64{"A": {"d1": 0.01}, "B": {"d2": 0.02}}, map[string]float64{"A": 0.5, "B": 0.5}, 252, 30, "d2")
	if missing.PredictedVolatility.Status != RiskStatusUnavailable || missing.PredictedVolatility.Reason == "" || missing.Items[0].Status != RiskStatusUnavailable {
		t.Fatalf("共同样本不足必须 unavailable: %+v", missing)
	}
}

func TestCorrelationStressAndRebalance(t *testing.T) {
	matrix := CorrelationFromReturns(map[string]map[string]float64{
		"A": {"d1": 0.1}, "B": {"d2": 0.2},
	}, 30, "d2")
	if matrix.Cells[0][1].Status != RiskStatusUnavailable || matrix.Cells[0][1].Value != 0 || matrix.Cells[0][1].Reason == "" {
		t.Fatalf("无共同样本的相关性不得显示为 0: %+v", matrix.Cells[0][1])
	}
	stress := ComputeStress([]StressHolding{
		{Symbol: "A", Name: "甲", Industry: "银行", Value: 10000, Price: 10, Known: true, ValuationKnown: true},
		{Symbol: "B", Name: "乙", Value: 5000, Known: false},
	}, StressScenario{Type: "industry", Industry: "银行", ShockPct: -20}, time.Unix(0, 0))
	if stress.EstimatedLossAmount != -2000 || stress.EstimatedLossPct != -20 || len(stress.Unknown) != 1 || !stress.ReadOnly {
		t.Fatalf("压力测试错误: %+v", stress)
	}
	draft := BuildRebalanceDraft([]RebalanceHolding{
		{Symbol: "600000", Name: "浦发", Market: "cn", Value: 10000, Quantity: 1000, Price: 10, Fresh: true},
	}, []TargetAllocationItem{{Type: "symbol", Key: "600000", TargetWeightPct: 60, Enabled: true}}, 20000)
	if len(draft) != 1 || draft[0].QuantityChange != 200 || draft[0].EstimatedFee != 5 {
		t.Fatalf("整百股/费用草案错误: %+v", draft)
	}
	blocked := BuildRebalanceDraft([]RebalanceHolding{{Symbol: "600001", Market: "cn", Value: 1000, Quantity: 100, Price: 10, Fresh: true, LimitUp: true}},
		[]TargetAllocationItem{{Type: "symbol", Key: "600001", TargetWeightPct: 20, Enabled: true}}, 10000)
	if blocked[0].Status != RiskStatusUnavailable || blocked[0].Reason == "" {
		t.Fatalf("涨停买入应不可执行: %+v", blocked)
	}
	cases := []struct {
		name    string
		holding RebalanceHolding
		reason  string
	}{
		{name: "停牌", holding: RebalanceHolding{Symbol: "600002", Market: "cn", Price: 10, Suspended: true}, reason: "标的停牌"},
		{name: "陈旧价格", holding: RebalanceHolding{Symbol: "600003", Market: "cn", Price: 10, FreshnessReason: "行情截至昨日，已过期"}, reason: "行情截至昨日，已过期"},
		{name: "缺价格", holding: RebalanceHolding{Symbol: "600004", Market: "cn"}, reason: "缺少 fresh 价格"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildRebalanceDraft([]RebalanceHolding{tc.holding}, []TargetAllocationItem{{Type: "symbol", Key: tc.holding.Symbol, TargetWeightPct: 10, Enabled: true}}, 10000)
			if len(got) != 1 || got[0].Status != RiskStatusUnavailable || got[0].Reason != tc.reason {
				t.Fatalf("不可执行原因错误: %+v", got)
			}
		})
	}
	sell := BuildRebalanceDraft([]RebalanceHolding{{Symbol: "600005", Market: "cn", Value: 10000, Quantity: 1000, Price: 10, Fresh: true}},
		[]TargetAllocationItem{{Type: "symbol", Key: "600005", TargetWeightPct: 40, Enabled: true}}, 20000)
	if sell[0].QuantityChange != -200 || sell[0].EstimatedFee <= 0 || sell[0].EstimatedTax <= 0 {
		t.Fatalf("卖出费用与税费估算错误: %+v", sell)
	}
}

func TestStressInputRequiresTarget(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	account, err := NewPortfolioAccountService().Create(406, PortfolioAccountInput{Name: "压力校验", Kind: model.PortfolioKindPaper, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	risk := NewPortfolioRiskService(&MarketService{}, NewPositionService(&MarketService{}))
	if _, err := risk.Stress(t.Context(), 406, account.ID, StressScenario{Type: "industry", ShockPct: -10}); err == nil {
		t.Fatal("行业场景缺行业必须拒绝")
	}
	if _, err := risk.Stress(t.Context(), 406, account.ID, StressScenario{Type: "symbol", ShockPct: -10}); err == nil {
		t.Fatal("单票场景缺代码必须拒绝")
	}
}
