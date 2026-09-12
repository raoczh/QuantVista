package service

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestStrategyConditionThreeValuedLogic(t *testing.T) {
	bars := genTrendBars(30, 10, 0)
	known := leafV("close", ">", 1)
	miss := leafV("close", ">", 100)
	// 使用目录内的缺失字段；不足 120 根时 is_false 也不得把未知当成否。
	unknown := leafTrue("above_ma120")
	cases := []struct {
		name   string
		tree   CondNode
		status string
		full   bool
	}{
		{"all met", allOf(known), strategyMatched, true},
		{"partial", allOf(known, miss), strategyMissed, false},
		{"unknown", allOf(known, unknown), strategyUnknown, false},
		{"false and unknown", allOf(miss, unknown), strategyMissed, false},
		{"true or unknown", CondNode{Any: []CondNode{known, unknown}}, strategyMatched, true},
		{"false or unknown", CondNode{Any: []CondNode{miss, unknown}}, strategyUnknown, false},
		{"unknown is false", CondNode{Factor: "above_ma120", Op: "is_false"}, strategyUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strat := &strategyTemplate{screen: &recScreenBinding{}, tree: &tc.tree}
			h := evaluateStrategyHit(strat, "600100", wideStockMeta{}, bars)
			if h.Status != tc.status || h.Full != tc.full {
				t.Fatalf("unexpected: %+v", h)
			}
			reason := strategyConditionReason(strat, h)
			if (reason == "") != tc.full {
				t.Fatalf("必要条件不一致: %+v reason=%s", h, reason)
			}
			if tc.status == strategyUnknown && (h.Missing == 0 || len(h.Unknown) == 0 || !strings.Contains(reason, "数据不足")) {
				t.Fatalf("缺失必须单列: %+v", h)
			}
			if !tc.full {
				if delta, _ := screenStrategyBonus(h); delta > 0 {
					t.Fatal("部分或未知命中不得加分")
				}
			}
		})
	}
	bars[len(bars)-1].Amount = 0
	vals := computeWideRow("600100", wideStockMeta{}, bars)
	if !math.IsNaN(vals[factorIndex["amount_yi"]]) {
		t.Fatal("未提供的成交额不能成为真实零值")
	}
}

func TestStrategyCurrentPriceGuardPreservesClosedSignals(t *testing.T) {
	bars := genTrendBars(80, 10, 0)
	tree := allOf(leafV("close", "<", 20), leafTrue("above_ma20"))
	strat := &strategyTemplate{screen: &recScreenBinding{}, tree: &tree}
	c := candidate{Symbol: "600100", Price: 10}
	c.StrategyHit = evaluateStrategyHit(strat, c.Symbol, wideStockMeta{}, bars)
	before := append([]float64(nil), c.StrategyHit.row...)
	check := currentStrategyCheck(strat, c, 25)
	if check.Status != strategyMissed || check.Checked != 2 || len(check.Missed) != 1 {
		t.Fatalf("越过价格约束必须拒绝: %+v", check)
	}
	for i, v := range before {
		if !(v == c.StrategyHit.row[i] || math.IsNaN(v) && math.IsNaN(c.StrategyHit.row[i])) {
			t.Fatal("复核改写了冻结因子")
		}
	}
	// OR 分支仍按原逻辑组合，不能把其中一个未满足分支变成额外的 AND 限制。
	tree = CondNode{Any: []CondNode{leafV("close", "<", 20), leafTrue("above_ma20")}}
	c.StrategyHit = evaluateStrategyHit(strat, c.Symbol, wideStockMeta{}, bars)
	check = currentStrategyCheck(strat, c, 25)
	if check.Status != strategyMatched || len(check.Missed) != 0 {
		t.Fatalf("OR 语义被改变: %+v", check)
	}
	// 收盘阴阳等形态保留在完整日线，不用新价拼造一根 K 线。
	tree = CondNode{Factor: "close", Op: ">=", Ref: "open"}
	c.StrategyHit = evaluateStrategyHit(strat, c.Symbol, wideStockMeta{}, bars)
	check = currentStrategyCheck(strat, c, 8)
	if check.Status != strategyMatched || check.Checked != 0 {
		t.Fatalf("历史 K 线不能被当前报价改写: %+v", check)
	}
}

