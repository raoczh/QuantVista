package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// bar 便捷构造。
func bar(date string, open, high, low, close float64) datasource.Bar {
	return datasource.Bar{TradeDate: date, Open: open, High: high, Low: low, Close: close}
}

// TestEvaluateTracking_TakeProfit 短线止盈：当日 high 达标即触发，收益/涨幅/alpha 正确。
func TestEvaluateTracking_TakeProfit(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 12, StopLoss: 9, ValidDays: 5, IsShort: true,
		Bars: []datasource.Bar{
			bar("2026-06-02", 10.1, 10.5, 9.8, 10.2),
			bar("2026-06-03", 11.0, 12.3, 11.0, 12.1),
		},
		ElapsedTradeDays: 2, BenchStart: 3000, BenchEnd: 3060,
	})
	if r.Outcome != model.RecOutcomeTakeProfit || !r.HitTakeProfit || !r.ReviewNeeded {
		t.Fatalf("应触发止盈: %+v", r)
	}
	if r.CurrentPrice != 12.1 {
		t.Fatalf("现价应为末根收盘 12.1，得到 %v", r.CurrentPrice)
	}
	if r.ReturnPct != 21 { // (12.1-10)/10*100
		t.Fatalf("收益应 21%%，得到 %v", r.ReturnPct)
	}
	if r.MaxGainPct != 23 { // (12.3-10)/10*100
		t.Fatalf("最大涨幅应 23%%，得到 %v", r.MaxGainPct)
	}
	if r.BenchReturnPct != 2 || r.AlphaPct != 19 {
		t.Fatalf("基准应 2%%、alpha 应 19%%，得到 bench=%v alpha=%v", r.BenchReturnPct, r.AlphaPct)
	}
	if r.MaxDrawdownPct != 2 { // 首日低点 9.8 相对峰值 10
		t.Fatalf("最大回撤应 2%%，得到 %v", r.MaxDrawdownPct)
	}
}

// TestEvaluateTracking_StopLoss 短线止损：当日 low 破位即触发。
func TestEvaluateTracking_StopLoss(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 12, StopLoss: 9, ValidDays: 5, IsShort: true,
		Bars: []datasource.Bar{
			bar("2026-06-02", 10.0, 10.2, 9.5, 9.7),
			bar("2026-06-03", 9.6, 9.8, 8.8, 8.9),
		},
		ElapsedTradeDays: 2,
	})
	if r.Outcome != model.RecOutcomeStopLoss || !r.HitStopLoss || !r.ReviewNeeded {
		t.Fatalf("应触发止损: %+v", r)
	}
}

// TestEvaluateTracking_BothSameDay 同一日既触止盈又触止损，保守取止损。
func TestEvaluateTracking_BothSameDay(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 11, StopLoss: 9, ValidDays: 5, IsShort: true,
		Bars:             []datasource.Bar{bar("2026-06-02", 10, 11.5, 8.5, 10)},
		ElapsedTradeDays: 1,
	})
	if r.Outcome != model.RecOutcomeStopLoss {
		t.Fatalf("同日双触应保守取止损，得到 %s", r.Outcome)
	}
	if !r.HitTakeProfit || !r.HitStopLoss {
		t.Fatalf("两个触发标记都应为真: %+v", r)
	}
}

// TestEvaluateTracking_EarliestTrigger 不同日触发取更早者（先止盈）。
func TestEvaluateTracking_EarliestTrigger(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 11, StopLoss: 9, ValidDays: 5, IsShort: true,
		Bars: []datasource.Bar{
			bar("2026-06-02", 10, 11.2, 10, 11), // 先触止盈
			bar("2026-06-03", 10, 10, 8.5, 8.8), // 后触止损
		},
		ElapsedTradeDays: 2,
	})
	if r.Outcome != model.RecOutcomeTakeProfit {
		t.Fatalf("更早触发的止盈应为主结局，得到 %s", r.Outcome)
	}
}

