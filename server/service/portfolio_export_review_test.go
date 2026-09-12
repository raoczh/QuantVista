package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestCashFlowCannotSaveAmountRoundedToZero(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1185)
	for i, input := range []CashFlowInput{
		{Type: model.CashFlowDeposit, Amount: 0.00001},
		{Type: model.CashFlowWithdrawal, Amount: -0.00001},
		{Type: model.CashFlowFeeAdjustment, Amount: -0.00001},
	} {
		input.TradeDate, input.IdempotencyKey = "2026-08-01", fmt.Sprintf("tiny-%d", i)
		if _, err := CreatePortfolioCashFlow(account.UserID, account.ID, input); err == nil {
			t.Errorf("%s 的极小金额被保存成零现金流", input.Type)
		}
	}
	var count int64
	if err := common.DB.Model(&model.PortfolioCashFlow{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("不得留下零金额流水：count=%d err=%v", count, err)
	}
	if row, err := CreatePortfolioCashFlow(account.UserID, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 0.0001, TradeDate: "2026-08-01", IdempotencyKey: "minimum"}); err != nil || row.Amount != 0.0001 {
		t.Fatalf("四位精度有效金额仍可保存：row=%+v err=%v", row, err)
	}
}

func TestExportLimitCannotSilentlyReturnIncompleteFile(t *testing.T) {
	for i, tc := range []struct {
		kind  string
		table string
	}{
		{"positions", "positions"}, {"watchlist", "watchlist_items"},
		{"recommendations", "recommendations"}, {"analyses", "analysis_records"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			setupTestDB(t)
			userID := int64(1186 + i)
			account := legacyImportAccount(t, userID)
			rows := make([]map[string]any, exportMaxRows+1)
			for i := range rows {
				rows[i] = map[string]any{"user_id": userID, "symbol": fmt.Sprintf("%06d", 600000+i)}
				if tc.kind == "positions" {
					rows[i]["account_id"] = account.ID
				}
			}
			if err := common.DB.Table(tc.table).CreateInBatches(rows, 200).Error; err != nil {
				t.Fatal(err)
			}
			data, _, err := NewExportService().ExportByAccount(userID, account.ID, tc.kind)
			if err == nil || len(data) != 0 || !strings.Contains(err.Error(), "5000") {
				t.Fatalf("超过单次上限时不能静默交付缺行文件：bytes=%d err=%v", len(data), err)
			}
		})
	}
}

func TestMySQLWatchlistExportKeepsGroupsAndItemsTogether(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.Watchlist{}, &model.WatchlistItem{})
	group := model.Watchlist{UserID: 1190, Name: "原分组名称"}
	if err := db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	item := model.WatchlistItem{UserID: group.UserID, WatchlistID: group.ID, Market: "cn", Symbol: "600901", Name: "原自选"}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	const callback = "review_export_between_group_and_items"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if changed || tx.Statement.Table != "watchlists" {
			return
		}
		changed = true
		if err := db.Transaction(func(writer *gorm.DB) error {
			if err := writer.Model(&group).Update("name", "新分组名称").Error; err != nil {
				return err
			}
			return writer.Create(&model.WatchlistItem{UserID: group.UserID, WatchlistID: group.ID, Market: "cn", Symbol: "600902", Name: "并发新增"}).Error
		}); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	data, _, err := NewExportService().Export(group.UserID, "watchlist")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(rows) != 2 || rows[1][0] != "原分组名称" || rows[1][1] != "600901" {
		t.Fatalf("不能把改名前的分组名与改名后的新增自选拼接到一个导出：%+v", rows)
	}
}

func TestLegacyImportRejectsInvalidTextEncoding(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1191)
	for _, text := range []string{"\xff\xfe", "包含\x00空字节"} {
		input := "symbol,buy_price,buy_date,quantity,reason\n600901,4.0375,2026-08-01,100," + text + "\n"
		if _, err := NewExportService().ImportPositionsByAccount(account.UserID, account.ID, strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Errorf("编码有损时应整体拒绝导入：%v", err)
		}
	}
	var count int64
	if err := common.DB.Model(&model.Position{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("编码错误不得先落部分数据：count=%d err=%v", count, err)
	}
}

func TestLegacyImportCancellationDuringReadCannotCreatePositions(t *testing.T) {
	setupTestDB(t)
	account := legacyImportAccount(t, 1192)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &legacyImportReadHook{Reader: strings.NewReader("symbol,buy_price,buy_date,quantity\n600901,4.0375,2026-08-01,100\n"), onRead: cancel}
	if _, err := NewExportService().ImportPositionsByAccountContext(ctx, account.UserID, account.ID, reader); !errors.Is(err, context.Canceled) {
		t.Errorf("读取期间取消必须返回取消原因：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.Position{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("取消读取不得继续新增持仓：count=%d err=%v", count, err)
	}
}
