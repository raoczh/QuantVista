package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func exitTestBars(now time.Time, closePrice, high, low float64, count int) []model.DailyBar {
	rows := make([]model.DailyBar, 0, count)
	start := now.AddDate(0, 0, -count)
	for i := 0; i < count; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		rows = append(rows, model.DailyBar{Symbol: "600901", Market: "cn", TradeDate: day,
			Open: closePrice, High: high, Low: low, Close: closePrice})
	}
	return rows
}

func freshExitQuote(now time.Time, price, high, low float64) FreshQuoteResult {
	return FreshQuoteResult{Quote: &datasource.Quote{Symbol: "600901", Market: "cn", Price: price, High: high, Low: low, DataTime: now}, Fresh: quoteFreshInfo{Status: freshStatusFresh, ExpectedDate: now.Format("2006-01-02")}}
}

func TestPositionExitAssessmentMatrixAndIdempotency(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 8, 10, 16, 0, 0, 0, time.Local)
	bars := exitTestBars(now, 10, 10.5, 9.5, 70)
	base := model.Position{ID: 901001, UserID: 901, Symbol: "600901", Market: "cn", Name: "测试持仓", Status: model.PositionStatusHolding,
		BuyPrice: 10, Quantity: 100, PeakPrice: 12, PeakDate: "2026-07-01", PeakFrom: "2026-07-01"}
	if err := common.DB.Create(&base).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewPositionExitAssessmentService(nil)
	input := positionExitInput{position: base, quote: freshExitQuote(now, 8, 8.2, 7.8), barRows: bars, now: now, session: model.PositionExitSessionIntraday}
	input.position.PlanStopLoss = 8.5
	got := evaluatePositionExit(input, defaultPositionExitParams)
	if got.Level != model.PositionExitLevelUrgent || got.PrimarySignal != "plan_stop" {
		t.Fatalf("计划止损必须 urgent: level=%s primary=%s reason=%s", got.Level, got.PrimarySignal, got.PrimaryReason)
	}
	if _, err := svc.EvaluateUserWithSnapshot(context.Background(), base.UserID, []model.Position{input.position}, map[string]FreshQuoteResult{QuoteKey("cn", base.Symbol): input.quote}, model.PositionExitSessionIntraday, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EvaluateUserWithSnapshot(context.Background(), base.UserID, []model.Position{input.position}, map[string]FreshQuoteResult{QuoteKey("cn", base.Symbol): input.quote}, model.PositionExitSessionIntraday, now); err != nil {
		t.Fatal(err)
	}
	var count int64
	common.DB.Model(&model.PositionExitAssessment{}).Where("position_id = ?", base.ID).Count(&count)
	if count != 1 {
		t.Fatalf("相同事实重复评估必须幂等，count=%d", count)
	}

	input.position.PlanStopLoss = 0
	input.position.PlanTakeProfit = 11
	input.quote = freshExitQuote(now, 12, 12, 12)
	got = evaluatePositionExit(input, defaultPositionExitParams)
	if got.Level != model.PositionExitLevelWatch {
		t.Fatalf("计划止盈单独应 watch，got %s", got.Level)
	}

	input.position.PlanTakeProfit = 0
	input.rules = []model.AlertRule{{Kind: model.AlertKindCostDrawdown, Threshold: 5}}
	drawBars := exitTestBars(now, 8, 8.2, 7.8, 70)
	for i := 50; i < len(drawBars); i++ {
		drawBars[i].Open, drawBars[i].High, drawBars[i].Low, drawBars[i].Close = 10, 10.2, 9.8, 10
	}
	input.barRows = drawBars
	input.quote = freshExitQuote(now, 9, 9.1, 9)
	got = evaluatePositionExit(input, defaultPositionExitParams)
	if got.Level != model.PositionExitLevelReview {
		t.Fatalf("cost_drawdown 应 review，got %s", got.Level)
	}
	input.rules = []model.AlertRule{{Kind: model.AlertKindCostGain, Threshold: 10}}
	input.quote = freshExitQuote(now, 12, 12, 12)
	got = evaluatePositionExit(input, defaultPositionExitParams)
	if got.Level != model.PositionExitLevelWatch {
		t.Fatalf("cost_gain 单独应 watch，got %s", got.Level)
	}

	input.rules = []model.AlertRule{{Kind: model.AlertKindPeakDrawdown, Threshold: 10}}
	input.quote = freshExitQuote(now, 10, 10.1, 9.9)
	got = evaluatePositionExit(input, defaultPositionExitParams)
	if got.Level != model.PositionExitLevelReview {
		t.Fatalf("peak_drawdown 单独应 review，got %s", got.Level)
	}
}

