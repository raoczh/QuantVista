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
	common.DB.Exec("DELETE FROM alert_events")
	common.DB.Exec("DELETE FROM recommendation_statuses")
	common.DB.Exec("DELETE FROM positions")

	// 未读的提醒命中事件（用户1）——批次 H 起待办以 alert_events unread 为准。
	now := time.Now()
	common.DB.Create(&model.AlertEvent{RuleID: 1, UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Message: "当日最高触及 ≥ 11", TriggeredAt: now, Status: model.AlertEventUnread})
	common.DB.Create(&model.AlertEvent{RuleID: 2, UserID: 1, Symbol: "600519", Market: "cn", Name: "贵州茅台",
		Kind: model.AlertKindVolumeSurge, Message: "当日量达 20 日均量的 2.50 倍", TriggeredAt: now, Status: model.AlertEventUnread})
	// 已读事件不进待办。
	common.DB.Create(&model.AlertEvent{RuleID: 3, UserID: 1, Symbol: "000002", Market: "cn", Name: "万科A",
		Kind: model.AlertKindPrice, Message: "已处理过的命中", TriggeredAt: now, Status: model.AlertEventRead})
	// 他人事件（不应出现）。
	common.DB.Create(&model.AlertEvent{RuleID: 4, UserID: 2, Symbol: "000001", Market: "cn",
		Kind: model.AlertKindPrice, Message: "他人命中", TriggeredAt: now, Status: model.AlertEventUnread})

	// 需复盘的短线推荐：止损（priority 1）+ 过期（priority 2）。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 1, BatchID: 10, UserID: 1, Symbol: "600519",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeStopLoss, ReviewNeeded: true, ReturnPct: -8})
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 2, BatchID: 11, UserID: 1, Symbol: "000002",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeExpired, ReviewNeeded: true, ReturnPct: 1, ValidDays: 5})
	// 不需复盘的（进行中）不计入。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 3, BatchID: 12, UserID: 1, Symbol: "600004",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeActive, ReviewNeeded: false})
	// 已读的复盘提示不再进清单（review_ack 人工标记，追踪刷新不覆盖）。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 4, BatchID: 13, UserID: 1, Symbol: "600005",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeExpired, ReviewNeeded: true, ReviewAck: true})

	svc := NewTodoService(&AlertService{}, &PositionService{market: nil}, nil)
	// position.List 需要 market 富化；这里无持仓，List 返回空即可（跳过持仓分支）。
	res, err := svc.Build(context.Background(), 1)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	if res.Alerts != 2 {
		t.Fatalf("命中提醒应为 2（未读事件，已读不计），得到 %d", res.Alerts)
	}
	if res.Reviews != 2 {
		t.Fatalf("推荐复盘应为 2，得到 %d", res.Reviews)
	}
	if res.Total != 4 {
		t.Fatalf("待办合计应为 4，得到 %d", res.Total)
	}
	// 排序：优先级 1 的（提醒命中 + 止损复盘）排在过期复盘（优先级 2）之前。
	if res.Items[len(res.Items)-1].Priority != 2 {
		t.Fatalf("最后一项应为优先级 2（过期），得到 %d", res.Items[len(res.Items)-1].Priority)
	}
	// alert 条目的 RefID 应为事件 id（供前端标记已读/忽略）；rec_review 条目的
	// RefID 应为追踪状态行 id（供前端「已读」消项）。
	for _, it := range res.Items {
		if it.Kind == TodoKindAlert && it.RefID == 0 {
			t.Fatalf("alert 待办应携带事件 id: %+v", it)
		}
		if it.Kind == TodoKindRecReview && it.RefID == 0 {
			t.Fatalf("rec_review 待办应携带追踪状态 id: %+v", it)
		}
	}

	// AckReview 标记已读后从清单消失；他人不可标记。
	var st model.RecommendationStatus
	if err := common.DB.Where("recommendation_id = ?", 1).First(&st).Error; err != nil {
		t.Fatalf("读取追踪状态失败: %v", err)
	}
	tracking := NewTrackingService(nil)
	if err := tracking.AckReview(2, st.ID); err == nil {
		t.Fatalf("他人标记已读应被拒绝")
	}
	if err := tracking.AckReview(1, st.ID); err != nil {
		t.Fatalf("AckReview 失败: %v", err)
	}
	resAck, _ := svc.Build(context.Background(), 1)
	if resAck.Reviews != 1 {
		t.Fatalf("已读后推荐复盘应剩 1，得到 %d", resAck.Reviews)
	}

	// 用户隔离：用户2 只见自己的 1 条命中提醒。
	res2, _ := svc.Build(context.Background(), 2)
	if res2.Total != 1 || res2.Alerts != 1 {
		t.Fatalf("用户2 应只见 1 条命中提醒，得到 total=%d alerts=%d", res2.Total, res2.Alerts)
	}
}
