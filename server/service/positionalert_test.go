package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// D14/D15 单测：成本止盈止损与持仓期回撤。
//
// 覆盖：纯函数触发/不触发与消息内容、fail-closed（无 fresh 行情不触发）、
// 「我的全部持仓」逐仓展开与多标的各落一条事件、峰值初始化/盘后抬升/加仓重置/
// 减仓不重置/除权折算与撤销还原、用户隔离。

// seedHoldingWithPeak 建一笔带峰值的持仓。
func seedHoldingWithPeak(t *testing.T, userID int64, symbol, name string,
	cost, qty, peak float64, peakFrom string) *model.Position {
	t.Helper()
	p := &model.Position{
		UserID: userID, Symbol: symbol, Market: "cn", Name: name,
		PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding,
		BuyPrice: cost, Quantity: qty, BuyDate: peakFrom,
		TotalBuyCost: cost * qty, TotalBuyQty: qty,
		PeakPrice: peak, PeakDate: peakFrom, PeakFrom: peakFrom,
	}
	if err := common.DB.Create(p).Error; err != nil {
		t.Fatalf("建持仓失败: %v", err)
	}
	if err := common.DB.Create(&model.PositionTrade{
		UserID: userID, PositionID: p.ID, Side: model.PositionTradeBuy,
		Price: cost, Quantity: qty, TradeDate: peakFrom,
		AvgCostAfter: cost, QuantityAfter: qty,
	}).Error; err != nil {
		t.Fatalf("建流水失败: %v", err)
	}
	return p
}

// TestEvaluatePositionAlertPure 纯函数逐 kind 手工验算（含边界与不可判定分支）。
func TestEvaluatePositionAlertPure(t *testing.T) {
	// 成本 10 元、现价 12、当日最高 13、当日最低 9.5、峰值 20。
	in := positionAlertEval{AvgCost: 10, Price: 12, DayHigh: 13, DayLow: 9.5, Peak: 20, PeakDate: "2026-06-01"}

	// cost_gain：按当日最高判触达 →(13-10)/10=30%。阈值 30 恰好命中（>= 边界）。
	gainRule := model.AlertRule{Kind: model.AlertKindCostGain, Threshold: 30, Symbol: "600000"}
	ok, v, msg := evaluatePositionAlert(gainRule, "浦发银行", in)
	if !ok || v != 30 {
		t.Fatalf("cost_gain 边界应命中且观测值 30，得到 ok=%v v=%v", ok, v)
	}
	if !containsAll(msg, "浦发银行", "成本 10.00", "现价 12.00", "盈利 20.00%") {
		t.Fatalf("cost_gain 消息必须带我的成本与当前浮盈亏: %s", msg)
	}
	// 阈值 30.01 不命中。
	if ok, _, _ := evaluatePositionAlert(
		model.AlertRule{Kind: model.AlertKindCostGain, Threshold: 30.01}, "", in); ok {
		t.Fatal("cost_gain 超阈值 0.01 不应命中")
	}

	// cost_drawdown：按当日最低判 →(10-9.5)/10=5%。
	ok, v, msg = evaluatePositionAlert(
		model.AlertRule{Kind: model.AlertKindCostDrawdown, Threshold: 5}, "浦发银行", in)
	if !ok || v != 5 {
		t.Fatalf("cost_drawdown 应命中且观测值 5，得到 ok=%v v=%v", ok, v)
	}
	if !containsAll(msg, "当日最低 9.50") {
		t.Fatalf("cost_drawdown 消息应带当日最低: %s", msg)
	}

	// peak_drawdown：(20-9.5)/20=52.5%。
	ok, v, msg = evaluatePositionAlert(
		model.AlertRule{Kind: model.AlertKindPeakDrawdown, Threshold: 30}, "浦发银行", in)
	if !ok || v != 52.5 {
		t.Fatalf("peak_drawdown 应命中且观测值 52.5，得到 ok=%v v=%v", ok, v)
	}
	if !containsAll(msg, "持仓期最高 20.00", "2026-06-01") {
		t.Fatalf("peak_drawdown 消息应带峰值与其日期: %s", msg)
	}

	// **不可判定 ≠ 未触发**：峰值为 0（未初始化）时 peak_drawdown 恒不命中且观测值为 0，
	// 调用方据此不写 last_value（否则用户会看到「回撤 0%」这个假状态）。
	noPeak := in
	noPeak.Peak = 0
	if ok, v, _ := evaluatePositionAlert(
		model.AlertRule{Kind: model.AlertKindPeakDrawdown, Threshold: 1}, "", noPeak); ok || v != 0 {
		t.Fatalf("峰值未建立时不应命中，得到 ok=%v v=%v", ok, v)
	}
	// 成本为 0（异常数据）一律不判。
	noCost := in
	noCost.AvgCost = 0
	if ok, _, _ := evaluatePositionAlert(
		model.AlertRule{Kind: model.AlertKindCostDrawdown, Threshold: 1}, "", noCost); ok {
		t.Fatal("成本为 0 时不应命中")
	}
	// 「赚过 30% 现在倒亏」——用户最痛的场景必须能触发（不加「仅浮盈时才提醒」的前置条件）。
	loss := positionAlertEval{AvgCost: 10, Price: 8, DayHigh: 8.2, DayLow: 7.9, Peak: 13, PeakDate: "2026-05-01"}
	ok, _, msg = evaluatePositionAlert(
		model.AlertRule{Kind: model.AlertKindPeakDrawdown, Threshold: 30}, "", loss)
	if !ok {
		t.Fatal("已由盈转亏时移动止盈仍须触发（这正是用户最后悔的场景）")
	}
	if !containsAll(msg, "亏损 20.00%") {
		t.Fatalf("倒亏场景消息应如实写亏损: %s", msg)
	}
}

