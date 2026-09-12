package service

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestPositionAlertUsesUnroundedThresholds(t *testing.T) {
	for _, kind := range []string{model.AlertKindCostGain, model.AlertKindCostDrawdown, model.AlertKindPeakDrawdown} {
		t.Run(kind, func(t *testing.T) {
			price := 9.0004
			if kind == model.AlertKindCostGain {
				price = 10.9996
			}
			hit, value, _ := evaluatePositionAlert(model.AlertRule{Kind: kind, Threshold: 10}, "精度持仓", positionAlertEval{AvgCost: 10, Peak: 10, Price: price})
			if hit || math.Abs(value-9.996) > 1e-9 {
				t.Fatalf("实际变化 9.996%% 不能舍入成命中 10%%：hit=%v value=%v", hit, value)
			}
			if kind == model.AlertKindCostGain {
				price = 11
			} else {
				price = 9
			}
			if hit, _, _ := evaluatePositionAlert(model.AlertRule{Kind: kind, Threshold: 10}, "精度持仓", positionAlertEval{AvgCost: 10, Peak: 10, Price: price}); !hit {
				t.Fatal("真实达到10%时仍应命中")
			}
			price = 9.4
			if kind == model.AlertKindCostGain {
				price = 10.6
			}
			if hit, _, _ := evaluatePositionAlert(model.AlertRule{Kind: kind, Threshold: 6}, "浮点边界", positionAlertEval{AvgCost: 10, Peak: 10, Price: price}); !hit {
				t.Fatal("真实恰好达到6%时不能被浮点尾差误拒绝")
			}
		})
	}
}

func TestPositionAlertMissingQuoteDoesNotAdvanceCheck(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithPeak(t, 12030, "600901", "缺行情持仓", 10, 100, 12, "2026-07-01")
	rule := model.AlertRule{UserID: p.UserID, Market: "cn", Kind: model.AlertKindCostDrawdown, Op: model.AlertOpGTE, Threshold: 10,
		Status: model.AlertStatusActive, LastCheckDate: "2026-07-01", LastValue: 3}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	svc := &AlertService{market: &fakeAlertMarket{getFreshQuote: func(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error) {
		return &datasource.Quote{Price: 8, DataTime: time.Now()}, quoteFreshInfo{Status: freshStatusStale}, nil
	}}}
	if hits, err := svc.evaluatePositionRules(context.Background(), p.UserID, []model.AlertRule{rule}); hits != 0 || err == nil {
		t.Errorf("没有可用行情不能宣称成功检查：hits=%d err=%v", hits, err)
	}
	var stored model.AlertRule
	if err := common.DB.First(&stored, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LastCheckDate != rule.LastCheckDate || stored.LastValue != rule.LastValue {
		t.Fatalf("未完成检查不得推进检查日期或覆盖观测值：%+v", stored)
	}
}

func TestPositionAlertRepeatDoesNotRetimestampOrCountOldEvent(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithPeak(t, 12031, "600901", "重复持仓提醒", 10, 100, 12, "2026-07-01")
	now := time.Now()
	rule := model.AlertRule{UserID: p.UserID, Market: "cn", Kind: model.AlertKindCostDrawdown, Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	svc := &AlertService{market: &fakeAlertMarket{getFreshQuote: func(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error) {
		return &datasource.Quote{Price: 8, High: 8, Low: 8, DataTime: now}, quoteFreshInfo{Status: freshStatusFresh}, nil
	}}}
	if hits, err := svc.evaluatePositionRules(context.Background(), p.UserID, []model.AlertRule{rule}); hits != 1 || err != nil {
		t.Fatalf("首次应新增一次：hits=%d err=%v", hits, err)
	}
	var original model.AlertRule
	if err := common.DB.First(&original, rule.ID).Error; err != nil || original.TriggeredAt == nil {
		t.Fatalf("首次触发应保存时间：rule=%+v err=%v", original, err)
	}
	if hits, err := svc.evaluatePositionRules(context.Background(), p.UserID, []model.AlertRule{rule}); hits != 0 || err != nil {
		t.Errorf("已有当日事件不能计作本轮新触发：hits=%d err=%v", hits, err)
	}
	var stored model.AlertRule
	if err := common.DB.First(&stored, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.TriggeredAt == nil || !stored.TriggeredAt.Equal(*original.TriggeredAt) {
		t.Fatalf("重复命中不得把旧事件伪装成刚刚发生：before=%v after=%v", original.TriggeredAt, stored.TriggeredAt)
	}
}

func TestPositionAlertEvidencePreservesPricePrecision(t *testing.T) {
	hit, _, message := evaluatePositionAlert(model.AlertRule{Kind: model.AlertKindCostDrawdown, Threshold: 1}, "四位价格持仓", positionAlertEval{AvgCost: 4.1375, Price: 4.0375})
	if !hit || !strings.Contains(message, "4.1375") || !strings.Contains(message, "4.0375") {
		t.Fatalf("持仓规则证据不能截断成本与现价：hit=%v message=%s", hit, message)
	}
}
