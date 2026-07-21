package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestInReportWindow 收盘日报生成窗口（15:35 ~ 20:00）。
func TestInReportWindow(t *testing.T) {
	cases := []struct {
		hhmm string
		want bool
	}{
		{"09:00", false},
		{"15:29", false},
		{"15:34", false},
		{"15:35", true},
		{"16:00", true},
		{"19:59", true},
		{"20:00", false},
		{"23:30", false},
	}
	for _, c := range cases {
		tm, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-03 "+c.hhmm, time.Local)
		if got := inReportWindow(tm); got != c.want {
			t.Errorf("inReportWindow(%s) = %v, 期望 %v", c.hhmm, got, c.want)
		}
	}
}

// TestIsTradingDayToday 优先交易日历，无记录时回退周一~五（通用行情链路的历史兼容语义）。
func TestIsTradingDayToday(t *testing.T) {
	setupTestDB(t)

	// 2026-07-03 是周五：无日历记录 → 回退工作日判定 = true。
	fri, _ := time.ParseInLocation("2006-01-02", "2026-07-03", time.Local)
	if !isTradingDayToday(fri) {
		t.Fatalf("无日历时周五应视为交易日")
	}
	// 2026-07-04 是周六 → false。
	sat := fri.AddDate(0, 0, 1)
	if isTradingDayToday(sat) {
		t.Fatalf("无日历时周六不应视为交易日")
	}
	// 日历声明周五休市（如节假日调休）→ 以日历为准。
	common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-03", IsOpen: false})
	if isTradingDayToday(fri) {
		t.Fatalf("日历标记休市应优先于工作日回退")
	}
}

func TestDailyTradingDayStatusFailClosed(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.Local)
	if got := dailyTradingDayStatus(now); got != dailyTradingDayUnknown {
		t.Fatalf("缺少日历行应为 unknown，got %q", got)
	}
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-06", IsOpen: false}).Error; err != nil {
		t.Fatal(err)
	}
	if got := dailyTradingDayStatus(now); got != dailyTradingDayClosed {
		t.Fatalf("日历明确休市应为 closed，got %q", got)
	}
	common.DB.Model(&model.TradingCalendar{}).Where("market = ? AND trade_date = ?", "cn", "2026-07-06").Update("is_open", true)
	if got := dailyTradingDayStatus(now); got != dailyTradingDayOpen {
		t.Fatalf("日历明确开市应为 open，got %q", got)
	}
}

func TestDailyReportCalendarUnknownRefusal(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	svc := &DailyReportService{nowFn: func() time.Time {
		return time.Date(2026, 7, 6, 16, 0, 0, 0, time.Local)
	}}
	_, err := svc.GenerateFor(nil, 1, true)
	if RefusalCodeOf(err) != RefusalMarketCalendarUnknown {
		t.Fatalf("日历缺行必须机读拒答 %q，got %v", RefusalMarketCalendarUnknown, err)
	}
}

func TestDailySnapshotDateGate(t *testing.T) {
	old := &reportSnapshot{TradeDate: "2026-07-06", Market: &reportMarket{
		Indices:  []map[string]any{{"name": "上证", "price": 1.0, "trade_date": "2026-07-03"}},
		Breadth:  map[string]any{"advances": 100, "declines": 1000, "trade_date": "2026-07-03"},
		FundFlow: map[string]any{"main_net_yi": 1.0, "trade_date": "2026-07-03"},
	}}
	defs := reportDataDeficiencies(old, old.TradeDate)
	if len(defs) < 3 {
		t.Fatalf("三个旧核心块都应进入缺口清单，got %v", defs)
	}
	if got := pickDailyStrategy(old); got != "momentum" {
		t.Fatalf("旧 breadth 不得驱动明日策略，got %q", got)
	}
	if _, ok := old.Market.Indices[0]["price"]; ok {
		t.Fatal("旧指数数值必须从 LLM 输入/evidence 值域剥离")
	}
	if _, ok := old.Market.Breadth["advances"]; ok {
		t.Fatal("旧涨跌家数必须从 LLM 输入/evidence 值域剥离")
	}
	if _, ok := old.Market.FundFlow["main_net_yi"]; ok {
		t.Fatal("旧资金流数值必须从 LLM 输入/evidence 值域剥离")
	}
	unknown := &reportSnapshot{TradeDate: "2026-07-06", Market: &reportMarket{
		Indices:  []map[string]any{{"name": "上证", "price": 1.0}},
		Breadth:  map[string]any{"advances": 100, "declines": 1000},
		FundFlow: map[string]any{"main_net_yi": 1.0},
	}}
	if defs := reportDataDeficiencies(unknown, unknown.TradeDate); len(defs) < 3 {
		t.Fatalf("业务日期缺失的三个核心块都必须 fail-closed，got %v", defs)
	}
	if got := pickDailyStrategy(unknown); got != "momentum" {
		t.Fatalf("业务日期未知的 breadth 不得驱动明日策略，got %q", got)
	}
	intradayIndex := &reportSnapshot{TradeDate: "2026-07-06", Market: &reportMarket{
		Indices:  []map[string]any{{"name": "上证", "price": 3200.0, "trade_date": "2026-07-06", "data_time": "2026-07-06 10:00:00"}},
		Breadth:  map[string]any{"advances": 100, "declines": 1000, "trade_date": "2026-07-06"},
		FundFlow: map[string]any{"main_net_yi": 1.0, "trade_date": "2026-07-06"},
		Mood:     map[string]any{"trade_date": "2026-07-06"},
	}}
	if defs := reportDataDeficiencies(intradayIndex, intradayIndex.TradeDate); len(defs) != 1 {
		t.Fatalf("同日早盘指数必须登记收盘口径缺口: %v", defs)
	}
	if _, ok := intradayIndex.Market.Indices[0]["price"]; ok {
		t.Fatal("同日早盘停滞指数不得进入 LLM 输入/evidence 值域")
	}
	fresh := &reportSnapshot{TradeDate: "2026-07-06", Market: &reportMarket{
		Breadth: map[string]any{"advances": 100, "declines": 1000, "trade_date": "2026-07-06"},
	}}
	if got := pickDailyStrategy(fresh); got != "pullback" {
		t.Fatalf("当日 breadth 应按比例选择 pullback，got %q", got)
	}
}

