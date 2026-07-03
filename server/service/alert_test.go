package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestEvaluateAlert_Price 到价：gte 用当日 high、lte 用当日 low 判盘中触达。
func TestEvaluateAlert_Price(t *testing.T) {
	// gte：现价未到但当日最高触及 → 命中。
	hit, _, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11},
		alertEval{Price: 10.5, DayHigh: 11.2, DayLow: 10.0},
	)
	if !hit || msg == "" {
		t.Fatalf("当日最高触及应命中: hit=%v", hit)
	}
	// lte：当日最低触及止损位 → 命中。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpLTE, Threshold: 9},
		alertEval{Price: 9.5, DayHigh: 9.8, DayLow: 8.9},
	)
	if !hit {
		t.Fatalf("当日最低触及应命中")
	}
	// 未触及。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 12},
		alertEval{Price: 10.5, DayHigh: 11.2, DayLow: 10.0},
	)
	if hit {
		t.Fatalf("未触及不应命中")
	}
}

// TestEvaluateAlert_PctChange 异动：涨跌幅阈值。
func TestEvaluateAlert_PctChange(t *testing.T) {
	hit, v, _ := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPctChange, Op: model.AlertOpGTE, Threshold: 5},
		alertEval{ChangePct: 6.3},
	)
	if !hit || v != 6.3 {
		t.Fatalf("涨幅达标应命中: %v %v", hit, v)
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPctChange, Op: model.AlertOpLTE, Threshold: -5},
		alertEval{ChangePct: -6.1},
	)
	if !hit {
		t.Fatalf("跌幅达标应命中")
	}
}

// TestEvaluateAlert_MA 均线：现价 vs MAn 站上/跌破，数据不足不命中。
func TestEvaluateAlert_MA(t *testing.T) {
	closes := []float64{9, 10, 11} // MA3 = 10
	hit, _, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 10.5, Closes: closes},
	)
	if !hit || msg == "" {
		t.Fatalf("站上 MA3 应命中")
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpLTE, Period: 3},
		alertEval{Price: 9.5, Closes: closes},
	)
	if !hit {
		t.Fatalf("跌破 MA3 应命中")
	}
	// 数据不足：closes 少于 period。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 5},
		alertEval{Price: 10.5, Closes: closes},
	)
	if hit {
		t.Fatalf("日线不足不应命中")
	}
}

// TestEvaluateAlert_Breakout 突破：创近 N 日新高/新低（用当日 high/low）。
func TestEvaluateAlert_Breakout(t *testing.T) {
	highs := []float64{10.5, 10.8, 11.0} // 近 3 日前高 11.0
	lows := []float64{9.5, 9.2, 9.0}     // 近 3 日前低 9.0
	hit, _, _ := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 11.3, DayHigh: 11.3, Highs: highs, Lows: lows},
	)
	if !hit {
		t.Fatalf("创新高应命中")
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpLTE, Period: 3},
		alertEval{Price: 8.8, DayLow: 8.8, Highs: highs, Lows: lows},
	)
	if !hit {
		t.Fatalf("创新低应命中")
	}
	// 未破前高。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 10.9, DayHigh: 10.9, Highs: highs, Lows: lows},
	)
	if hit {
		t.Fatalf("未破前高不应命中")
	}
}

// TestEvaluateAlert_VolumeSurge 放量：当日量 vs 20 日均量倍数，数据不足不命中。
func TestEvaluateAlert_VolumeSurge(t *testing.T) {
	// 20 根均量 1000 手，当日 2500 手 = 2.5 倍。
	volumes := make([]int64, 20)
	for i := range volumes {
		volumes[i] = 1000
	}
	hit, v, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2},
		alertEval{DayVolume: 2500, Volumes: volumes},
	)
	if !hit || v != 2.5 || msg == "" {
		t.Fatalf("放量 2.5 倍 ≥ 2 应命中: hit=%v v=%v", hit, v)
	}
	// 未达倍数。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 3},
		alertEval{DayVolume: 2500, Volumes: volumes},
	)
	if hit {
		t.Fatalf("2.5 倍 < 3 不应命中")
	}
	// lte：缩量。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpLTE, Threshold: 0.5},
		alertEval{DayVolume: 300, Volumes: volumes},
	)
	if !hit {
		t.Fatalf("缩量 0.3 倍 ≤ 0.5 应命中")
	}
	// 历史量不足 20 根 → 不命中。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2},
		alertEval{DayVolume: 2500, Volumes: volumes[:10]},
	)
	if hit {
		t.Fatalf("均量数据不足不应命中")
	}
	// 当日量缺失 → 不命中。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2},
		alertEval{DayVolume: 0, Volumes: volumes},
	)
	if hit {
		t.Fatalf("当日量缺失不应命中")
	}
}

