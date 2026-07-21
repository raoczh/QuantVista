package service

import (
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// 行情时效 fail-closed 批（P0/P1）针对性测试：
//   - 同日早盘停滞价在交易时段/午休/盘后一律 stale（不能只看日期）；
//   - fresh 刷新候选后重新执行价格/涨停筛选（applyFreshQuoteToCand）；
//   - 全 stale 提前失败也落池快照 + quote_stale 事实账本（failEmptyShortlist）；
//   - ev3 数值核验来源分类汇总（snapshot/plan/user/context）；
//   - 日报数据水位（reportDataDeficiencies：缺块/旧 mood 不冒充今日）；
//   - M3a/M3b 信号消费水位（openDaysBehind：旧记录不永久冒充最近信号）。

// pinCalendarTo 把交易日历钉到给定开市日（其余日期无行）：prevOpenTradeDate(今天)
// 解析为其中最大者，信号/宽表水位判定（signalDateUsable/openDaysBehind）与 seed 数据
// 齐平。openDaysBehind 收紧后（区间无日历记录返回 -1 fail-closed），seed 旧日期数据的
// 测试必须显式钉日历，不能再依赖「无日历时旧数据被当可用」的旧 fail-open 行为。
// 内存库 cache=shared：进场清空 + Cleanup 清空，防污染他测。
func pinCalendarTo(t *testing.T, openDates ...string) {
	t.Helper()
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	for _, d := range openDates {
		common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: d, IsOpen: true})
	}
	t.Cleanup(func() { common.DB.Where("1 = 1").Delete(&model.TradingCalendar{}) })
}

// TestQuoteFreshnessIntradayStagnation 同日 09:30 停滞价：14:00（交易时段）距今远超
// 10 分钟容忍，恒 stale——「日期是今天」不等于「数据有效」。
func TestQuoteFreshnessIntradayStagnation(t *testing.T) {
	exp := "2026-07-17"
	if fi := quoteFreshness(tm("09:30"), tm("14:00"), marketStateTrading, exp); fi.Status != freshStatusStale {
		t.Fatalf("14:00 收到同日 09:30 停滞价应 stale, got %s", fi.Status)
	}
	// 昨日行情「请求成功」：盘中期望今日，昨日 15:00 收盘价 stale。
	yesterdayClose := time.Date(2026, 7, 16, 15, 0, 0, 0, time.Local)
	if fi := quoteFreshness(yesterdayClose, tm("10:00"), marketStateTrading, exp); fi.Status != freshStatusStale {
		t.Fatalf("盘中收到昨日收盘价应 stale, got %s", fi.Status)
	}
	// 零时间（timestamp_unknown）在任意时段恒 stale。
	for _, st := range []string{marketStateTrading, marketStateBreak, marketStatePostClose, marketStateClosed, marketStatePreOpen} {
		if fi := quoteFreshness(time.Time{}, tm("10:00"), st, exp); fi.Status != freshStatusStale {
			t.Fatalf("零值 DataTime 在 %s 应 stale, got %s", st, fi.Status)
		}
	}
}