// TestEvaluateTracking_Expired 未触发但超有效期 → 过期，需复盘。
func TestEvaluateTracking_Expired(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 20, StopLoss: 5, ValidDays: 3, IsShort: true,
		Bars: []datasource.Bar{
			bar("2026-06-02", 10, 10.3, 9.9, 10.1),
			bar("2026-06-03", 10.1, 10.4, 10.0, 10.2),
		},
		ElapsedTradeDays: 4,
	})
	if r.Outcome != model.RecOutcomeExpired || !r.ReviewNeeded {
		t.Fatalf("应过期需复盘: %+v", r)
	}
}

// TestEvaluateTracking_Active 未触发且未过期 → 进行中，不需复盘。
func TestEvaluateTracking_Active(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 20, StopLoss: 5, ValidDays: 10, IsShort: true,
		Bars:             []datasource.Bar{bar("2026-06-02", 10, 10.3, 9.9, 10.1)},
		ElapsedTradeDays: 2,
	})
	if r.Outcome != model.RecOutcomeActive || r.ReviewNeeded {
		t.Fatalf("应进行中不需复盘: %+v", r)
	}
}

// TestEvaluateTracking_NoData 无日线 → no_data。
func TestEvaluateTracking_NoData(t *testing.T) {
	r := evaluateTracking(trackInput{RefPrice: 10, IsShort: true})
	if r.Outcome != model.RecOutcomeNoData {
		t.Fatalf("空日线应 no_data，得到 %s", r.Outcome)
	}
}

// TestEvaluateTracking_ExpiredThenHit 有效期过后才触达止盈价 → 结局应为过期，
// 不得记为止盈（触发窗口受 ValidDays 约束，PRD 3.7 expired 为独立终态）。
func TestEvaluateTracking_ExpiredThenHit(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 12, StopLoss: 8, ValidDays: 2, IsShort: true,
		Bars: []datasource.Bar{
			bar("2026-06-02", 10, 10.3, 9.9, 10.1),  // 窗口内第 1 日：未触发
			bar("2026-06-03", 10.1, 10.4, 10, 10.2), // 窗口内第 2 日：未触发
			bar("2026-06-10", 11, 12.5, 11, 12.2),   // 已过有效期后才碰到止盈价
		},
		ElapsedTradeDays: 6,
	})
	if r.Outcome != model.RecOutcomeExpired {
		t.Fatalf("有效期后触达不应记止盈，应过期: %+v", r)
	}
	if r.HitTakeProfit {
		t.Fatalf("有效期外的触达不应置 HitTakeProfit: %+v", r)
	}
}

// TestEvaluateTracking_BadLowBar Low=0 的坏行不得误报止损，也不得算出 ~100% 回撤。
func TestEvaluateTracking_BadLowBar(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, TakeProfit: 20, StopLoss: 9, ValidDays: 5, IsShort: true,
		Bars: []datasource.Bar{
			bar("2026-06-02", 10, 10.3, 0, 10.1), // 坏行：Low=0（解析失败）
			bar("2026-06-03", 10.1, 10.4, 9.9, 10.2),
		},
		ElapsedTradeDays: 2,
	})
	if r.Outcome != model.RecOutcomeActive || r.HitStopLoss {
		t.Fatalf("Low=0 坏行不应触发止损: %+v", r)
	}
	if r.PeriodLow != 9.9 {
		t.Fatalf("区间低点应忽略坏行取 9.9，得到 %v", r.PeriodLow)
	}
	if r.MaxDrawdownPct > 5 {
		t.Fatalf("坏行不应产生虚假深回撤，得到 %v%%", r.MaxDrawdownPct)
	}
}

