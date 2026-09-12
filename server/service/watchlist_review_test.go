package service

import (
	"context"
	"errors"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestWatchlistAddRechecksGroupAfterQuoteFetch(t *testing.T) {
	setupTestDB(t)
	svc := &WatchlistService{}
	group, err := svc.CreateGroup(995, "将被删除")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	svc.market = NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{hook: func() {
		called = true
		if err := svc.DeleteGroup(995, group.ID); err != nil {
			t.Error(err)
		}
	}}))
	_, err = svc.AddItem(context.Background(), 995, group.ID, WatchlistItemInput{Symbol: "600099", Market: "cn"})
	if err == nil {
		t.Error("取名期间组已删除，添加必须失败")
	}
	var count int64
	if err := common.DB.Model(&model.WatchlistItem{}).Where("user_id = ?", 995).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !called || count != 0 {
		t.Fatalf("留下无分组条目：called=%v count=%d", called, count)
	}
}

func TestWatchlistUndoReadFailureDoesNotCommitPartialDeletion(t *testing.T) {
	setupTestDB(t)
	group := model.Watchlist{UserID: 996, Name: "撤销故障"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	resultID := persistBatchTestScan(t, 996, "600101", "600102")
	batch, err := CreateWatchlistBatch(996, resultID, WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"600101", "600102"}})
	if err != nil {
		t.Fatal(err)
	}
	const callback = "review:watchlist_undo_read_failure"
	reads := 0
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(db *gorm.DB) {
		if _, ok := db.Statement.Dest.(*model.WatchlistItem); ok {
			reads++
			if reads == 2 {
				db.AddError(errors.New("注入第二个条目读取失败"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	if _, err := UndoWatchlistBatch(996, batch.ID); err == nil {
		t.Error("存储故障不能当成普通冲突提交")
	}
	var count int64
	if err := common.DB.Model(&model.WatchlistItem{}).Where("user_id = ?", 996).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if reads != 2 || count != 2 {
		t.Fatalf("故障后应回滚第一项删除：reads=%d items=%d", reads, count)
	}
	var after model.WatchlistBatch
	if err := common.DB.First(&after, "id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != model.WatchlistBatchApplied {
		t.Fatalf("故障后审计状态被误写：%+v", after)
	}
}

func TestWatchlistDefaultCreationInterleavingIsIdempotent(t *testing.T) {
	setupTestDB(t)
	svc := &WatchlistService{}
	interleaved := false
	var firstID int64
	const callback = "review_default_group_interleave"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "watchlists" || interleaved {
			return
		}
		interleaved = true
		group, err := svc.EnsureDefaultGroup(1025)
		if err != nil {
			t.Fatal(err)
		}
		firstID = group.ID
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	group, err := svc.EnsureDefaultGroup(1025)
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := common.DB.Model(&model.Watchlist{}).Where("user_id = ?", 1025).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !interleaved || count != 1 || group.ID != firstID {
		t.Fatalf("首次并发访问应返回同一分组：count=%d ids=%d/%d", count, firstID, group.ID)
	}
}