func TestSelectReportWatchItemsRejectsStaleQuotes(t *testing.T) {
	groups := []WatchlistGroupView{{Items: []WatchlistItemView{
		{WatchlistItem: model.WatchlistItem{Symbol: "600001", Name: "旧行情"}, QuoteOK: true, ChangePct: 9.9, FreshnessStatus: freshStatusStale},
		{WatchlistItem: model.WatchlistItem{Symbol: "600002", Name: "新行情"}, QuoteOK: true, ChangePct: -3.2, FreshnessStatus: freshStatusFresh},
		{WatchlistItem: model.WatchlistItem{Symbol: "600003", Name: "未知"}, QuoteOK: true, ChangePct: 8.8, FreshnessStatus: freshStatusUnknown},
	}}}
	items := selectReportWatchItems(groups)
	if len(items) != 1 || items[0].Symbol != "600002" {
		t.Fatalf("日报自选异动只允许 fresh 行情，got %+v", items)
	}
}

// TestDailyReport_ListGetIsolation 日报按用户隔离；列表排除大字段。
func TestDailyReport_ListGetIsolation(t *testing.T) {
	setupTestDB(t)
	svc := &DailyReportService{rec: &RecommendationService{}}

	common.DB.Create(&model.DailyReport{UserID: 1, TradeDate: "2026-07-02", Market: "cn",
		Status: model.ReportStatusSuccess, ReviewJSON: `{"summary":"ok"}`, SnapshotJSON: `{}`})
	common.DB.Create(&model.DailyReport{UserID: 2, TradeDate: "2026-07-02", Market: "cn",
		Status: model.ReportStatusSuccess})

	rows, err := svc.List(1, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("用户1 应只见自己的 1 条: %v %d", err, len(rows))
	}
	if rows[0].ReviewJSON != "" {
		t.Fatalf("列表应排除大字段 review_json")
	}

	v, err := svc.Get(1, rows[0].ID)
	if err != nil || v.Review == nil || v.Review.Summary != "ok" {
		t.Fatalf("详情应含解析后的复盘: %v %+v", err, v)
	}
	if _, err := svc.Get(2, rows[0].ID); err == nil {
		t.Fatalf("跨用户 Get 应失败（隔离）")
	}

	// Latest：用户1 有、用户3 无（nil 不报错）。
	lv, err := svc.Latest(1)
	if err != nil || lv == nil {
		t.Fatalf("Latest 应返回用户1 的报告: %v", err)
	}
	lv3, err := svc.Latest(3)
	if err != nil || lv3 != nil {
		t.Fatalf("无日报用户 Latest 应返回 nil, nil: %v %+v", err, lv3)
	}
}

// TestChargeAction 手动动作计次辅助：只加 action_used。
func TestChargeAction(t *testing.T) {
	setupTestDB(t)
	if _, err := getUserQuota(9); err != nil {
		t.Fatalf("建配额行失败: %v", err)
	}
	chargeAction(9)
	q, _ := getUserQuota(9)
	if q.ActionUsed != 1 || q.TokenUsed != 0 || q.RequestCount != 0 {
		t.Fatalf("chargeAction 只应加次数: %+v", q)
	}
}
