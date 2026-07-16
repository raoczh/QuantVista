package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestParseGuardConfig 空串/坏格式回退默认全开；合法 JSON 阈值钳制；缺字段保持默认布尔。
func TestParseGuardConfig(t *testing.T) {
	def := defaultGuardConfig()
	if !def.Enabled || def.PosPct != 5 || def.WatchPct != 7 || !def.StopLoss || !def.TakeProfit || !def.Evening {
		t.Fatalf("默认配置应全开 pos5/watch7（含盘后事件），得到 %+v", def)
	}

	if c := parseGuardConfig(""); c != def {
		t.Fatalf("空串应回退默认，得到 %+v", c)
	}
	if c := parseGuardConfig("{bad json"); c != def {
		t.Fatalf("坏格式应回退默认，得到 %+v", c)
	}

	// 合法完整配置。
	c := parseGuardConfig(`{"enabled":false,"pos_pct":8,"watch_pct":10,"stop_loss":false,"take_profit":true,"evening":false}`)
	if c.Enabled || c.PosPct != 8 || c.WatchPct != 10 || c.StopLoss || !c.TakeProfit || c.Evening {
		t.Fatalf("解析结果不符：%+v", c)
	}

	// 阈值越界钳制：pos>30→30、watch<=0→默认 7。
	c = parseGuardConfig(`{"enabled":true,"pos_pct":99,"watch_pct":0}`)
	if c.PosPct != 30 || c.WatchPct != 7 {
		t.Fatalf("阈值钳制失败：pos=%v watch=%v", c.PosPct, c.WatchPct)
	}

	// 缺子开关字段：默认填充后保持 true（不因半份 JSON 静默关闭）——
	// 存量用户的旧配置 JSON 无 evening 字段，盘后事件默认开启。
	c = parseGuardConfig(`{"pos_pct":6}`)
	if !c.Enabled || !c.StopLoss || !c.TakeProfit || !c.Evening || c.PosPct != 6 {
		t.Fatalf("缺字段应保留默认 true：%+v", c)
	}
}

// TestInGuardWindow 交易时段边界（周一~五 09:30~15:05）。
func TestInGuardWindow(t *testing.T) {
	mon := func(h, m int) time.Time { // 2026-07-13 是周一
		return time.Date(2026, 7, 13, h, m, 0, 0, time.Local)
	}
	cases := []struct {
		t    time.Time
		want bool
		desc string
	}{
		{mon(9, 29), false, "开盘前一分钟"},
		{mon(9, 30), true, "开盘整点"},
		{mon(11, 0), true, "上午盘中"},
		{mon(13, 30), true, "下午盘中"},
		{mon(15, 4), true, "收盘缓冲内"},
		{mon(15, 5), false, "缓冲结束"},
		{mon(16, 0), false, "盘后"},
		{time.Date(2026, 7, 11, 11, 0, 0, 0, time.Local), false, "周六（非工作日）"},
		{time.Date(2026, 7, 12, 11, 0, 0, 0, time.Local), false, "周日"},
	}
	for _, c := range cases {
		if got := inGuardWindow(c.t); got != c.want {
			t.Errorf("%s: inGuardWindow=%v want %v", c.desc, got, c.want)
		}
	}
}