func TestPositionExitUnknownStaleInsufficientBadBarsAndLongBelowNoRepeat(t *testing.T) {
	now := time.Date(2026, 8, 10, 16, 0, 0, 0, time.Local)
	base := model.Position{UserID: 902, Symbol: "600901", Market: "cn", Name: "未知持仓", Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100, PeakPrice: 12, PeakFrom: "2026-07-01"}
	input := positionExitInput{position: base, quote: freshExitQuote(now, 8, 8.1, 7.9), barRows: exitTestBars(now, 10, 10.5, 9.5, 20), now: now, session: model.PositionExitSessionIntraday}
	input.quote.Fresh.Status = freshStatusStale
	unknown := evaluatePositionExit(input, defaultPositionExitParams)
	if unknown.Level != model.PositionExitLevelUnknown || unknown.ShouldTodo {
		t.Fatalf("stale 必须 unknown 且不进 Todo: %+v", unknown)
	}
	input.quote.Fresh.Status = freshStatusFresh
	if got := evaluatePositionExit(input, defaultPositionExitParams); got.Level != model.PositionExitLevelUnknown {
		t.Fatalf("日线不足必须 unknown")
	}
	input.barRows[2].High = 1
	input.barRows[2].Low = 99
	if got := evaluatePositionExit(input, defaultPositionExitParams); got.Level != model.PositionExitLevelUnknown {
		t.Fatalf("坏 OHLC 必须 unknown")
	}

	// 只在首次跌破时产生 MA20 信号；长期位于均线下方没有新的 crossing。
	goodBars := exitTestBars(now, 8, 8.2, 7.8, 70)
	for i := 50; i < len(goodBars); i++ {
		goodBars[i].Open, goodBars[i].High, goodBars[i].Low, goodBars[i].Close = 10, 10.2, 9.8, 10
	}
	first := positionExitInput{position: base, quote: freshExitQuote(now, 9, 9.1, 8.9), barRows: goodBars, now: now, session: model.PositionExitSessionIntraday}
	first.position.PeakPrice = 12 // ATR 保护线位于现价下方，不参与这个纯 MA 场景
	row := evaluatePositionExit(first, defaultPositionExitParams)
	if row.Level != model.PositionExitLevelWatch {
		t.Fatalf("MA20 首次跌破应 watch，got %s", row.Level)
	}
	second := first
	second.quote = freshExitQuote(now, 8.5, 8.6, 8.4)
	second.barRows[len(second.barRows)-1].Close = 8.5
	second.barRows[len(second.barRows)-1].Open = 8.5
	second.barRows[len(second.barRows)-1].High = 8.6
	second.barRows[len(second.barRows)-1].Low = 8.4
	second.position.PeakPrice = 12
	// 已经低于均线的“第二天”用最新完成日线低于 MA20，prev close < prev MA20。
	second.now = now.AddDate(0, 0, 1)
	second.barRows = append(second.barRows, model.DailyBar{Symbol: base.Symbol, Market: base.Market, TradeDate: second.now.AddDate(0, 0, -1).Format("2006-01-02"), Open: 8.5, High: 8.6, Low: 8.4, Close: 8.5})
	second.quote = freshExitQuote(second.now, 8.5, 8.6, 8.4)
	second.quote.Quote.DataTime = second.now
	secondRow := evaluatePositionExit(second, defaultPositionExitParams)
	for _, signal := range secondRowSignals(secondRow) {
		if signal.Key == "ma20_break" {
			t.Fatal("长期位于均线下方不能每天重复产生刚跌破")
		}
	}
}

