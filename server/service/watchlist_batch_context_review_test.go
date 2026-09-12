package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestWatchlistBatchCancelWhileWaitingKeepsItems(t *testing.T) {
	for _, action := range []string{"create", "undo"} {
		t.Run(action, func(t *testing.T) {
			setupTestDB(t)
			group := model.Watchlist{UserID: 8964, Name: "批量等锁取消"}
			if err := common.DB.Create(&group).Error; err != nil {
				t.Fatal(err)
			}
			resultID := persistBatchTestScan(t, group.UserID, "600001")
			request := WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"600001"}}
			batchID := ""
			if action == "undo" {
				batch, err := CreateWatchlistBatch(group.UserID, resultID, request)
				if err != nil {
					t.Fatal(err)
				}
				batchID = batch.ID
			}
			if err := watchlistBatchMu.Lock(); err != nil {
				t.Fatal(err)
			}
			defer watchlistBatchMu.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if action == "create" {
					_, err = CreateWatchlistBatch(group.UserID, resultID, request, ctx)
				} else {
					_, err = UndoWatchlistBatch(group.UserID, batchID, ctx)
				}
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("创建锁未释放却提前返回：%v", err)
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("应传播取消：%v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("取消后仍等待批量锁")
			}
			var count int64
			if err := common.DB.Model(&model.WatchlistItem{}).Where("user_id = ?", group.UserID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if action == "undo" {
				want = 1
			}
			if count != want {
				t.Errorf("取消后条目数改变：%d want=%d", count, want)
			}
		})
	}
}

func TestWatchlistBatchRejectsNullScanBody(t *testing.T) {
	setupTestDB(t)
	group := model.Watchlist{UserID: 8965, Name: "坏扫描体"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	id := persistBatchTestScan(t, group.UserID, "600001")
	if err := common.DB.Model(&model.StrategyRunResult{}).Where("id = ?", id).Updates(map[string]any{"result_json": "null", "content_hash": sha256Hex([]byte("null"))}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWatchlistBatch(group.UserID, id, WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"600001"}}); err == nil {
		t.Fatal("损坏的 null 扫描不能创建批量业务记录")
	}
}

func TestWatchlistStageCanceledDuringQuoteDoesNotChangeStage(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	item := model.WatchlistItem{UserID: 8966, WatchlistID: 1, Symbol: "600001", Market: "cn", ResearchStage: model.StageWatching}
	if err := common.DB.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	svc := NewWatchlistService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: cancel})))
	if _, err := svc.SetItemStage(ctx, item.UserID, item.ID, model.StagePassed, "取消中"); !errors.Is(err, context.Canceled) {
		t.Fatalf("行情等待期间取消应终止阶段变更：%v", err)
	}
	var after model.WatchlistItem
	if err := common.DB.First(&after, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ResearchStage != model.StageWatching || after.StageAt != nil || after.PassedPrice != 0 {
		t.Fatalf("取消期间仍写入放弃基准：%+v", after)
	}
}
