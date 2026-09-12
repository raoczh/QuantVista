package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestJobOpsCanceledSubmissionAndRetryDoNotCreate(t *testing.T) {
	setupTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	actor := int64(8992)
	var before, after int64
	if err := common.DB.Model(&model.JobRun{}).Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	if run, started, err := StartSystemDataSyncJob(JobKindInitMarketHistory, &actor, DataSyncJobRequest{Market: "cn"}, ctx); run != nil || started || !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的系统提交仍被接受: run=%v started=%v err=%v", run, started, err)
	}
	if run, err := RetryJobRun(actor, 1, ctx); run != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的重试必须在读取和创建前终止: run=%v err=%v", run, err)
	}
	if err := common.DB.Model(&model.JobRun{}).Count(&after).Error; err != nil || before != after {
		t.Fatalf("已取消的请求留下作业: before=%d after=%d err=%v", before, after, err)
	}
}

func TestJobOpsCancelReceiptFailureRollsBack(t *testing.T) {
	setupTestDB(t)
	run := model.JobRun{UserID: 8993, Kind: JobKindQA, Status: model.JobStatusQueued}
	if err := common.DB.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review_cancel_receipt_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_steps" {
			tx.AddError(errors.New("本地故障：回执步骤读取失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	db := common.DB
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	if _, err := CancelJobRun(run.UserID, run.ID, t.Context()); err == nil {
		t.Fatal("回执失败没有传回调用方")
	}
	var after model.JobRun
	if err := db.First(&after, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != model.JobStatusQueued || after.CancelRequested {
		t.Fatalf("失败回执留下了已提交的取消事实: %+v", after)
	}
}

func TestMySQLJobOpsReadKeepsRunAndStepsTogether(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.JobRun{}, &model.JobStep{})
	run := model.JobRun{UserID: 8994, Kind: JobKindQA, Status: model.JobStatusRunning, QueuedAt: time.Now()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	step := model.JobStep{JobRunID: run.ID, Sequence: 1, Name: "fetch", Status: model.JobStatusRunning}
	if err := db.Create(&step).Error; err != nil {
		t.Fatal(err)
	}
	var changed atomic.Bool
	const callback = "review_job_run_steps_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "job_runs" || !changed.CompareAndSwap(false, true) {
			return
		}
		if err := db.Transaction(func(writer *gorm.DB) error {
			if err := writer.Model(&run).Update("status", model.JobStatusFailed).Error; err != nil {
				return err
			}
			return writer.Model(&step).Update("status", model.JobStatusFailed).Error
		}); err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	view, err := GetJobRun(run.UserID, run.ID, true, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Load() || view.Status != model.JobStatusRunning || len(view.Steps) != 1 || view.Steps[0].Status != model.JobStatusRunning {
		t.Fatalf("任务状态与步骤混用了读取时点: changed=%v view=%+v", changed.Load(), view)
	}
}