func TestPositionExitAllModelsMigrateNoHoldingAndCloseRace(t *testing.T) {
	setupTestDB(t)
	if err := common.DB.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatal(err)
	}
	var tableCount int64
	common.DB.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'position_exit_assessments'").Scan(&tableCount)
	if tableCount != 1 {
		t.Fatalf("AllModels/重复 AutoMigrate 应包含评估表，count=%d", tableCount)
	}
	svc := NewPositionExitAssessmentService(nil)
	if n, err := svc.EvaluateUser(context.Background(), 903001, model.PositionExitSessionIntraday); err != nil || n != 0 {
		t.Fatalf("无持仓评估应为空且无错: n=%d err=%v", n, err)
	}
	now := time.Date(2026, 8, 10, 16, 0, 0, 0, time.Local)
	p := model.Position{UserID: 903, Symbol: "600903", Market: "cn", Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100, PeakPrice: 12, PeakFrom: "2026-07-01"}
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	row := evaluatePositionExit(positionExitInput{position: p, quote: freshExitQuote(now, 10, 10.1, 9.9), barRows: exitTestBars(now, 10, 10.5, 9.5, 70), now: now, session: model.PositionExitSessionIntraday}, defaultPositionExitParams)
	common.DB.Model(&model.Position{}).Where("id = ?", p.ID).Update("status", model.PositionStatusClosed)
	inserted, err := persistPositionExitAssessment(context.Background(), row)
	if err != nil || inserted {
		t.Fatalf("扫描中平仓不得落孤儿事实: inserted=%v err=%v", inserted, err)
	}

	other := model.Position{UserID: 904, Symbol: "600904", Market: "cn", Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100}
	if err := common.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := svc.EvaluateUserWithSnapshot(context.Background(), 903, []model.Position{other}, nil,
		model.PositionExitSessionIntraday, now); err != nil || n != 0 {
		t.Fatalf("跨用户扫描必须跳过: n=%d err=%v", n, err)
	}
	leaked := row
	leaked.UserID, leaked.PositionID, leaked.Symbol = 903, other.ID, other.Symbol
	if inserted, err := persistPositionExitAssessment(context.Background(), leaked); err != nil || inserted {
		t.Fatalf("落库前用户归属复核必须阻止跨用户事实: inserted=%v err=%v", inserted, err)
	}
}

func TestPositionExitTechnicalAndSellReviewResonanceMatrix(t *testing.T) {
	now := time.Date(2026, 8, 10, 16, 0, 0, 0, time.Local)
	base := model.Position{UserID: 907, Symbol: "600901", Market: "cn", Name: "矩阵持仓", Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100, PeakPrice: 9, PeakFrom: "2026-07-01"}
	constant := exitTestBars(now, 10, 10.5, 9.5, 70)
	in := positionExitInput{position: base, quote: freshExitQuote(now, 8, 8.1, 7.9), barRows: constant, now: now, session: model.PositionExitSessionIntraday}
	ma60 := evaluatePositionExit(in, defaultPositionExitParams)
	if ma60.Level != model.PositionExitLevelReview || ma60.PrimarySignal != "ma60_break" {
		t.Fatalf("MA60 单独首次跌破应 review: level=%s primary=%s", ma60.Level, ma60.PrimarySignal)
	}

	atrBars := exitTestBars(now, 8, 8.2, 7.8, 70)
	atrBars[len(atrBars)-1].Open, atrBars[len(atrBars)-1].High, atrBars[len(atrBars)-1].Low, atrBars[len(atrBars)-1].Close = 10, 10.2, 9.8, 10
	in.position.PeakPrice = 11
	in.barRows = atrBars
	in.quote = freshExitQuote(now, 9, 9.1, 8.9)
	atr := evaluatePositionExit(in, defaultPositionExitParams)
	if atr.Level != model.PositionExitLevelReview || atr.PrimarySignal != "atr14_break" {
		t.Fatalf("ATR14 单独首次有效跌破应 review: level=%s primary=%s reason=%s", atr.Level, atr.PrimarySignal, atr.PrimaryReason)
	}

	in.position.PeakPrice = 12
	in.barRows = constant
	in.quote = freshExitQuote(now, 8, 8.1, 7.9)
	both := evaluatePositionExit(in, defaultPositionExitParams)
	if both.Level != model.PositionExitLevelUrgent {
		t.Fatalf("MA60 与 ATR 同时命中应 urgent，got %s", both.Level)
	}

	quiet := positionExitInput{position: base, quote: freshExitQuote(now, 10, 10.1, 9.9), barRows: constant, now: now, session: model.PositionExitSessionIntraday}
	quiet.position.PeakPrice = 12
	quiet.reviews = []model.SellReview{{ID: 1, Trigger: model.SellReviewLift, Severity: model.SellReviewSeverityHigh, Title: "高风险事件", Detail: "需要复核"}}
	if row := evaluatePositionExit(quiet, defaultPositionExitParams); row.Level != model.PositionExitLevelReview {
		t.Fatalf("SellReview high 应从 review 起步，got %s", row.Level)
	}
	quiet.reviews[0].Severity = model.SellReviewSeverityMed
	if row := evaluatePositionExit(quiet, defaultPositionExitParams); row.Level != model.PositionExitLevelWatch {
		t.Fatalf("SellReview med 应从 watch 起步，got %s", row.Level)
	}
	quiet.reviews[0] = model.SellReview{ID: 2, Trigger: model.SellReviewExDiv, Severity: model.SellReviewSeverityLow, Title: "除权口径变化", Detail: "账面口径变化"}
	if row := evaluatePositionExit(quiet, defaultPositionExitParams); row.Level != model.PositionExitLevelNormal {
		t.Fatalf("除权不得单独成为卖出风险，got %s", row.Level)
	}

	ma20Bars := exitTestBars(now, 10, 10.2, 9.8, 70)
	for i := 0; i < 50; i++ {
		ma20Bars[i].Open, ma20Bars[i].High, ma20Bars[i].Low, ma20Bars[i].Close = 8, 8.2, 7.8, 8
	}
	ma20WithMed := positionExitInput{position: base, quote: freshExitQuote(now, 9, 9.1, 8.9), barRows: ma20Bars, now: now, session: model.PositionExitSessionIntraday}
	ma20WithMed.position.PeakPrice = 12
	ma20WithMed.reviews = []model.SellReview{{ID: 4, Trigger: model.SellReviewLhbSell, Severity: model.SellReviewSeverityMed, Title: "中风险事件", Detail: "需要复核"}}
	if row := evaluatePositionExit(ma20WithMed, defaultPositionExitParams); row.Level != model.PositionExitLevelReview {
		t.Fatalf("SellReview med 与独立 MA20 风险共振应升 review，got %s", row.Level)
	}
	ma20WithMed.reviews[0].Severity = model.SellReviewSeverityHigh
	if row := evaluatePositionExit(ma20WithMed, defaultPositionExitParams); row.Level != model.PositionExitLevelUrgent {
		t.Fatalf("SellReview high 与独立 MA20 风险共振应升 urgent，got %s", row.Level)
	}

	resonance := in
	resonance.position.PeakPrice = 9
	resonance.rules = []model.AlertRule{{Kind: model.AlertKindCostDrawdown, Threshold: 10}}
	if row := evaluatePositionExit(resonance, defaultPositionExitParams); row.Level != model.PositionExitLevelUrgent {
		t.Fatalf("成本回撤叠加 MA60 强结构破坏应 urgent，got %s", row.Level)
	}
	unknown := quiet
	unknown.quote.Fresh.Status = freshStatusStale
	unknown.reviews = []model.SellReview{{ID: 3, Trigger: model.SellReviewLift, Severity: model.SellReviewSeverityHigh, Title: "高风险事件"}}
	if row := evaluatePositionExit(unknown, defaultPositionExitParams); row.Level != model.PositionExitLevelUnknown {
		t.Fatalf("unknown 不得与高风险事件拼成共振，got %s", row.Level)
	}
}

