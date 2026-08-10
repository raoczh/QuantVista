package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const watchlistBatchMaxItems = 100

var watchlistBatchMu sync.Mutex

type WatchlistBatchRequest struct {
	GroupID int64    `json:"group_id"`
	Symbols []string `json:"symbols"`
}

type WatchlistBatchView struct {
	model.WatchlistBatch
	Items []model.WatchlistBatchItem `json:"items"`
}

type watchlistBatchIdentity struct {
	ResultID int64    `json:"result_id"`
	GroupID  int64    `json:"group_id"`
	Symbols  []string `json:"symbols"`
}

type watchlistBatchFingerprint struct {
	ID            int64   `json:"id"`
	UserID        int64   `json:"user_id"`
	WatchlistID   int64   `json:"watchlist_id"`
	Symbol        string  `json:"symbol"`
	Market        string  `json:"market"`
	Name          string  `json:"name"`
	Note          string  `json:"note"`
	FocusReason   string  `json:"focus_reason"`
	IsPinned      bool    `json:"is_pinned"`
	ResearchStage string  `json:"research_stage"`
	PassedReason  string  `json:"passed_reason"`
	PassedPrice   float64 `json:"passed_price"`
	StageAt       string  `json:"stage_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func batchWatchlistFingerprint(item model.WatchlistItem) string {
	stageAt := ""
	if item.StageAt != nil {
		stageAt = item.StageAt.UTC().Format(time.RFC3339Nano)
	}
	fact := watchlistBatchFingerprint{
		ID: item.ID, UserID: item.UserID, WatchlistID: item.WatchlistID,
		Symbol: item.Symbol, Market: item.Market, Name: item.Name,
		Note: item.Note, FocusReason: item.FocusReason, IsPinned: item.IsPinned,
		ResearchStage: item.ResearchStage, PassedReason: item.PassedReason,
		PassedPrice: item.PassedPrice, StageAt: stageAt,
		UpdatedAt: item.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	b, _ := json.Marshal(fact)
	return hashBytes(b)
}

func normalizeBatchSymbols(input []string) ([]string, error) {
	if len(input) == 0 {
		return nil, errors.New("请至少选择一只股票")
	}
	if len(input) > watchlistBatchMaxItems {
		return nil, fmt.Errorf("单次最多选择 %d 只股票", watchlistBatchMaxItems)
	}
	seen := make(map[string]struct{}, len(input))
	for _, raw := range input {
		symbol := strings.ToUpper(strings.TrimSpace(raw))
		if symbol == "" {
			continue
		}
		seen[symbol] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, errors.New("请至少选择一只有效股票")
	}
	out := make([]string, 0, len(seen))
	for symbol := range seen {
		out = append(out, symbol)
	}
	sort.Strings(out)
	return out, nil
}

func watchlistBatchRequestHash(resultID, groupID int64, symbols []string) string {
	b, _ := json.Marshal(watchlistBatchIdentity{ResultID: resultID, GroupID: groupID, Symbols: symbols})
	return hashBytes(b)
}

func loadSuccessfulScanResult(userID, resultID int64) (*ScanResult, error) {
	view, err := GetStrategyRun(userID, JobKindScreenerScan, resultID)
	if err != nil {
		return nil, errors.New("扫描结果不存在")
	}
	if view.Status != model.JobStatusSuccess || len(view.Result) == 0 {
		return nil, errors.New("扫描结果尚未成功完成")
	}
	var result ScanResult
	if err := json.Unmarshal(view.Result, &result); err != nil {
		return nil, errors.New("扫描结果无法读取")
	}
	return &result, nil
}

func loadWatchlistBatch(tx *gorm.DB, userID int64, batchID string) (*WatchlistBatchView, error) {
	var batch model.WatchlistBatch
	if err := tx.Where("id = ? AND user_id = ?", batchID, userID).First(&batch).Error; err != nil {
		return nil, errors.New("批量操作记录不存在")
	}
	var items []model.WatchlistBatchItem
	if err := tx.Where("batch_id = ? AND user_id = ?", batch.ID, userID).Order("id").Find(&items).Error; err != nil {
		return nil, err
	}
	if items == nil {
		items = []model.WatchlistBatchItem{}
	}
	return &WatchlistBatchView{WatchlistBatch: batch, Items: items}, nil
}

func GetWatchlistBatch(userID int64, batchID string) (*WatchlistBatchView, error) {
	if common.DB == nil || userID <= 0 || strings.TrimSpace(batchID) == "" {
		return nil, errors.New("批量操作记录不存在")
	}
	return loadWatchlistBatch(common.DB, userID, strings.TrimSpace(batchID))
}

// CreateWatchlistBatch 把当前用户某次成功扫描的选中结果加入本人分组。全部业务写入
// 与逐项审计在一个事务内完成；不在结果中的代码作为逐项失败保存，不信任前端名称。
func CreateWatchlistBatch(userID, resultID int64, req WatchlistBatchRequest) (*WatchlistBatchView, error) {
	if common.DB == nil || userID <= 0 || resultID <= 0 {
		return nil, errors.New("扫描结果不存在")
	}
	if req.GroupID <= 0 {
		return nil, errors.New("请选择自选分组")
	}
	symbols, err := normalizeBatchSymbols(req.Symbols)
	if err != nil {
		return nil, err
	}
	scan, err := loadSuccessfulScanResult(userID, resultID)
	if err != nil {
		return nil, err
	}
	hits := make(map[string]ScanHit, len(scan.Items))
	for _, hit := range scan.Items {
		hits[strings.ToUpper(strings.TrimSpace(hit.Symbol))] = hit
	}
	requestHash := watchlistBatchRequestHash(resultID, req.GroupID, symbols)

	watchlistBatchMu.Lock()
	defer watchlistBatchMu.Unlock()

	batchID, err := newImportID()
	if err != nil {
		return nil, errors.New("无法创建批量操作记录")
	}
	batch := model.WatchlistBatch{
		ID: batchID, UserID: userID, ResultID: resultID, GroupID: req.GroupID,
		RequestHash: requestHash, Status: model.WatchlistBatchApplied, Requested: len(symbols),
	}
	var view *WatchlistBatchView
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		var group model.Watchlist
		if err := tx.Where("id = ? AND user_id = ?", req.GroupID, userID).First(&group).Error; err != nil {
			return errors.New("自选分组不存在")
		}
		created := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "request_hash"}},
			DoNothing: true,
		}).Create(&batch)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			var existingBatch model.WatchlistBatch
			if err := tx.Select("id").Where("user_id = ? AND request_hash = ?", userID, requestHash).First(&existingBatch).Error; err != nil {
				return err
			}
			existing, err := loadWatchlistBatch(tx, userID, existingBatch.ID)
			if err != nil {
				return err
			}
			view = existing
			return nil
		}

		for _, symbol := range symbols {
			hit, ok := hits[symbol]
			itemFact := model.WatchlistBatchItem{
				UserID: userID, BatchID: batch.ID, Symbol: symbol, Market: "cn",
			}
			if !ok {
				itemFact.Status = model.WatchlistBatchItemFailed
				itemFact.ErrorCode = "not_in_result"
				itemFact.Message = "该股票不在本次扫描结果中"
				batch.Failed++
				if err := tx.Create(&itemFact).Error; err != nil {
					return err
				}
				continue
			}
			itemFact.Name = truncateRunes(strings.TrimSpace(hit.Name), 64)
			var existing model.WatchlistItem
			err := tx.Where(
				"user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?",
				userID, req.GroupID, symbol, "cn",
			).First(&existing).Error
			if err == nil {
				itemFact.WatchlistItemID = existing.ID
				itemFact.Status = model.WatchlistBatchItemExisted
				batch.Existed++
				if err := tx.Create(&itemFact).Error; err != nil {
					return err
				}
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			newItem := model.WatchlistItem{
				UserID: userID, WatchlistID: req.GroupID, Symbol: symbol,
				Market: "cn", Name: itemFact.Name,
			}
			inserted := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "user_id"}, {Name: "watchlist_id"}, {Name: "symbol"}, {Name: "market"}},
				DoNothing: true,
			}).Create(&newItem)
			if inserted.Error != nil {
				return inserted.Error
			}
			if inserted.RowsAffected == 0 {
				if err := tx.Where(
					"user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?",
					userID, req.GroupID, symbol, "cn",
				).First(&newItem).Error; err != nil {
					return err
				}
				itemFact.Status = model.WatchlistBatchItemExisted
				batch.Existed++
			} else {
				// 用数据库回读值（尤其是时间精度）生成撤销指纹。
				if err := tx.Where("id = ? AND user_id = ?", newItem.ID, userID).First(&newItem).Error; err != nil {
					return err
				}
				itemFact.Status = model.WatchlistBatchItemCreated
				itemFact.AfterHash = batchWatchlistFingerprint(newItem)
				batch.Created++
			}
			itemFact.WatchlistItemID = newItem.ID
			if err := tx.Create(&itemFact).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&model.WatchlistBatch{}).Where("id = ? AND user_id = ?", batch.ID, userID).Updates(map[string]any{
			"created": batch.Created, "existed": batch.Existed, "failed": batch.Failed,
		}).Error; err != nil {
			return err
		}
		if batch.Created > 0 || batch.Existed > 0 {
			if err := setOnboardingStepTx(tx, userID, OnboardingStepPortfolio, model.OnboardingStepCompleted, 0); err != nil {
				return err
			}
		}
		loaded, err := loadWatchlistBatch(tx, userID, batch.ID)
		if err != nil {
			return err
		}
		view = loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

// UndoWatchlistBatch 只删除本批实际创建且此后未发生任何更新的条目。已有项和失败项
// 永不受影响；任何缺失、移动、备注或阶段修改都转为逐项冲突并保留业务数据。
func UndoWatchlistBatch(userID int64, batchID string) (*WatchlistBatchView, error) {
	if common.DB == nil || userID <= 0 || strings.TrimSpace(batchID) == "" {
		return nil, errors.New("批量操作记录不存在")
	}
	watchlistBatchMu.Lock()
	defer watchlistBatchMu.Unlock()

	var view *WatchlistBatchView
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var lockedBatch model.WatchlistBatch
		query := tx.Where("id = ? AND user_id = ?", strings.TrimSpace(batchID), userID)
		if !common.UsingSQLite {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&lockedBatch).Error; err != nil {
			return errors.New("批量操作记录不存在")
		}
		current, err := loadWatchlistBatch(tx, userID, strings.TrimSpace(batchID))
		if err != nil {
			return err
		}
		if current.Status == model.WatchlistBatchUndone {
			view = current
			return nil
		}
		if len(current.Items) != current.Requested || current.Created+current.Existed+current.Failed != current.Requested {
			return errors.New("批量操作审计事实不完整，已拒绝自动撤销")
		}
		removed, conflicts := 0, 0
		for i := range current.Items {
			if current.Items[i].Status == model.WatchlistBatchItemRemoved {
				removed++
			}
		}
		for i := range current.Items {
			fact := &current.Items[i]
			if fact.Status != model.WatchlistBatchItemCreated && fact.Status != model.WatchlistBatchItemConflict {
				continue
			}
			var item model.WatchlistItem
			query := tx.Where("id = ? AND user_id = ?", fact.WatchlistItemID, userID)
			if !common.UsingSQLite {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			err := query.First(&item).Error
			if err != nil {
				fact.Status = model.WatchlistBatchItemConflict
				fact.ErrorCode = "item_missing"
				fact.Message = "本批创建的自选项已不存在，无法自动撤销"
				conflicts++
			} else if batchWatchlistFingerprint(item) != fact.AfterHash {
				fact.Status = model.WatchlistBatchItemConflict
				fact.ErrorCode = "item_modified"
				fact.Message = "自选项在批量加入后已被修改，未自动删除"
				conflicts++
			} else {
				res := tx.Where("id = ? AND user_id = ?", item.ID, userID).Delete(&model.WatchlistItem{})
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					return errors.New("撤销期间自选项发生变化，请重试")
				}
				fact.Status = model.WatchlistBatchItemRemoved
				fact.ErrorCode, fact.Message = "", ""
				removed++
			}
			if err := tx.Model(&model.WatchlistBatchItem{}).Where("id = ? AND user_id = ?", fact.ID, userID).Updates(map[string]any{
				"status": fact.Status, "error_code": fact.ErrorCode, "message": fact.Message,
			}).Error; err != nil {
				return err
			}
		}
		now := time.Now()
		status := model.WatchlistBatchUndone
		if conflicts > 0 {
			status = model.WatchlistBatchUndoConflict
		}
		if err := tx.Model(&model.WatchlistBatch{}).Where("id = ? AND user_id = ?", current.ID, userID).Updates(map[string]any{
			"status": status, "removed": removed, "conflicts": conflicts, "undone_at": now,
		}).Error; err != nil {
			return err
		}
		view, err = loadWatchlistBatch(tx, userID, current.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}