// TestApplyFreshQuoteToCand fresh 行情应用到候选：quote 派生字段刷新 + 复筛重新执行
// （涨停判定/价格区间基于新价）。
func TestApplyFreshQuoteToCand(t *testing.T) {
	dt := time.Date(2026, 7, 17, 10, 30, 0, 0, time.Local)
	// 刷新：旧价 9.0 → 新价 9.8，Amount/QuoteAsOf 一并更新。
	c := candidate{Symbol: "600000", Market: "cn", Name: "浦发银行", Price: 9.0, ChangePct: 1.0, Amount: 1e8, LimitUp: 10.45}
	q := &datasource.Quote{Price: 9.8, ChangePct: 3.2, Amount: 2e8, DataTime: dt}
	if reason := applyFreshQuoteToCand(&c, q, RecFilters{}); reason != "" {
		t.Fatalf("正常新价不应被复筛淘汰: %s", reason)
	}
	if c.Price != 9.8 || c.ChangePct != 3.2 || c.Amount != 2e8 {
		t.Fatalf("quote 派生字段应刷新: %+v", c)
	}
	if c.QuoteAsOf != "2026-07-17 10:30" {
		t.Fatalf("QuoteAsOf 应记录行情时刻: %q", c.QuoteAsOf)
	}

	// 复筛-涨停：旧价 9.0 未涨停（建池初筛通过），刷新后 10.45=涨停价 → 淘汰。
	c2 := candidate{Symbol: "600001", Market: "cn", Name: "测试", Price: 9.0, ChangePct: 1.0, Amount: 2e8, LimitUp: 10.45}
	q2 := &datasource.Quote{Price: 10.45, ChangePct: 10.0, Amount: 3e8, DataTime: dt}
	if reason := applyFreshQuoteToCand(&c2, q2, RecFilters{ExcludeLimitUp: true}); reason == "" {
		t.Fatal("刷新后已涨停应被复筛淘汰")
	}

	// 复筛-价格上限：旧价 45 在上限内，新价 55 越界 → 淘汰。
	c3 := candidate{Symbol: "600002", Market: "cn", Name: "测试2", Price: 45, ChangePct: 1.0, Amount: 2e8}
	q3 := &datasource.Quote{Price: 55, ChangePct: 2.0, Amount: 3e8, DataTime: dt}
	if reason := applyFreshQuoteToCand(&c3, q3, RecFilters{PriceMax: 50}); reason == "" {
		t.Fatal("刷新后越过价格上限应被复筛淘汰")
	}
}

// TestFailEmptyShortlistRecordsFacts 全 stale 提前失败：池快照/筛选参数回填、批次置
// failed、quote_stale 反事实事件与事实账本照落（不能在 recordBatchFacts 前直接 return）。
func TestFailEmptyShortlistRecordsFacts(t *testing.T) {
	setupTestDB(t)
	for _, tbl := range []string{"recommendation_batches", "recommendation_candidate_events", "recommendation_labels"} {
		common.DB.Exec("DELETE FROM " + tbl)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"recommendation_batches", "recommendation_candidate_events", "recommendation_labels"} {
			common.DB.Exec("DELETE FROM " + tbl)
		}
	})
	batch := &model.RecommendationBatch{UserID: 77, Type: model.RecTypeShortTerm, Market: "cn", Status: model.RecStatusProcessing}
	if err := common.DB.Create(batch).Error; err != nil {
		t.Fatalf("建批次失败: %v", err)
	}
	staleReason := "行情时效硬门：行情仅更新至 2026-07-17 09:30（期望交易日 2026-07-17），基于旧价的筛选、评分与价位不可信，透明排除"
	pool := []candidate{
		{Symbol: "600000", Market: "cn", Name: "浦发银行", Price: 9.8, Excluded: staleReason},
		{Symbol: "000001", Market: "cn", Name: "平安银行", Price: 11.2, Excluded: staleReason},
	}
	gates := []gateNote{
		{Symbol: "600000", GateType: model.GateQuoteStale, GateVersion: quoteFreshGateVersion, Reason: staleReason},
		{Symbol: "000001", GateType: model.GateQuoteStale, GateVersion: quoteFreshGateVersion, Reason: staleReason},
	}
	svc := &RecommendationService{} // market nil：failEmptyShortlist 不触上游
	err := svc.failEmptyShortlist(batch, pool, RecFilters{}, gates, nil, 0, true)
	if err == nil {
		t.Fatal("应返回失败错误")
	}

	var got model.RecommendationBatch
	if err := common.DB.First(&got, batch.ID).Error; err != nil {
		t.Fatalf("批次应存在: %v", err)
	}
	if got.Status != model.RecStatusFailed {
		t.Fatalf("批次应 failed: %s", got.Status)
	}
	if got.CandidatePool == "" || got.FiltersJSON == "" {
		t.Fatal("提前失败也应回填可复核的池快照与筛选参数")
	}
	if !got.FactsRecorded {
		t.Fatal("事实账本完整性标记应为 true")
	}
	var events []model.RecommendationCandidateEvent
	common.DB.Where("batch_id = ?", batch.ID).Find(&events)
	if len(events) != 2 {
		t.Fatalf("应落 2 条候选事件, got %d", len(events))
	}
	for _, e := range events {
		if e.GateType != model.GateQuoteStale {
			t.Fatalf("事件 gate_type 应为 quote_stale: %+v", e)
		}
	}
}

