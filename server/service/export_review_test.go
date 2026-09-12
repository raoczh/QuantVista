package service

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func legacyImportAccount(t *testing.T, userID int64) *model.PortfolioAccount {
	t.Helper()
	account, err := EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestLegacyImportRejectsTruncatedOversizeFile(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1001)
	input := "symbol,buy_price,buy_date,quantity\n600000,10,2026-08-01,100\n" + strings.Repeat("\n", importMaxSize)
	if result, err := (&ExportService{}).ImportPositionsByAccount(account.UserID, account.ID, strings.NewReader(input)); err == nil {
		t.Fatalf("超大文件不能当作正常 EOF 导入前缀：%+v", result)
	}
	var count int64
	if err := common.DB.Model(&model.Position{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("整体拒绝超限文件时不得留下持仓：count=%d err=%v", count, err)
	}
}

func TestLegacyImportUsesPositionNumericAndDateRules(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1002)
	rows := []string{
		"600000,10,2026-08-01,100,0,0",
		"600001,NaN,2026-08-01,100,0,0",
		"600002,10,2026-08-01,Inf,0,0",
		"600003,10,2026-08-01,100,NaN,0",
		"600004,10,2026-08-01,100,0,Inf",
		"600005,10,2026-08-01,0.00001,0,0",
		"600006,1e20,2026-08-01,100,0,0",
		"600007,10," + time.Now().AddDate(0, 0, 1).Format("2006-01-02") + ",100,0,0",
	}
	input := "symbol,buy_price,buy_date,quantity,buy_fee,buy_tax\n" + strings.Join(rows, "\n")
	result, err := (&ExportService{}).ImportPositionsByAccount(account.UserID, account.ID, strings.NewReader(input))
	if err != nil || result.Imported != 1 || len(result.Failed) != len(rows)-1 {
		t.Fatalf("坏数值应逐行拒绝，正常行仍可导入：result=%+v err=%v", result, err)
	}
}

type legacyImportReadHook struct {
	io.Reader
	onRead func()
}

func (r *legacyImportReadHook) Read(p []byte) (int, error) {
	if r.onRead != nil {
		call := r.onRead
		r.onRead = nil
		call()
	}
	return r.Reader.Read(p)
}

func TestLegacyImportRechecksArchiveBeforeCommit(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1003)
	var archiveErr error
	input := &legacyImportReadHook{Reader: strings.NewReader("symbol,buy_price,buy_date,quantity\n600000,10,2026-08-01,100\n"),
		onRead: func() { archiveErr = common.DB.Model(account).Update("status", model.PortfolioStatusArchived).Error }}
	result, err := (&ExportService{}).ImportPositionsByAccount(account.UserID, account.ID, input)
	if archiveErr != nil || err == nil {
		t.Fatalf("解析文件期间归档后不能继续写入：result=%+v archive=%v err=%v", result, archiveErr, err)
	}
}

func TestLegacyImportInvalidatesAffectedSnapshots(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1004)
	for _, date := range []string{"2026-08-01", "2026-08-03"} {
		if err := common.DB.Create(&model.PortfolioSnapshot{UserID: account.UserID, AccountID: account.ID,
			Kind: model.SnapshotKindReal, TradeDate: date, MarketValue: 2000}).Error; err != nil {
			t.Fatal(err)
		}
	}
	result, err := (&ExportService{}).ImportPositionsByAccount(account.UserID, account.ID,
		strings.NewReader("symbol,buy_price,buy_date,quantity\n600000,10,2026-08-02,100\n"))
	if err != nil || result.Imported != 1 {
		t.Fatalf("建仓导入失败：result=%+v err=%v", result, err)
	}
	var snapshots []model.PortfolioSnapshot
	if err := common.DB.Where("account_id = ?", account.ID).Order("trade_date").Find(&snapshots).Error; err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].Partial || !snapshots[1].Partial || snapshots[1].MarketValue != 2000 {
		t.Fatalf("只标记交易日之后的快照，保留原始估值：%+v", snapshots)
	}
}

func TestLegacyImportSnapshotFailureRollsBackLedger(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1005)
	fault := errors.New("历史快照标记失败")
	const callback = "review_legacy_import_snapshot_failure"
	if err := common.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_snapshots" {
			tx.AddError(fault)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Update().Remove(callback) })
	if _, err := (&ExportService{}).ImportPositionsByAccount(account.UserID, account.ID,
		strings.NewReader("symbol,buy_price,buy_date,quantity\n600000,10,2026-08-02,100\n")); !errors.Is(err, fault) {
		t.Fatalf("快照标记失败必须返回并回滚账本：%v", err)
	}
	for _, table := range []any{&model.Position{}, &model.PositionTrade{}} {
		var count int64
		if err := common.DB.Model(table).Where("account_id = ?", account.ID).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%T 不得部分提交：count=%d err=%v", table, count, err)
		}
	}
}
