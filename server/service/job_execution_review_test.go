package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestJobClaimDoesNotDependOnSecondRead(t *testing.T) {
	resetDurableJobs(t)
	run := createPersistedJobForRecovery(t, 2060, JobKindCompare, model.JobStatusQueued, map[string]int{"case": 1})
	runtime := newJobRuntime(1, 2)
	t.Cleanup(runtime.close)
	called := false
	runtime.register(JobKindCompare, time.Minute, func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		called = true
		return DurableJobResult{Value: map[string]bool{"ok": true}, Status: model.JobStatusSuccess}, nil
	})
	reads := 0
	const callback = "review_job_claim_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" && len(tx.Statement.Selects) == 0 {
			reads++
			if reads == 2 {
				tx.AddError(errors.New("领取后的冗余读取失败"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	runtime.execute(run.ID)
	_ = common.DB.Callback().Query().Remove(callback)
	if err := common.DB.First(&run, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !called || run.Status != model.JobStatusSuccess {
		t.Fatalf("领取已成功时不应因重复读取而永久遗留 running：called=%v status=%s", called, run.Status)
	}
}

func TestJobMaintenanceRetriesTransientRecoveryReads(t *testing.T) {
	for _, stage := range []string{"running", "queued"} {
		t.Run(stage, func(t *testing.T) {
			resetDurableJobs(t)
			queued := createPersistedJobForRecovery(t, 2061, JobKindQA, model.JobStatusQueued, map[string]int{"case": 2})
			runtime := newJobRuntime(1, 2)
			previous := defaultJobRuntime
			defaultJobRuntime = runtime
			t.Cleanup(func() { runtime.close(); defaultJobRuntime = previous })
			runtime.register(JobKindQA, time.Minute, func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
				return DurableJobResult{Value: map[string]bool{"ok": true}, Status: model.JobStatusSuccess}, nil
			})
			var failed atomic.Bool
			const callback = "review_job_recovery_read"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "job_runs" {
					return
				}
				_, batch := tx.Statement.Dest.(*[]model.JobRun)
				runningRead := batch && len(tx.Statement.Selects) == 0
				queueRead := batch && len(tx.Statement.Selects) == 3 && tx.Statement.Selects[1] == "kind"
				if ((stage == "running" && runningRead) || (stage == "queued" && queueRead)) && failed.CompareAndSwap(false, true) {
					tx.AddError(errors.New("首次恢复查询暂时失败"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
			StartJobRuntime()
			if done := waitJobRun(t, 2061, queued.ID); done.Status != model.JobStatusSuccess || !failed.Load() {
				t.Fatalf("临时故障恢复后应自行继续排队任务，无需其他请求唤醒：status=%s injected=%v", done.Status, failed.Load())
			}
		})
	}
}

func TestJobRecoveryDoesNotInterruptOwnedExecution(t *testing.T) {
	resetDurableJobs(t)
	run := createPersistedJobForRecovery(t, 2062, JobKindCompare, model.JobStatusRunning, map[string]int{"case": 3})
	runtime := newJobRuntime(1, 2)
	t.Cleanup(runtime.close)
	// 与实际 worker 持有运行中任务时相同的所有权；恢复重试不能误伤本进程在途任务。
	runtime.scheduled[run.ID] = struct{}{}
	if err := runtime.recoverPersisted(); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.First(&run, run.ID).Error; err != nil || run.Status != model.JobStatusRunning {
		t.Fatalf("恢复扫描不能把当前进程在执行的任务标成重启中断：status=%s err=%v", run.Status, err)
	}
}

func TestJobRecoveryReportsFailedTerminalWrite(t *testing.T) {
	resetDurableJobs(t)
	run := createPersistedJobForRecovery(t, 2063, JobKindCompare, model.JobStatusRunning, map[string]int{"case": 4})
	runtime := newJobRuntime(1, 2)
	t.Cleanup(runtime.close)
	fault := errors.New("中断状态写入暂时失败")
	const callback = "review_job_recovery_write"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_events" {
			tx.AddError(fault)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
	if err := runtime.recoverPersisted(); !errors.Is(err, fault) {
		t.Errorf("终态没有保存时不能报告恢复成功：%v", err)
	}
	_ = common.DB.Callback().Create().Remove(callback)
	if err := runtime.recoverPersisted(); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.First(&run, run.ID).Error; err != nil || run.Status != model.JobStatusFailed {
		t.Fatalf("存储恢复后应可收敛中断任务：status=%s err=%v", run.Status, err)
	}
}