// TestEvidenceOriginSummary ev3 来源分类汇总：同一次核验中快照佐证 / plan 复述 /
// user 复述 / context 复述分开计数，Matched 仍为总和（legacy 兼容）。
func TestEvidenceOriginSummary(t *testing.T) {
	vals := []labeledValue{
		{Path: "quote.price", Value: 9.88},
		{Path: "交易计划", Value: 10.5, Origin: "plan"},
		{Path: "用户提问", Value: 8.8, Origin: "user"},
		{Path: "新闻标题", Value: 3.33, Origin: "context"},
	}
	check := verifyEvidenceLabeled([]evidenceSection{
		{Module: "回答", Text: "现价 9.88，目标价 10.5；你提到的成本 8.8 与新闻里的 3.33 亿"},
	}, vals)
	if check.Version != "ev3" {
		t.Fatalf("核验版本应为 ev3: %s", check.Version)
	}
	if check.Matched != 4 {
		t.Fatalf("总命中应 4: %+v", check)
	}
	if check.SnapshotMatched != 1 || check.PlanMatched != 1 || check.UserMatched != 1 || check.ContextMatched != 1 {
		t.Fatalf("来源分类计数错误: snap=%d plan=%d user=%d ctx=%d",
			check.SnapshotMatched, check.PlanMatched, check.UserMatched, check.ContextMatched)
	}
	// 用户提问数字不得冒充快照佐证：明细里 origin 必须为 user。
	foundUser := false
	for _, it := range check.Items {
		if it.SnapValue == 8.8 && it.Origin == "user" {
			foundUser = true
		}
		if it.SnapValue == 8.8 && it.Origin == "" {
			t.Fatal("用户提问数字被标成快照佐证（origin 空）")
		}
	}
	if !foundUser {
		t.Fatalf("应有 origin=user 的命中项: %+v", check.Items)
	}
}

// TestReportDataDeficiencies 日报数据水位：缺块逐项登记；mood 归属日早于报告日时
// 标注 stale_for_today 且不冒充今日情绪。
func TestReportDataDeficiencies(t *testing.T) {
	// 全缺：市场概览整体缺失。
	snap := &reportSnapshot{TradeDate: "2026-07-17"}
	defs := reportDataDeficiencies(snap, "2026-07-17")
	if len(defs) != 1 {
		t.Fatalf("市场概览缺失应恰 1 条: %v", defs)
	}
	// mood 为上一交易日：登记 + 打 stale_for_today。
	snap2 := &reportSnapshot{TradeDate: "2026-07-17", Market: &reportMarket{
		Indices:  []map[string]any{{"name": "上证指数", "trade_date": "2026-07-17", "data_time": "2026-07-17 15:00:00"}},
		Breadth:  map[string]any{"advances": 3000, "trade_date": "2026-07-17"},
		FundFlow: map[string]any{"main_net_yi": 12.5, "trade_date": "2026-07-17"},
		Mood:     map[string]any{"trade_date": "2026-07-16"},
	}}
	defs2 := reportDataDeficiencies(snap2, "2026-07-17")
	if len(defs2) != 1 {
		t.Fatalf("旧 mood 应恰 1 条水位登记: %v", defs2)
	}
	if v, _ := snap2.Market.Mood["stale_for_today"].(bool); !v {
		t.Fatal("旧 mood 应打 stale_for_today 标记")
	}
	// 当日 mood 完整：零缺口。
	snap3 := &reportSnapshot{TradeDate: "2026-07-17", Market: &reportMarket{
		Indices:  []map[string]any{{"name": "上证指数", "trade_date": "2026-07-17", "data_time": "2026-07-17 15:00:00"}},
		Breadth:  map[string]any{"advances": 3000, "trade_date": "2026-07-17"},
		FundFlow: map[string]any{"main_net_yi": 12.5, "trade_date": "2026-07-17"},
		Mood:     map[string]any{"trade_date": "2026-07-17"},
	}}
	if defs3 := reportDataDeficiencies(snap3, "2026-07-17"); len(defs3) != 0 {
		t.Fatalf("完整快照不应有水位登记: %v", defs3)
	}
}