func TestPositionExitConsumesAlertEventAndPersistsMeaningfulTransitions(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 8, 10, 14, 0, 0, 0, time.Local)
	p := model.Position{UserID: 908, Symbol: "600901", Market: "cn", Name: "事件持仓", Status: model.PositionStatusHolding,
		BuyPrice: 10, Quantity: 100, PeakPrice: 12, PeakFrom: "2026-07-01"}
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	threshold := 8.0
	ctxJSON, _ := json.Marshal(AlertEventContext{Version: alertEventContextVersion,
		Rule:    AlertContextRule{Kind: model.AlertKindCostDrawdown, Threshold: &threshold},
		Trigger: AlertContextTrigger{Field: "position.cost_drawdown_pct", Threshold: &threshold, Reason: "已命中成本回撤"},
	})
	event := model.AlertEvent{ID: 9081, UserID: p.UserID, PositionID: p.ID, Symbol: p.Symbol, Market: p.Market,
		Kind: model.AlertKindCostDrawdown, Message: "盘中已触达 8% 成本回撤", TradeDate: now.Format("2006-01-02"),
		ContextVersion: alertEventContextVersion, ContextJSON: string(ctxJSON)}
	in := positionExitInput{position: p, quote: freshExitQuote(now, 10, 10.1, 9.9), barRows: exitTestBars(now, 10, 10.2, 9.8, 70),
		now: now, session: model.PositionExitSessionIntraday, events: []model.AlertEvent{event}}
	first := evaluatePositionExit(in, defaultPositionExitParams)
	if first.Level != model.PositionExitLevelReview || first.PrimarySignal != model.AlertKindCostDrawdown {
		t.Fatalf("已落库 AlertEvent 在规则暂停后仍必须参与统一风险: level=%s primary=%s", first.Level, first.PrimarySignal)
	}
	if inserted, err := persistPositionExitAssessment(context.Background(), first); err != nil || !inserted {
		t.Fatalf("首次评估应落库: inserted=%v err=%v", inserted, err)
	}
	closeSnapshot := first
	closeSnapshot.Session = model.PositionExitSessionClose
	closeSnapshot.EvaluatedAt = now.Add(2 * time.Hour)
	if inserted, err := persistPositionExitAssessment(context.Background(), closeSnapshot); err != nil || !inserted {
		t.Fatalf("盘后收盘快照即使事实相同也应追加: inserted=%v err=%v", inserted, err)
	}
	if inserted, err := persistPositionExitAssessment(context.Background(), closeSnapshot); err != nil || inserted {
		t.Fatalf("相同盘后槽位必须幂等: inserted=%v err=%v", inserted, err)
	}

	changedPrimary := closeSnapshot
	changedPrimary.Session = model.PositionExitSessionIntraday
	changedPrimary.EvaluatedAt = now.Add(3 * time.Hour)
	changedPrimary.PrimarySignal = "atr14_break"
	changedPrimary.PrimaryReason = "首次跌破 ATR14 保护线"
	changedPrimary.FactHash = "changed-primary-fact"
	if inserted, err := persistPositionExitAssessment(context.Background(), changedPrimary); err != nil || !inserted {
		t.Fatalf("同级主因变化必须追加: inserted=%v err=%v", inserted, err)
	}
	recovered := changedPrimary
	recovered.EvaluatedAt = now.Add(4 * time.Hour)
	recovered.Level, recovered.PrimarySignal = model.PositionExitLevelNormal, "normal"
	recovered.PrimaryReason, recovered.FactHash = "当前没有新触发的卖出风险事实", "recovered-normal-fact"
	recovered.ShouldTodo = false
	if inserted, err := persistPositionExitAssessment(context.Background(), recovered); err != nil || !inserted {
		t.Fatalf("风险恢复为 normal 必须追加: inserted=%v err=%v", inserted, err)
	}
}

