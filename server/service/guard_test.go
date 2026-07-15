package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestParseGuardConfig 空串/坏格式回退默认全开；合法 JSON 阈值钳制；缺字段保持默认布尔。
func TestParseGuardConfig(t *testing.T) {
	def := defaultGuardConfig()
	if !def.Enabled || def.PosPct != 5 || def.WatchPct != 7 || !def.StopLoss || !def.TakeProfit {
		t.Fatalf("默认配置应全开 pos5/watch7，得到 %+v", def)
	}

	if c := parseGuardConfig(""); c != def {
		t.Fatalf("空串应回退默认，得到 %+v", c)
	}
	if c := parseGuardConfig("{bad json"); c != def {
		t.Fatalf("坏格式应回退默认，得到 %+v", c)
	}

	// 合法完整配置。
	c := parseGuardConfig(`{"enabled":false,"pos_pct":8,"watch_pct":10,"stop_loss":false,"take_profit":true}`)
	if c.Enabled || c.PosPct != 8 || c.WatchPct != 10 || c.StopLoss || !c.TakeProfit {
		t.Fatalf("解析结果不符：%+v", c)
	}

	// 阈值越界钳制：pos>30→30、watch<=0→默认 7。
	c = parseGuardConfig(`{"enabled":true,"pos_pct":99,"watch_pct":0}`)
	if c.PosPct != 30 || c.WatchPct != 7 {
		t.Fatalf("阈值钳制失败：pos=%v watch=%v", c.PosPct, c.WatchPct)
	}

	// 缺子开关字段：默认填充后保持 true（不因半份 JSON 静默关闭）。
	c = parseGuardConfig(`{"pos_pct":6}`)
	if !c.Enabled || !c.StopLoss || !c.TakeProfit || c.PosPct != 6 {
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

// TestEvalPositionGuard 止损/止盈用当日 low/high 兜底触达、异动阈值、子开关关闭。
func TestEvalPositionGuard(t *testing.T) {
	cfg := defaultGuardConfig()
	pos := model.Position{Symbol: "600519", Market: "cn", Name: "贵州茅台", PlanStopLoss: 1580, PlanTakeProfit: 1800}

	// 止损：现价高于止损价，但当日最低触及 → 命中（盘中触达不漏判）。
	hits := evalPositionGuard(pos, cfg, guardObs{Price: 1590, DayHigh: 1595, DayLow: 1578, ChangePct: -1.2})
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
	hits = evalPositionGuard(pos, cfg, guardObs{Price: 1790, DayHigh: 1805, DayLow: 1780, ChangePct: 2.0})
	if len(hits) != 1 || hits[0].Kind != model.GuardKindTakeProfit {
		t.Fatalf("当日最高触及止盈应命中止盈，得到 %+v", hits)
	}

	// 持仓异动：|涨跌幅| ≥ pos_pct（默认 5%），未触及止损止盈 → 一条异动。
	hits = evalPositionGuard(pos, cfg, guardObs{Price: 1700, DayHigh: 1710, DayLow: 1690, ChangePct: -5.3})
	if len(hits) != 1 || hits[0].Kind != model.GuardKindPosMove {
		t.Fatalf("持仓异动应命中一条，得到 %+v", hits)
	}
	if hits[0].Route != "/stocks/cn/600519" {
		t.Fatalf("异动 Route 应为个股详情，得到 %s", hits[0].Route)
	}

	// 子开关关闭：止损止盈都关，触达价位也不推；异动仍生效。
	off := guardConfig{Enabled: true, PosPct: 5, WatchPct: 7, StopLoss: false, TakeProfit: false}
	hits = evalPositionGuard(pos, off, guardObs{Price: 1570, DayHigh: 1580, DayLow: 1560, ChangePct: -1.0})
	if len(hits) != 0 {
		t.Fatalf("止损止盈关闭且异动未达阈值应无命中，得到 %+v", hits)
	}

	// 无计划止损止盈价（=0）：不触达，仅按异动判。
	bare := model.Position{Symbol: "000001", Market: "cn", Name: "平安银行"}
	hits = evalPositionGuard(bare, cfg, guardObs{Price: 10, DayHigh: 10.2, DayLow: 9.9, ChangePct: 1.0})
	if len(hits) != 0 {
		t.Fatalf("无止损止盈价且异动不达阈值应无命中，得到 %+v", hits)
	}

	// 现价缺失（0）：整体跳过。
	if h := evalPositionGuard(pos, cfg, guardObs{Price: 0}); h != nil {
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
