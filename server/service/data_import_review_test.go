package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestImportRechecksSymbolWhenReusingPositionState(t *testing.T) {
	resetImportTestData(t)
	p := seedHoldingWithLedger(t, 971, "600001", 10, 100, 0, 0, "2026-07-01")
	svc := NewDataImportService()
	raw := fmt.Sprintf("position_id,symbol,side,quantity,price,trade_date\n%d,600001,buy,10,11,2026-08-01\n%d,600002,buy,20,12,2026-08-02\n", p.ID, p.ID)
	uploaded, err := svc.Upload(971, "trade", "symbols.csv", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 971, uploaded, 0)
	if preview.ConflictRows != 1 || preview.Rows[1].ErrorCode != "position_mismatch" {
		t.Fatalf("同一持仓 ID 后续行的股票也必须逐行核对：%+v", preview.Rows)
	}
}

func TestImportRejectsOldPreviewWithWrongSymbol(t *testing.T) {
	resetImportTestData(t)
	p := seedHoldingWithLedger(t, 972, "600001", 10, 100, 0, 0, "2026-07-01")
	svc := NewDataImportService()
	raw := fmt.Sprintf("position_id,symbol,side,quantity,price,trade_date\n%d,600001,buy,10,11,2026-08-01\n%d,600001,buy,20,12,2026-08-02\n", p.ID, p.ID)
	uploaded, err := svc.Upload(972, "trade", "legacy-preview.csv", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 972, uploaded, 0)
	var row model.ImportRow
	if err := common.DB.Where("batch_id = ?", preview.ID).Order("row_number DESC").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	// 复刻升级前已冻结的错误预检：同一 position_id 的后一行属于另一只股票。
	if err := common.DB.Model(&row).Update("normalized_json", strings.ReplaceAll(row.NormalizedJSON, "600001", "600002")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Confirm(context.Background(), 972, preview.ID, ImportConfirmInput{Version: preview.Version}); err == nil {
		t.Error("确认阶段不能继续执行旧版本冻结的错配标的")
	}
	var after model.Position
	if err := common.DB.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Quantity != 100 {
		t.Fatalf("错配批次应整体回滚，实际数量 %v", after.Quantity)
	}
}

func TestImportRejectsValuesRoundedToZeroAndKeepsCSVLineNumbers(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	raw := "symbol,name,price,quantity,trade_date\n600001,\"跨\n行名称\",10,0.00001,2026-08-01\n\n600002,样本,0.00001,100,2026-08-01\n"
	uploaded, err := svc.Upload(973, "position", "precision.csv", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Rows[0].Row != 2 || uploaded.Rows[1].Row != 5 {
		t.Errorf("行号必须对应原 CSV：%+v", uploaded.Rows)
	}
	preview := previewWithSuggestions(t, svc, 973, uploaded, 0)
	if preview.ErrorRows != 2 || preview.ValidRows != 0 {
		t.Fatalf("四位精度归零的价格或数量不能通过预检：%+v", preview.Rows)
	}
}

type importReviewReader struct {
	io.Reader
	beforeRead func()
}

func (r *importReviewReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		hook := r.beforeRead
		r.beforeRead = nil
		hook()
	}
	return r.Reader.Read(p)
}

func TestImportUploadRechecksAccountAfterReadingFile(t *testing.T) {
	resetImportTestData(t)
	accounts := NewPortfolioAccountService()
	if _, err := accounts.Create(974, PortfolioAccountInput{Name: "默认账户", Kind: "real"}); err != nil {
		t.Fatal(err)
	}
	account, err := accounts.Create(974, PortfolioAccountInput{Name: "临时账户", Kind: "real"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &importReviewReader{Reader: strings.NewReader("symbol,price,quantity,trade_date\n600001,10,100,2026-08-01\n"), beforeRead: func() {
		if err := accounts.Delete(974, account.ID); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := NewDataImportService().UploadByAccount(974, account.ID, "position", "race.csv", reader); err == nil {
		t.Error("读取文件期间账户已删除，不得创建导入批次")
	}
	var count int64
	if err := common.DB.Model(&model.ImportBatch{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("留下已删除账户的导入批次：%d", count)
	}
}

func TestImportHistoryFiltersAccountBeforeLimit(t *testing.T) {
	resetImportTestData(t)
	accounts := NewPortfolioAccountService()
	first, err := accounts.Create(975, PortfolioAccountInput{Name: "账户 A", Kind: "real"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := accounts.Create(975, PortfolioAccountInput{Name: "账户 B", Kind: "real"})
	if err != nil {
		t.Fatal(err)
	}
	rows := []model.ImportBatch{
		{ID: "a", UserID: 975, AccountID: first.ID, Kind: "position", FileDigest: "a"},
		{ID: "b", UserID: 975, AccountID: second.ID, Kind: "position", FileDigest: "b"},
		{ID: "watch", UserID: 975, Kind: "watchlist", FileDigest: "watch"},
		{ID: "other-watch", UserID: 976, Kind: "watchlist", FileDigest: "other"},
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	history, err := NewDataImportService().ListByAccount(975, first.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("仅能返回 A 账户与本人自选批次：%+v", history)
	}
	for _, batch := range history {
		if batch.ID != "a" && batch.ID != "watch" {
			t.Fatalf("混入其他账户或他人批次：%+v", batch)
		}
	}
}