// TestPositionAlertAllPositionsAndFailClosed 「我的全部持仓」逐仓展开、
// 多标的各落一条事件、无 fresh 行情不触发。
func TestPositionAlertAllPositionsAndFailClosed(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")

	seedHoldingWithPeak(t, 1, "600000", "浦发银行", 10, 1000, 10, today)
	seedHoldingWithPeak(t, 1, "600519", "贵州茅台", 100, 100, 100, today)
	// 第三只：行情 stale，fail-closed 不参与判定。
	seedHoldingWithPeak(t, 1, "000002", "万科A", 10, 1000, 10, today)
	// 他人持仓（同样满足触发条件）——用户隔离，绝不能出现在用户 1 的事件里。
	seedHoldingWithPeak(t, 2, "600004", "白云机场", 10, 1000, 10, today)

	market := &fakeAlertMarket{
		getFreshQuote: func(_ context.Context, _, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
			switch symbol {
			case "600000": // 当日最低 7.9 → 相对成本 10 跌 21%
				return &datasource.Quote{Price: 8, High: 8.2, Low: 7.9}, quoteFreshInfo{Status: freshStatusFresh}, nil
			case "600519": // 当日最低 69 → 相对成本 100 跌 31%
				return &datasource.Quote{Price: 70, High: 72, Low: 69}, quoteFreshInfo{Status: freshStatusFresh}, nil
			case "000002": // 跌得更多，但行情已过期 → 本轮不评
				return &datasource.Quote{Price: 5, High: 5, Low: 5}, quoteFreshInfo{Status: freshStatusStale}, nil
			}
			return &datasource.Quote{Price: 8, High: 8, Low: 8}, quoteFreshInfo{Status: freshStatusFresh}, nil
		},
	}
	svc := &AlertService{market: market}

	// 一条不绑 symbol 的规则 = 我的全部持仓，跌 10% 提醒。
	rule := &model.AlertRule{
		UserID: 1, Symbol: "", Market: "cn", Name: positionAlertAllName,
		Kind: model.AlertKindCostDrawdown, Op: model.AlertOpGTE, Threshold: 10,
		Once: false, Status: model.AlertStatusActive,
	}
	if err := common.DB.Create(rule).Error; err != nil {
		t.Fatalf("建规则失败: %v", err)
	}

	hits, err := svc.evaluatePositionRules(context.Background(), 1, []model.AlertRule{*rule})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if hits != 2 {
		t.Fatalf("两只 fresh 持仓应各命中一次（stale 的不评），得到 %d", hits)
	}
	var events []model.AlertEvent
	common.DB.Where("rule_id = ?", rule.ID).Find(&events)
	if len(events) != 2 {
		t.Fatalf("一条规则应为两只标的各落一条事件（旧的按规则判当日会压成一条），得到 %d", len(events))
	}
	bySym := map[string]model.AlertEvent{}
	for _, e := range events {
		bySym[e.Symbol] = e
		if e.UserID != 1 {
			t.Fatalf("事件必须归属规则所有者: %+v", e)
		}
		if e.TradeDate != today {
			t.Fatalf("事件必须带交易日（去重键的一部分）: %+v", e)
		}
		if e.PositionID == 0 {
			t.Fatalf("持仓类事件必须带 position_id（同一标的可有多笔仓位）: %+v", e)
		}
	}
	if _, ok := bySym["000002"]; ok {
		t.Fatal("fail-closed 失守：行情已过期的持仓不得触发提醒")
	}
	if _, ok := bySym["600004"]; ok {
		t.Fatal("用户隔离失守：他人持仓不得进本用户的事件")
	}

	// 规则行的 last_value 取本轮最极端观测值（茅台 31% > 浦发 21%），
	// trigger_msg 也应是最紧急的那一笔。
	var saved model.AlertRule
	common.DB.First(&saved, rule.ID)
	if saved.LastValue != 31 {
		t.Fatalf("last_value 应为本轮最极端观测值 31，得到 %v", saved.LastValue)
	}
	if !containsAll(saved.TriggerMsg, "贵州茅台") {
		t.Fatalf("trigger_msg 应取最紧急的那笔: %s", saved.TriggerMsg)
	}
	// 规则**不得**被置为 triggered（持仓类 Once 恒 false，暂停整条会让其余持仓失联）。
	if saved.Status != model.AlertStatusActive {
		t.Fatalf("持仓类规则命中后必须保持 active，得到 %s", saved.Status)
	}

	// 重复评估同一天：不再新增事件（去重键 rule+symbol+trade_date 幂等）。
	if _, err := svc.evaluatePositionRules(context.Background(), 1, []model.AlertRule{saved}); err != nil {
		t.Fatalf("重复评估失败: %v", err)
	}
	var again int64
	common.DB.Model(&model.AlertEvent{}).Where("rule_id = ?", rule.ID).Count(&again)
	if again != 2 {
		t.Fatalf("同日重复评估不应新增事件，得到 %d", again)
	}
}

