package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestPositionRiskDetectsUnmaterializedShareAction(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 992, today, 0, 10, 0)
	pending, err := positionsWithUnconfirmedShareAction(context.Background(), p.UserID, []int64{p.ID}, today)
	if err != nil || !pending[p.ID] {
		t.Fatalf("送转已到期且持仓有权，即使尚无调整建议也不能按旧成本评估：pending=%v err=%v", pending, err)
	}
}

func TestPositionAlertRejectsCorpLookupFailure(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 993
	now := time.Now()
	seedHoldingWithPeak(t, userID, "600096", "成本核验", 10, 100, 10, now.AddDate(0, 0, -1).Format("2006-01-02"))
	rule := model.AlertRule{UserID: userID, Market: "cn", Kind: model.AlertKindCostDrawdown,
		Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	fault := errors.New("除权状态存储不可用")
	const callback = "review_position_corp_lookup_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "position_corp_adjusts" {
			tx.AddError(fault)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	svc := &AlertService{market: &fakeAlertMarket{getFreshQuote: func(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error) {
		return &datasource.Quote{Price: 5, High: 5, Low: 5, DataTime: now}, quoteFreshInfo{Status: freshStatusFresh}, nil
	}}}
	if hits, err := svc.evaluatePositionRules(context.Background(), userID, []model.AlertRule{rule}); hits != 0 || !errors.Is(err, fault) {
		t.Fatalf("无法确认除权口径时不能发出确定的成本止损提醒：hits=%d err=%v", hits, err)
	}
	var count int64
	if err := common.DB.Model(&model.AlertEvent{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("未知口径不能落命中事件：count=%d err=%v", count, err)
	}
}

func TestPositionAlertCommitRejectsChangedCost(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	date := now.Format("2006-01-02")
	p := seedHoldingWithPeak(t, 994, "600097", "成本已修改", 10, 100, 10, previousDate(date))
	rule := model.AlertRule{UserID: p.UserID, Market: "cn", Kind: model.AlertKindCostDrawdown,
		Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	quote := &datasource.Quote{Price: 8, High: 8, Low: 8, DataTime: now}
	input := positionAlertEval{AvgCost: 10, Price: 8, DayHigh: 8, DayLow: 8, Peak: 10}
	hit, value, message := evaluatePositionAlert(rule, p.Name, input)
	if !hit {
		t.Fatal("旧成本下应先命中，用于验证迟到提交")
	}
	if err := common.DB.Model(p).Updates(map[string]any{"buy_price": 5, "remaining_cost": 500}).Error; err != nil {
		t.Fatal(err)
	}
	old := *p
	old.BuyPrice = 10 // GORM Model(p) 可能同步更新结构体；保留明确的旧评估快照。
	h := positionAlertHit{Position: old, Value: value, Message: message, TradeDate: date,
		Context: buildPositionAlertContext(rule, input, quote, old, value, message, date)}
	created, _, err := persistPositionAlertEvaluation(context.Background(), rule, []positionAlertHit{h}, value, true, date, now)
	if err != nil || len(created) != 0 {
		t.Fatalf("成本已修改后，旧成本计算的亏损不能继续落库：created=%v err=%v", created, err)
	}
	var stored model.AlertRule
	if err := common.DB.First(&stored, rule.ID).Error; err != nil || stored.LastValue != 0 {
		t.Fatalf("旧成本观测值也不能覆盖规则：value=%v err=%v", stored.LastValue, err)
	}
}

func TestPositionExitCommitRejectsChangedCost(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	p := seedHoldingWithPeak(t, 995, "600098", "旧评估", 10, 100, 10, previousDate(now.Format("2006-01-02")))
	row := evaluatePositionExit(positionExitInput{position: *p,
		quote: FreshQuoteResult{Quote: &datasource.Quote{Price: 8, High: 8, Low: 8, DataTime: now}, Fresh: quoteFreshInfo{Status: freshStatusFresh}},
		rules: []model.AlertRule{{Kind: model.AlertKindCostDrawdown, Threshold: 10}}, now: now,
		session: model.PositionExitSessionIntraday}, defaultPositionExitParams)
	if err := common.DB.Model(p).Updates(map[string]any{"buy_price": 5, "remaining_cost": 500}).Error; err != nil {
		t.Fatal(err)
	}
	inserted, notify, err := persistPositionExitAssessment(context.Background(), &row)
	if err != nil || inserted || notify {
		t.Fatalf("成本已变更后不能提交旧卖出风险事实：inserted=%v notify=%v err=%v", inserted, notify, err)
	}
}

func TestPositionExitNotificationUsesCommittedID(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	p := seedHoldingWithPeak(t, 996, "600099", "通知精确记录", 10, 100, 10, previousDate(now.Format("2006-01-02")))
	if err := common.DB.Model(p).Update("plan_stop_loss", 9).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.UserPreference{UserID: p.UserID, EnableNotify: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.NotifyChannel{UserID: p.UserID, Kind: model.NotifyKindWebhook, Name: "本地拦截", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	notifier := &recordingAlertNotifier{userID: p.UserID}
	svc := &PositionExitAssessmentService{notify: notifier}
	quotes := map[string]FreshQuoteResult{QuoteKey(p.Market, p.Symbol): {
		Quote: &datasource.Quote{Price: 8, High: 8, Low: 8, DataTime: now}, Fresh: quoteFreshInfo{Status: freshStatusFresh},
	}}
	if n, err := svc.EvaluateUserWithSnapshot(context.Background(), p.UserID, []model.Position{*p}, quotes, model.PositionExitSessionIntraday, now); err != nil || n != 1 {
		t.Fatalf("应生成一条计划止损事实：n=%d err=%v", n, err)
	}
	var stored model.PositionExitAssessment
	if err := common.DB.Where("position_id = ?", p.ID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	wantRoute := fmt.Sprintf("/positions?position_id=%d&assessment_id=%d", p.ID, stored.ID)
	message := notifier.lastMessage
	if notifier.calls.Load() != 1 || message.Route != wantRoute || len(message.BrowserEvents) != 1 || message.BrowserEvents[0].SourceID != stored.ID {
		t.Fatalf("真实评估→提交→通知链路必须引用已提交事实，不能使用 ID=0：want=%s message=%+v", wantRoute, message)
	}
}

func TestAlertCommitRejectsChangedRule(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	rule := model.AlertRule{UserID: 997, Market: "cn", Symbol: "600100", Kind: model.AlertKindPrice,
		Op: model.AlertOpLTE, Threshold: 8, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.AlertRule{}).Where("id = ?", rule.ID).Update("threshold", 5).Error; err != nil {
		t.Fatal(err)
	}
	result, err := persistAlertEvaluation(context.Background(), rule, 7, true, "旧阈值命中", now.Format("2006-01-02"), now)
	if err != nil || result.active || result.eventCreated {
		t.Fatalf("修改后的规则不能提交旧阈值命中：result=%+v err=%v", result, err)
	}
}