// TestEvaluateTracking_LongReview 长线超过复盘周期 → ReviewNeeded=true（时间型触发）。
func TestEvaluateTracking_LongReview(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, IsShort: false, ReviewAfterDays: 60,
		Bars:             []datasource.Bar{bar("2026-06-02", 10, 11, 10, 10.8)},
		ElapsedTradeDays: 61,
	})
	if r.Outcome != model.RecOutcomeTracking || !r.ReviewNeeded {
		t.Fatalf("长线超复盘周期应提示复盘: %+v", r)
	}
	// 未到周期不提示。
	r2 := evaluateTracking(trackInput{
		RefPrice: 10, IsShort: false, ReviewAfterDays: 60,
		Bars:             []datasource.Bar{bar("2026-06-02", 10, 11, 10, 10.8)},
		ElapsedTradeDays: 30,
	})
	if r2.ReviewNeeded {
		t.Fatalf("未到复盘周期不应提示: %+v", r2)
	}
}

// TestEvaluateTracking_Long 长线不做价格触发，结局为 tracking，但仍算收益/alpha。
func TestEvaluateTracking_Long(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, IsShort: false,
		Bars:       []datasource.Bar{bar("2026-06-02", 10, 11, 10, 10.8)},
		BenchStart: 100, BenchEnd: 105,
	})
	if r.Outcome != model.RecOutcomeTracking || r.ReviewNeeded {
		t.Fatalf("长线应 tracking 不需复盘: %+v", r)
	}
	if r.ReturnPct != 8 || r.AlphaPct != 3 { // 8% - 5%
		t.Fatalf("长线收益/alpha 计算错误: %+v", r)
	}
}

// TestEvaluateTracking_NoBench 无基准数据时 alpha=0 且标记 HasBench=false。
func TestEvaluateTracking_NoBench(t *testing.T) {
	r := evaluateTracking(trackInput{
		RefPrice: 10, IsShort: false,
		Bars: []datasource.Bar{bar("2026-06-02", 10, 11, 10, 11)},
	})
	if r.HasBench || r.AlphaPct != 0 {
		t.Fatalf("无基准应 alpha=0 HasBench=false: %+v", r)
	}
}

// TestEvaluateTracking_NodeReturns 节点收益：已到节点按第 N 交易日收盘记录，未到为 nil。
func TestEvaluateTracking_NodeReturns(t *testing.T) {
	bars := make([]datasource.Bar, 0, 8)
	dates := []string{"2026-06-01", "2026-06-02", "2026-06-03", "2026-06-04", "2026-06-05", "2026-06-08", "2026-06-09", "2026-06-10"}
	closes := []float64{10.1, 10.2, 10.3, 10.4, 10.5, 10.6, 10.7, 10.8}
	for i, d := range dates {
		bars = append(bars, bar(d, closes[i], closes[i]+0.1, closes[i]-0.1, closes[i]))
	}
	r := evaluateTracking(trackInput{
		RefPrice: 10, IsShort: false, Bars: bars, ElapsedTradeDays: 8,
	})
	if r.Return7d == nil || *r.Return7d != 7 { // bars[6].Close=10.7 → +7%
		t.Fatalf("第 7 交易日收益应 7%%，得到 %v", r.Return7d)
	}
	if r.Return14d != nil || r.Return30d != nil {
		t.Fatalf("未到 14/30 节点应为 nil: %v %v", r.Return14d, r.Return30d)
	}

	// 已过节点但日线不足（停牌缺 bar）：顺延不记录。
	r2 := evaluateTracking(trackInput{
		RefPrice: 10, IsShort: false, Bars: bars[:5], ElapsedTradeDays: 9,
	})
	if r2.Return7d != nil {
		t.Fatalf("日线不足 7 根时不应记节点收益: %v", r2.Return7d)
	}
}