// TestOpenDaysBehindAndSignalUsable 信号消费水位：按交易日历算落后开市日数，
// 旧记录（落后超容忍）不再冒充「最近信号」。
func TestOpenDaysBehindAndSignalUsable(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM trading_calendars")
	t.Cleanup(func() { common.DB.Exec("DELETE FROM trading_calendars") })
	// 造日历：以今天为锚往前 10 个工作日开市。
	now := time.Now()
	var open []string
	for d := 0; len(open) < 10; d++ {
		day := now.AddDate(0, 0, -d)
		if wd := day.Weekday(); wd >= time.Monday && wd <= time.Friday {
			ds := day.Format("2006-01-02")
			common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: ds, IsOpen: true})
			open = append(open, ds) // open[0]=最近（可能是今天）
		}
	}
	expected := prevOpenTradeDate(now.Format("2006-01-02"))
	if got := openDaysBehind(expected, expected); got != 0 {
		t.Fatalf("齐平应 0, got %d", got)
	}
	// 落后 3 个开市日：从 expected 往前数 3 个开市日。
	idx := 0
	for i, d := range open {
		if d == expected {
			idx = i
			break
		}
	}
	if idx+3 < len(open) {
		older := open[idx+3]
		if got := openDaysBehind(older, expected); got != 3 {
			t.Fatalf("应落后 3 个开市日, got %d", got)
		}
		if signalDateUsable(older) {
			t.Fatal("落后 3 个开市日的信号应判过期（容忍 2）")
		}
	}
	if !signalDateUsable(expected) {
		t.Fatal("齐平信号应可用")
	}
	if signalDateUsable("") {
		t.Fatal("空日期应不可用")
	}
	// 区间无日历记录（dbMax 严格早于 expected 却数不出开市日）应返回 -1「无法判定」
	// 而非 0「齐平」——日历缺失时旧数据不得冒充新鲜（消费方 fail-closed）。
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	if got := openDaysBehind("2026-07-01", "2026-07-17"); got != -1 {
		t.Fatalf("日历缺失应 -1（无法判定）, got %d", got)
	}
	if signalDateUsable("2026-07-01") {
		t.Fatal("日历缺失时信号时效无法判定，应按不可用处理")
	}
}

