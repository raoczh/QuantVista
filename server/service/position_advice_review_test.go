package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestPositionAdviceSignalReadFailureIsDisclosed(t *testing.T) {
	for _, table := range []string{"alert_events", "sell_reviews"} {
		t.Run(table, func(t *testing.T) {
			setupTestDB(t)
			const callback = "review_advice_signals_read_error"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("本地信号存储故障"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			rows := []positionAdviceRow{{PositionID: 91, Symbol: "600001"}}
			attachAdviceSignals(8950, rows)
			body, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "读取失败") {
				t.Fatalf("模型输入没有说明信号读取缺口：%s", body)
			}
		})
	}
}

func TestLegacyLLMSubmissionContextProtectsQueue(t *testing.T) {
	for _, stage := range []string{"before", "waiting", "commit"} {
		t.Run(stage, func(t *testing.T) {
			setupTestDB(t)
			previous := defaultJobRuntime
			runtime := newJobRuntime(1, 4)
			runtime.workers = 0
			defaultJobRuntime = runtime
			t.Cleanup(func() { runtime.close(); defaultJobRuntime = previous })
			svc := NewPositionAdviceService(&PositionService{}, &LLMService{})
			account, err := NewPortfolioAccountService().Create(8951, PortfolioAccountInput{Name: "提交取消边界", Kind: model.PortfolioKindReal})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if stage == "before" {
				cancel()
			}
			if stage == "commit" {
				const callback = "review_advice_cancel_create"
				if err := common.DB.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "job_runs" {
						cancel()
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Create().Remove(callback) })
			}
			if stage == "waiting" {
				if err := runtime.createMu.Lock(); err != nil {
					t.Fatal(err)
				}
				defer runtime.createMu.Unlock()
			}
			done := make(chan error, 1)
			go func() {
				_, err := svc.AdviseAsync(8951, false, PositionAdviceRequest{AccountID: account.ID}, ctx)
				done <- err
			}()
			if stage == "waiting" {
				select {
				case err := <-done:
					t.Fatalf("持有创建锁时不应提前完成：%v", err)
				case <-time.After(50 * time.Millisecond):
				}
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("应保留取消原因：%v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("取消后仍等待创建锁")
			}
			for _, row := range []any{&model.JobRun{}, &model.LLMTask{}} {
				var count int64
				if err := common.DB.Model(row).Where("user_id = ?", 8951).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Errorf("取消后仍留下 %T 记录：%d", row, count)
				}
			}
		})
	}
}
