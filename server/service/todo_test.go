package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestTodoBuild 聚合命中提醒 + 推荐复盘 + 持仓复盘，验证计数、排序与用户隔离。
func TestTodoBuild(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_rules")
	common.DB.Exec("DELETE FROM recommendation_statuses")
	common.DB.Exec("DELETE FROM positions")

	// 命中的提醒（用户1）。
	now := time.Now()
	common.DB.Create(&model.AlertRule{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11,
		Status: model.AlertStatusTriggered, TriggerMsg: "当日最高触及 ≥ 11", TriggeredAt: &now})
	// 他人命中（不应出现）。
	common.DB.Create(&model.AlertRule{UserID: 2, Symbol: "000001", Market: "cn",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 1, Status: model.AlertStatusTriggered, TriggeredAt: &now})

	// 需复盘的短线推荐：止损（priority 1）+ 过期（priority 2）。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 1, BatchID: 10, UserID: 1, Symbol: "600519",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeStopLoss, ReviewNeeded: true, ReturnPct: -8})
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 2, BatchID: 11, UserID: 1, Symbol: "000002",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeExpired, ReviewNeeded: true, ReturnPct: 1, ValidDays: 5})
	// 不需复盘的（进行中）不计入。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 3, BatchID: 12, UserID: 1, Symbol: "600004",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeActive, ReviewNeeded: false})

	svc := NewTodoService(&AlertService{}, &PositionService{market: nil})
	// position.List 需要 market 富化；这里无持仓，List 返回空即可（跳过持仓分支）。
	res, err := svc.Build(context.Background(), 1)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	if res.Alerts != 1 {
		t.Fatalf("命中提醒应为 1，得到 %d", res.Alerts)
	}
	if res.Reviews != 2 {
		t.Fatalf("推荐复盘应为 2，得到 %d", res.Reviews)
	}
	if res.Total != 3 {
		t.Fatalf("待办合计应为 3，得到 %d", res.Total)
	}
	// 排序：优先级 1 的（提醒命中 + 止损复盘）排在过期复盘（优先级 2）之前。
	if res.Items[len(res.Items)-1].Priority != 2 {
		t.Fatalf("最后一项应为优先级 2（过期），得到 %d", res.Items[len(res.Items)-1].Priority)
	}

	// 用户隔离：用户2 只见自己的 1 条命中提醒。
	res2, _ := svc.Build(context.Background(), 2)
	if res2.Total != 1 || res2.Alerts != 1 {
		t.Fatalf("用户2 应只见 1 条命中提醒，得到 total=%d alerts=%d", res2.Total, res2.Alerts)
	}
}
