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

func TestMarketAlertEventContextsCoverEveryKind(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 910001
	common.DB.Where("user_id = ?", userID).Delete(&model.AlertEvent{})
	common.DB.Where("user_id = ?", userID).Delete(&model.AlertRule{})

	now := time.Now().In(time.Local)
	bars := make([]datasource.Bar, 21)
	for i := range bars {
		bars[i] = datasource.Bar{
			TradeDate: now.AddDate(0, 0, i-20).Format("2006-01-02"),
			Open:      9.8, High: 10 + float64(i)/10, Low: 9, Close: 10,
			Volume: 100, Source: "eastmoney",
		}
	}
	market := &fakeAlertMarket{
		getFreshQuote: func(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error) {
			return &datasource.Quote{
				Symbol: "600000", Market: "cn", Price: 12, Open: 10, High: 15, Low: 10,
				PrevClose: 10, ChangePct: 6, Volume: 300, Source: "eastmoney", DataTime: now,
			}, quoteFreshInfo{Status: freshStatusFresh}, nil
		},
		getDailyBars: func(context.Context, string, string, int) ([]datasource.Bar, error) {
			return bars, nil
		},
		getValuation: func(context.Context, string, string) (*datasource.Valuation, error) {
			return &datasource.Valuation{Amplitude: 8, Source: "tencent", DataTime: now}, nil
		},
	}
	rules := []model.AlertRule{
		{UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindPctChange, Op: model.AlertOpGTE, Threshold: 5, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 3, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 3, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindAmplitude, Op: model.AlertOpGTE, Threshold: 5, Status: model.AlertStatusActive},
	}
	for i := range rules {
		if err := common.DB.Create(&rules[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := &AlertService{market: market}
	if hits, err := svc.evaluateRules(context.Background(), rules); err != nil || hits != len(rules) {
		t.Fatalf("全部行情提醒应命中并落上下文: hits=%d err=%v", hits, err)
	}
	var events []model.AlertEvent
	if err := common.DB.Where("user_id = ?", userID).Order("id").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != len(rules) {
		t.Fatalf("事件数=%d want=%d", len(events), len(rules))
	}
	for _, event := range events {
		ctx, ok := parseAlertEventContext(event.ContextVersion, event.ContextJSON)
		if !ok || len(event.ContextJSON) > alertEventContextMaxBytes || ctx.Quote == nil || ctx.Trigger.Value == nil {
			t.Fatalf("%s 上下文不完整: event=%+v ctx=%+v", event.Kind, event, ctx)
		}
		if event.Kind == model.AlertKindMA || event.Kind == model.AlertKindBreakout || event.Kind == model.AlertKindVolumeSurge {
			if ctx.Bar == nil || ctx.Indicator == nil || ctx.Indicator.Source != "eastmoney" || ctx.Indicator.AsOf != ctx.Bar.TradeDate {
				t.Fatalf("%s 必须固化 bar 与指标摘要: %+v", event.Kind, ctx)
			}
			if event.Kind == model.AlertKindMA && ctx.Bar.TradeDate != now.Format("2006-01-02") {
				t.Fatalf("均线快照应保留参与计算的当日在途 bar: %+v", ctx.Bar)
			}
			if event.Kind != model.AlertKindMA && ctx.Bar.TradeDate == now.Format("2006-01-02") {
				t.Fatalf("%s 快照不得冒充使用已从指标窗口剔除的当日在途 bar: %+v", event.Kind, ctx.Bar)
			}
		}
		if event.Kind == model.AlertKindAmplitude && (ctx.Indicator == nil || ctx.Source != "tencent") {
			t.Fatalf("振幅应记录实际估值来源: %+v", ctx)
		}
	}
}

func TestAlertEventContextVersionSizeLegacyImmutableAndIsolation(t *testing.T) {
	setupTestDB(t)
	if !common.DB.Migrator().HasColumn(&model.AlertEvent{}, "ContextVersion") ||
		!common.DB.Migrator().HasColumn(&model.AlertEvent{}, "ContextJSON") {
		t.Fatal("AlertEvent 上下文字段未进入统一自动迁移链路")
	}
	const userID int64 = 910002
	common.DB.Where("user_id IN ?", []int64{userID, userID + 1}).Delete(&model.AlertEvent{})
	common.DB.Where("user_id = ?", userID).Delete(&model.AlertRule{})
	now := time.Now().In(time.Local)
	today := now.Format("2006-01-02")
	rule := model.AlertRule{
		UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindPrice,
		Op: model.AlertOpGTE, Threshold: 11, Status: model.AlertStatusActive,
	}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	quote := &datasource.Quote{Price: 12, High: 12, Low: 10, Source: "Bearer should-not-leak", DataTime: now}
	input := alertEval{Price: 12, DayHigh: 12, DayLow: 10}
	ctx := buildMarketAlertContext(rule, input, quote, nil, quote.Source, alertContextTime(now), "首次命中")
	res, err := persistAlertEvaluation(context.Background(), rule, 12, true, "首次命中", today, now, ctx)
	if err != nil || !res.eventCreated {
		t.Fatalf("首次事件失败: res=%+v err=%v", res, err)
	}
	var first model.AlertEvent
	if err := common.DB.First(&first, res.eventID).Error; err != nil {
		t.Fatal(err)
	}
	if first.ContextVersion != alertEventContextVersion || len(first.ContextJSON) > alertEventContextMaxBytes ||
		strings.Contains(strings.ToLower(first.ContextJSON), "bearer") {
		t.Fatalf("版本/大小/脱敏不符合契约: %+v", first)
	}
	originalJSON := first.ContextJSON
	changed := buildMarketAlertContext(rule, alertEval{Price: 99, DayHigh: 99},
		&datasource.Quote{Price: 99, High: 99, Source: "eastmoney", DataTime: now}, nil, "eastmoney", alertContextTime(now), "重复命中")
	res, err = persistAlertEvaluation(context.Background(), rule, 99, true, "重复命中", today, now.Add(time.Minute), changed)
	if err != nil || res.eventCreated {
		t.Fatalf("同日重复不应新建: res=%+v err=%v", res, err)
	}
	if err := common.DB.First(&first, first.ID).Error; err != nil || first.ContextJSON != originalJSON {
		t.Fatalf("同日去重不得覆盖既有快照: err=%v event=%+v", err, first)
	}

	legacy := model.AlertEvent{RuleID: 77, UserID: userID, Symbol: "000001", Market: "cn", Kind: model.AlertKindPrice,
		Message: "旧事件", TriggeredAt: now, Status: model.AlertEventUnread}
	unknownVersion := model.AlertEvent{RuleID: 78, UserID: userID, Symbol: "000002", Market: "cn", Kind: model.AlertKindPrice,
		Message: "未知版本", ContextVersion: 99, ContextJSON: `{"version":99,"rule":{"kind":"price"},"trigger":{"field":"quote.price","reason":"x"}}`,
		TriggeredAt: now, Status: model.AlertEventUnread}
	other := model.AlertEvent{RuleID: 79, UserID: userID + 1, Symbol: "000003", Market: "cn", Kind: model.AlertKindPrice,
		Message: "他人事件", TriggeredAt: now, Status: model.AlertEventUnread}
	for _, event := range []*model.AlertEvent{&legacy, &unknownVersion, &other} {
		if err := common.DB.Create(event).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := &AlertService{}
	view, err := svc.GetEvent(userID, first.ID)
	if err != nil || !view.ContextAvailable || view.DeepLink != alertEventDeepLink(first.ID) || view.Status != model.AlertEventUnread {
		t.Fatalf("本人详情视图不完整或擅自已读: view=%+v err=%v", view, err)
	}
	legacyView, err := svc.GetEvent(userID, legacy.ID)
	if err != nil || legacyView.ContextAvailable || legacyView.Context != nil {
		t.Fatalf("旧行应兼容为上下文不可用: view=%+v err=%v", legacyView, err)
	}
	unknownView, _ := svc.GetEvent(userID, unknownVersion.ID)
	if unknownView.ContextAvailable {
		t.Fatalf("未知版本不得猜测解析: %+v", unknownView)
	}
	if _, err := svc.GetEvent(userID, other.ID); err == nil {
		t.Fatal("事件详情必须按 user_id 隔离")
	}

	oversized := newAlertEventContext(rule, "quote.price", alertFloat(12), alertFloat(11), "元", "x")
	oversized.Unknown = make([]string, 8)
	for i := range oversized.Unknown {
		oversized.Unknown[i] = strings.Repeat("x", 700)
	}
	if _, _, err := marshalAlertEventContext(oversized); err == nil {
		t.Fatal("超过大小上限的上下文必须拒绝")
	}
}