func TestPositionExitNotificationDedupAndUpgrade(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 909009
	common.DB.Where("user_id = ?", userID).Delete(&model.GuardEvent{})
	common.DB.Where("user_id = ?", userID).Delete(&model.NotifyChannel{})
	common.DB.Where("user_id = ?", userID).Delete(&model.UserPreference{})
	if err := common.DB.Create(&model.UserPreference{UserID: userID, EnableNotify: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.NotifyChannel{UserID: userID, Kind: model.NotifyKindWebhook, Name: "测试通道", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	notifier := &recordingAlertNotifier{userID: userID}
	svc := NewPositionExitAssessmentService(nil)
	svc.notify = notifier
	row := model.PositionExitAssessment{UserID: userID, PositionID: 9091, Symbol: "600909", Market: "cn", Name: "通知持仓",
		TradeDate: "2026-08-10", Level: model.PositionExitLevelWatch, PrimaryReason: "仅观察", NextAction: "继续观察"}
	svc.notifyAssessment(context.Background(), row)
	if got := notifier.calls.Load(); got != 0 {
		t.Fatalf("watch 不得推送，got %d", got)
	}
	row.Level, row.PrimaryReason = model.PositionExitLevelReview, "需要复核"
	svc.notifyAssessment(context.Background(), row)
	row.PrimaryReason = "同日另一条 review 事实"
	svc.notifyAssessment(context.Background(), row)
	if got := notifier.calls.Load(); got != 1 {
		t.Fatalf("review 同日应按统一 GuardEvent 去重一次，got %d", got)
	}
	row.Level, row.PrimaryReason = model.PositionExitLevelUrgent, "风险升级"
	svc.notifyAssessment(context.Background(), row)
	if got := notifier.calls.Load(); got != 2 {
		t.Fatalf("review 升 urgent 应允许再次提醒，got %d", got)
	}
	var events int64
	common.DB.Model(&model.GuardEvent{}).Where("user_id = ?", userID).Count(&events)
	if events != 2 {
		t.Fatalf("统一通知台账应仅保留 review/urgent 两条，got %d", events)
	}
}

func secondRowSignals(row model.PositionExitAssessment) []PositionExitSignal {
	var signals []PositionExitSignal
	_ = json.Unmarshal([]byte(row.SignalsJSON), &signals)
	return signals
}