// TestEvaluateAlert_Amplitude 振幅：达到阈值命中，数据缺失（0）不命中。
func TestEvaluateAlert_Amplitude(t *testing.T) {
	hit, v, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpGTE, Threshold: 5},
		alertEval{Amplitude: 6.8},
	)
	if !hit || v != 6.8 || msg == "" {
		t.Fatalf("振幅 6.8%% ≥ 5%% 应命中: hit=%v v=%v", hit, v)
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpGTE, Threshold: 8},
		alertEval{Amplitude: 6.8},
	)
	if hit {
		t.Fatalf("6.8%% < 8%% 不应命中")
	}
	// lte：窄幅震荡。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpLTE, Threshold: 2},
		alertEval{Amplitude: 1.1},
	)
	if !hit {
		t.Fatalf("振幅 1.1%% ≤ 2%% 应命中")
	}
	// 数据缺失。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpGTE, Threshold: 5},
		alertEval{Amplitude: 0},
	)
	if hit {
		t.Fatalf("振幅数据缺失不应命中")
	}
}

// TestAlertCRUDIsolation CRUD + 用户隔离（DB 集成）。
func TestAlertCRUDIsolation(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_rules")
	common.DB.Exec("DELETE FROM alert_events")
	svc := &AlertService{}

	// 直接落库（跳过 Create 的行情校验，避免网络依赖）。
	r1 := &model.AlertRule{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11, Status: model.AlertStatusActive}
	r2 := &model.AlertRule{UserID: 1, Symbol: "000001", Market: "cn", Name: "平安银行",
		Kind: model.AlertKindPctChange, Op: model.AlertOpLTE, Threshold: -5, Status: model.AlertStatusTriggered}
	r3 := &model.AlertRule{UserID: 1, Symbol: "600519", Market: "cn", Name: "贵州茅台",
		Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2, Status: model.AlertStatusActive}
	other := &model.AlertRule{UserID: 2, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 20, Status: model.AlertStatusActive}
	for _, r := range []*model.AlertRule{r1, r2, r3, other} {
		if err := common.DB.Create(r).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	// List 用户1 → 3 条（隔离），triggered 排在前。
	rows, err := svc.List(1, "")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("用户1 应有 3 条，得到 %d", len(rows))
	}
	if rows[0].Status != model.AlertStatusTriggered {
		t.Fatalf("命中的规则应排在前，得到 %s", rows[0].Status)
	}

	// 跨用户 Update/Delete 隔离。
	if _, err := svc.Update(2, r1.ID, AlertInput{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 1}); err == nil {
		t.Fatalf("跨用户 Update 应失败")
	}
	if err := svc.Delete(2, r1.ID); err == nil {
		t.Fatalf("跨用户 Delete 应失败")
	}

	// 暂停恢复：SetStatus。
	if _, err := svc.SetStatus(1, r2.ID, model.AlertStatusActive); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	var got model.AlertRule
	common.DB.First(&got, r2.ID)
	if got.Status != model.AlertStatusActive || got.TriggerMsg != "" {
		t.Fatalf("恢复后应 active 且清除命中标记: %+v", got)
	}

	// 删除规则：其未读事件应转已忽略（退出待办、保留历史）。
	common.DB.Create(&model.AlertEvent{RuleID: r1.ID, UserID: 1, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindPrice, Message: "触及", TriggeredAt: time.Now(), Status: model.AlertEventUnread})
	if err := svc.Delete(1, r1.ID); err != nil {
		t.Fatalf("本人删除失败: %v", err)
	}
	rows, _ = svc.List(1, "")
	if len(rows) != 2 {
		t.Fatalf("删除后应剩 2 条，得到 %d", len(rows))
	}
	var ev model.AlertEvent
	common.DB.Where("rule_id = ?", r1.ID).First(&ev)
	if ev.Status != model.AlertEventDismissed {
		t.Fatalf("删规则后其未读事件应转 dismissed，得到 %s", ev.Status)
	}
}