// TestPerformanceNodeAverages 节点均值：仅统计已到节点（非 NULL）的样本。
func TestPerformanceNodeAverages(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM recommendation_statuses")
	svc := &TrackingService{}

	f := func(v float64) *float64 { return &v }
	rows := []model.RecommendationStatus{
		{RecommendationID: 11, UserID: 1, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeActive, Return7d: f(5), Return14d: f(8)},
		{RecommendationID: 12, UserID: 1, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeActive, Return7d: f(-1)},
		{RecommendationID: 13, UserID: 1, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeActive}, // 未到任何节点
	}
	for i := range rows {
		if err := common.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	p, err := svc.Performance(1, "")
	if err != nil {
		t.Fatalf("Performance 失败: %v", err)
	}
	if p.Sample7d != 2 || p.Avg7dPct != 2 { // (5-1)/2
		t.Fatalf("7 日节点均值应 2%%（n=2），得到 %v (n=%d)", p.Avg7dPct, p.Sample7d)
	}
	if p.Sample14d != 1 || p.Avg14dPct != 8 {
		t.Fatalf("14 日节点均值应 8%%（n=1），得到 %v (n=%d)", p.Avg14dPct, p.Sample14d)
	}
	if p.Sample30d != 0 || p.Avg30dPct != 0 {
		t.Fatalf("无 30 日样本应为 0，得到 %v (n=%d)", p.Avg30dPct, p.Sample30d)
	}
}

// TestUpsertStatusIdempotent 同一 recommendation_id 重复 upsert 只有一行且被覆盖。
func TestUpsertStatusIdempotent(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM recommendation_statuses")
	svc := &TrackingService{}

	st := &model.RecommendationStatus{
		RecommendationID: 1, BatchID: 1, UserID: 1, Symbol: "600000", Market: "cn",
		Type: model.RecTypeShortTerm, Action: model.RecActionBuy, RefPrice: 10,
		ReturnPct: 5, Outcome: model.RecOutcomeActive,
	}
	if err := svc.upsertStatus(st); err != nil {
		t.Fatalf("首次 upsert 失败: %v", err)
	}
	st.ReturnPct = 12
	st.Outcome = model.RecOutcomeTakeProfit
	if err := svc.upsertStatus(st); err != nil {
		t.Fatalf("二次 upsert 失败: %v", err)
	}

	var cnt int64
	common.DB.Model(&model.RecommendationStatus{}).Where("recommendation_id = ?", 1).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("应只有 1 行（幂等），得到 %d", cnt)
	}
	var got model.RecommendationStatus
	common.DB.Where("recommendation_id = ?", 1).First(&got)
	if got.ReturnPct != 12 || got.Outcome != model.RecOutcomeTakeProfit {
		t.Fatalf("二次 upsert 应覆盖: %+v", got)
	}
}

// TestPerformanceStats 表现统计：样本量/胜率/结局计数/基准样本，且用户隔离。
func TestPerformanceStats(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM recommendation_statuses")
	svc := &TrackingService{}

	rows := []model.RecommendationStatus{
		{RecommendationID: 1, UserID: 1, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeTakeProfit, ReturnPct: 15, AlphaPct: 10, Note: ""},
		{RecommendationID: 2, UserID: 1, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeStopLoss, ReturnPct: -8, AlphaPct: -5, Note: ""},
		{RecommendationID: 3, UserID: 1, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeActive, ReturnPct: 3, AlphaPct: 0, Note: benchUnavailableNote},
		{RecommendationID: 4, UserID: 1, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeNoData, Note: "无数据"},
		{RecommendationID: 5, UserID: 2, Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeTakeProfit, ReturnPct: 100, AlphaPct: 50, Note: ""},
	}
	for i := range rows {
		if err := common.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("插入状态失败: %v", err)
		}
	}

	p, err := svc.Performance(1, "")
	if err != nil {
		t.Fatalf("Performance 失败: %v", err)
	}
	if p.Sample != 3 { // 排除 no_data
		t.Fatalf("样本量应为 3，得到 %d", p.Sample)
	}
	if p.TakeProfit != 1 || p.StopLoss != 1 || p.Active != 1 {
		t.Fatalf("结局计数错误: %+v", p)
	}
	if p.WinRate != 66.67 { // 2/3
		t.Fatalf("胜率应 66.67，得到 %v", p.WinRate)
	}
	if p.BenchSample != 2 { // 排除 benchUnavailableNote 那条
		t.Fatalf("基准样本应 2，得到 %d", p.BenchSample)
	}
	if p.AvgAlphaPct != 2.5 { // (10-5)/2
		t.Fatalf("平均 alpha 应 2.5，得到 %v", p.AvgAlphaPct)
	}

	// 用户隔离。
	p2, _ := svc.Performance(2, "")
	if p2.Sample != 1 {
		t.Fatalf("用户2 应只见自己 1 条，得到 %d", p2.Sample)
	}
}

