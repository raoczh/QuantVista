package service

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func persistBatchTestScan(t *testing.T, userID int64, symbols ...string) int64 {
	t.Helper()
	requestJSON := `{"strategy_key":"test","tree":{"type":"leaf","factor":"close","op":">","value":0}}`
	result := ScanResult{Strategy: "测试扫描", TradeDate: "2026-08-08"}
	for _, symbol := range symbols {
		result.Items = append(result.Items, ScanHit{Symbol: symbol, Name: "股票" + symbol, Price: 10})
	}
	result.Matched = len(result.Items)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	row := model.StrategyRunResult{
		UserID: userID, Kind: JobKindScreenerScan, StrategyIdentity: model.StrategyIdentityBuiltin,
		StrategyKey: "test", StrategyRevision: 1, StrategyHash: hashBytes([]byte("strategy")),
		StrategyName: "测试扫描", RequestJSON: requestJSON, RequestHash: sha256Hex([]byte(requestJSON)),
		ResultJSON: string(resultJSON), ContentHash: sha256Hex(resultJSON), Status: model.JobStatusSuccess,
		JobRunID: time.Now().UnixNano(),
	}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatalf("create strategy result: %v", err)
	}
	return row.ID
}

func TestWatchlistBatchIdempotenceAndPartialFailure(t *testing.T) {
	setupTestDB(t)
	userID := int64(88101)
	common.DB.Where("user_id = ?", userID).Delete(&model.WatchlistBatchItem{})
	common.DB.Where("user_id = ?", userID).Delete(&model.WatchlistBatch{})
	common.DB.Where("user_id = ?", userID).Delete(&model.WatchlistItem{})
	common.DB.Where("user_id = ?", userID).Delete(&model.Watchlist{})
	common.DB.Where("user_id = ?", userID).Delete(&model.StrategyRunResult{})
	group := model.Watchlist{UserID: userID, Name: "扫描观察"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.WatchlistItem{
		UserID: userID, WatchlistID: group.ID, Symbol: "000002", Market: "cn", Name: "原有股票",
	}).Error; err != nil {
		t.Fatal(err)
	}
	resultID := persistBatchTestScan(t, userID, "000001", "000002")

	first, err := CreateWatchlistBatch(userID, resultID, WatchlistBatchRequest{
		GroupID: group.ID, Symbols: []string{"000003", "000002", "000001", "000001"},
	})
	if err != nil {
		t.Fatalf("CreateWatchlistBatch: %v", err)
	}
	if first.Requested != 3 || first.Created != 1 || first.Existed != 1 || first.Failed != 1 || len(first.Items) != 3 {
		t.Fatalf("unexpected batch summary: %+v items=%+v", first.WatchlistBatch, first.Items)
	}
	second, err := CreateWatchlistBatch(userID, resultID, WatchlistBatchRequest{
		GroupID: group.ID, Symbols: []string{"000001", "000002", "000003"},
	})
	if err != nil {
		t.Fatalf("repeat CreateWatchlistBatch: %v", err)
	}
	if second.ID != first.ID || second.Created != 1 || second.Existed != 1 || second.Failed != 1 {
		t.Fatalf("repeat did not converge: first=%+v second=%+v", first.WatchlistBatch, second.WatchlistBatch)
	}
	var count int64
	common.DB.Model(&model.WatchlistItem{}).Where("user_id = ? AND watchlist_id = ?", userID, group.ID).Count(&count)
	if count != 2 {
		t.Fatalf("watchlist item count = %d, want 2", count)
	}
}