// TestEvalPositionGuard 止损/止盈用当日 low/high 兜底触达、异动阈值、涨跌停语义、子开关关闭。
func TestEvalPositionGuard(t *testing.T) {
	cfg := defaultGuardConfig()
	pos := model.Position{Symbol: "600519", Market: "cn", Name: "贵州茅台", PlanStopLoss: 1580, PlanTakeProfit: 1800}

	// 止损：现价高于止损价，但当日最低触及 → 命中（盘中触达不漏判）。
	hits := evalPositionGuard(pos, cfg, guardObs{Price: 1590, DayHigh: 1595, DayLow: 1578, ChangePct: -1.2}, 10)
	if len(hits) != 1 || hits[0].Kind != model.GuardKindStopLoss {
		t.Fatalf("当日最低触及止损应命中一条止损，得到 %+v", hits)
	}
	if hits[0].Priority != 4 {
		t.Fatalf("止损优先级应为 4，得到 %d", hits[0].Priority)
	}
	if hits[0].Route != "/positions" {
		t.Fatalf("止损 Route 应为 /positions，得到 %s", hits[0].Route)
	}

	// 止盈：当日最高触及止盈价 → 命中。
	hits = evalPositionGuard(pos, cfg, guardObs{Price: 1790, DayHigh: 1805, DayLow: 1780, ChangePct: 2.0}, 10)
	if len(hits) != 1 || hits[0].Kind != model.GuardKindTakeProfit {
		t.Fatalf("当日最高触及止盈应命中止盈，得到 %+v", hits)
	}

	// 持仓异动：|涨跌幅| ≥ pos_pct（默认 5%），未触及止损止盈 → 一条异动。
	hits = evalPositionGuard(pos, cfg, guardObs{Price: 1700, DayHigh: 1710, DayLow: 1690, ChangePct: -5.3}, 10)
	if len(hits) != 1 || hits[0].Kind != model.GuardKindPosMove {
		t.Fatalf("持仓异动应命中一条，得到 %+v", hits)
	}
	if hits[0].Route != "/stocks/cn/600519" {
		t.Fatalf("异动 Route 应为个股详情，得到 %s", hits[0].Route)
	}
	if hits[0].Priority != 0 || strings.Contains(hits[0].Message, "跌停") {
		t.Fatalf("普通异动不应带跌停语义：%+v", hits[0])
	}

	// 持仓跌停：跌幅接近板块跌停幅（主板 10%，容差 0.3）→ 换跌停文案 + 优先级 4（kind 不变，台账去重不受影响）。
	bare := model.Position{Symbol: "000001", Market: "cn", Name: "平安银行"}
	hits = evalPositionGuard(bare, cfg, guardObs{Price: 9, DayHigh: 9.9, DayLow: 9, ChangePct: -9.8}, 10)
	if len(hits) != 1 || hits[0].Kind != model.GuardKindPosMove {
		t.Fatalf("跌停应命中一条 pos_move，得到 %+v", hits)
	}
	if hits[0].Priority != 4 || !strings.Contains(hits[0].Message, "持仓跌停") {
		t.Fatalf("跌停应为紧急优先级 4 + 跌停文案：%+v", hits[0])
	}

	// 持仓涨停：优先级保持 0，文案标涨停。
	hits = evalPositionGuard(bare, cfg, guardObs{Price: 11, DayHigh: 11, DayLow: 10.1, ChangePct: 9.9}, 10)
	if len(hits) != 1 || hits[0].Priority != 0 || !strings.Contains(hits[0].Message, "持仓涨停") {
		t.Fatalf("涨停应命中且优先级 0：%+v", hits)
	}

	// 阈值配得比涨停幅还高（15%）：涨跌停仍必推（对齐 watch 侧先例）。
	high := guardConfig{Enabled: true, PosPct: 15, StopLoss: false, TakeProfit: false}
	hits = evalPositionGuard(bare, high, guardObs{Price: 9, ChangePct: -9.9}, 10)
	if len(hits) != 1 || !strings.Contains(hits[0].Message, "持仓跌停") {
		t.Fatalf("阈值高于涨停幅时跌停仍应推：%+v", hits)
	}

	// 子开关关闭：止损止盈都关，触达价位也不推；异动仍生效。
	off := guardConfig{Enabled: true, PosPct: 5, WatchPct: 7, StopLoss: false, TakeProfit: false}
	hits = evalPositionGuard(pos, off, guardObs{Price: 1570, DayHigh: 1580, DayLow: 1560, ChangePct: -1.0}, 10)
	if len(hits) != 0 {
		t.Fatalf("止损止盈关闭且异动未达阈值应无命中，得到 %+v", hits)
	}

	// 无计划止损止盈价（=0）：不触达，仅按异动判。
	hits = evalPositionGuard(bare, cfg, guardObs{Price: 10, DayHigh: 10.2, DayLow: 9.9, ChangePct: 1.0}, 10)
	if len(hits) != 0 {
		t.Fatalf("无止损止盈价且异动不达阈值应无命中，得到 %+v", hits)
	}

	// 现价缺失（0）：整体跳过。
	if h := evalPositionGuard(pos, cfg, guardObs{Price: 0}, 10); h != nil {
		t.Fatalf("现价缺失应无命中，得到 %+v", h)
	}
}