// TestFrozenTerminal 终态冻结判定（#6）：止盈/止损/过期冻结；active/tracking/no_data 不冻结。
func TestFrozenTerminal(t *testing.T) {
	for _, o := range []string{model.RecOutcomeTakeProfit, model.RecOutcomeStopLoss, model.RecOutcomeExpired} {
		if !frozenTerminal(o) {
			t.Fatalf("%s 应为终态冻结", o)
		}
	}
	for _, o := range []string{model.RecOutcomeActive, model.RecOutcomeTracking, model.RecOutcomeNoData, ""} {
		if frozenTerminal(o) {
			t.Fatalf("%s 不应冻结（no_data 是暂态、tracking 收益应继续演进）", o)
		}
	}
}

// TestActualReturnEndPrice 已平仓持仓实际收益按真实卖出价定格（#13a），未平仓/缺卖出价用最新价。
func TestActualReturnEndPrice(t *testing.T) {
	closed := model.Position{Status: model.PositionStatusClosed, SellPrice: 13.5}
	if p := actualReturnEndPrice(closed, 20); p != 13.5 {
		t.Fatalf("已平仓应按卖出价 13.5 定格，得到 %v", p)
	}
	holding := model.Position{Status: model.PositionStatusHolding}
	if p := actualReturnEndPrice(holding, 20); p != 20 {
		t.Fatalf("持有中应用最新价 20，得到 %v", p)
	}
	closedNoSell := model.Position{Status: model.PositionStatusClosed, SellPrice: 0}
	if p := actualReturnEndPrice(closedNoSell, 20); p != 20 {
		t.Fatalf("已平仓缺卖出价应回退最新价 20，得到 %v", p)
	}
}

// TestShouldAppendQuoteBar 周末/节假日不追加实时 bar（#27b）：优先交易日历，缺失回退周一~五。
func TestShouldAppendQuoteBar(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	t.Cleanup(func() { common.DB.Where("1 = 1").Delete(&model.TradingCalendar{}) })

	sat := time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local) // 周六
	fri := time.Date(2026, 7, 17, 10, 0, 0, 0, time.Local) // 周五
	recDate := "2026-07-01"

	// 交易日历标注周六休市：不追加（显式 Select 强制写 is_open=false）。
	common.DB.Select("Market", "TradeDate", "IsOpen").
		Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-18", IsOpen: false})
	if shouldAppendQuoteBar(sat, recDate, nil) {
		t.Fatalf("周六休市不应追加实时 bar")
	}
	// 交易日历标注周五开市：追加。
	common.DB.Select("Market", "TradeDate", "IsOpen").
		Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-17", IsOpen: true})
	if !shouldAppendQuoteBar(fri, recDate, nil) {
		t.Fatalf("周五开市应追加实时 bar")
	}
	// 已有今日日线根：不重复追加。
	if shouldAppendQuoteBar(fri, recDate, []datasource.Bar{bar("2026-07-17", 10, 10, 10, 10)}) {
		t.Fatalf("已有今日日线不应再追加")
	}
	// 今天不晚于推荐日：不追加。
	if shouldAppendQuoteBar(fri, "2026-07-17", nil) {
		t.Fatalf("今天不晚于推荐日不应追加")
	}
	// 无日历回退周一~五：周六不追加、周五追加。
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	if shouldAppendQuoteBar(sat, recDate, nil) {
		t.Fatalf("无日历回退：周六不应追加")
	}
	if !shouldAppendQuoteBar(fri, recDate, nil) {
		t.Fatalf("无日历回退：周五应追加")
	}
}