// TestAlertEvents 命中事件状态机：同日去重落库、未读筛选、已读/忽略、全部已读、用户隔离。
func TestAlertEvents(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_events")
	svc := &AlertService{}
	now := time.Now()
	today := now.In(time.Local).Format("2006-01-02")

	// recordEvent 首次命中（无 triggered_at）→ 落库。
	rule := model.AlertRule{ID: 101, UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行", Kind: model.AlertKindPrice}
	if !recordEvent(rule, "当日最高 11.20 触及目标价 ≥ 11.00", today, now) {
		t.Fatalf("首次命中应落事件")
	}
	// 同日再命中（triggered_at=今天）→ 去重不落。
	rule.TriggeredAt = &now
	if recordEvent(rule, "重复命中", today, now) {
		t.Fatalf("同日重复命中不应再落事件")
	}
	// 昨日命中过（triggered_at=昨天）→ 今天再命中应落。
	old := now.AddDate(0, 0, -1)
	rule.TriggeredAt = &old
	if !recordEvent(rule, "次日再命中", today, now) {
		t.Fatalf("跨日再命中应落事件")
	}
	var cnt int64
	common.DB.Model(&model.AlertEvent{}).Where("user_id = ?", 1).Count(&cnt)
	if cnt != 2 {
		t.Fatalf("用户1 应有 2 条事件，得到 %d", cnt)
	}

	// 他人事件（隔离用）。
	common.DB.Create(&model.AlertEvent{RuleID: 201, UserID: 2, Symbol: "000001", Market: "cn",
		Kind: model.AlertKindPrice, Message: "他人命中", TriggeredAt: now, Status: model.AlertEventUnread})

	// TriggeredForUser 只回本人 unread。
	trig, err := svc.TriggeredForUser(1)
	if err != nil {
		t.Fatalf("TriggeredForUser 失败: %v", err)
	}
	if len(trig) != 2 {
		t.Fatalf("用户1 未读事件应为 2，得到 %d", len(trig))
	}

	// SetEventStatus：已读；跨用户拒绝。
	if _, err := svc.SetEventStatus(1, trig[0].ID, model.AlertEventRead); err != nil {
		t.Fatalf("标记已读失败: %v", err)
	}
	if _, err := svc.SetEventStatus(2, trig[1].ID, model.AlertEventRead); err == nil {
		t.Fatalf("跨用户标记应失败")
	}
	if _, err := svc.SetEventStatus(1, trig[1].ID, "bogus"); err == nil {
		t.Fatalf("非法状态应拒绝")
	}
	trig, _ = svc.TriggeredForUser(1)
	if len(trig) != 1 {
		t.Fatalf("已读一条后未读应剩 1，得到 %d", len(trig))
	}

	// ListEvents 状态过滤。
	reads, _ := svc.ListEvents(1, model.AlertEventRead, 0)
	if len(reads) != 1 {
		t.Fatalf("已读列表应 1 条，得到 %d", len(reads))
	}
	all, _ := svc.ListEvents(1, "", 0)
	if len(all) != 2 {
		t.Fatalf("全部列表应 2 条（隔离他人），得到 %d", len(all))
	}

	// MarkAllEventsRead。
	n, err := svc.MarkAllEventsRead(1)
	if err != nil || n != 1 {
		t.Fatalf("全部已读应影响 1 条: n=%d err=%v", n, err)
	}
	trig, _ = svc.TriggeredForUser(1)
	if len(trig) != 0 {
		t.Fatalf("全部已读后未读应为 0，得到 %d", len(trig))
	}
	// 用户2 不受影响。
	trig2, _ := svc.TriggeredForUser(2)
	if len(trig2) != 1 {
		t.Fatalf("用户2 未读应仍为 1，得到 %d", len(trig2))
	}
}
