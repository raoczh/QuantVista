package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func resetTaskCenterTestDB(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	for _, table := range []string{
		"analysis_records", "recommendation_batches", "daily_reports", "llm_tasks", "data_sync_logs",
	} {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理 %s 失败: %v", table, err)
		}
	}
}

func createTaskCenterRow(t *testing.T, value any) {
	t.Helper()
	if err := common.DB.Create(value).Error; err != nil {
		t.Fatalf("创建任务测试数据失败: %v", err)
	}
}

func taskBySource(items []TaskCenterItem, source string) *TaskCenterItem {
	for i := range items {
		if items[i].Source == source {
			return &items[i]
		}
	}
	return nil
}

func TestTaskCenterUserIsolationAdminSystemAndStatusMapping(t *testing.T) {
	resetTaskCenterTestDB(t)
	base := time.Date(2026, 8, 5, 10, 0, 0, 0, time.Local)
	analysis := &model.AnalysisRecord{
		UserID: 1, Module: model.AnalysisModuleStock, Symbol: "600000", Target: "浦发银行",
		Title: "个股分析 · 浦发银行", Status: model.AnalysisStatusSuccess,
		Provider: "openai", Model: "gpt-test", PromptTokens: 11, CompletionTokens: 7,
		TotalTokens: 18, LatencyMs: 1234, TraceID: "trace-analysis",
		CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute),
	}
	recommendation := &model.RecommendationBatch{
		UserID: 1, Type: model.RecTypeShortTerm, Market: "cn", Title: "短线推荐",
		Status: model.RecStatusDegraded, Error: "量化降级", CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute),
	}
	report := &model.DailyReport{
		UserID: 1, TradeDate: "2026-08-05", Market: "cn", Status: model.ReportStatusPartial,
		Error: "推荐分支失败", CreatedAt: base.Add(3 * time.Minute), UpdatedAt: base.Add(3 * time.Minute),
	}
	llmTask := &model.LLMTask{
		UserID: 1, Kind: "qa", RequestHash: "owner-hash", Status: model.LLMTaskStatusFailed,
		Error: "调用失败", ErrorCode: AsyncLLMTaskErrorFailed,
		CreatedAt: base.Add(4 * time.Minute), UpdatedAt: base.Add(4 * time.Minute),
	}
	for _, row := range []any{analysis, recommendation, report, llmTask} {
		createTaskCenterRow(t, row)
	}
	createTaskCenterRow(t, &model.AnalysisRecord{
		UserID: 2, Module: model.AnalysisModuleMarket, Title: "其他用户任务", Status: model.AnalysisStatusSuccess,
		CreatedAt: base.Add(20 * time.Minute), UpdatedAt: base.Add(20 * time.Minute),
	})
	syncLog := &model.DataSyncLog{
		Task: "sync_daily_bars", Market: "cn", Status: "partial", Total: 10, Succeeded: 8, Failed: 2,
		DurationMs: 4321, Message: "2 只同步失败", CreatedAt: base.Add(5 * time.Minute),
	}
	createTaskCenterRow(t, syncLog)

	svc := NewTaskCenterService()
	userItems, err := svc.List(1, model.RoleUser, TaskCenterListOptions{IncludeSystem: true})
	if err != nil {
		t.Fatalf("查询普通用户任务失败: %v", err)
	}
	if len(userItems) != 4 {
		t.Fatalf("普通用户只能看到自己的四类任务，got=%d items=%+v", len(userItems), userItems)
	}
	for _, item := range userItems {
		if item.Source == TaskSourceDataSync || item.Title == "其他用户任务" {
			t.Fatalf("普通用户泄露系统或其他用户任务: %+v", item)
		}
		if item.ID != fmt.Sprintf("%s:%d", item.Source, item.SourceID) {
			t.Fatalf("复合 ID 不稳定: %+v", item)
		}
	}
	if got := taskBySource(userItems, TaskSourceAnalysis); got == nil || got.Provider != "openai" ||
		got.TotalTokens != 18 || got.TraceID != "trace-analysis" || got.Stage != "finished" {
		t.Fatalf("分析任务元数据映射不完整: %+v", got)
	}
	if got := taskBySource(userItems, TaskSourceRecommendation); got == nil ||
		got.Status != TaskStatusDegraded || got.RawStatus != model.RecStatusDegraded {
		t.Fatalf("推荐 degraded 状态映射错误: %+v", got)
	}
	if got := taskBySource(userItems, TaskSourceDailyReport); got == nil ||
		got.Status != TaskStatusDegraded || got.RawStatus != model.ReportStatusPartial {
		t.Fatalf("日报 partial 应规范为 degraded 并保留 raw_status: %+v", got)
	}
	degradedItems, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Status: TaskStatusDegraded})
	if err != nil || len(degradedItems) != 2 {
		t.Fatalf("degraded 筛选应同时包含 raw degraded 与 partial: items=%+v err=%v", degradedItems, err)
	}
	for _, item := range degradedItems {
		if item.Status != TaskStatusDegraded {
			t.Fatalf("degraded 筛选返回了非规范降级状态: %+v", item)
		}
	}

	withoutSystem, err := svc.List(1, model.RoleAdmin, TaskCenterListOptions{})
	if err != nil || len(withoutSystem) != 4 {
		t.Fatalf("管理员未显式请求时不应附带系统任务: len=%d err=%v", len(withoutSystem), err)
	}
	adminItems, err := svc.List(1, model.RoleAdmin, TaskCenterListOptions{IncludeSystem: true})
	if err != nil {
		t.Fatalf("管理员查询系统任务失败: %v", err)
	}
	if len(adminItems) != 5 || adminItems[0].Source != TaskSourceDataSync {
		t.Fatalf("管理员显式请求应附带最新系统任务: %+v", adminItems)
	}
	system := taskBySource(adminItems, TaskSourceDataSync)
	if system == nil || system.Status != TaskStatusDegraded || system.RawStatus != "partial" ||
		system.Total != 10 || system.Succeeded != 8 || system.Failed != 2 || system.LatencyMs != 4321 {
		t.Fatalf("系统任务映射错误: %+v", system)
	}
	blocked, err := svc.List(1, model.RoleUser, TaskCenterListOptions{
		Source: TaskSourceDataSync, IncludeSystem: true,
	})
	if err != nil || len(blocked) != 0 {
		t.Fatalf("普通用户定向请求 data_sync 也必须返回空列表: items=%+v err=%v", blocked, err)
	}
}

