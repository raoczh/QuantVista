package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestTodoCancelDuringAggregationCannotGenerateAdjustments(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 12081, today, 0, 10, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := false
	const hook = "review_todo_cancel_after_alert_read"
	if err := common.DB.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "alert_events" && !canceled {
			canceled = true
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(hook) })
	if _, err := todoTestService(true).BuildInbox(ctx, p.UserID, TodoListOptions{Scope: TodoScopeAll}); !canceled || !errors.Is(err, context.Canceled) {
		t.Errorf("聚合中途取消必须返回取消：canceled=%v err=%v", canceled, err)
	}
	var count int64
	if err := common.DB.Model(&model.PositionCorpAdjust{}).Where("user_id = ?", p.UserID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("已取消的查询不能继续生成公司行动建议：count=%d err=%v", count, err)
	}
}

func TestTodoNonDefaultAccountAdjustmentsRemainVisible(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 12082
	if _, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindReal); err != nil {
		t.Fatal(err)
	}
	account := model.PortfolioAccount{UserID: userID, Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive, Name: "另一真实账户"}
	if err := common.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	p, _ := seedAdjustCase(t, userID, time.Now().Format("2006-01-02"), 0, 10, 1)
	if err := common.DB.Model(p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	res, err := todoTestService(true).BuildInbox(t.Context(), userID, TodoListOptions{Scope: TodoScopeLedger, Source: TodoKindCorpAdjust})
	if err != nil || res.Total != 1 || res.Items[0].ChildCount != 1 {
		t.Fatalf("今日待办应包含另一活动账户的待处理折算：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Items[0].Children[0].DeepLink, "account_id=") {
		t.Fatal("跨账户待办跳转必须定位实际账户")
	}
}

func TestTodoRevertedAdjustmentAppearsOnce(t *testing.T) {
	setupTestDB(t)
	p, _, adjust := seedCorpAdjustReview(t)
	svc := &PositionService{}
	if _, err := svc.ConfirmCorpAdjust(p.UserID, adjust.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevertCorpAdjust(p.UserID, adjust.ID); err != nil {
		t.Fatal(err)
	}
	res, err := todoTestService(true).BuildInbox(t.Context(), p.UserID, TodoListOptions{Scope: TodoScopeLedger, Source: TodoKindCorpAdjust})
	if err != nil || res.Total != 1 || res.Items[0].ChildCount != 1 {
		t.Fatalf("同一撤销事实不能成为两条子事项：res=%+v err=%v", res, err)
	}
}

func TestTodoMarketScopeAndCompletedHistory(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 12083
	today := time.Now().Format("2006-01-02")
	seedHoldingWithPeak(t, userID, "600000", "持仓", 10, 100, 10, today)
	own := seedTodoAlert(t, userID, "600000", model.AlertKindPrice, today, model.AlertEventRead)
	other := seedTodoAlert(t, userID, "600000", model.AlertKindPrice, today, model.AlertEventUnread)
	if err := common.DB.Model(&other).Update("market", "hk").Error; err != nil {
		t.Fatal(err)
	}
	svc := todoTestService(true)
	res, err := svc.BuildInbox(t.Context(), userID, TodoListOptions{Scope: TodoScopeAll, Status: TodoStatusAll})
	if err != nil {
		t.Fatal(err)
	}
	findGroup := func(id int64) TodoItem {
		for _, group := range res.Items {
			for _, child := range group.Children {
				if child.SourceKind == TodoKindAlert && child.SourceID == id {
					return group
				}
			}
		}
		t.Fatalf("未找到提醒 %d", id)
		return TodoItem{}
	}
	if group := findGroup(other.ID); group.Scope != TodoScopeResearch {
		t.Errorf("另一市场同代码不能被归为我的持仓提醒：%+v", group)
	}
	if group := findGroup(own.ID); group.Scope != TodoScopeLedger || group.Status != TodoStatusCompleted {
		t.Errorf("持仓标的普通提醒完成后仍应出现在账本已完成清单：%+v", group)
	}
}

func TestThesisStatusReadFailureRollsBack(t *testing.T) {
	setupTestDB(t)
	card := model.ThesisCard{UserID: 12084, Symbol: "600084", Market: "cn", Thesis: "保留原假设", Status: model.ThesisStatusActive}
	if err := common.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	const hook = "review_thesis_status_receipt_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "thesis_cards" {
			tx.AddError(errors.New("receipt storage failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(hook) })
	if _, err := (&ThesisService{}).SetStatus(card.UserID, card.ID, model.ThesisStatusInvalidated, "待校验原因"); err == nil {
		t.Fatal("回执读取失败应明确返回失败")
	}
	_ = common.DB.Callback().Query().Remove(hook)
	var stored model.ThesisCard
	if err := common.DB.First(&stored, card.ID).Error; err != nil || stored.Status != model.ThesisStatusActive || stored.InvalidReason != "" {
		t.Fatalf("报告失败时状态与失效原因必须一并回滚：stored=%+v err=%v", stored, err)
	}
}