// TestRefreshBatchesFreezeTerminal 终态行二次刷新不被重算改写（#6），且不触碰上游
// （svc.market 为 nil，全部终态冻结时应在拉取基准/上游前提前返回）。
func TestRefreshBatchesFreezeTerminal(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM recommendation_batches")
	common.DB.Exec("DELETE FROM recommendations")
	common.DB.Exec("DELETE FROM recommendation_statuses")

	batch := model.RecommendationBatch{UserID: 1, Type: model.RecTypeShortTerm, Market: "cn", Status: model.RecStatusSuccess}
	if err := common.DB.Create(&batch).Error; err != nil {
		t.Fatalf("插入批次失败: %v", err)
	}
	rec := model.Recommendation{BatchID: batch.ID, UserID: 1, Symbol: "600000", Market: "cn", Action: model.RecActionBuy, RefPrice: 10}
	if err := common.DB.Create(&rec).Error; err != nil {
		t.Fatalf("插入推荐失败: %v", err)
	}
	// 已止盈终态，收益定格 15%、现价 11.5。
	st := model.RecommendationStatus{
		RecommendationID: rec.ID, BatchID: batch.ID, UserID: 1, Symbol: "600000", Market: "cn",
		Type: model.RecTypeShortTerm, Action: model.RecActionBuy, RefPrice: 10,
		Outcome: model.RecOutcomeTakeProfit, ReturnPct: 15, CurrentPrice: 11.5,
	}
	if err := common.DB.Create(&st).Error; err != nil {
		t.Fatalf("插入状态失败: %v", err)
	}

	svc := &TrackingService{} // nil market：终态冻结路径不得触碰上游，否则会 panic
	n := svc.refreshBatches(context.Background(), []model.RecommendationBatch{batch})
	if n != 0 {
		t.Fatalf("全部终态冻结应处理 0 条，得到 %d", n)
	}
	var got model.RecommendationStatus
	common.DB.Where("recommendation_id = ?", rec.ID).First(&got)
	if got.ReturnPct != 15 || got.Outcome != model.RecOutcomeTakeProfit || got.CurrentPrice != 11.5 {
		t.Fatalf("终态行不应被重算改写: %+v", got)
	}
}

// TestTrackableUserIDsIncludesDegraded 后台枚举纳入 degraded 用户（#27c）：只有降级批次
// 的用户也应被刷新；失败批次与超窗批次不计。
func TestTrackableUserIDsIncludesDegraded(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM recommendation_batches")
	now := time.Now()
	cutoff := now.AddDate(0, 0, -trackWindowDays)

	rows := []model.RecommendationBatch{
		{UserID: 10, Status: model.RecStatusDegraded, CreatedAt: now},                                   // 仅降级：应枚举
		{UserID: 20, Status: model.RecStatusSuccess, CreatedAt: now},                                    // 成功：应枚举
		{UserID: 30, Status: model.RecStatusFailed, CreatedAt: now},                                     // 失败：不枚举
		{UserID: 40, Status: model.RecStatusDegraded, CreatedAt: now.AddDate(0, 0, -trackWindowDays-5)}, // 超窗降级：不枚举
	}
	for i := range rows {
		if err := common.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("插入批次失败: %v", err)
		}
	}

	ids, err := trackableUserIDs(cutoff)
	if err != nil {
		t.Fatalf("trackableUserIDs 失败: %v", err)
	}
	set := map[int64]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if !set[10] {
		t.Fatalf("仅降级批次的用户 10 应被枚举，得到 %v", ids)
	}
	if !set[20] {
		t.Fatalf("成功批次的用户 20 应被枚举，得到 %v", ids)
	}
	if set[30] {
		t.Fatalf("仅失败批次的用户 30 不应枚举，得到 %v", ids)
	}
	if set[40] {
		t.Fatalf("超窗降级的用户 40 不应枚举，得到 %v", ids)
	}
}