// TestEvalWatchGuard 自选异动阈值命中、涨跌停即使未达阈值也推、普通波动不推。
func TestEvalWatchGuard(t *testing.T) {
	cfg := defaultGuardConfig() // watch_pct=7
	item := model.WatchlistItem{Symbol: "600000", Market: "cn", Name: "浦发银行", IsPinned: true}

	// 达阈值异动 → 命中。
	if h := evalWatchGuard(item, cfg, guardObs{Price: 9, ChangePct: 7.5}, 10); h == nil || h.Kind != model.GuardKindWatchMove {
		t.Fatalf("自选达阈值异动应命中，得到 %+v", h)
	}
	// 未达阈值普通波动 → 不推。
	if h := evalWatchGuard(item, cfg, guardObs{Price: 9, ChangePct: 3.0}, 10); h != nil {
		t.Fatalf("普通波动不应命中，得到 %+v", h)
	}
	// 涨停：涨幅未达 7% 阈值但接近板块涨停幅度（主板 10%，容差 0.3）→ 推。
	h := evalWatchGuard(item, cfg, guardObs{Price: 11, ChangePct: 9.8}, 10)
	if h == nil {
		t.Fatalf("触及涨停应命中")
	}
	// 科创板 20cm：涨 8%（超 watch_pct）算异动，但未到 20% 涨停 → label 是异动非涨停。
	star := model.WatchlistItem{Symbol: "688001", Market: "cn", Name: "华兴源创"}
	h = evalWatchGuard(star, cfg, guardObs{Price: 50, ChangePct: 8.0}, 20)
	if h == nil {
		t.Fatalf("科创板 8%% 超阈值应命中异动")
	}

	// watch_pct=0（阈值被关）不推。
	off := guardConfig{Enabled: true, WatchPct: 0}
	if h := evalWatchGuard(item, off, guardObs{Price: 9, ChangePct: 9.0}, 10); h != nil {
		t.Fatalf("watch 阈值为 0 应不推，得到 %+v", h)
	}
}

// TestRecordGuardEventDedup 同日同标的同类事件唯一索引去重：首次落库返回 true，重复返回 false。
func TestRecordGuardEventDedup(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM guard_events")
	common.EncryptionKey = "test-encryption-key-1234567890"

	h := guardHit{Symbol: "600519", Market: "cn", Name: "贵州茅台", Kind: model.GuardKindStopLoss, Price: 1576, Message: "触及止损"}

	if !recordGuardEvent(7, "2026-07-13", h) {
		t.Fatalf("首次落库应返回新事件 true")
	}
	if recordGuardEvent(7, "2026-07-13", h) {
		t.Fatalf("同日同标的同类重复应去重返回 false")
	}
	// 不同类事件（止盈）同日同标的 → 新事件。
	h2 := h
	h2.Kind = model.GuardKindTakeProfit
	if !recordGuardEvent(7, "2026-07-13", h2) {
		t.Fatalf("不同 kind 应为新事件")
	}
	// 次日同事件 → 新事件（跨日不去重）。
	if !recordGuardEvent(7, "2026-07-14", h) {
		t.Fatalf("次日应为新事件")
	}
	// 不同用户同事件 → 新事件（user_id 进唯一键）。
	if !recordGuardEvent(8, "2026-07-13", h) {
		t.Fatalf("不同用户应为新事件")
	}

	var cnt int64
	common.DB.Model(&model.GuardEvent{}).Count(&cnt)
	if cnt != 4 {
		t.Fatalf("应落库 4 条不同事件，得到 %d", cnt)
	}
}