func TestTaskCenterFiltersStableSortAndLimit(t *testing.T) {
	resetTaskCenterTestDB(t)
	base := time.Now().Truncate(time.Second)
	a1 := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleStock, Title: "个股一", Status: model.AnalysisStatusSuccess, CreatedAt: base, UpdatedAt: base}
	a2 := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleStock, Title: "个股二", Status: model.AnalysisStatusSuccess, CreatedAt: base, UpdatedAt: base}
	marketFailed := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleMarket, Title: "市场失败", Status: model.AnalysisStatusFailed, CreatedAt: base.Add(-time.Minute), UpdatedAt: base.Add(-time.Minute)}
	rec := &model.RecommendationBatch{UserID: 1, Type: model.RecTypeLongTerm, Title: "长线", Status: model.RecStatusSuccess, CreatedAt: base, UpdatedAt: base}
	report := &model.DailyReport{UserID: 1, TradeDate: "2026-08-04", Status: model.ReportStatusSuccess, CreatedAt: base.Add(-2 * time.Minute), UpdatedAt: base.Add(-2 * time.Minute)}
	qa := &model.LLMTask{UserID: 1, Kind: "qa", RequestHash: "qa-hash", Status: model.LLMTaskStatusProcessing, CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)}
	for _, row := range []any{a1, a2, marketFailed, rec, report, qa} {
		createTaskCenterRow(t, row)
	}

	svc := NewTaskCenterService()
	items, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{
		taskCompositeID(TaskSourceLLM, qa.ID),
		taskCompositeID(TaskSourceAnalysis, a2.ID),
		taskCompositeID(TaskSourceAnalysis, a1.ID),
		taskCompositeID(TaskSourceRecommendation, rec.ID),
	}
	if len(items) != len(wantIDs) {
		t.Fatalf("limit 未在全局排序后生效: %+v", items)
	}
	for i := range wantIDs {
		if items[i].ID != wantIDs[i] {
			t.Fatalf("稳定排序错误 at=%d got=%s want=%s all=%+v", i, items[i].ID, wantIDs[i], items)
		}
	}

	stockItems, err := svc.List(1, model.RoleUser, TaskCenterListOptions{
		Source: TaskSourceAnalysis, Kind: model.AnalysisModuleStock, Status: TaskStatusSuccess,
	})
	if err != nil || len(stockItems) != 2 {
		t.Fatalf("source/kind/status 联合筛选错误: items=%+v err=%v", stockItems, err)
	}
	qaItems, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Kind: "qa", Status: TaskStatusProcessing})
	if err != nil || len(qaItems) != 1 || qaItems[0].Source != TaskSourceLLM || qaItems[0].Stage != "processing" {
		t.Fatalf("跨来源 kind/status 筛选错误: items=%+v err=%v", qaItems, err)
	}
	wrongDailyKind, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Source: TaskSourceDailyReport, Kind: "qa"})
	if err != nil || len(wrongDailyKind) != 0 {
		t.Fatalf("日报固定 kind 筛选错误: items=%+v err=%v", wrongDailyKind, err)
	}
	failedItems, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Status: TaskStatusFailed})
	if err != nil || len(failedItems) != 1 || failedItems[0].SourceID != marketFailed.ID {
		t.Fatalf("failed 筛选错误: items=%+v err=%v", failedItems, err)
	}
	degradedItems, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Status: TaskStatusDegraded})
	if err != nil || len(degradedItems) != 0 {
		t.Fatalf("degraded 筛选不应混入其他规范状态: items=%+v err=%v", degradedItems, err)
	}
	if _, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Source: "unknown"}); err == nil {
		t.Fatal("非法 source 应报错")
	}
	if _, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Status: "partial"}); err == nil {
		t.Fatal("筛选状态只接受规范状态，raw partial 应拒绝")
	}
}

