package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestImportPreviewRejectsUnsavableLedgerAmounts(t *testing.T) {
	for _, kind := range []string{model.ImportKindPosition, model.ImportKindTrade} {
		t.Run(kind, func(t *testing.T) {
			resetImportTestData(t)
			svc := NewDataImportService()
			uploaded, err := svc.Upload(1181, kind, "amount.csv", strings.NewReader("symbol,side,price,quantity,trade_date\n600901,buy,80000000,200000000,2026-08-01\n"))
			if err != nil {
				t.Fatal(err)
			}
			preview := previewWithSuggestions(t, svc, 1181, uploaded, 0)
			if preview.ValidRows != 0 || preview.ErrorRows+preview.ConflictRows != 1 {
				t.Fatalf("单项可保存但成交总额超过 decimal(20,4) 时必须拒绝：%+v", preview.Rows)
			}
		})
	}
}

func TestPositionLedgerRejectsAccumulatedOverflow(t *testing.T) {
	initial := positionLedger{AvgCost: 100000000, Quantity: 60000000, TotalBuyQty: 60000000, TotalBuyCost: 6e15, RemainingCost: 6e15}
	if _, err := ledgerBuy(initial, 100000000, 60000000, 0, 0); err == nil {
		t.Error("每笔成交都可保存，但累计成本溢出时仍允许加仓")
	}
	initial.TotalSellNet, initial.RealizedPnl = 9e15, 9e15
	if _, _, err := ledgerSell(initial, 300000000, 30000000, 0, 0); err == nil {
		t.Error("累计卖出净额或已实现盈亏溢出时仍允许减仓")
	}
}

func TestMySQLImportDetailsKeepPreviewVersionAndRowsTogether(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.ImportBatch{}, &model.ImportRow{})
	batch := model.ImportBatch{ID: "preview-read", UserID: 1182, Kind: model.ImportKindPosition, Version: 2,
		Status: model.ImportStatusPreviewed, TotalRows: 1, ValidRows: 1, HeaderJSON: `["symbol","price"]`, MappingJSON: `{"price":"price"}`}
	row := model.ImportRow{BatchID: batch.ID, UserID: batch.UserID, RowNumber: 2, Status: model.ImportRowValid,
		RawJSON: `["600901","4.0375"]`, NormalizedJSON: `{"symbol":"600901","market":"cn","price":4.0375}`}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	const callback = "review_import_detail_between_batch_and_rows"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if changed || tx.Statement.Table != "import_batches" {
			return
		}
		changed = true
		if err := db.Transaction(func(writer *gorm.DB) error {
			if err := writer.Model(&batch).Updates(map[string]any{"version": 3, "mapping_json": `{"price":"quantity"}`}).Error; err != nil {
				return err
			}
			return writer.Model(&row).Update("normalized_json", `{"symbol":"600901","market":"cn","price":100}`).Error
		}); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	view, err := NewDataImportService().Get(batch.UserID, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || view.Version != 2 || len(view.Rows) != 1 || view.Rows[0].Normalized["price"] != 4.0375 {
		t.Fatalf("旧版本与旧映射必须配套旧预检行，不能混入另一页的新预检：%+v", view)
	}
	var stored model.ImportBatch
	if err := db.First(&stored, "id = ?", batch.ID).Error; err != nil || stored.Version != 3 {
		t.Fatalf("交错预检必须确实提交：version=%d err=%v", stored.Version, err)
	}
}

func TestMySQLImportConfirmCancelsWhileWaitingForAccount(t *testing.T) {
	db := setupMySQLReviewDB(t, model.AllModels()...)
	account := model.PortfolioAccount{UserID: 1183, Kind: model.PortfolioKindReal, Name: "导入取消隔离账户", Status: model.PortfolioStatusActive, Currency: "CNY"}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewDataImportService()
	uploaded, err := svc.UploadByAccount(account.UserID, account.ID, "position", "cancel.csv", strings.NewReader("symbol,price,quantity,trade_date\n600901,4.0375,100,2026-08-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, account.UserID, uploaded, 0)
	writer := db.Begin()
	if writer.Error != nil {
		t.Fatal(writer.Error)
	}
	t.Cleanup(func() { writer.Rollback() })
	var locked model.PortfolioAccount
	if err := writer.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	const callback = "review_import_confirm_account_wait"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_accounts" {
			once.Do(func() { close(waiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := svc.Confirm(ctx, account.UserID, preview.ID, ImportConfirmInput{Version: preview.Version})
		done <- err
	}()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("没有进入账户锁等待：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("没有进入账户锁等待")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("取消原因丢失：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("取消请求后仍在等待账户锁")
		writer.Rollback()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("释放账户锁后取消请求仍继续提交：%v", err)
		}
	}
	writer.Rollback()
	var count int64
	if err := db.Model(&model.Position{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("取消确认不应新增持仓：count=%d err=%v", count, err)
	}
}

func TestImportResponseFailureCannotLeaveCommittedBusinessRows(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	uploaded, err := svc.Upload(1184, "position", "response.csv", strings.NewReader("symbol,price,quantity,trade_date\n600901,4.0375,100,2026-08-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1184, uploaded, 0)
	const callback = "review_import_response_read_failure"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "import_batches" {
			return
		}
		if batch, ok := tx.Statement.Dest.(*model.ImportBatch); ok && batch.Status == model.ImportStatusConfirmed {
			tx.AddError(errors.New("本地注入返回详情读取失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	_, err = svc.Confirm(t.Context(), 1184, preview.ID, ImportConfirmInput{Version: preview.Version})
	var count int64
	if e := common.DB.Model(&model.Position{}).Where("user_id = ?", 1184).Count(&count).Error; e != nil {
		t.Fatal(e)
	}
	if err != nil && count != 0 {
		t.Fatalf("接口返回失败却已经新增 %d 笔持仓：%v", count, err)
	}
	var rows []model.ImportRow
	if err := common.DB.Where("batch_id = ?", preview.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var normalized map[string]any
	if len(rows) != 1 || json.Unmarshal([]byte(rows[0].NormalizedJSON), &normalized) != nil || normalized["price"] != 4.0375 {
		t.Fatal("故障后不能破坏原先可重试的预检内容")
	}
}
