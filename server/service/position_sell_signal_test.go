package service

import (
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestPositionCloseAndDeleteFinalizeSellSignals 平仓/删除只终结目标 position 的活动
// 卖出信号；同代码兄弟仓及既有历史终态保持不变。
func TestPositionCloseAndDeleteFinalizeSellSignals(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now().In(time.Local)
	buyDate := now.AddDate(0, 0, -2).Format("2006-01-02")
	sellDate := now.AddDate(0, 0, -1).Format("2006-01-02")
	p1 := seedHoldingWithLedger(t, 1, "600000", 10, 100, 0, 0, buyDate)
	p2 := seedHoldingWithLedger(t, 1, "600000", 12, 50, 0, 0, buyDate)

	a1 := model.AlertEvent{RuleID: 1, UserID: 1, PositionID: p1.ID, Symbol: p1.Symbol, Market: "cn",
		Kind: model.AlertKindCostDrawdown, TradeDate: sellDate, Status: model.AlertEventUnread}
	a1History := model.AlertEvent{RuleID: 2, UserID: 1, PositionID: p1.ID, Symbol: p1.Symbol, Market: "cn",
		Kind: model.AlertKindPeakDrawdown, TradeDate: sellDate, Status: model.AlertEventRead}
	a2 := model.AlertEvent{RuleID: 3, UserID: 1, PositionID: p2.ID, Symbol: p2.Symbol, Market: "cn",
		Kind: model.AlertKindCostDrawdown, TradeDate: sellDate, Status: model.AlertEventUnread}
	for _, row := range []*model.AlertEvent{&a1, &a1History, &a2} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatalf("建提醒事件失败: %v", err)
		}
	}
	r1 := model.SellReview{UserID: 1, PositionID: p1.ID, Symbol: p1.Symbol, Market: "cn",
		Trigger: model.SellReviewLift, TradeDate: sellDate, Status: model.SellReviewStatusOpen}
	r1History := model.SellReview{UserID: 1, PositionID: p1.ID, Symbol: p1.Symbol, Market: "cn",
		Trigger: model.SellReviewEarnFcst, TradeDate: sellDate, Status: model.SellReviewStatusResolved}
	r2 := model.SellReview{UserID: 1, PositionID: p2.ID, Symbol: p2.Symbol, Market: "cn",
		Trigger: model.SellReviewLift, TradeDate: sellDate, Status: model.SellReviewStatusOpen}
	for _, row := range []*model.SellReview{&r1, &r1History, &r2} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatalf("建卖出复核失败: %v", err)
		}
	}

	svc := &PositionService{}
	if _, err := svc.Close(1, p1.ID, CloseInput{SellPrice: 11, SellDate: sellDate}); err != nil {
		t.Fatalf("平仓失败: %v", err)
	}
	common.DB.First(&a1, a1.ID)
	common.DB.First(&a1History, a1History.ID)
	common.DB.First(&a2, a2.ID)
	if a1.Status != model.AlertEventRead || a1History.Status != model.AlertEventRead || a2.Status != model.AlertEventUnread {
		t.Fatalf("平仓提醒终结范围错误: target=%s history=%s sibling=%s", a1.Status, a1History.Status, a2.Status)
	}
	common.DB.First(&r1, r1.ID)
	common.DB.First(&r1History, r1History.ID)
	common.DB.First(&r2, r2.ID)
	if r1.Status != model.SellReviewStatusResolved || r1.ResolvedAt == nil ||
		r1History.Status != model.SellReviewStatusResolved || r2.Status != model.SellReviewStatusOpen {
		t.Fatalf("平仓复核终结范围错误: target=%+v history=%+v sibling=%+v", r1, r1History, r2)
	}
	if _, err := SetSellReviewStatus(1, r1.ID, model.SellReviewStatusOpen); err == nil {
		t.Fatal("已平仓 position 的复核不得恢复 open")
	}

	if err := svc.Delete(1, p2.ID); err != nil {
		t.Fatalf("删除兄弟仓失败: %v", err)
	}
	common.DB.First(&a2, a2.ID)
	common.DB.First(&r2, r2.ID)
	if a2.Status != model.AlertEventDismissed || r2.Status != model.SellReviewStatusDismissed || r2.ResolvedAt == nil {
		t.Fatalf("删除应把活动卖出信号标为 dismissed: alert=%s review=%+v", a2.Status, r2)
	}
}

// TestPositionCloseRollsBackWhenSignalFinalizeFails 卖出复核终结失败时，持仓、流水
// 以及此前已更新的提醒状态必须处于同一个回滚边界。
func TestPositionCloseRollsBackWhenSignalFinalizeFails(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now().In(time.Local)
	buyDate := now.AddDate(0, 0, -2).Format("2006-01-02")
	sellDate := now.AddDate(0, 0, -1).Format("2006-01-02")
	p := seedHoldingWithLedger(t, 1, "600001", 10, 100, 0, 0, buyDate)
	ev := model.AlertEvent{RuleID: 11, UserID: 1, PositionID: p.ID, Symbol: p.Symbol, Market: "cn",
		Kind: model.AlertKindCostDrawdown, TradeDate: sellDate, Status: model.AlertEventUnread}
	review := model.SellReview{UserID: 1, PositionID: p.ID, Symbol: p.Symbol, Market: "cn",
		Trigger: model.SellReviewLift, TradeDate: sellDate, Status: model.SellReviewStatusOpen}
	common.DB.Create(&ev)
	common.DB.Create(&review)

	const trigger = "test_fail_sell_review_finalize"
	common.DB.Exec("DROP TRIGGER IF EXISTS " + trigger)
	t.Cleanup(func() { common.DB.Exec("DROP TRIGGER IF EXISTS " + trigger) })
	ddl := fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF status ON sell_reviews
		WHEN OLD.position_id = %d AND OLD.status = 'open'
		BEGIN SELECT RAISE(ABORT, 'forced finalize failure'); END`, trigger, p.ID)
	if err := common.DB.Exec(ddl).Error; err != nil {
		t.Fatalf("建失败注入触发器失败: %v", err)
	}
	if _, err := (&PositionService{}).Close(1, p.ID, CloseInput{SellPrice: 11, SellDate: sellDate}); err == nil {
		t.Fatal("终结卖出复核失败时平仓必须失败")
	}

	var got model.Position
	common.DB.First(&got, p.ID)
	if got.Status != model.PositionStatusHolding || got.Quantity != 100 {
		t.Fatalf("失败平仓必须回滚持仓: %+v", got)
	}
	var trades int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&trades)
	if trades != 1 {
		t.Fatalf("失败平仓必须回滚卖出流水，得到 %d 笔", trades)
	}
	common.DB.First(&ev, ev.ID)
	common.DB.First(&review, review.ID)
	if ev.Status != model.AlertEventUnread || review.Status != model.SellReviewStatusOpen {
		t.Fatalf("失败平仓必须回滚信号状态: alert=%s review=%s", ev.Status, review.Status)
	}
}