func TestTaskCenterDefaultAndMaximumLimit(t *testing.T) {
	resetTaskCenterTestDB(t)
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.Local)
	rows := make([]model.LLMTask, 105)
	for i := range rows {
		rows[i] = model.LLMTask{
			UserID: 1, Kind: "qa", RequestHash: fmt.Sprintf("hash-%03d", i), Status: model.LLMTaskStatusSuccess,
			CreatedAt: base.Add(time.Duration(i) * time.Second), UpdatedAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	if err := common.DB.CreateInBatches(&rows, 25).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewTaskCenterService()
	defaults, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Source: TaskSourceLLM})
	if err != nil || len(defaults) != defaultTaskCenterLimit {
		t.Fatalf("默认 limit 应为 %d: len=%d err=%v", defaultTaskCenterLimit, len(defaults), err)
	}
	capped, err := svc.List(1, model.RoleUser, TaskCenterListOptions{Source: TaskSourceLLM, Limit: 1000})
	if err != nil || len(capped) != maxTaskCenterLimit {
		t.Fatalf("limit 应钳制到 %d: len=%d err=%v", maxTaskCenterLimit, len(capped), err)
	}
	if capped[0].SourceID != rows[len(rows)-1].ID {
		t.Fatalf("limit 前必须先倒序: first=%+v", capped[0])
	}
}

