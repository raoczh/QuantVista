package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestTodoReviewHugePageCannotOverflow(t *testing.T) {
	setupTestDB(t)
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("超大页码导致切片越界：%v", recovered)
		}
	}()
	result, err := todoTestService(false).BuildInbox(t.Context(), 930030, TodoListOptions{Scope: TodoScopeAll, Status: TodoStatusCompleted, Page: int(^uint(0) >> 1), PageSize: 100})
	if err != nil || len(result.Items) != 0 || result.HasMore {
		t.Fatalf("超出历史范围的页码应安全返回空页：result=%+v err=%v", result, err)
	}
}

func TestTodoReviewBatchFailureRollsBackSourceAndInbox(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 930031
	today := time.Now().Format("2006-01-02")
	first := seedTodoAlert(t, userID, "600001", model.AlertKindPrice, today, model.AlertEventUnread)
	second := seedTodoAlert(t, userID, "600002", model.AlertKindPrice, today, model.AlertEventUnread)
	refs := make([]TodoSourceRef, 0, 2)
	for _, id := range []int64{first.ID, second.ID} {
		ref, err := currentTodoSourceRef(userID, TodoKindAlert, id)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	const hook = "review_todo_second_state_failure"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*model.TodoInboxState); ok && row.SourceID == second.ID {
			tx.AddError(errors.New("模拟第二条状态写入失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Create().Remove(hook) })
	if err := todoTestService(false).ApplyInboxAction(userID, TodoActionRequest{Action: TodoActionRead, Items: refs}); err == nil {
		t.Fatal("应报告真实写入故障")
	}
	var changed, states int64
	if err := common.DB.Model(&model.AlertEvent{}).Where("user_id = ? AND status <> ?", userID, model.AlertEventUnread).Count(&changed).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.TodoInboxState{}).Where("user_id = ?", userID).Count(&states).Error; err != nil {
		t.Fatal(err)
	}
	if changed != 0 || states != 0 {
		t.Errorf("批量失败不应留下部分已读事实：changed=%d states=%d", changed, states)
	}
}

func TestTodoReviewNewRiskVersionCannotBeMarkedRead(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 930032
	now := time.Now()
	today := now.Format("2006-01-02")
	position := seedHoldingWithPeak(t, userID, "600032", "并发风险回归", 10, 100, 12, today)
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	position.AccountID = account.ID
	assessment := model.PositionExitAssessment{UserID: userID, PositionID: position.ID, Symbol: position.Symbol, Market: "cn", Name: position.Name,
		TradeDate: today, Session: model.PositionExitSessionIntraday, EvaluatedAt: now, Level: model.PositionExitLevelReview,
		PrimarySignal: model.AlertKindCostDrawdown, PrimaryReason: "原风险", NextAction: "复核", DataStatus: model.PositionExitDataReady,
		ShouldTodo: true, Version: model.PositionExitAssessmentVersion, PositionStateHash: positionRiskBasisHash(*position),
		FactHash: "review-old-risk", EventKey: "review-old-event"}
	if err := common.DB.Create(&assessment).Error; err != nil {
		t.Fatal(err)
	}
	ref, err := currentTodoSourceRef(userID, TodoKindPositionExit, position.ID)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := false
	const hook = "review_todo_new_risk_after_check"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*model.PositionExitAssessment); ok && !upgraded {
			upgraded = true
			urgent := assessment
			urgent.ID = 0
			urgent.Level = model.PositionExitLevelUrgent
			urgent.FactHash = "review-new-risk"
			urgent.EventKey = "review-new-event"
			urgent.EvaluatedAt = now.Add(time.Second)
			if err := common.DB.Create(&urgent).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	svc := todoTestService(true)
	_ = svc.ApplyInboxAction(userID, TodoActionRequest{Action: TodoActionRead, Items: []TodoSourceRef{ref}})
	if !upgraded {
		t.Fatal("必须在旧版本读取之后插入新的 urgent 事实")
	}
	result, err := svc.BuildInbox(t.Context(), userID, TodoListOptions{Scope: TodoScopeAll, Status: TodoStatusNeedsAction})
	if err != nil || result.Total != 1 || result.Items[0].Severity != "critical" {
		t.Fatalf("用户只看过旧 review，不能把新 urgent 一起标成已读：result=%+v err=%v", result, err)
	}
}

func TestTodoReviewChangedAlertCannotBeAcknowledged(t *testing.T) {
	setupTestDB(t)
	checkTodoReviewChangedAlert(t)
}

func checkTodoReviewChangedAlert(t *testing.T) {
	t.Helper()
	const userID int64 = 930033
	event := seedTodoAlert(t, userID, "600033", model.AlertKindPrice, time.Now().Format("2006-01-02"), model.AlertEventUnread)
	ref, err := currentTodoSourceRef(userID, TodoKindAlert, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	const hook = "review_todo_changed_alert"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*model.AlertEvent); ok && !changed {
			changed = true
			if err := common.DB.Model(&model.AlertEvent{}).Where("id = ?", event.ID).Update("context_version", event.ContextVersion+1).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	err = todoTestService(false).ApplyInboxAction(userID, TodoActionRequest{Action: TodoActionRead, Items: []TodoSourceRef{ref}})
	if !changed {
		t.Fatal("必须在旧版本校验读取后提交新内容")
	}
	if err == nil {
		t.Error("来源变更应要求刷新后再处理")
	}
	var stored model.AlertEvent
	if err := common.DB.First(&stored, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.AlertEventUnread {
		t.Errorf("旧页面点击收下吞掉了新事件内容：status=%s", stored.Status)
	}
}

func TestMySQLTodoReviewChangedAlertCannotBeAcknowledged(t *testing.T) {
	setupMySQLReviewDB(t, model.AllModels()...)
	checkTodoReviewChangedAlert(t)
}

func TestTodoReviewListVersionMatchesDisplayedContent(t *testing.T) {
	setupTestDB(t)
	checkTodoReviewListVersionMatchesDisplayedContent(t)
}

func TestMySQLTodoReviewListVersionMatchesDisplayedContent(t *testing.T) {
	setupMySQLReviewDB(t, model.AllModels()...)
	checkTodoReviewListVersionMatchesDisplayedContent(t)
}

func checkTodoReviewListVersionMatchesDisplayedContent(t *testing.T) {
	t.Helper()
	const userID int64 = 930034
	event := seedTodoAlert(t, userID, "600034", model.AlertKindPrice, time.Now().Format("2006-01-02"), model.AlertEventUnread)
	changed := false
	const hook = "review_todo_list_content_version"
	if err := common.DB.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		rows, ok := tx.Statement.Dest.(*[]model.AlertEvent)
		if !ok || changed || len(*rows) != 1 || (*rows)[0].ID != event.ID {
			return
		}
		changed = true
		if err := common.DB.Model(&model.AlertEvent{}).Where("id = ?", event.ID).
			Updates(map[string]any{"message": "新的风险内容", "context_version": event.ContextVersion + 1}).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	svc := todoTestService(false)
	result, err := svc.BuildInbox(t.Context(), userID, TodoListOptions{Scope: TodoScopeAll, Status: TodoStatusAwareness})
	if err != nil || !changed {
		t.Fatalf("必须在正文读取后变更来源：changed=%v err=%v", changed, err)
	}
	child := findTodoChild(t, result, TodoKindAlert, event.ID)
	if child.Detail != event.Message {
		t.Fatalf("本次列表应该保留已读取的旧正文：%+v", child)
	}
	ref := TodoSourceRef{SourceKind: child.SourceKind, SourceID: child.SourceID, SourceVersion: child.SourceVersion}
	if err := svc.ApplyInboxAction(userID, TodoActionRequest{Action: TodoActionRead, Items: []TodoSourceRef{ref}}); err == nil {
		t.Error("展示旧正文却携带新版本，用户收下了未见过的新内容")
	}
	var stored model.AlertEvent
	if err := common.DB.First(&stored, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.AlertEventUnread {
		t.Errorf("新内容必须保持未读：message=%s status=%s", stored.Message, stored.Status)
	}
}

func TestTodoReviewConcurrentCompletionAppearsOnce(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 930035
	event := seedTodoAlert(t, userID, "600035", model.AlertKindPrice, time.Now().Format("2006-01-02"), model.AlertEventUnread)
	changed := false
	const hook = "review_todo_concurrent_completion"
	if err := common.DB.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		rows, ok := tx.Statement.Dest.(*[]model.AlertEvent)
		if !ok || changed || len(*rows) != 1 || (*rows)[0].ID != event.ID {
			return
		}
		changed = true
		if err := common.DB.Model(&model.AlertEvent{}).Where("id = ?", event.ID).Update("status", model.AlertEventRead).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	result, err := todoTestService(false).BuildInbox(t.Context(), userID, TodoListOptions{Scope: TodoScopeAll, Status: TodoStatusAll})
	if err != nil || !changed {
		t.Fatalf("必须在未读清单读取后完成来源：changed=%v err=%v", changed, err)
	}
	if result.Total != 1 || result.Items[0].Status != TodoStatusCompleted || result.Items[0].ChildCount != 1 || result.StatusCounts[TodoStatusAwareness] != 0 {
		t.Fatalf("同时读取到完成历史时不得保留重复未读来源：%+v", result)
	}
}

func TestTodoReviewMuteInformationalIpo(t *testing.T) {
	setupTestDB(t)
	cleanTodoInboxTables(t)
	const userID int64 = 930036
	sub := model.IpoSubscription{Kind: model.IpoKindStock, Code: "301036", Name: "信息级待办", ApplyCode: "301036", ApplyDate: time.Now().Format("2006-01-02")}
	if err := common.DB.Create(&sub).Error; err != nil {
		t.Fatal(err)
	}
	ref, err := currentTodoSourceRef(userID, TodoKindIpo, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	svc := todoTestService(false)
	if err := svc.ApplyInboxAction(userID, TodoActionRequest{Action: TodoActionMuteToday, Items: []TodoSourceRef{ref}}); err != nil {
		t.Fatal(err)
	}
	result, err := svc.BuildInbox(t.Context(), userID, TodoListOptions{Scope: TodoScopeMarket, Status: TodoStatusAwareness})
	if err != nil || result.Total != 0 {
		t.Fatalf("信息级事项也应遵守今天不再提醒：result=%+v err=%v", result, err)
	}
}

func TestTodoReviewCompletedInboxCannotBeReopenedByStaleSnooze(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 930037
	notice := model.JobFailureNotification{JobRunID: 930037, UserID: userID, Kind: JobKindAnalysis, ErrorCode: "failed", Status: model.JobFailureNoticeAttempted}
	if err := common.DB.Create(&notice).Error; err != nil {
		t.Fatal(err)
	}
	ref, err := currentTodoSourceRef(userID, TodoKindJobFailure, notice.ID)
	if err != nil {
		t.Fatal(err)
	}
	svc := todoTestService(false)
	if err := svc.ApplyInboxAction(userID, TodoActionRequest{Action: TodoActionRead, Items: []TodoSourceRef{ref}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyInboxAction(userID, TodoActionRequest{Action: TodoActionSnooze, Items: []TodoSourceRef{ref}}); err == nil {
		t.Error("旧页面的稍后请求应识别事项已完成")
	}
	var state model.TodoInboxState
	if err := common.DB.Where("user_id = ? AND source_kind = ? AND source_id = ?", userID, ref.SourceKind, ref.SourceID).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if !state.Read || state.SnoozedUntil != nil {
		t.Errorf("已收下事项被旧稍后操作改回未读：%+v", state)
	}
}

func TestMySQLTodoReviewCancelDuringSourceLock(t *testing.T) {
	db := setupMySQLReviewDB(t, model.AllModels()...)
	const userID int64 = 930038
	event := seedTodoAlert(t, userID, "600038", model.AlertKindPrice, time.Now().Format("2006-01-02"), model.AlertEventUnread)
	ref, err := currentTodoSourceRef(userID, TodoKindAlert, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	writer := db.Begin()
	if writer.Error != nil {
		t.Fatal(writer.Error)
	}
	defer writer.Rollback()
	if err := writer.Clauses(clause.Locking{Strength: "UPDATE"}).First(&event, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	const hook = "review_todo_source_cancel"
	if err := db.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, locking := tx.Statement.Clauses["FOR"]; tx.Statement.Table == "alert_events" && locking {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(hook) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- todoTestService(false).ApplyInboxActionContext(ctx, userID, TodoActionRequest{Action: TodoActionRead, Items: []TodoSourceRef{ref}})
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("未进入来源锁：%v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("未进入来源锁等待")
	}
	select {
	case err := <-result:
		t.Fatalf("请求未等待已持有的来源锁：%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("应返回取消原因：%v", err)
		}
	case <-time.After(700 * time.Millisecond):
		t.Error("取消后仍阻塞在来源锁")
		writer.Rollback()
		select {
		case <-result:
		case <-time.After(3 * time.Second):
			t.Fatal("释放锁后请求仍未结束")
		}
	}
	if err := db.First(&event, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if event.Status != model.AlertEventUnread {
		t.Errorf("取消后不得标记已读：%s", event.Status)
	}
}