// TestEvalPosEveningPure 盘后事件四个纯函数：公告按日聚合、龙虎榜聚合原因、
// 财报披露临近窗口边界、业绩预告回看窗口。
func TestEvalPosEveningPure(t *testing.T) {
	// 公告：两天 3 条 → 2 条事件（同日合并），EventDate=发布日。
	anns := []model.Announcement{
		{Symbol: "600519", Title: "关于回购股份的公告", NoticeDate: "2026-07-15"},
		{Symbol: "600519", Title: "董事会决议公告", NoticeDate: "2026-07-15"},
		{Symbol: "600519", Title: "股东减持计划", NoticeDate: "2026-07-16"},
	}
	hits := evalPosAnnouncements("600519", "贵州茅台", anns)
	if len(hits) != 2 {
		t.Fatalf("两天公告应聚成 2 条事件，得到 %d", len(hits))
	}
	if hits[0].EventDate != "2026-07-15" || hits[0].Kind != model.GuardKindPosNotice {
		t.Fatalf("首条事件日期/类型不符：%+v", hits[0])
	}
	if !strings.Contains(hits[0].Message, "回购股份") || !strings.Contains(hits[0].Message, "董事会决议") {
		t.Fatalf("同日两条公告应合并进一条消息：%s", hits[0].Message)
	}
	if !strings.Contains(hits[0].Message, "2 条") {
		t.Fatalf("消息应带条数：%s", hits[0].Message)
	}
	if evalPosAnnouncements("600519", "贵州茅台", nil) != nil {
		t.Fatalf("无公告应返回 nil")
	}

	// 龙虎榜：同日两行（多上榜原因）→ 1 条事件，原因去重拼接，净买额换算亿。
	lhbs := []model.LhbEntry{
		{Symbol: "000001", Name: "平安银行", TradeDate: "2026-07-16", ChangeType: "01", Reason: "日涨幅偏离值达7%", NetBuy: 1.23e8, Close: 12.5},
		{Symbol: "000001", Name: "平安银行", TradeDate: "2026-07-16", ChangeType: "05", Reason: "日振幅值达15%", NetBuy: 1.23e8, Close: 12.5},
	}
	hits = evalPosLhb("000001", "", lhbs)
	if len(hits) != 1 || hits[0].Kind != model.GuardKindPosLhb || hits[0].EventDate != "2026-07-16" {
		t.Fatalf("同日多原因应聚成 1 条：%+v", hits)
	}
	if !strings.Contains(hits[0].Message, "日涨幅偏离值达7%") || !strings.Contains(hits[0].Message, "日振幅值达15%") {
		t.Fatalf("上榜原因应都在消息里：%s", hits[0].Message)
	}
	if !strings.Contains(hits[0].Message, "+1.23 亿") {
		t.Fatalf("净买额应换算亿元：%s", hits[0].Message)
	}
	if !strings.Contains(hits[0].Message, "平安银行") {
		t.Fatalf("持仓名缺失时应回退数据行名称：%s", hits[0].Message)
	}

	// 财报披露临近：today=07-16，披露日 07-19（3 天后）命中边界；07-20（4 天后）不推。
	sched := &model.DisclosureSchedule{Symbol: "600519", AppointDate: "2026-07-19", ReportTypeName: "2026年 半年报"}
	h := evalPosEarnDate("600519", "贵州茅台", sched, "2026-07-16")
	if h == nil || h.EventDate != "2026-07-19" || !strings.Contains(h.Message, "3 天后") {
		t.Fatalf("3 天后披露应命中且 EventDate=披露日：%+v", h)
	}
	sched.AppointDate = "2026-07-20"
	if h = evalPosEarnDate("600519", "贵州茅台", sched, "2026-07-16"); h != nil {
		t.Fatalf("4 天后披露不应推：%+v", h)
	}
	sched.AppointDate = "2026-07-16"
	if h = evalPosEarnDate("600519", "贵州茅台", sched, "2026-07-16"); h == nil || !strings.Contains(h.Message, "今日") {
		t.Fatalf("当日披露应命中「今日」：%+v", h)
	}
	if evalPosEarnDate("600519", "贵州茅台", nil, "2026-07-16") != nil {
		t.Fatalf("无披露计划应返回 nil")
	}

	// 业绩预告：发布日在回看窗口内推；窗口前不推。
	fc := &model.EarningsForecast{Symbol: "000001", PredictType: "预增", PredictFinance: "净利润",
		AmpLower: 50, AmpUpper: 80, NoticeDate: "2026-07-15"}
	h = evalPosEarnFcst("000001", "平安银行", fc, "2026-07-14")
	if h == nil || h.EventDate != "2026-07-15" || !strings.Contains(h.Message, "预增") {
		t.Fatalf("窗口内新预告应命中：%+v", h)
	}
	if !strings.Contains(h.Message, "50.00%~80.00%") {
		t.Fatalf("预告变动幅度应进消息：%s", h.Message)
	}
	fc.NoticeDate = "2026-07-10"
	if h = evalPosEarnFcst("000001", "平安银行", fc, "2026-07-14"); h != nil {
		t.Fatalf("窗口前的旧预告不应推：%+v", h)
	}
}