func TestTaskCenterConvergesStaleTasksForCurrentUser(t *testing.T) {
	resetTaskCenterTestDB(t)
	if taskProcessingStaleAfter != 15*time.Minute || analysisProcessingStale != taskProcessingStaleAfter {
		t.Fatalf("stale 口径必须唯一为 15 分钟: shared=%s analysis_alias=%s", taskProcessingStaleAfter, analysisProcessingStale)
	}
	now := time.Now()
	old := now.Add(-taskProcessingStaleAfter - time.Minute)
	fresh := now.Add(-taskProcessingStaleAfter + time.Minute)
	oldAnalysis := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleStock, Status: model.AnalysisStatusProcessing, CreatedAt: old, UpdatedAt: old}
	freshAnalysis := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleMarket, Status: model.AnalysisStatusProcessing, CreatedAt: fresh, UpdatedAt: fresh}
	oldRec := &model.RecommendationBatch{UserID: 1, Type: model.RecTypeShortTerm, Status: model.RecStatusProcessing, CreatedAt: old, UpdatedAt: old}
	freshRec := &model.RecommendationBatch{UserID: 1, Type: model.RecTypeLongTerm, Status: model.RecStatusProcessing, CreatedAt: fresh, UpdatedAt: fresh}
	oldReport := &model.DailyReport{UserID: 1, TradeDate: "2026-08-01", Status: model.ReportStatusProcessing, CreatedAt: old, UpdatedAt: old}
	freshReport := &model.DailyReport{UserID: 1, TradeDate: "2026-08-02", Status: model.ReportStatusProcessing, CreatedAt: fresh, UpdatedAt: fresh}
	oldLLM := &model.LLMTask{UserID: 1, Kind: "qa", RequestHash: "old", Status: model.LLMTaskStatusProcessing, CreatedAt: old, UpdatedAt: old}
	freshLLM := &model.LLMTask{UserID: 1, Kind: "compare", RequestHash: "fresh", Status: model.LLMTaskStatusProcessing, CreatedAt: fresh, UpdatedAt: fresh}
	otherUser := &model.AnalysisRecord{UserID: 2, Module: model.AnalysisModuleStock, Status: model.AnalysisStatusProcessing, CreatedAt: old, UpdatedAt: old}
	for _, row := range []any{oldAnalysis, freshAnalysis, oldRec, freshRec, oldReport, freshReport, oldLLM, freshLLM, otherUser} {
		createTaskCenterRow(t, row)
	}

	items, err := NewTaskCenterService().List(1, model.RoleUser, TaskCenterListOptions{})
	if err != nil {
		t.Fatalf("stale 收敛查询失败: %v", err)
	}
	oldIDs := map[string]bool{
		taskCompositeID(TaskSourceAnalysis, oldAnalysis.ID):  true,
		taskCompositeID(TaskSourceRecommendation, oldRec.ID): true,
		taskCompositeID(TaskSourceDailyReport, oldReport.ID): true,
		taskCompositeID(TaskSourceLLM, oldLLM.ID):            true,
	}
	freshIDs := map[string]bool{
		taskCompositeID(TaskSourceAnalysis, freshAnalysis.ID):  true,
		taskCompositeID(TaskSourceRecommendation, freshRec.ID): true,
		taskCompositeID(TaskSourceDailyReport, freshReport.ID): true,
		taskCompositeID(TaskSourceLLM, freshLLM.ID):            true,
	}
	seenOld := make(map[string]bool, len(oldIDs))
	seenFresh := make(map[string]bool, len(freshIDs))
	for _, item := range items {
		if oldIDs[item.ID] {
			seenOld[item.ID] = true
			if item.Status != TaskStatusFailed || item.Stage != "finished" ||
				item.ErrorCode != AsyncLLMTaskErrorStale {
				t.Fatalf("遗留任务未统一收敛: %+v", item)
			}
		}
		if freshIDs[item.ID] {
			seenFresh[item.ID] = true
			if item.Status != TaskStatusProcessing || item.Stage != "processing" {
				t.Fatalf("新鲜 processing 不应被误判: %+v", item)
			}
		}
	}
	if len(seenOld) != len(oldIDs) || len(seenFresh) != len(freshIDs) {
		t.Fatalf("四类 stale/fresh 任务必须全部进入统一列表: seen_old=%v seen_fresh=%v items=%+v", seenOld, seenFresh, items)
	}
	var other model.AnalysisRecord
	if err := common.DB.First(&other, otherUser.ID).Error; err != nil || other.Status != model.AnalysisStatusProcessing {
		t.Fatalf("清理不得影响其他用户: row=%+v err=%v", other, err)
	}
	var persistedAnalysis model.AnalysisRecord
	if err := common.DB.First(&persistedAnalysis, oldAnalysis.ID).Error; err != nil ||
		persistedAnalysis.Status != model.AnalysisStatusFailed || persistedAnalysis.ErrorCode != AsyncLLMTaskErrorStale {
		t.Fatalf("分析 stale 终态未落库: row=%+v err=%v", persistedAnalysis, err)
	}
	var persistedRec model.RecommendationBatch
	if err := common.DB.First(&persistedRec, oldRec.ID).Error; err != nil ||
		persistedRec.Status != model.RecStatusFailed || !strings.Contains(persistedRec.Error, "服务重启或执行超时") {
		t.Fatalf("推荐 stale 终态未落库: row=%+v err=%v", persistedRec, err)
	}
	var persistedReport model.DailyReport
	if err := common.DB.First(&persistedReport, oldReport.ID).Error; err != nil ||
		persistedReport.Status != model.ReportStatusFailed || !strings.Contains(persistedReport.Error, "服务重启或执行超时") {
		t.Fatalf("日报 stale 终态未落库: row=%+v err=%v", persistedReport, err)
	}
	var persistedLLM model.LLMTask
	if err := common.DB.First(&persistedLLM, oldLLM.ID).Error; err != nil ||
		persistedLLM.Status != model.LLMTaskStatusFailed || persistedLLM.ErrorCode != AsyncLLMTaskErrorStale {
		t.Fatalf("通用任务 stale 终态未落库: row=%+v err=%v", persistedLLM, err)
	}
}

