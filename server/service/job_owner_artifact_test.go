package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func createJobTestUser(t *testing.T, id int64, role string) {
	t.Helper()
	user := &model.User{ID: id, Username: "job-owner-" + role + "-" + time.Now().Format("150405.000000000"), Role: role, Status: model.StatusEnabled}
	if err := common.DB.Create(user).Error; err != nil {
		t.Fatal(err)
	}
}

func resetJobOwnerArtifactTestDB(t *testing.T) {
	t.Helper()
	resetDurableJobs(t)
	for _, table := range []string{"research_artifacts", "data_sync_logs", "analysis_records", "recommendations", "recommendation_batches", "daily_reports"} {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理 %s 失败: %v", table, err)
		}
	}
	_ = common.DB.Where("username LIKE ?", "job-owner-%").Delete(&model.User{}).Error
	t.Cleanup(func() {
		for _, table := range []string{"research_artifacts", "data_sync_logs", "analysis_records", "recommendations", "recommendation_batches", "daily_reports", "job_events", "job_steps", "llm_tasks", "job_runs"} {
			_ = common.DB.Exec("DELETE FROM " + table).Error
		}
		_ = common.DB.Where("username LIKE ?", "job-owner-%").Delete(&model.User{}).Error
	})
}

func TestJobOwnerContractRejectsZeroUserAndStoresSystemNull(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	now := time.Now()
	invalid := &model.JobRun{UserID: 0, Kind: JobKindQA, RequestHash: strings.Repeat("a", 64),
		Status: model.JobStatusQueued, SnapshotVersion: 1, RequestSnapshot: `{}`, QueuedAt: now}
	if err := common.DB.Create(invalid).Error; err == nil {
		t.Fatal("user_id=0 不得作为用户 owner 落库")
	}
	actor := int64(9001)
	system := &model.JobRun{OwnerType: model.JobOwnerSystem, TriggeredBy: &actor,
		Kind: JobKindSnapshotMarket, RequestHash: strings.Repeat("b", 64), Status: model.JobStatusQueued,
		SnapshotVersion: 1, RequestSnapshot: `{}`, QueuedAt: now}
	if err := common.DB.Create(system).Error; err != nil {
		t.Fatal(err)
	}
	var stored sql.NullInt64
	if err := common.DB.Raw("SELECT user_id FROM job_runs WHERE id = ?", system.ID).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("系统作业 user_id 必须为 NULL，实际=%d", stored.Int64)
	}
}

func TestSystemJobAuthorizationAndTriggeredBy(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	createJobTestUser(t, 9101, model.RoleUser)
	createJobTestUser(t, 9102, model.RoleAdmin)
	now := time.Now()
	actor := int64(9102)
	run := &model.JobRun{OwnerType: model.JobOwnerSystem, TriggeredBy: &actor,
		Kind: JobKindInitMarketHistory, RequestHash: strings.Repeat("c", 64), Status: model.JobStatusRunning,
		SnapshotVersion: 1, RequestSnapshot: `{}`, QueuedAt: now, StartedAt: &now}
	if err := common.DB.Create(run).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := GetJobRun(9101, run.ID, true); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("普通用户不得读取系统作业: %v", err)
	}
	if _, err := CancelJobRun(9101, run.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("普通用户不得取消系统作业: %v", err)
	}
	view, err := GetJobRun(9102, run.ID, true)
	if err != nil || view.Owner != model.JobOwnerSystem || view.TriggeredBy == nil || *view.TriggeredBy != actor {
		t.Fatalf("管理员应可审计系统作业: view=%+v err=%v", view, err)
	}
	view, err = CancelJobRun(9102, run.ID)
	if err != nil || !view.CancelRequested {
		t.Fatalf("管理员应可请求取消系统作业: view=%+v err=%v", view, err)
	}
}