// TestPositionAlertBoundToSymbol 绑定单只 symbol 的规则只评该标的。
func TestPositionAlertBoundToSymbol(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	seedHoldingWithPeak(t, 1, "600000", "浦发银行", 10, 1000, 10, today)
	seedHoldingWithPeak(t, 1, "600519", "贵州茅台", 100, 100, 100, today)

	market := &fakeAlertMarket{
		getFreshQuote: func(_ context.Context, _, _ string) (*datasource.Quote, quoteFreshInfo, error) {
			return &datasource.Quote{Price: 5, High: 5, Low: 5}, quoteFreshInfo{Status: freshStatusFresh}, nil
		},
	}
	svc := &AlertService{market: market}
	rule := &model.AlertRule{
		UserID: 1, Symbol: "600519", Market: "cn", Name: "贵州茅台",
		Kind: model.AlertKindCostDrawdown, Op: model.AlertOpGTE, Threshold: 10,
		Status: model.AlertStatusActive,
	}
	common.DB.Create(rule)
	if _, err := svc.evaluatePositionRules(context.Background(), 1, []model.AlertRule{*rule}); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	var events []model.AlertEvent
	common.DB.Where("rule_id = ?", rule.ID).Find(&events)
	if len(events) != 1 || events[0].Symbol != "600519" {
		t.Fatalf("绑定 symbol 的规则只应评该标的，得到 %+v", events)
	}
}

