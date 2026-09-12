package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func submissionReviewRuntime(t *testing.T) *jobRuntime {
	t.Helper()
	runtime := newJobRuntime(1, 4)
	// 仅审查接受事务与回执；不启动模型或行情后台任务。
	runtime.startOnce.Do(func() {})
	previous := defaultJobRuntime
	defaultJobRuntime = runtime
	t.Cleanup(func() { runtime.close(); defaultJobRuntime = previous })
	return runtime
}

func TestBusinessSubmissionReceiptFailureRollsBack(t *testing.T) {
	for _, kind := range []string{JobKindRecommendation, JobKindAnalysis, JobKindDailyReport} {
		t.Run(kind, func(t *testing.T) {
			const userID int64 = 8824
			seedReportEnv(t, userID, "http://127.0.0.1:1")
			submissionReviewRuntime(t)
			var submit func() error
			var table string
			switch kind {
			case JobKindRecommendation:
				svc := NewRecommendationService(nil, nil, NewLLMService())
				submit = func() error {
					_, err := svc.Generate(context.Background(), userID, true, RecommendRequest{Type: model.RecTypeShortTerm, Market: "cn"})
					return err
				}
				table = "recommendation_batches"
			case JobKindAnalysis:
				svc := NewAnalysisService(nil, nil, nil, NewLLMService(), nil)
				submit = func() error {
					_, err := svc.AnalyzeAsync(userID, true, AnalyzeRequest{Module: model.AnalysisModuleStock, Mode: model.AnalysisModeStandard, Symbol: "600000", Market: "cn"})
					return err
				}
				table = "analysis_records"
			case JobKindDailyReport:
				svc := fakeReportSvc(nil)
				submit = func() error { _, err := svc.GenerateFor(context.Background(), userID, true); return err }
				table = "daily_reports"
			}
			var accepted atomic.Bool
			const created, query = "review_business_receipt_created", "review_business_receipt_query"
			if err := common.DB.Callback().Create().After("gorm:create").Register(created, func(tx *gorm.DB) {
				if tx.Statement.Table == "job_events" {
					accepted.Store(true)
				}
			}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("本机业务回执读取故障")
			if err := common.DB.Callback().Query().Before("gorm:query").Register(query, func(tx *gorm.DB) {
				if tx.Statement.Table == table && accepted.Load() {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Create().Remove(created); common.DB.Callback().Query().Remove(query) })
			if err := submit(); !errors.Is(err, failure) {
				t.Fatalf("未触发预期回执读取故障: %v", err)
			}
			var count int64
			if err := common.DB.Model(&model.JobRun{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Errorf("回执报错却保留 %d 个已接受的 %s 作业", count, kind)
			}
		})
	}
}

func TestRecommendationSubmissionCancellation(t *testing.T) {
	for _, phase := range []string{"before_request", "before_commit"} {
		t.Run(phase, func(t *testing.T) {
			const userID int64 = 8825
			seedRecEnv(t, userID, "http://127.0.0.1:1", 0)
			submissionReviewRuntime(t)
			svc := NewRecommendationService(nil, nil, NewLLMService())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before_request" {
				cancel()
			} else {
				const callback = "review_recommendation_cancel_submission"
				if err := common.DB.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "job_events" {
						cancel()
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Create().Remove(callback) })
			}
			_, err := svc.Generate(ctx, userID, true, RecommendRequest{Type: model.RecTypeShortTerm, Market: "cn"})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("提交事务之前取消应返回取消原因: %v", err)
			}
			var count int64
			if err := common.DB.Model(&model.JobRun{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Errorf("取消请求留下了 %d 个新作业", count)
			}
		})
	}
}

func TestRecommendationSubmissionCancelsWhileWaiting(t *testing.T) {
	const userID int64 = 8826
	seedRecEnv(t, userID, "http://127.0.0.1:1", 0)
	runtime := submissionReviewRuntime(t)
	svc := NewRecommendationService(nil, nil, NewLLMService())
	runtime.createMu.Lock()
	var once sync.Once
	unlock := func() { once.Do(runtime.createMu.Unlock) }
	t.Cleanup(unlock)
	read := make(chan struct{})
	var seen atomic.Bool
	const callback = "review_recommendation_wait_cancel"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" && seen.CompareAndSwap(false, true) {
			close(read)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := svc.Generate(ctx, userID, true, RecommendRequest{Type: model.RecTypeShortTerm, Market: "cn"})
		done <- err
	}()
	select {
	case <-read:
	case <-time.After(3 * time.Second):
		t.Fatal("提交未进入作业去重查询")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("取消等待未保留原因: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("取消后仍在等待提交锁")
		unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("释放锁后提交仍未结束")
		}
	}
	unlock()
	var count int64
	if err := common.DB.Model(&model.JobRun{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("取消等待后仍留下 %d 个新作业", count)
	}
}
