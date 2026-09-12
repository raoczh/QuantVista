package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMySQLTrackingCommitRechecksAfterBatchLock(t *testing.T) {
	for _, action := range []string{"delete", "freeze", "cancel"} {
		t.Run(action, func(t *testing.T) {
			db := setupMySQLReviewDB(t, &model.RecommendationBatch{}, &model.Recommendation{}, &model.RecommendationStatus{})
			batch, rec := seedLinkFixture(t, 8910, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
			initial := model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: rec.UserID,
				Symbol: rec.Symbol, Market: rec.Market, Outcome: model.RecOutcomeActive, ReturnPct: 5, LastEvalDate: "2026-09-09"}
			if err := db.Create(&initial).Error; err != nil {
				t.Fatal(err)
			}
			locked, release, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce, waitOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var armed atomic.Bool
			const callback = "review_mysql_tracking_wait"
			if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if armed.Load() && tx.Statement.Table == "recommendation_batches" {
					waitOnce.Do(func() { close(waiting) })
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Query().Remove(callback) })
			writer := make(chan error, 1)
			go func() {
				writer <- db.Transaction(func(tx *gorm.DB) error {
					var current model.RecommendationBatch
					if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, batch.ID).Error; err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
					case <-tx.Statement.Context.Done():
						return tx.Statement.Context.Err()
					}
					if action == "delete" {
						if err := tx.Delete(&initial).Error; err != nil {
							return err
						}
						if err := tx.Delete(&rec).Error; err != nil {
							return err
						}
						return tx.Delete(&current).Error
					}
					if action == "freeze" {
						return tx.Model(&initial).Updates(map[string]any{"outcome": model.RecOutcomeStopLoss, "return_pct": -10}).Error
					}
					return nil
				})
			}()
			select {
			case <-locked:
			case err := <-writer:
				t.Fatalf("未取得批次锁: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("等待批次锁超时")
			}
			armed.Store(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			late := initial
			late.ID = 0
			late.ReturnPct = 30
			started := time.Now()
			go func() { _, err := (&TrackingService{}).commitStatus(ctx, &late, started); done <- err }()
			select {
			case <-waiting:
			case err := <-done:
				t.Fatalf("提交未核对批次锁: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("等待提交核验超时")
			}
			if action == "cancel" {
				cancel()
			}
			unblock()
			if err := <-writer; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if action == "delete" && err == nil {
					t.Error("等待删除事务后仍接受旧状态")
				}
				if action == "cancel" && !errors.Is(err, context.Canceled) {
					t.Errorf("取消提交未保留取消原因: %v", err)
				}
				if action == "freeze" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("追踪提交未结束")
			}
			var rows []model.RecommendationStatus
			if err := db.Where("recommendation_id = ?", rec.ID).Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if action == "delete" {
				if len(rows) != 0 {
					t.Errorf("删除后复活了状态: %+v", rows)
				}
			} else {
				want := 5.0
				if action == "freeze" {
					want = -10
				}
				if len(rows) != 1 || rows[0].ReturnPct != want {
					t.Errorf("旧评估改写了当前状态: %+v", rows)
				}
			}
		})
	}
}

func TestMySQLTrackingPerformanceKeepsReadSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.RecommendationBatch{}, &model.Recommendation{}, &model.RecommendationStatus{})
	batch, rec := seedLinkFixture(t, 8911, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	if err := db.Create(&model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: rec.UserID,
		Action: model.RecActionBuy, Outcome: model.RecOutcomeExpired, ReturnPct: 10}).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_mysql_tracking_performance_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_statuses" && first.CompareAndSwap(false, true) {
			close(read)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
				tx.AddError(tx.Statement.Context.Err())
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	type reply struct {
		stats *PerformanceStats
		err   error
	}
	done := make(chan reply, 1)
	go func() { stats, err := (&TrackingService{}).Performance(rec.UserID, ""); done <- reply{stats, err} }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("表现查询未进入读取快照")
	}
	if err := db.Model(&batch).Update("status", model.RecStatusDegraded).Error; err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.stats.BuyMatured != 1 || result.stats.DegradedExcluded != 0 {
			t.Errorf("同一统计拼入后来的批次分类: %+v", result.stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("表现查询未结束")
	}
	after, err := (&TrackingService{}).Performance(rec.UserID, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.BuyMatured != 0 || after.DegradedExcluded != 1 {
		t.Errorf("后续查询未看到已提交分类: %+v", after)
	}
}