func TestScorePoolStrictStrategyBeforeEnrichment(t *testing.T) {
	setupTestDB(t)
	resetRecommendationPreheatState(t)
	pool := recScorePoolCandidates(2)
	pool[0].Sources = []string{"watchlist"}
	pool[1].Sources = []string{"strategy_signal"}
	svc := NewRecommendationService(NewMarketService(datasource.NewManagerWithAdapters(&recPreheatMarketAdapter{})), nil, nil)
	calls := 0
	svc.em.SetFetchForTest(func(context.Context, string, map[string]string) ([]byte, int, error) {
		calls++
		return nil, 503, datasource.ErrNoData
	})
	tree := allOf(leafV("close", ">", 0), leafV("chg_pct", ">", 999))
	strat := &strategyTemplate{baseKey: "momentum", screen: &recScreenBinding{}, tree: &tree}
	svc.scorePool(context.Background(), model.RecTypeShortTerm, strat, pool, RecFilters{}, nil)
	for _, c := range pool {
		if c.SentToLLM || c.Rank != 0 || c.StrategyHit == nil || c.StrategyHit.Hit != 1 || !strings.Contains(c.Excluded, "必要条件") {
			t.Fatalf("混合来源也必须满足指定策略: %+v", c)
		}
	}
	if calls != 0 {
		t.Fatal("必要条件失败不应浪费富化预算")
	}
}

func TestRecommendationTimeFactsAndIntradayGainGate(t *testing.T) {
	setupTestDB(t)
	for _, date := range []string{"2026-09-10", "2026-09-11", "2026-09-14"} {
		if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	end := time.Date(2026, 9, 10, 0, 0, 0, 0, time.Local)
	bars := make([]datasource.Bar, 6)
	for i := range bars {
		p := 100 + float64(i)*2
		bars[i] = datasource.Bar{TradeDate: end.AddDate(0, 0, i-5).Format("2006-01-02"), Open: p, High: p + 1, Low: p - 1, Close: p, Volume: 1000}
	}
	partial := datasource.Bar{TradeDate: "2026-09-11", Open: 110, High: 140, Low: 105, Close: 130, Volume: 1}
	input := append(append([]datasource.Bar(nil), bars...), partial)
	before := append([]datasource.Bar(nil), input...)
	c := candidate{Symbol: "600100", Market: "cn", Price: 110, QuoteAsOf: "2026-09-11 10:30"}
	completed, reason := recommendationCompletedBars(input, c)
	if reason != "" || len(completed) != 6 || !reflect.DeepEqual(before, input) {
		t.Fatalf("盘中根应被剔除且不修改原序列: len=%d reason=%s", len(completed), reason)
	}
	c.Timing = buildRecTimeFacts(c, completed)
	c.Factors = computeCandFactors(c.Price, completed)
	if c.Timing.ReturnAnchors["5"].Close != 102 || c.Factors.Chg5d != 10 {
		t.Fatalf("信号涨幅与当前窗口应各自正确: %+v %+v", c.Timing, c.Factors)
	}
	if reason := applyGainFilter(c, c.Factors, RecFilters{MaxGain5dPct: 20}); reason != "" {
		t.Fatal(reason)
	}
	latestAt, _ := time.ParseInLocation("2006-01-02 15:04", "2026-09-11 10:40", time.Local)
	if reason := finalQuoteFilterReason(c, &datasource.Quote{Price: 130, DataTime: latestAt}, RecFilters{MaxGain5dPct: 20}); !strings.Contains(reason, "近5日涨幅") {
		t.Fatalf("盘中大涨应在终选追高复核中排除: %s", reason)
	}
	if c.Price != 110 || c.Factors.Chg5d != 10 || c.Timing.CurrentReturns["5"] != 7.84 {
		t.Fatal("最新报价不能改写原评分事实")
	}
	sameDay := c
	sameDay.QuoteAsOf = "2026-09-10 15:00"
	if got := buildRecTimeFacts(sameDay, bars); got.ReturnAnchors["5"].Close != 100 {
		t.Fatalf("同收盘日的窗口偏移错误: %+v", got)
	}
	stale := c
	stale.QuoteAsOf = "2026-09-14 10:30"
	if _, reason := recommendationCompletedBars(bars, stale); reason == "" {
		t.Fatal("缺失最近完整交易日日线不能通过")
	}
	unknown := c
	unknown.QuoteAsOf = ""
	if _, reason := recommendationCompletedBars(bars, unknown); reason == "" {
		t.Fatal("没有行情时点不能证明信号可用")
	}
	short := c
	short.Timing = buildRecTimeFacts(c, bars[len(bars)-3:])
	if reason := applyGainFilter(short, c.Factors, RecFilters{MaxGain5dPct: 20}); !strings.Contains(reason, "数据不足") {
		t.Fatal("窗口不足不得按零涨幅通过")
	}
}