// TestEvaluateGuardUserEvening 盘后事件端到端：seed 持仓与四类数据 → 批量评估落台账 →
// 幂等（二次评估零新事件）；closed 持仓与他人持仓不评。
func TestEvaluateGuardUserEvening(t *testing.T) {
	setupTestDB(t)
	common.EncryptionKey = "test-encryption-key-1234567890"
	clean := func() {
		for _, tbl := range []string{"guard_events", "positions", "announcements", "lhb_entries",
			"disclosure_schedules", "earnings_forecasts", "notify_channels"} {
			common.DB.Exec("DELETE FROM " + tbl)
		}
	}
	clean()
	t.Cleanup(clean) // 内存库 cache=shared，退场清表防污染其他测试

	today := "2026-07-16"
	since := "2026-07-14"
	uid := int64(7)

	// 持仓：600519 与 000001 holding；600000 已平仓不评；用户 8 的持仓不掺入。
	seedPos := []model.Position{
		{UserID: uid, Symbol: "600519", Market: "cn", Name: "贵州茅台", Status: model.PositionStatusHolding},
		{UserID: uid, Symbol: "000001", Market: "cn", Name: "平安银行", Status: model.PositionStatusHolding},
		{UserID: uid, Symbol: "600000", Market: "cn", Name: "浦发银行", Status: model.PositionStatusClosed},
		{UserID: 8, Symbol: "300750", Market: "cn", Name: "宁德时代", Status: model.PositionStatusHolding},
	}
	for i := range seedPos {
		if err := common.DB.Create(&seedPos[i]).Error; err != nil {
			t.Fatalf("seed 持仓失败: %v", err)
		}
	}
	// 四类事件数据：600519 公告+披露临近；000001 龙虎榜+预告；600000/300750 也有公告（不该推给 uid）。
	common.DB.Create(&model.Announcement{Symbol: "600519", Market: "cn", ArtCode: "A1", Title: "重大合同公告", NoticeDate: today})
	common.DB.Create(&model.Announcement{Symbol: "600000", Market: "cn", ArtCode: "A2", Title: "平仓股公告", NoticeDate: today})
	common.DB.Create(&model.LhbEntry{Symbol: "000001", Market: "cn", TradeDate: today, ChangeType: "01", Reason: "日涨幅偏离值达7%", NetBuy: 2e8})
	common.DB.Create(&model.DisclosureSchedule{Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", AppointDate: "2026-07-18", ReportTypeName: "半年报"})
	common.DB.Create(&model.EarningsForecast{Symbol: "000001", Market: "cn", ReportDate: "2026-06-30", NoticeDate: "2026-07-15", PredictType: "预增"})

	svc := &GuardService{notify: NewNotifyService()} // 无启用通道，SendMsg 自然空操作
	n := svc.evaluateGuardUserEvening(uid, today, since)
	if n != 4 {
		t.Fatalf("应产生 4 条新事件（公告+披露+龙虎榜+预告），得到 %d", n)
	}
	var cnt int64
	common.DB.Model(&model.GuardEvent{}).Where("user_id = ?", uid).Count(&cnt)
	if cnt != 4 {
		t.Fatalf("台账应 4 行，得到 %d", cnt)
	}
	var kinds []string
	common.DB.Model(&model.GuardEvent{}).Where("user_id = ?", uid).Order("kind").Pluck("kind", &kinds)
	want := fmt.Sprintf("%v", []string{model.GuardKindPosEarnDate, model.GuardKindPosEarnFcst, model.GuardKindPosLhb, model.GuardKindPosNotice})
	if got := fmt.Sprintf("%v", kinds); got != want {
		t.Fatalf("事件类型不符：got %s want %s", got, want)
	}
	// 平仓股/他人持仓的公告不得进本用户台账。
	var leak int64
	common.DB.Model(&model.GuardEvent{}).Where("user_id = ? AND symbol IN ?", uid, []string{"600000", "300750"}).Count(&leak)
	if leak != 0 {
		t.Fatalf("平仓股不应产生事件，泄漏 %d 条", leak)
	}

	// 幂等：同窗口重复评估零新事件（台账唯一键去重）。
	if n = svc.evaluateGuardUserEvening(uid, today, since); n != 0 {
		t.Fatalf("重复评估应零新事件，得到 %d", n)
	}
}
