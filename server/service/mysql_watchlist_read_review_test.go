package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestMySQLWatchlistListKeepsGroupsAndItemsTogether(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.Watchlist{}, &model.WatchlistItem{}, &model.TradingCalendar{})
	group := model.Watchlist{UserID: 8960, Name: "原分组"}
	if err := db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	item := model.WatchlistItem{UserID: group.UserID, WatchlistID: group.ID, Symbol: "600001", Market: "cn"}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_watchlist_groups_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "watchlists" && count.Add(1) == 2 {
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
		groups []WatchlistGroupView
		err    error
	}
	done := make(chan reply, 1)
	svc := NewWatchlistService(NewMarketService(datasource.NewManagerWithAdapters()))
	go func() { groups, err := svc.List(t.Context(), group.UserID); done <- reply{groups, err} }()
	select {
	case <-read:
	case result := <-done:
		t.Fatalf("未读取分组：%v", result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("未进入分组读取")
	}
	second := model.Watchlist{UserID: group.UserID, Name: "后建分组"}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&second).Error; err != nil {
			return err
		}
		return tx.Model(&item).Update("watchlist_id", second.ID).Error
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil || len(result.groups) != 1 || len(result.groups[0].Items) != 1 {
			t.Errorf("自选列表混用不同时点导致条目消失：%+v %v", result.groups, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("自选列表读取未结束")
	}
}

func TestMySQLWatchlistBatchGetKeepsAuditSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.WatchlistBatch{}, &model.WatchlistBatchItem{})
	batch := model.WatchlistBatch{ID: "local-review-batch", UserID: 8961, Status: model.WatchlistBatchApplied, Requested: 1, Created: 1, RequestHash: "local-hash"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	item := model.WatchlistBatchItem{UserID: batch.UserID, BatchID: batch.ID, Symbol: "600001", Market: "cn", Status: model.WatchlistBatchItemCreated}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_watchlist_batch_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "watchlist_batches" && first.CompareAndSwap(false, true) {
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
		batch *WatchlistBatchView
		err   error
	}
	done := make(chan reply, 1)
	go func() { view, err := GetWatchlistBatch(batch.UserID, batch.ID); done <- reply{view, err} }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("未进入批次读取")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&batch).Updates(map[string]any{"status": model.WatchlistBatchUndone, "removed": 1}).Error; err != nil {
			return err
		}
		return tx.Model(&item).Update("status", model.WatchlistBatchItemRemoved).Error
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil || result.batch.Status != model.WatchlistBatchApplied || len(result.batch.Items) != 1 || result.batch.Items[0].Status != model.WatchlistBatchItemCreated {
			t.Errorf("审计头与条目不在同一时点：%+v %v", result.batch, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("审计读取未结束")
	}
}
