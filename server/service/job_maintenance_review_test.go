package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func createMaintenanceReviewJob(test *testing.T, request DataSyncJobRequest, status string) model.JobRun {
	test.Helper()
	request.Version, request.Market, request.TriggerSource = 1, "cn", "admin"
	requestJSON, err := json.Marshal(request)
	if err != nil {
		test.Fatal(err)
	}
	snapshotJSON, err := json.Marshal(persistedJobSnapshot{Version: jobSnapshotVersion, Kind: request.Task, Request: requestJSON})
	if err != nil {
		test.Fatal(err)
	}
	run := model.JobRun{
		OwnerType: model.JobOwnerSystem, Kind: request.Task, Status: status,
		SnapshotVersion: jobSnapshotVersion, RequestSnapshot: string(snapshotJSON), QueuedAt: time.Now(),
	}
	createTaskCenterRow(test, &run)
	return run
}

func TestTaskCenterFocusedJobPreservesSystemAuthorization(test *testing.T) {
	resetTaskCenterTestDB(test)
	actorID := int64(1)
	owned := model.JobRun{UserID: actorID, Kind: JobKindQA, Status: model.JobStatusFailed}
	foreign := model.JobRun{UserID: 2, Kind: JobKindQA, Status: model.JobStatusFailed}
	createTaskCenterRow(test, &owned)
	createTaskCenterRow(test, &foreign)
	system := createMaintenanceReviewJob(test, DataSyncJobRequest{Task: JobKindBackfillCalendar, HasMaintenance: true}, model.JobStatusFailed)
	createMaintenanceReviewJob(test, DataSyncJobRequest{Task: JobKindSnapshotMarket}, model.JobStatusSuccess)
	if err := common.DB.Model(&system).Update("triggered_by", actorID).Error; err != nil {
		test.Fatal(err)
	}
	for _, run := range []model.JobRun{owned, system} {
		createTaskCenterRow(test, &model.JobStep{JobRunID: run.ID, Sequence: 1, Name: "execute", Status: model.JobStatusFailed})
	}
	createTaskCenterRow(test, &model.DataSyncLog{ID: system.ID, Task: JobKindBackfillCalendar, Status: "failed", Message: "unrelated legacy log"})

	for _, scenario := range []struct {
		name          string
		role          string
		jobID         int64
		includeSystem bool
		wantSource    string
	}{
		{name: "user_own", role: model.RoleUser, jobID: owned.ID, wantSource: TaskSourceJob},
		{name: "user_cannot_read_foreign", role: model.RoleUser, jobID: foreign.ID, includeSystem: true},
		{name: "user_cannot_read_system_even_if_triggered_by_self", role: model.RoleUser, jobID: system.ID, includeSystem: true},
		{name: "admin_requires_explicit_system_opt_in", role: model.RoleAdmin, jobID: system.ID},
		{name: "admin_finds_exact_system_job", role: model.RoleAdmin, jobID: system.ID, includeSystem: true, wantSource: TaskSourceDataSync},
		{name: "admin_still_cannot_read_foreign", role: model.RoleAdmin, jobID: foreign.ID, includeSystem: true},
		{name: "admin_can_read_own", role: model.RoleAdmin, jobID: owned.ID, includeSystem: true, wantSource: TaskSourceJob},
		{name: "missing_job", role: model.RoleAdmin, jobID: 999999, includeSystem: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			items, err := NewTaskCenterService().List(actorID, scenario.role, TaskCenterListOptions{
				JobID: scenario.jobID, IncludeSystem: scenario.includeSystem, IncludeSteps: true,
				Status: TaskStatusSuccess, Limit: 1,
			})
			if err != nil {
				test.Fatal(err)
			}
			if scenario.wantSource == "" {
				if len(items) != 0 {
					test.Fatalf("focused query exposed an unauthorized or unrelated task: %+v", items)
				}
				return
			}
			if len(items) != 1 || items[0].SourceID != scenario.jobID || items[0].Source != scenario.wantSource || items[0].ID != taskCompositeID(TaskSourceJob, scenario.jobID) {
				test.Fatalf("focused query did not locate the exact authorized job: %+v", items)
			}
			if len(items[0].Steps) != 1 || items[0].Steps[0].Name != "execute" || items[0].Status != TaskStatusFailed {
				test.Fatalf("focused query lost terminal status or steps: %+v", items[0])
			}
		})
	}
}

