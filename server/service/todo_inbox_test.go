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

func cleanTodoInboxTables(t *testing.T) {
	t.Helper()
	cleanCorpTables(t)
	for _, table := range []string{
		"todo_inbox_states", "recommendation_statuses", "job_failure_notifications", "job_runs",
	} {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func todoTestService(withMarket bool) *TodoService {
	position := &PositionService{market: nil}
	if withMarket {
		position = NewPositionService(NewMarketService(datasource.NewManagerWithAdapters()))
	}
	return NewTodoService(&AlertService{}, position, nil)
}

func seedTodoAlert(t *testing.T, userID int64, symbol, kind, tradeDate, status string) model.AlertEvent {
	t.Helper()
	now := time.Now()
	row := model.AlertEvent{
		RuleID: time.Now().UnixNano(), UserID: userID, Symbol: symbol, Market: "cn", Name: symbol,
		Kind: kind, Message: "命中说明", TradeDate: tradeDate, TriggeredAt: now, Status: status,
	}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func findTodoChild(t *testing.T, res *TodoResult, sourceKind string, sourceID int64) TodoChild {
	t.Helper()
	for _, group := range res.Items {
		for _, child := range group.Children {
			if child.SourceKind == sourceKind && child.SourceID == sourceID {
				return child
			}
		}
	}
	t.Fatalf("未找到来源 %s/%d: %+v", sourceKind, sourceID, res.Items)
	return TodoChild{}
}

func TestTodoInboxIsolationSnoozeExpiryOriginalStatusAndLegacy(t *testing.T) {
	setupTestDB(t)
	if !common.DB.Migrator().HasTable(&model.TodoInboxState{}) {
		t.Fatal("TodoInboxState 未进入统一 AllModels/AutoMigrate 链路")
	}
	cleanTodoInboxTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	legacy := seedTodoAlert(t, 930001, "600000", model.AlertKindPrice, "", model.AlertEventUnread)
	seedTodoAlert(t, 930002, "000001", model.AlertKindPrice, today, model.AlertEventUnread)
	svc := todoTestService(false)

	res, err := svc.BuildInbox(context.Background(), 930001, TodoListOptions{Scope: TodoScopeAll})
	if err != nil || res.Total != 1 {
		t.Fatalf("旧提醒行应兼容进入投影: total=%d err=%v items=%+v", res.Total, err, res.Items)
	}
	child := findTodoChild(t, res, TodoKindAlert, legacy.ID)
	if child.SourceVersion == "" || child.DeepLink == "" {
		t.Fatalf("旧行也必须有来源版本和原业务深链: %+v", child)
	}
	ref := TodoSourceRef{SourceKind: child.SourceKind, SourceID: child.SourceID, SourceVersion: child.SourceVersion}
	if err := svc.ApplyInboxAction(930002, TodoActionRequest{Action: TodoActionSnooze, Items: []TodoSourceRef{ref}}); err == nil {
		t.Fatal("他人不得更新本用户来源状态")
	}
	if err := svc.ApplyInboxAction(930001, TodoActionRequest{Action: TodoActionSnooze, Items: []TodoSourceRef{ref}}); err != nil {
		t.Fatalf("稍后失败: %v", err)
	}
	snoozed, _ := svc.BuildInbox(context.Background(), 930001, TodoListOptions{Scope: TodoScopeAll})
	if snoozed.Total != 0 {
		t.Fatalf("稍后期间不应显示: %+v", snoozed.Items)
	}
	past := time.Now().Add(-time.Minute)
	if err := common.DB.Model(&model.TodoInboxState{}).
		Where("user_id = ? AND source_kind = ? AND source_id = ?", 930001, TodoKindAlert, legacy.ID).
		Update("snoozed_until", past).Error; err != nil {
		t.Fatal(err)
	}
	expired, _ := svc.BuildInbox(context.Background(), 930001, TodoListOptions{Scope: TodoScopeAll})
	if expired.Total != 1 {
		t.Fatalf("稍后到期后应重新出现: %+v", expired.Items)
	}
	child = findTodoChild(t, expired, TodoKindAlert, legacy.ID)
	if err := svc.ApplyInboxAction(930001, TodoActionRequest{Action: TodoActionRead, Items: []TodoSourceRef{{
		SourceKind: child.SourceKind, SourceID: child.SourceID, SourceVersion: child.SourceVersion,
	}}}); err != nil {
		t.Fatalf("收下提醒失败: %v", err)
	}
	var stored model.AlertEvent
	if err := common.DB.Where("id = ? AND user_id = ?", legacy.ID, 930001).First(&stored).Error; err != nil || stored.Status != model.AlertEventRead {
		t.Fatalf("必须联动 AlertEvent 原状态机: status=%s err=%v", stored.Status, err)
	}
	history, _ := svc.BuildInbox(context.Background(), 930001, TodoListOptions{
		Scope: TodoScopeAll, Status: TodoStatusCompleted, PageSize: 20,
	})
	if history.Total != 1 || history.Items[0].Status != TodoStatusCompleted {
		t.Fatalf("完成历史应保留原事实投影: %+v", history.Items)
	}
}

func TestTodoInboxSameDayMergeMuteVersionAndSeverityUpgrade(t *testing.T) {
	setupTestDB(t)
	cleanTodoInboxTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	first := seedTodoAlert(t, 930010, "600519", model.AlertKindPrice, today, model.AlertEventUnread)
	second := seedTodoAlert(t, 930010, "600519", model.AlertKindPrice, today, model.AlertEventUnread)
	svc := todoTestService(false)

	res, _ := svc.BuildInbox(context.Background(), 930010, TodoListOptions{Scope: TodoScopeAll})
	if res.Total != 1 || len(res.Items[0].Children) != 2 {
		t.Fatalf("同股同类同日应合并且保留全部子项: %+v", res.Items)
	}
	child := findTodoChild(t, res, TodoKindAlert, first.ID)
	if err := svc.ApplyInboxAction(930010, TodoActionRequest{Action: TodoActionMuteToday, Items: []TodoSourceRef{{
		SourceKind: child.SourceKind, SourceID: child.SourceID, SourceVersion: child.SourceVersion,
	}}}); err != nil {
		t.Fatalf("同日静默失败: %v", err)
	}
	muted, _ := svc.BuildInbox(context.Background(), 930010, TodoListOptions{Scope: TodoScopeAll})
	if muted.Total != 0 {
		t.Fatalf("同组其它子事件也应当日静默: %+v", muted.Items)
	}
	// 第二条业务事实出现新版本，不能被第一条的旧组静默永久吞掉。
	if err := common.DB.Model(&model.AlertEvent{}).Where("id = ?", second.ID).
		Updates(map[string]any{"context_version": 1, "updated_at": time.Now().Add(time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	versioned, _ := svc.BuildInbox(context.Background(), 930010, TodoListOptions{Scope: TodoScopeAll})
	if versioned.Total != 1 || len(versioned.Items[0].Children) != 1 || versioned.Items[0].Children[0].SourceID != second.ID {
		t.Fatalf("新版本必须重新出现，旧静默子项仍隐藏: %+v", versioned.Items)
	}

	// 严重度升级同样通过来源版本变化重新出现。
	cleanTodoInboxTables(t)
	position := seedHoldingWithPeak(t, 930010, "600000", "浦发银行", 10, 1000, 10, today)
	review := model.SellReview{UserID: 930010, PositionID: position.ID, Symbol: position.Symbol, Market: "cn",
		Name: position.Name, Trigger: model.SellReviewLift, TradeDate: today, Severity: model.SellReviewSeverityMed,
		Title: "解禁临近", Detail: "需复核", Status: model.SellReviewStatusOpen}
	if err := common.DB.Create(&review).Error; err != nil {
		t.Fatal(err)
	}
	svc = todoTestService(true)
	before, _ := svc.BuildInbox(context.Background(), 930010, TodoListOptions{Scope: TodoScopeAll})
	reviewChild := findTodoChild(t, before, TodoKindSellReview, review.ID)
	if err := svc.ApplyInboxAction(930010, TodoActionRequest{Action: TodoActionMuteToday, Items: []TodoSourceRef{{
		SourceKind: reviewChild.SourceKind, SourceID: reviewChild.SourceID, SourceVersion: reviewChild.SourceVersion,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.SellReview{}).Where("id = ?", review.ID).
		Updates(map[string]any{"severity": model.SellReviewSeverityHigh, "updated_at": time.Now().Add(time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	upgraded, _ := svc.BuildInbox(context.Background(), 930010, TodoListOptions{Scope: TodoScopeAll})
	if upgraded.Total == 0 || findTodoChild(t, upgraded, TodoKindSellReview, review.ID).Severity != model.SellReviewSeverityHigh {
		t.Fatalf("严重度升级不得被旧静默吞掉: %+v", upgraded.Items)
	}
}

func TestTodoInboxMuteRejectsExpandedGroupOverLimit(t *testing.T) {
	setupTestDB(t)
	cleanTodoInboxTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	for i := 0; i < 101; i++ {
		seedTodoAlert(t, 930011, "600519", model.AlertKindPrice, today, model.AlertEventUnread)
	}
	svc := todoTestService(false)
	res, err := svc.BuildInbox(context.Background(), 930011, TodoListOptions{Scope: TodoScopeAll})
	if err != nil || len(res.Items) != 1 || len(res.Items[0].Children) != 101 {
		t.Fatalf("同组测试数据构造失败: items=%d err=%v", len(res.Items), err)
	}
	child := res.Items[0].Children[0]
	err = svc.ApplyInboxAction(930011, TodoActionRequest{Action: TodoActionMuteToday, Items: []TodoSourceRef{{
		SourceKind: child.SourceKind, SourceID: child.SourceID, SourceVersion: child.SourceVersion,
	}}})
	if err == nil || !strings.Contains(err.Error(), "超过 100 条") {
		t.Fatalf("静音扩展后的数量上限必须 fail-closed: %v", err)
	}
	var count int64
	if dbErr := common.DB.Model(&model.TodoInboxState{}).Where("user_id = ?", 930011).Count(&count).Error; dbErr != nil || count != 0 {
		t.Fatalf("超限拒绝前不得产生部分状态: count=%d err=%v", count, dbErr)
	}
}

func TestTodoInboxCompletedPositionAlertStaysInLedgerScope(t *testing.T) {
	setupTestDB(t)
	cleanTodoInboxTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	seedTodoAlert(t, 930012, "600000", model.AlertKindCostDrawdown, today, model.AlertEventRead)
	svc := todoTestService(false)
	res, err := svc.BuildInbox(context.Background(), 930012, TodoListOptions{
		Scope: TodoScopeLedger, Status: TodoStatusCompleted, PageSize: 20, HistoryDays: 30,
	})
	if err != nil || len(res.Items) != 1 || res.Items[0].Scope != TodoScopeLedger {
		t.Fatalf("持仓提醒完成后仍应归入我的账本: items=%+v err=%v", res.Items, err)
	}
}

func TestTodoInboxPartialTaskRedactionAndCompletedPagination(t *testing.T) {
	setupTestDB(t)
	cleanTodoInboxTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	seedTodoAlert(t, 930020, "600000", model.AlertKindPrice, today, model.AlertEventUnread)
	notice := model.JobFailureNotification{JobRunID: 987654, UserID: 930020, Kind: JobKindAnalysis,
		ErrorCode: "authorization=Bearer sk-secret", ErrorSummary: "Bearer sk-secret 完整请求模型正文",
		Status: model.JobFailureNoticeAttempted}
	if err := common.DB.Create(&notice).Error; err != nil {
		t.Fatal(err)
	}
	merged := model.JobFailureNotification{JobRunID: 987655, UserID: 930020, Kind: JobKindAnalysis,
		ErrorCode: "failed", ErrorSummary: "另一个原始错误", MergeRootID: &notice.ID,
		Status: model.JobFailureNoticeMerged}
	if err := common.DB.Create(&merged).Error; err != nil {
		t.Fatal(err)
	}
	svc := todoTestService(false)
	res, err := svc.BuildInbox(context.Background(), 930020, TodoListOptions{Scope: TodoScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	jobChild := findTodoChild(t, res, TodoKindJobFailure, notice.ID)
	if mergedChild := findTodoChild(t, res, TodoKindJobFailure, merged.ID); mergedChild.DeepLink == jobChild.DeepLink {
		t.Fatalf("合并展示必须保留每个失败任务的独立来源深链: root=%+v merged=%+v", jobChild, mergedChild)
	}
	lower := strings.ToLower(jobChild.Detail)
	for _, secret := range []string{"bearer", "sk-secret", "完整请求", "模型正文", "authorization"} {
		if strings.Contains(lower, strings.ToLower(secret)) {
			t.Fatalf("任务失败投影泄露敏感字段 %q: %+v", secret, jobChild)
		}
	}
	if err := common.DB.Migrator().DropTable(&model.JobFailureNotification{}); err != nil {
		t.Fatal(err)
	}
	partial, buildErr := svc.BuildInbox(context.Background(), 930020, TodoListOptions{Scope: TodoScopeAll})
	if buildErr != nil || !partial.Partial || partial.Complete || partial.Total == 0 {
		t.Fatalf("单一来源失败应保留其它来源并明确 partial: total=%d complete=%v partial=%v err=%v errors=%v",
			partial.Total, partial.Complete, partial.Partial, buildErr, partial.Errors)
	}
	if err := common.DB.AutoMigrate(&model.JobFailureNotification{}); err != nil {
		t.Fatal(err)
	}

	cleanTodoInboxTables(t)
	for i, symbol := range []string{"600001", "600002", "600003"} {
		row := seedTodoAlert(t, 930020, symbol, model.AlertKindPrice, today, model.AlertEventRead)
		if err := common.DB.Model(&model.AlertEvent{}).Where("id = ?", row.ID).
			Update("updated_at", time.Now().Add(time.Duration(i)*time.Second)).Error; err != nil {
			t.Fatal(err)
		}
	}
	page1, _ := svc.BuildInbox(context.Background(), 930020, TodoListOptions{
		Scope: TodoScopeAll, Status: TodoStatusCompleted, Page: 1, PageSize: 2, HistoryDays: 30,
	})
	page2, _ := svc.BuildInbox(context.Background(), 930020, TodoListOptions{
		Scope: TodoScopeAll, Status: TodoStatusCompleted, Page: 2, PageSize: 2, HistoryDays: 30,
	})
	if page1.MatchedTotal != 3 || len(page1.Items) != 2 || !page1.HasMore || len(page2.Items) != 1 || page2.HasMore {
		t.Fatalf("完成历史必须有界分页: p1=%+v p2=%+v", page1, page2)
	}
}