func TestDataSyncJobResultAndRunConvergeAtomically(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	handler := func(ctx context.Context, _ int64, _ bool, _ json.RawMessage) (DurableJobResult, error) {
		if err := JobStepTransition(ctx, JobKindSnapshotMarket); err != nil {
			return DurableJobResult{}, err
		}
		log := &model.DataSyncLog{Task: JobKindSnapshotMarket, Market: "cn", Status: "partial",
			Total: 3, Succeeded: 2, Failed: 1, Message: "一项失败", TriggerSource: "scheduler"}
		return DurableJobResult{Value: log, Status: model.JobStatusDegraded, Total: 3, Succeeded: 2, Failed: 1}, nil
	}
	runtime.registerWithBinding(JobKindSnapshotMarket, time.Minute, handler, dataSyncJobBinding(), false)
	req := DataSyncJobRequest{Version: 1, Task: JobKindSnapshotMarket, Market: "cn", TriggerSource: "scheduler"}
	run, err := runtime.startSystemWithBinding(nil, JobKindSnapshotMarket, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := common.DB.First(run, run.ID).Error; err == nil && run.Status == model.JobStatusDegraded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.Status != model.JobStatusDegraded || run.ResultID == nil {
		t.Fatalf("JobRun 未收敛为 degraded: %+v", run)
	}
	var log model.DataSyncLog
	if err := common.DB.First(&log, *run.ResultID).Error; err != nil {
		t.Fatal(err)
	}
	if log.JobRunID == nil || *log.JobRunID != run.ID || log.Status != "partial" || log.Succeeded != 2 || log.Failed != 1 {
		t.Fatalf("DataSyncLog 与 JobRun 不一致: %+v", log)
	}
	var processing int64
	common.DB.Model(&model.DataSyncLog{}).Where("status = ?", "processing").Count(&processing)
	if processing != 0 {
		t.Fatalf("不得遗留 processing 日志: %d", processing)
	}

	failureHandler := func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		log := &model.DataSyncLog{Task: JobKindFactorRebuild, Market: "cn", Status: "failed",
			Total: 4, Succeeded: 1, Failed: 3, Message: "批次失败", TriggerSource: "scheduler"}
		return DurableJobResult{Value: log, Status: model.JobStatusFailed, Total: 4, Succeeded: 1, Failed: 3}, errors.New("批次失败")
	}
	runtime.registerWithBinding(JobKindFactorRebuild, time.Minute, failureHandler, dataSyncJobBinding(), false)
	failedRun, err := runtime.startSystemWithBinding(nil, JobKindFactorRebuild,
		DataSyncJobRequest{Version: 1, Task: JobKindFactorRebuild, Market: "cn", TriggerSource: "scheduler"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := common.DB.First(failedRun, failedRun.ID).Error; err == nil && failedRun.Status == model.JobStatusFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var failedLog model.DataSyncLog
	if failedRun.Status != model.JobStatusFailed || failedRun.ResultID == nil || failedRun.Succeeded != 1 || failedRun.Failed != 3 ||
		common.DB.First(&failedLog, *failedRun.ResultID).Error != nil ||
		failedLog.Status != "failed" || failedLog.Succeeded != 1 || failedLog.Failed != 3 {
		t.Fatalf("失败作业与逐项结果必须一致: run=%+v log=%+v", failedRun, failedLog)
	}
}

func TestDataSyncCancellationPreservesPartialCounts(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	createJobTestUser(t, 9150, model.RoleAdmin)
	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	started := make(chan struct{})
	handler := func(ctx context.Context, _ int64, _ bool, _ json.RawMessage) (DurableJobResult, error) {
		close(started)
		<-ctx.Done()
		log := &model.DataSyncLog{Task: JobKindSyncDailyBars, Market: "cn", Status: "partial",
			Total: 5, Succeeded: 2, Failed: 0, Message: "已完成两项", TriggerSource: "admin"}
		return DurableJobResult{Value: log, Status: model.JobStatusDegraded, Total: 5, Succeeded: 2}, ctx.Err()
	}
	runtime.registerWithBinding(JobKindSyncDailyBars, time.Minute, handler, dataSyncJobBinding(), false)
	actor := int64(9150)
	run, err := runtime.startSystemWithBinding(&actor, JobKindSyncDailyBars,
		DataSyncJobRequest{Version: 1, Task: JobKindSyncDailyBars, Market: "cn", TriggerSource: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := CancelJobRun(actor, run.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := common.DB.First(run, run.ID).Error; err == nil && run.Status == model.JobStatusCanceled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.Status != model.JobStatusCanceled || run.ResultID == nil || run.Succeeded != 2 || run.Total != 5 {
		t.Fatalf("取消后的 JobRun 未保留部分结果: %+v", run)
	}
	var log model.DataSyncLog
	if err := common.DB.First(&log, *run.ResultID).Error; err != nil {
		t.Fatal(err)
	}
	if log.Status != model.JobStatusCanceled || log.Total != 5 || log.Succeeded != 2 || log.Failed != 0 {
		t.Fatalf("取消后的 DataSyncLog 未保留部分结果: %+v", log)
	}
}

func TestDataSyncEarlyFailureClosesPlaceholder(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	runtime := newJobRuntime(1, 1)
	defer runtime.close()
	handler := func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		return DurableJobResult{}, errors.New("计划在执行前失效")
	}
	runtime.registerWithBinding(JobKindBackfillCalendar, time.Minute, handler, dataSyncJobBinding(), false)
	run, err := runtime.startSystemWithBinding(nil, JobKindBackfillCalendar, DataSyncJobRequest{
		Version: 1, Task: JobKindBackfillCalendar, Market: "cn", TriggerSource: "scheduler",
		ParameterSummary: "lookback=60",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := common.DB.First(run, run.ID).Error; err == nil && run.Status == model.JobStatusFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.Status != model.JobStatusFailed || run.ResultID == nil || run.Failed != 1 {
		t.Fatalf("早期失败 JobRun 未收敛: %+v", run)
	}
	var log model.DataSyncLog
	if err := common.DB.First(&log, *run.ResultID).Error; err != nil {
		t.Fatal(err)
	}
	if log.Status != model.JobStatusFailed || log.Task != JobKindBackfillCalendar || log.Market != "cn" ||
		log.TriggerSource != "scheduler" || log.ParameterSummary != "lookback=60" || log.Failed != 1 {
		t.Fatalf("早期失败覆盖了占位日志元数据: %+v", log)
	}
}

func TestSystemSchedulerRetriesQueueBackpressureWithoutOrphans(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	runtime := newJobRuntime(1, 1)
	defer runtime.close()
	started := make(chan struct{})
	release := make(chan struct{})
	handler := func(_ context.Context, _ int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
		var req DataSyncJobRequest
		_ = json.Unmarshal(raw, &req)
		if req.Task == JobKindSnapshotMarket {
			close(started)
			<-release
		}
		log := &model.DataSyncLog{Task: req.Task, Market: "cn", Status: "success", Total: 1, Succeeded: 1, TriggerSource: "scheduler"}
		return DurableJobResult{Value: log, Status: model.JobStatusSuccess, Total: 1, Succeeded: 1}, nil
	}
	for _, kind := range []string{JobKindSnapshotMarket, JobKindFactorRebuild} {
		runtime.registerWithBinding(kind, time.Minute, handler, dataSyncJobBinding(), false)
	}
	first, err := runtime.startSystemWithBinding(nil, JobKindSnapshotMarket,
		DataSyncJobRequest{Version: 1, Task: JobKindSnapshotMarket, Market: "cn", TriggerSource: "scheduler"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	actor := int64(9151)
	existing, created, err := runtime.startSystemWithBindingStatus(&actor, JobKindSnapshotMarket,
		DataSyncJobRequest{Version: 1, Task: JobKindSnapshotMarket, Market: "cn", TriggerSource: "admin"}, nil)
	if err != nil || created || existing == nil || existing.ID != first.ID {
		t.Fatalf("同 kind 在途系统作业必须返回 started=false 与原 run: existing=%+v created=%v err=%v", existing, created, err)
	}
	originalDelay := systemScheduleRetryDelay
	systemScheduleRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { systemScheduleRetryDelay = originalDelay })
	scheduleSystemDataSyncJob(runtime, JobKindFactorRebuild,
		DataSyncJobRequest{Version: 1, Market: "cn", TriggerSource: "scheduler", Reason: "背压测试"})
	var before int64
	common.DB.Model(&model.JobRun{}).Where("kind = ?", JobKindFactorRebuild).Count(&before)
	if before != 0 {
		t.Fatalf("队列拒绝时不得预建孤儿 JobRun: %d", before)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var run model.JobRun
		if common.DB.Where("kind = ?", JobKindFactorRebuild).First(&run).Error == nil && run.Status == model.JobStatusSuccess {
			var logs int64
			common.DB.Model(&model.DataSyncLog{}).Where("job_run_id = ? AND status = ?", run.ID, "success").Count(&logs)
			if logs == 1 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("背压释放后系统任务未重试成功，first=%d", first.ID)
}

func TestResearchArtifactIdempotentImmutableAndNoHistoricalGuess(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	now := time.Now()
	userID := int64(9201)
	run := &model.JobRun{UserID: userID, Kind: JobKindAnalysis, RequestHash: strings.Repeat("d", 64),
		Status: model.JobStatusSuccess, SnapshotVersion: 1, RequestSnapshot: `{}`, ResultType: JobResultAnalysis,
		QueuedAt: now, FinishedAt: &now}
	if err := common.DB.Create(run).Error; err != nil {
		t.Fatal(err)
	}
	legacy := &model.AnalysisRecord{UserID: userID, Module: model.AnalysisModuleSector, Market: "cn",
		Target: "半导体", Status: model.AnalysisStatusSuccess, ResultJSON: `{"rating":"neutral"}`}
	if err := common.DB.Create(legacy).Error; err != nil {
		t.Fatal(err)
	}
	var before int64
	common.DB.Model(&model.ResearchArtifact{}).Count(&before)
	if before != 0 {
		t.Fatal("历史业务记录不得在迁移时被猜测补建 Artifact")
	}
	run.ResultID = &legacy.ID
	if err := persistAnalysisArtifact(common.DB, run, *legacy, now); err != nil {
		t.Fatal(err)
	}
	if err := persistAnalysisArtifact(common.DB, run, *legacy, now); err != nil {
		t.Fatal(err)
	}

	batch := &model.RecommendationBatch{UserID: userID, Type: model.RecTypeShortTerm, Market: "cn",
		Strategy: "momentum", Status: model.RecStatusSuccess, CandidatePool: `{"large":"candidate-body"}`, TraceID: "trace-rec"}
	if err := common.DB.Create(batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.Recommendation{BatchID: batch.ID, UserID: userID, Symbol: "600000",
		Market: "cn", Action: model.RecActionWatch, DetailJSON: `{"large":"recommendation-body"}`}).Error; err != nil {
		t.Fatal(err)
	}
	recRun := &model.JobRun{UserID: userID, Kind: JobKindRecommendation, RequestHash: strings.Repeat("1", 64),
		Status: model.JobStatusSuccess, SnapshotVersion: 1, RequestSnapshot: `{}`, ResultType: JobResultRecommendation,
		ResultID: &batch.ID, QueuedAt: now, FinishedAt: &now}
	if err := common.DB.Create(recRun).Error; err != nil {
		t.Fatal(err)
	}
	if err := persistRecommendationArtifact(common.DB, recRun, *batch, now); err != nil {
		t.Fatal(err)
	}
	if err := persistRecommendationArtifact(common.DB, recRun, *batch, now); err != nil {
		t.Fatal(err)
	}

	report := &model.DailyReport{UserID: userID, TradeDate: "2026-08-07", Market: "cn",
		Status: model.ReportStatusSuccess, ReviewJSON: `{"large":"report-body"}`, SnapshotJSON: `{"large":"snapshot-body"}`}
	if err := common.DB.Create(report).Error; err != nil {
		t.Fatal(err)
	}
	reportRun := &model.JobRun{UserID: userID, Kind: JobKindDailyReport, RequestHash: strings.Repeat("2", 64),
		Status: model.JobStatusSuccess, SnapshotVersion: 1, RequestSnapshot: `{}`, ResultType: JobResultDailyReport,
		ResultID: &report.ID, QueuedAt: now, FinishedAt: &now}
	if err := common.DB.Create(reportRun).Error; err != nil {
		t.Fatal(err)
	}
	if err := persistDailyReportArtifact(common.DB, reportRun, *report, now); err != nil {
		t.Fatal(err)
	}
	if err := persistDailyReportArtifact(common.DB, reportRun, *report, now); err != nil {
		t.Fatal(err)
	}
	retryParent := reportRun.ID
	retryRun := &model.JobRun{UserID: userID, Kind: JobKindDailyReport, RequestHash: strings.Repeat("3", 64),
		Status: model.JobStatusSuccess, SnapshotVersion: 1, RequestSnapshot: `{}`, ResultType: JobResultDailyReport,
		ResultID: &report.ID, ParentID: &retryParent, QueuedAt: now, FinishedAt: &now}
	if err := common.DB.Create(retryRun).Error; err != nil {
		t.Fatal(err)
	}
	if err := persistDailyReportArtifact(common.DB, retryRun, *report, now); err != nil {
		t.Fatal(err)
	}

	var artifacts []model.ResearchArtifact
	if err := common.DB.Order("id").Find(&artifacts).Error; err != nil || len(artifacts) != 4 {
		t.Fatalf("三类 Artifact 应按 JobRun 幂等并保留重跑血缘: count=%d err=%v", len(artifacts), err)
	}
	for _, artifact := range artifacts {
		if artifact.Type == ArtifactTypeAnalysis && artifact.Subject != "sector:cn:半导体" {
			t.Fatalf("非个股分析 Artifact 必须保留真实 target: %+v", artifact)
		}
		for _, body := range []string{"rating", "candidate-body", "recommendation-body", "report-body", "snapshot-body"} {
			if strings.Contains(artifact.SourceRefs, body) {
				t.Fatalf("Artifact 不得复制业务正文 type=%s body=%s", artifact.Type, body)
			}
		}
	}
	if err := common.DB.Model(&artifacts[0]).Update("subject", "changed").Error; err == nil {
		t.Fatal("Artifact 不得修改")
	}
	if err := common.DB.Delete(&artifacts[0]).Error; err == nil {
		t.Fatal("Artifact 不得删除")
	}
}

func TestJobEventRetentionAndMetricsPermissions(t *testing.T) {
	resetJobOwnerArtifactTestDB(t)
	createJobTestUser(t, 9301, model.RoleUser)
	createJobTestUser(t, 9302, model.RoleAdmin)
	now := time.Now()
	old := now.Add(-JobEventRetention - time.Hour)
	terminal := &model.JobRun{UserID: 9301, Kind: JobKindQA, RequestHash: strings.Repeat("e", 64),
		Status: model.JobStatusFailed, SnapshotVersion: 1, RequestSnapshot: `{}`, QueuedAt: old, FinishedAt: &old}
	active := &model.JobRun{UserID: 9301, Kind: JobKindCompare, RequestHash: strings.Repeat("f", 64),
		Status: model.JobStatusRunning, SnapshotVersion: 1, RequestSnapshot: `{}`, QueuedAt: old, StartedAt: &old}
	if err := common.DB.Create(terminal).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(active).Error; err != nil {
		t.Fatal(err)
	}
	for _, run := range []*model.JobRun{terminal, active} {
		event := &model.JobEvent{UserID: 9301, JobRunID: run.ID, Type: "status", Status: run.Status, CreatedAt: old}
		if err := common.DB.Create(event).Error; err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := CleanupExpiredJobEvents(now)
	if err != nil || deleted != 1 {
		t.Fatalf("只应清理终态旧事件: deleted=%d err=%v", deleted, err)
	}
	if _, err := GetJobRuntimeMetrics(9301); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("普通用户不得读取容量指标: %v", err)
	}
	metrics, err := GetJobRuntimeMetrics(9302)
	if err != nil || metrics.Capacity.Capacity <= 0 || len(metrics.Buckets) == 0 || metrics.OldestQueued != nil {
		t.Fatalf("管理员指标异常: metrics=%+v err=%v", metrics, err)
	}
}