func TestTaskCenterMaintenanceRetryEligibility(test *testing.T) {
	resetTaskCenterTestDB(test)
	expected := make(map[int64]bool)
	for _, kind := range []string{JobKindSyncDailyBars, JobKindBackfillCalendar, JobKindSyncMarketWide, JobKindSnapshotMarket, JobKindInitMarketHistory, JobKindFactorRebuild} {
		for _, hasMaintenance := range []bool{false, true} {
			run := createMaintenanceReviewJob(test, DataSyncJobRequest{
				Task: kind, HasMaintenance: hasMaintenance,
				Maintenance: MaintenanceRequest{PlanHash: strings.Repeat("a", 64)},
			}, model.JobStatusFailed)
			expected[run.ID] = !hasMaintenance || kind == JobKindSnapshotMarket || kind == JobKindInitMarketHistory || kind == JobKindFactorRebuild
		}
	}
	missingHash := createMaintenanceReviewJob(test, DataSyncJobRequest{Task: JobKindBackfillCalendar, HasMaintenance: true}, model.JobStatusFailed)
	expected[missingHash.ID] = false
	corrupt := createMaintenanceReviewJob(test, DataSyncJobRequest{Task: JobKindSyncDailyBars}, model.JobStatusFailed)
	if err := common.DB.Model(&corrupt).Update("request_snapshot", "invalid JSON").Error; err != nil {
		test.Fatal(err)
	}
	expected[corrupt.ID] = false
	finished := createMaintenanceReviewJob(test, DataSyncJobRequest{Task: JobKindSyncDailyBars}, model.JobStatusSuccess)
	expected[finished.ID] = false
	items, err := NewTaskCenterService().List(1, model.RoleAdmin, TaskCenterListOptions{Source: TaskSourceDataSync, IncludeSystem: true})
	if err != nil || len(items) != len(expected) {
		test.Fatalf("maintenance task listing failed: count=%d err=%v", len(items), err)
	}
	for _, item := range items {
		if item.CanRetry != expected[item.SourceID] {
			test.Errorf("incorrect retry eligibility for %s #%d: got=%v want=%v", item.Kind, item.SourceID, item.CanRetry, expected[item.SourceID])
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil || strings.Contains(string(encoded), strings.Repeat("a", 64)) || strings.Contains(string(encoded), "request_snapshot") {
		test.Fatalf("task list exposed a saved request or plan hash: %s err=%v", encoded, err)
	}
}

func registerMaintenanceReviewHandlers(test *testing.T) *atomic.Int32 {
	test.Helper()
	resetDurableJobs(test)
	createJobTestUser(test, 9911, model.RoleAdmin)
	createJobTestUser(test, 9912, model.RoleUser)
	calls := &atomic.Int32{}
	handler := func(_ context.Context, _ int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
		calls.Add(1)
		var request DataSyncJobRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return DurableJobResult{}, err
		}
		log := &model.DataSyncLog{Task: request.Task, Market: "cn", Status: "success", Total: 1, Succeeded: 1, TriggerSource: "admin"}
		return DurableJobResult{Value: log, Status: model.JobStatusSuccess, Total: 1, Succeeded: 1}, nil
	}
	for _, kind := range []string{JobKindSyncDailyBars, JobKindBackfillCalendar, JobKindSyncMarketWide, JobKindSnapshotMarket, JobKindInitMarketHistory, JobKindFactorRebuild} {
		defaultJobRuntime.registerWithBinding(kind, time.Minute, handler, dataSyncJobBinding(), false)
	}
	return calls
}

func TestJobMaintenanceRetryRequiresNewPreview(test *testing.T) {
	calls := registerMaintenanceReviewHandlers(test)
	for _, kind := range []string{JobKindSyncDailyBars, JobKindBackfillCalendar, JobKindSyncMarketWide} {
		for _, planHash := range []string{"", strings.Repeat("b", 64)} {
			parent := createMaintenanceReviewJob(test, DataSyncJobRequest{
				Task: kind, HasMaintenance: true, Maintenance: MaintenanceRequest{PlanHash: planHash},
			}, model.JobStatusFailed)
			child, err := RetryJobRun(9911, parent.ID, test.Context())
			if child != nil || !errors.Is(err, ErrJobMaintenancePreviewRequired) {
				test.Fatalf("planned maintenance retry must request a new preview: child=%+v err=%v", child, err)
			}
			if child, err := RetryJobRun(9912, parent.ID, test.Context()); child != nil || !errors.Is(err, ErrJobNotFound) {
				test.Fatalf("ordinary user learned about a system maintenance job: child=%+v err=%v", child, err)
			}
			var stored model.JobRun
			if err := common.DB.First(&stored, parent.ID).Error; err != nil {
				test.Fatal(err)
			}
			if stored.RequestSnapshot != parent.RequestSnapshot || stored.Status != model.JobStatusFailed {
				test.Fatal("rejected retry mutated its parent")
			}
		}
	}
	for _, table := range []string{"data_sync_logs", "job_steps", "job_events"} {
		var count int64
		if err := common.DB.Table(table).Count(&count).Error; err != nil || count != 0 {
			test.Fatalf("rejected retry left %s side effects: count=%d err=%v", table, count, err)
		}
	}
	var children int64
	if err := common.DB.Model(&model.JobRun{}).Where("parent_id IS NOT NULL").Count(&children).Error; err != nil || children != 0 || calls.Load() != 0 {
		test.Fatalf("rejected retry executed or created a child: children=%d calls=%d err=%v", children, calls.Load(), err)
	}
}

func TestJobMaintenanceRetryPreservesLegacyAndOtherSystemJobs(test *testing.T) {
	calls := registerMaintenanceReviewHandlers(test)
	for _, kind := range []string{JobKindSyncDailyBars, JobKindBackfillCalendar, JobKindSyncMarketWide, JobKindSnapshotMarket, JobKindInitMarketHistory, JobKindFactorRebuild} {
		parent := createMaintenanceReviewJob(test, DataSyncJobRequest{
			Task: kind, HasMaintenance: kind == JobKindSnapshotMarket || kind == JobKindInitMarketHistory || kind == JobKindFactorRebuild,
		}, model.JobStatusFailed)
		child, err := RetryJobRun(9911, parent.ID, test.Context())
		if err != nil || child == nil || child.ID == parent.ID || child.ParentID == nil || *child.ParentID != parent.ID || child.Owner != model.JobOwnerSystem || child.TriggeredBy == nil || *child.TriggeredBy != 9911 {
			test.Fatalf("unplanned system retry lost its behavior or audit identity: child=%+v err=%v", child, err)
		}
		if finished := waitJobRun(test, 9911, child.ID); finished.Status != model.JobStatusSuccess {
			test.Fatalf("synthetic retry failed: %+v", finished)
		}
	}
	if calls.Load() != 6 {
		test.Fatalf("expected six allowed synthetic retries, got=%d", calls.Load())
	}
}