type taskSQLRecorder struct {
	logger.Interface
	mu      sync.Mutex
	selects []string
}

func (r *taskSQLRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
		r.mu.Lock()
		r.selects = append(r.selects, sql)
		r.mu.Unlock()
	}
}

func TestTaskCenterUsesBoundedLightweightQueries(t *testing.T) {
	resetTaskCenterTestDB(t)
	now := time.Now()
	heavy := "HEAVY_BODY_SENTINEL"
	createTaskCenterRow(t, &model.AnalysisRecord{
		UserID: 1, Module: model.AnalysisModuleStock, Status: model.AnalysisStatusSuccess,
		ResultJSON: heavy, DataSnapshot: heavy, LlmRunJSON: heavy, CreatedAt: now, UpdatedAt: now,
	})
	createTaskCenterRow(t, &model.RecommendationBatch{
		UserID: 1, Type: model.RecTypeShortTerm, Status: model.RecStatusSuccess,
		CandidatePool: heavy, DataSnapshot: heavy, RejectedJSON: heavy, FiltersJSON: heavy,
		ReviewJSON: heavy, RegimeJSON: heavy, ReflectionJSON: heavy, LlmRunJSON: heavy,
		CreatedAt: now, UpdatedAt: now,
	})
	createTaskCenterRow(t, &model.DailyReport{
		UserID: 1, TradeDate: "2026-08-05", Status: model.ReportStatusSuccess,
		ReviewJSON: heavy, SnapshotJSON: heavy, LlmRunJSON: heavy, CreatedAt: now, UpdatedAt: now,
	})
	createTaskCenterRow(t, &model.LLMTask{
		UserID: 1, Kind: "qa", RequestHash: "secret-request-hash", ResultJSON: heavy,
		Status: model.LLMTaskStatusSuccess, CreatedAt: now, UpdatedAt: now,
	})
	createTaskCenterRow(t, &model.DataSyncLog{Task: "sync_daily_bars", Status: "success", CreatedAt: now})

	recorder := &taskSQLRecorder{Interface: logger.Discard}
	originalDB := common.DB
	common.DB = common.DB.Session(&gorm.Session{Logger: recorder})
	t.Cleanup(func() { common.DB = originalDB })
	items, err := NewTaskCenterService().List(1, model.RoleAdmin, TaskCenterListOptions{IncludeSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 {
		t.Fatalf("应聚合五类任务: %+v", items)
	}
	encoded, err := json.Marshal(items)
	if err != nil || strings.Contains(string(encoded), heavy) || strings.Contains(string(encoded), "secret-request-hash") {
		t.Fatalf("任务 DTO 泄露正文或请求哈希: json=%s err=%v", encoded, err)
	}
	recorder.mu.Lock()
	selects := append([]string(nil), recorder.selects...)
	recorder.mu.Unlock()
	if len(selects) != 5 {
		t.Fatalf("聚合应固定为每来源一次 SELECT、不得 N+1，got=%d sql=%v", len(selects), selects)
	}
	forbidden := []string{
		"result_json", "data_snapshot", "candidate_pool", "rejected_json", "filters_json",
		"review_json", "snapshot_json", "regime_json", "reflection_json", "llm_run_json",
		"request_hash", "active_key",
	}
	for _, sql := range selects {
		lower := strings.ToLower(sql)
		for _, column := range forbidden {
			if strings.Contains(lower, column) {
				t.Fatalf("任务列表禁止读取大字段 %s: %s", column, sql)
			}
		}
	}
}