// TestAlertValidatePositionKinds 持仓类规则的校验：op 归一、Once 强制 false、阈值区间。
func TestAlertValidatePositionKinds(t *testing.T) {
	svc := &AlertService{}
	in := AlertInput{Kind: model.AlertKindPeakDrawdown, Op: "lte", Threshold: 15, Once: true}
	if err := svc.validate(&in); err != nil {
		t.Fatalf("合法入参不应报错: %v", err)
	}
	if in.Op != model.AlertOpGTE {
		t.Fatalf("持仓类 op 应归一为 gte，得到 %s", in.Op)
	}
	if in.Once {
		t.Fatal("持仓类 Once 必须强制 false（否则命中一笔就暂停整条规则，其余持仓失联）")
	}
	for _, bad := range []AlertInput{
		{Kind: model.AlertKindPeakDrawdown, Threshold: 0},
		{Kind: model.AlertKindPeakDrawdown, Threshold: 100},
		{Kind: model.AlertKindCostDrawdown, Threshold: -1},
		{Kind: model.AlertKindCostGain, Threshold: 1001},
	} {
		b := bad
		if err := svc.validate(&b); err == nil {
			t.Fatalf("越界阈值应被拒绝: %+v", bad)
		}
	}
}

// TestPositionPeakLifecycle 峰值全生命周期：初始化 → 盘后抬升 → 加仓重置 → 减仓不重置。
func TestPositionPeakLifecycle(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	svc := &PositionService{}

	// 建仓：峰值 = 买入价、起算日 = 买入日。
	p := seedHoldingWithPeak(t, 1, "600000", "浦发银行", 10, 1000, 0, "")
	common.DB.Model(p).Updates(map[string]any{"peak_price": 0, "peak_date": "", "peak_from": "", "buy_date": "2026-06-01"})
	// 惰性初始化（无本地日线 → 回退买入价，不标 backfilled）。
	var rows []model.Position
	common.DB.Where("user_id = ?", 1).Find(&rows)
	if !backfillPositionPeaks(1, rows) {
		t.Fatal("首次读取应初始化峰值")
	}
	var cur model.Position
	common.DB.First(&cur, p.ID)
	if cur.PeakPrice != 10 || cur.PeakFrom != "2026-06-01" {
		t.Fatalf("峰值初值应为买入价与买入日，得到 %v / %s", cur.PeakPrice, cur.PeakFrom)
	}
	if cur.PeakBackfilled {
		t.Fatal("无本地日线时不应标记回填")
	}

	// 盘后抬升：当日日线 high=15 → 峰值 15。
	common.DB.Create(&model.DailyBar{Symbol: "600000", Market: "cn", TradeDate: "2026-06-10",
		Open: 12, High: 15, Low: 11, Close: 14})
	if n := RunPositionPeakUpdate("2026-06-10"); n != 1 {
		t.Fatalf("应更新 1 笔峰值，得到 %d", n)
	}
	common.DB.First(&cur, p.ID)
	if cur.PeakPrice != 15 || cur.PeakDate != "2026-06-10" {
		t.Fatalf("峰值应被当日最高抬到 15，得到 %v / %s", cur.PeakPrice, cur.PeakDate)
	}
	// 只抬不降：回落的一天不改写峰值。
	common.DB.Create(&model.DailyBar{Symbol: "600000", Market: "cn", TradeDate: "2026-06-11",
		Open: 13, High: 13.5, Low: 12, Close: 12.5})
	if n := RunPositionPeakUpdate("2026-06-11"); n != 0 {
		t.Fatalf("低于峰值的一天不应更新，得到 %d", n)
	}

	// **减仓不重置**：剩余仓位的持有期是连续的。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 13, Quantity: 300, TradeDate: "2026-06-12",
	}); err != nil {
		t.Fatalf("减仓失败: %v", err)
	}
	common.DB.First(&cur, p.ID)
	if cur.PeakPrice != 15 {
		t.Fatalf("减仓后峰值必须保持 15（口径铁律），得到 %v", cur.PeakPrice)
	}

	// **加仓重置**：成本已变，加仓前的高点不再是这本账赚到过的利润。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 12, Quantity: 700, TradeDate: "2026-06-13",
	}); err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	common.DB.First(&cur, p.ID)
	if cur.PeakPrice != 12 || cur.PeakFrom != "2026-06-13" || cur.PeakDate != "2026-06-13" {
		t.Fatalf("加仓应把峰值重置为加仓价与加仓日，得到 %v / from=%s date=%s",
			cur.PeakPrice, cur.PeakFrom, cur.PeakDate)
	}
	// 反例锁定：若不重置，此刻回撤 =(15-12)/15=20%，一条 15% 的移动止盈规则会在
	// 用户刚刚回调加仓时当场误报。
	if dd := peakDrawdownPct(15, 12); dd < 15 {
		t.Fatalf("回撤算式意外变化：不重置峰值时应有 20%% 回撤，得到 %v", dd)
	}
}