func TestWatchlistBatchIncompleteAuditBlocksUndo(t *testing.T) {
	setupTestDB(t)
	userID := int64(88104)
	for _, target := range []any{&model.WatchlistBatchItem{}, &model.WatchlistBatch{}, &model.WatchlistItem{}, &model.Watchlist{}, &model.StrategyRunResult{}} {
		if err := common.DB.Where("user_id = ?", userID).Delete(target).Error; err != nil {
			t.Fatal(err)
		}
	}
	group := model.Watchlist{UserID: userID, Name: "审计测试"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	resultID := persistBatchTestScan(t, userID, "000010")
	batch, err := CreateWatchlistBatch(userID, resultID, WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"000010"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Where("batch_id = ? AND user_id = ?", batch.ID, userID).Delete(&model.WatchlistBatchItem{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := UndoWatchlistBatch(userID, batch.ID); err == nil {
		t.Fatal("逐项事实不完整时必须拒绝自动撤销")
	}
	var count int64
	common.DB.Model(&model.WatchlistItem{}).Where("user_id = ? AND watchlist_id = ?", userID, group.ID).Count(&count)
	if count != 1 {
		t.Fatalf("拒绝撤销不得删除自选项，得到 %d", count)
	}
}

func TestWatchlistBatchUndoAndConflict(t *testing.T) {
	setupTestDB(t)
	userID := int64(88102)
	common.DB.Where("user_id = ?", userID).Delete(&model.WatchlistBatchItem{})
	common.DB.Where("user_id = ?", userID).Delete(&model.WatchlistBatch{})
	common.DB.Where("user_id = ?", userID).Delete(&model.WatchlistItem{})
	common.DB.Where("user_id = ?", userID).Delete(&model.Watchlist{})
	common.DB.Where("user_id = ?", userID).Delete(&model.StrategyRunResult{})
	group := model.Watchlist{UserID: userID, Name: "观察"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	resultID := persistBatchTestScan(t, userID, "000001", "000002")
	batch, err := CreateWatchlistBatch(userID, resultID, WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"000001", "000002"}})
	if err != nil {
		t.Fatal(err)
	}
	var editedID int64
	for _, item := range batch.Items {
		if item.Symbol == "000002" {
			editedID = item.WatchlistItemID
		}
	}
	if err := common.DB.Model(&model.WatchlistItem{}).Where("id = ? AND user_id = ?", editedID, userID).Update("note", "用户后续研究记录").Error; err != nil {
		t.Fatal(err)
	}
	undone, err := UndoWatchlistBatch(userID, batch.ID)
	if err != nil {
		t.Fatalf("UndoWatchlistBatch: %v", err)
	}
	if undone.Status != model.WatchlistBatchUndoConflict || undone.Removed != 1 || undone.Conflicts != 1 {
		t.Fatalf("unexpected undo summary: %+v", undone.WatchlistBatch)
	}
	var edited model.WatchlistItem
	if err := common.DB.Where("id = ? AND user_id = ?", editedID, userID).First(&edited).Error; err != nil || edited.Note == "" {
		t.Fatalf("edited item was not preserved: %+v err=%v", edited, err)
	}

	resultID2 := persistBatchTestScan(t, userID, "000003")
	batch2, err := CreateWatchlistBatch(userID, resultID2, WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"000003"}})
	if err != nil {
		t.Fatal(err)
	}
	undone2, err := UndoWatchlistBatch(userID, batch2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if undone2.Status != model.WatchlistBatchUndone || undone2.Removed != 1 || undone2.Conflicts != 0 {
		t.Fatalf("unexpected clean undo: %+v", undone2.WatchlistBatch)
	}
	repeated, err := UndoWatchlistBatch(userID, batch2.ID)
	if err != nil || repeated.Status != model.WatchlistBatchUndone || repeated.Removed != 1 {
		t.Fatalf("repeated undo did not converge: %+v err=%v", repeated, err)
	}
}

func TestWatchlistBatchIsolationAndLimit(t *testing.T) {
	setupTestDB(t)
	userID, otherUserID := int64(88103), int64(88104)
	for _, id := range []int64{userID, otherUserID} {
		common.DB.Where("user_id = ?", id).Delete(&model.WatchlistBatchItem{})
		common.DB.Where("user_id = ?", id).Delete(&model.WatchlistBatch{})
		common.DB.Where("user_id = ?", id).Delete(&model.WatchlistItem{})
		common.DB.Where("user_id = ?", id).Delete(&model.Watchlist{})
		common.DB.Where("user_id = ?", id).Delete(&model.StrategyRunResult{})
	}
	group := model.Watchlist{UserID: userID, Name: "本人分组"}
	otherGroup := model.Watchlist{UserID: otherUserID, Name: "他人分组"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&otherGroup).Error; err != nil {
		t.Fatal(err)
	}
	resultID := persistBatchTestScan(t, userID, "000001")
	batch, err := CreateWatchlistBatch(userID, resultID, WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"000001"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetWatchlistBatch(otherUserID, batch.ID); err == nil {
		t.Fatal("other user read batch")
	}
	if _, err := UndoWatchlistBatch(otherUserID, batch.ID); err == nil {
		t.Fatal("other user undid batch")
	}
	if _, err := CreateWatchlistBatch(otherUserID, resultID, WatchlistBatchRequest{GroupID: otherGroup.ID, Symbols: []string{"000001"}}); err == nil {
		t.Fatal("other user used scan result")
	}
	if _, err := CreateWatchlistBatch(userID, resultID, WatchlistBatchRequest{GroupID: otherGroup.ID, Symbols: []string{"000001"}}); err == nil {
		t.Fatal("user wrote into another user's group")
	}
	many := make([]string, watchlistBatchMaxItems+1)
	for i := range many {
		many[i] = fmt.Sprintf("%06d", i)
	}
	if _, err := CreateWatchlistBatch(userID, resultID, WatchlistBatchRequest{GroupID: group.ID, Symbols: many}); err == nil {
		t.Fatal("expected item limit error")
	}
}