// TestEnforceStaleModeResult 历史解释模式程序化硬约束：summary 强制前缀、suggestions
// 剔除当前买卖行动词（提示词只是软引导，模型不遵守由服务端兜底保证输出形态）。
func TestEnforceStaleModeResult(t *testing.T) {
	// 模型没写前缀：补写；suggestions 含行动词：剔除，剔空补固定句。
	r := &AnalysisResult{
		Summary:     "该股走势偏强",
		Suggestions: []string{"可逢低吸纳建仓", "跌破 9.5 止损离场"},
	}
	note := enforceStaleModeResult(r, "2026-07-17 10:30:00")
	if !strings.HasPrefix(r.Summary, "截至 2026-07-17 10:30:00 的历史数据解释：") {
		t.Fatalf("summary 应被强制加历史解释前缀: %q", r.Summary)
	}
	if len(r.Suggestions) != 1 || !strings.Contains(r.Suggestions[0], "行情恢复") {
		t.Fatalf("行动建议应全被剔除并补固定句: %v", r.Suggestions)
	}
	if note == "" || !strings.Contains(note, "2 条") {
		t.Fatalf("应返回剔除说明: %q", note)
	}

	// 模型已合规：前缀不重复叠加、合规建议保留、无剔除说明。
	r2 := &AnalysisResult{
		Summary:     "截至 2026-07-17 10:30:00 的历史数据解释：该股走势偏强",
		Suggestions: []string{"待行情恢复后再评估", "关注复牌公告"},
	}
	if note2 := enforceStaleModeResult(r2, "2026-07-17 10:30:00"); note2 != "" {
		t.Fatalf("合规输出不应有剔除说明: %q", note2)
	}
	if strings.Count(r2.Summary, "历史数据解释") != 1 {
		t.Fatalf("前缀不应重复叠加: %q", r2.Summary)
	}
	if len(r2.Suggestions) != 2 {
		t.Fatalf("合规建议应保留: %v", r2.Suggestions)
	}
}

// TestTextLabeledValuesUnitInts 用户输入的带单位整数（10 元、8%）必须进值域：核验侧
// 对带显式单位的整数不跳过，值域侧只认亿/万会产生「复述用户输入被判幻觉」的误报。
func TestTextLabeledValuesUnitInts(t *testing.T) {
	vals := textLabeledValues("用户提问", "user", []string{"成本 10 元的话，涨 8% 就卖，仓位 3 成"})
	// 「10 元」「8%」入域；裸整数「3」不入（无单位，交给核验侧跳过规则）。
	byVal := map[float64]bool{}
	for _, v := range vals {
		if v.Origin != "user" {
			t.Fatalf("origin 应为 user: %+v", v)
		}
		byVal[v.Value] = true
	}
	if !byVal[10] || !byVal[8] {
		t.Fatalf("带单位整数 10 元 / 8%% 应进值域: %+v", vals)
	}
	if byVal[3] {
		t.Fatalf("无单位整数不应进值域: %+v", vals)
	}
	// 端到端：模型复述「你提到的 10 元」不再误报未命中。
	check := verifyEvidenceLabeled([]evidenceSection{{Module: "回答", Text: "按你提到的成本 10 元与 8% 目标计算"}}, vals)
	if check.Matched != check.Total || check.UserMatched != check.Matched {
		t.Fatalf("用户输入复述应全命中且计入 user_matched: %+v", check)
	}
}

// TestQaCurrentFreshness 旧问答会话按当前时刻重判：昨天 fresh 的快照今天必须能判 stale
// （详情页与续问的时效声明都吃这个重判，不再只读快照创建时的 freshness_status）。
func TestQaCurrentFreshness(t *testing.T) {
	setupTestDB(t)
	pinCalendarTo(t, prevOpenTradeDate(time.Now().Format("2006-01-02")))
	svc := &QaService{market: NewMarketService(nil)}
	meta := &qaSnapshotMeta{QuoteAsOf: "2026-07-08 15:00:00", FreshnessStatus: "fresh"}
	st, note := svc.qaCurrentFreshness("cn", meta)
	if st != freshStatusStale {
		t.Fatalf("远旧快照按当前时刻应判 stale, got %s", st)
	}
	if note == "" || !strings.Contains(note, "2026-07-08") {
		t.Fatalf("说明应含快照行情时刻: %q", note)
	}
	// 无行情时间：不判（空状态），不误报。
	if st, _ := svc.qaCurrentFreshness("cn", &qaSnapshotMeta{}); st != "" {
		t.Fatalf("无行情时间不应判定, got %s", st)
	}
	// 非 cn 市场：unknown（无日历），fail-closed 交给消费方。
	if st, _ := svc.qaCurrentFreshness("us", meta); st != freshStatusUnknown {
		t.Fatalf("us 市场应 unknown, got %s", st)
	}
}