// TestPositionPeakBackfillFromLocalBars 存量持仓用本地日线回填历史最高并标口径。
func TestPositionPeakBackfillFromLocalBars(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -20).Format("2006-01-02")
	mid := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
	// 买入日之前的高点**不算**（那不是用户赚到过的利润）。
	before := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	common.DB.Create(&model.DailyBar{Symbol: "600000", Market: "cn", TradeDate: before, High: 99, Close: 98})
	common.DB.Create(&model.DailyBar{Symbol: "600000", Market: "cn", TradeDate: mid, High: 18, Close: 17})

	p := seedHoldingWithPeak(t, 1, "600000", "浦发银行", 10, 1000, 0, from)
	common.DB.Model(p).Updates(map[string]any{"peak_price": 0, "peak_date": "", "peak_from": ""})
	var rows []model.Position
	common.DB.Where("user_id = ?", 1).Find(&rows)
	backfillPositionPeaks(1, rows)

	var cur model.Position
	common.DB.First(&cur, p.ID)
	if cur.PeakPrice != 18 || cur.PeakDate != mid {
		t.Fatalf("应回填持仓期内最高 18，得到 %v / %s（买入日之前的 99 不得计入）", cur.PeakPrice, cur.PeakDate)
	}
	if !cur.PeakBackfilled {
		t.Fatal("日线回填必须标记 backfilled（前复权口径与账面价可能有出入）")
	}
	if cur.PeakFrom != from {
		t.Fatalf("峰值起算日应为买入日，得到 %s", cur.PeakFrom)
	}
	_ = today
}

// TestAdjustPriceForCorpAction 价格侧折算与成本侧口径一致（手工验算）。
func TestAdjustPriceForCorpAction(t *testing.T) {
	// 每 10 股送 10 股：价格减半。
	if got := adjustPriceForCorpAction(20, 10, 0, 0); got != 10 {
		t.Fatalf("10 送 10 后峰值应减半为 10，得到 %v", got)
	}
	// 每 10 股派 5 元：单价减 0.5。
	if got := adjustPriceForCorpAction(20, 0, 0, 5); got != 19.5 {
		t.Fatalf("每10股派5元后峰值应为 19.5，得到 %v", got)
	}
	// 与成本折算同构：对同一笔持仓，成本与峰值必须按同一比例移动，
	// 否则回撤百分比会在除权当天凭空跳变。
	res, ok := computeCorpAdjust(1000, 20, 10, 0, 5)
	if !ok {
		t.Fatal("成本折算应成立")
	}
	peak := adjustPriceForCorpAction(20, 10, 0, 5)
	if peak != res.CostAfter {
		t.Fatalf("同一价位的成本折算(%v)与峰值折算(%v)必须一致", res.CostAfter, peak)
	}
}

// containsAll 断言辅助：msg 是否包含全部片段。
func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
