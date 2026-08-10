package service

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func resetImportTestData(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	for _, table := range []string{
		"import_effects", "import_row_claims", "import_rows", "import_batches", "position_trades", "positions",
		"watchlist_items", "watchlists", "alert_events", "position_corp_adjusts", "sell_reviews",
	} {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理 %s: %v", table, err)
		}
	}
}

func previewWithSuggestions(t *testing.T, svc *DataImportService, userID int64, batch *ImportBatchView, groupID int64) *ImportBatchView {
	t.Helper()
	view, err := svc.Preview(userID, batch.ID, ImportMappingInput{Version: batch.Version, Mapping: batch.Suggestions, TargetGroupID: groupID})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	return view
}

func TestDataImportPositionFrozenPreviewConfirmIdempotentAndRollback(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	csvText := "\ufeff股票代码,市场,持仓类型,买入价格,数量,买入日期,手续费,税费,买入理由\n" +
		"600000,cn,长线,8.5,1000,2026-08-01,5,0,统一导入测试\n"
	uploaded, err := svc.Upload(1, model.ImportKindPosition, "positions.csv", strings.NewReader(csvText))
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if uploaded.Status != model.ImportStatusUploaded || uploaded.TotalRows != 1 || uploaded.Suggestions["price"] != "买入价格" {
		t.Fatalf("上传识别错误: %+v", uploaded)
	}
	var businessCount int64
	common.DB.Model(&model.Position{}).Where("user_id = ?", 1).Count(&businessCount)
	if businessCount != 0 {
		t.Fatal("上传阶段不得写业务数据")
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	if preview.Status != model.ImportStatusPreviewed || preview.ValidRows != 1 || preview.ErrorRows != 0 {
		t.Fatalf("预检结果错误: %+v", preview)
	}
	common.DB.Model(&model.Position{}).Where("user_id = ?", 1).Count(&businessCount)
	if businessCount != 0 {
		t.Fatal("只读预检不得写业务数据")
	}

	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if confirmed.Status != model.ImportStatusConfirmed || confirmed.CreatedRows != 1 {
		t.Fatalf("确认摘要错误: %+v", confirmed)
	}
	var p model.Position
	if err := common.DB.Where("user_id = ?", 1).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	if p.Symbol != "600000" || p.RemainingCost != 8505 || p.TotalBuyCost != 8505 || p.Quantity != 1000 {
		t.Fatalf("持仓账本聚合错误: %+v", p)
	}
	var trades []model.PositionTrade
	common.DB.Where("position_id = ? AND user_id = ?", p.ID, 1).Find(&trades)
	if len(trades) != 1 || trades[0].Side != model.PositionTradeBuy || trades[0].QuantityAfter != 1000 {
		t.Fatalf("导入必须复用 PositionTrade 单一流水: %+v", trades)
	}
	// 重复确认即使携带旧版本也应收敛到同一已确认事实，不产生第二套数据。
	if _, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version}); err != nil {
		t.Fatalf("重复确认应幂等: %v", err)
	}
	common.DB.Model(&model.Position{}).Where("user_id = ?", 1).Count(&businessCount)
	if businessCount != 1 {
		t.Fatalf("重复确认产生重复持仓: %d", businessCount)
	}
	// 重复上传按 user+kind+文件摘要返回原批次。
	duplicate, err := svc.Upload(1, model.ImportKindPosition, "rename.csv", strings.NewReader(csvText))
	if err != nil || duplicate.ID != confirmed.ID || duplicate.Status != model.ImportStatusConfirmed {
		t.Fatalf("重复上传未安全收敛: %v %+v", err, duplicate)
	}
	if _, err := svc.Get(2, confirmed.ID); err == nil {
		t.Fatal("跨用户批次 ID 必须不可见")
	}

	rolled, err := svc.Rollback(1, confirmed.ID)
	if err != nil || rolled.Status != model.ImportStatusRolledBack || len(rolled.Conflicts) != 0 {
		t.Fatalf("回滚失败: %v %+v", err, rolled)
	}
	common.DB.Model(&model.Position{}).Where("id = ?", p.ID).Count(&businessCount)
	if businessCount != 0 {
		t.Fatal("无依赖的新建持仓应被本批回滚")
	}
	var effectCount int64
	common.DB.Model(&model.ImportEffect{}).Where("batch_id = ?", confirmed.ID).Count(&effectCount)
	if effectCount != 2 {
		t.Fatalf("回滚后必须保留审计效果事实，得到 %d", effectCount)
	}
	var claimCount int64
	common.DB.Model(&model.ImportRowClaim{}).Where("batch_id = ?", confirmed.ID).Count(&claimCount)
	if claimCount != 0 {
		t.Fatalf("成功回滚后应释放行幂等声明，得到 %d", claimCount)
	}
	// 回滚后保留旧审计批次，但相同文件可以建立下一次尝试；未回滚时仍收敛到同一批次。
	reuploaded, err := svc.Upload(1, model.ImportKindPosition, "positions.csv", strings.NewReader(csvText))
	if err != nil {
		t.Fatalf("回滚后重新上传失败: %v", err)
	}
	if reuploaded.ID == confirmed.ID || reuploaded.Attempt != confirmed.Attempt+1 || reuploaded.Status != model.ImportStatusUploaded {
		t.Fatalf("回滚后应创建下一次审计尝试: old=%+v new=%+v", confirmed.ImportBatch, reuploaded.ImportBatch)
	}
	repreview := previewWithSuggestions(t, svc, 1, reuploaded, 0)
	if _, err := svc.Confirm(context.Background(), 1, reuploaded.ID, ImportConfirmInput{Version: repreview.Version}); err != nil {
		t.Fatalf("回滚后相同文件应可重新确认: %v", err)
	}
}

func TestDataImportConcurrentConfirmConverges(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	uploaded, err := svc.Upload(1, "position", "concurrent.csv", strings.NewReader("symbol,price,quantity,trade_date\n600036,9.9,200,2026-08-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errs[index] = svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("并发确认应收敛成功: %v", err)
		}
	}
	var count int64
	common.DB.Model(&model.Position{}).Where("user_id = ?", 1).Count(&count)
	if count != 1 {
		t.Fatalf("并发确认产生 %d 条持仓", count)
	}
}

func TestDataImportTradeUsesLedgerAndRollbackConflict(t *testing.T) {
	resetImportTestData(t)
	position := model.Position{UserID: 1, Symbol: "600519", Market: "cn", Name: "测试", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 100, BuyDate: "2026-07-01", Quantity: 100, TotalBuyCost: 10000, TotalBuyQty: 100, RemainingCost: 10000}
	if err := common.DB.Create(&position).Error; err != nil {
		t.Fatal(err)
	}
	initial := buildInitialTrade(&position)
	if err := common.DB.Create(&initial).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewDataImportService()
	csvText := "股票代码,市场,买卖方向,成交数量,成交价,成交日期,手续费,税费\n" +
		"600519,cn,买入,100,120,2026-07-10,5,0\n" +
		"600519,cn,卖出,50,130,2026-07-11,3,1\n"
	uploaded, err := svc.Upload(1, "trade", "trades.csv", strings.NewReader(csvText))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	if preview.ValidRows != 2 {
		t.Fatalf("流水预检应全部有效: %+v", preview.Rows)
	}
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.UpdatedRows != 2 || confirmed.CreatedRows != 0 {
		t.Fatalf("流水摘要错误: %+v", confirmed.ImportBatch)
	}
	var after model.Position
	common.DB.First(&after, position.ID)
	if after.Quantity != 150 || after.TotalBuyQty != 200 || after.TotalSellNet != 6496 || after.RealizedPnl <= 0 {
		t.Fatalf("导入流水未走既有费税/加减仓账本: %+v", after)
	}
	// 后续人工流水会改变指纹和流水尾，回滚必须拒绝并列出冲突，不能删用户后续记录。
	ps := NewPositionService(nil)
	if _, err := ps.AddTrade(1, position.ID, PositionTradeInput{Side: "buy", Price: 125, Quantity: 10, TradeDate: "2026-07-12"}); err != nil {
		t.Fatalf("追加人工流水: %v", err)
	}
	conflicted, err := svc.Rollback(1, confirmed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conflicted.Status != "conflict" || len(conflicted.Conflicts) == 0 {
		t.Fatalf("存在后续交易时必须拒绝回滚并列冲突: %+v", conflicted)
	}
	var tradeCount int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", position.ID).Count(&tradeCount)
	if tradeCount != 4 {
		t.Fatalf("拒绝回滚不得删除任何流水，得到 %d", tradeCount)
	}
}

func TestDataImportTradeRollbackRestoresExistingPosition(t *testing.T) {
	resetImportTestData(t)
	position := model.Position{UserID: 1, Symbol: "000001", Market: "cn", Name: "测试", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 10, BuyDate: "2026-07-01", Quantity: 100, TotalBuyCost: 1000, TotalBuyQty: 100, RemainingCost: 1000}
	common.DB.Create(&position)
	initial := buildInitialTrade(&position)
	common.DB.Create(&initial)
	svc := NewDataImportService()
	uploaded, _ := svc.Upload(1, "trade", "restore.csv", strings.NewReader("symbol,side,quantity,price,trade_date,fee,tax\n000001,sell,40,12,2026-07-02,2,1\n"))
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	rolled, err := svc.Rollback(1, confirmed.ID)
	if err != nil || rolled.Status != model.ImportStatusRolledBack {
		t.Fatalf("回滚已有持仓流水: %v %+v", err, rolled)
	}
	var restored model.Position
	common.DB.First(&restored, position.ID)
	if restored.Quantity != 100 || restored.RemainingCost != 1000 || restored.RealizedPnl != 0 || restored.Status != model.PositionStatusHolding {
		t.Fatalf("未恢复导入前账本: %+v", restored)
	}
	var count int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", position.ID).Count(&count)
	if count != 1 {
		t.Fatalf("应仅保留原流水，得到 %d", count)
	}
}

func TestDataImportTradeRollbackRemovesBatchBackfill(t *testing.T) {
	resetImportTestData(t)
	// 复刻升级前只有聚合持仓、没有流水和累计字段的真实状态。
	legacy := model.Position{UserID: 1, Symbol: "000002", Market: "cn", Name: "旧持仓", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 10, BuyDate: "2026-07-01", Quantity: 100}
	common.DB.Create(&legacy)
	svc := NewDataImportService()
	uploaded, _ := svc.Upload(1, "trade", "legacy.csv", strings.NewReader("symbol,side,quantity,price,trade_date\n000002,buy,20,11,2026-07-02\n"))
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	rolled, err := svc.Rollback(1, confirmed.ID)
	if err != nil || rolled.Status != model.ImportStatusRolledBack {
		t.Fatalf("legacy 回滚失败: %v %+v", err, rolled)
	}
	var restored model.Position
	common.DB.First(&restored, legacy.ID)
	if restored.Quantity != 100 || restored.TotalBuyCost != 0 || restored.RemainingCost != 0 {
		t.Fatalf("legacy 聚合态未原样恢复: %+v", restored)
	}
	var count int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", legacy.ID).Count(&count)
	if count != 0 {
		t.Fatalf("本批惰性补建和导入流水都应删除，得到 %d", count)
	}
}

func TestDataImportTradeReopensAsNewLedgerAndRollsBack(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	csvText := "symbol,side,quantity,price,trade_date,fee,tax\n" +
		"600010,buy,100,10,2026-07-01,1,0\n" +
		"600010,sell,100,11,2026-07-02,1,1\n" +
		"600010,buy,50,12,2026-07-03,1,0\n"
	uploaded, err := svc.Upload(1, "trade", "reopen.csv", strings.NewReader(csvText))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	if preview.ValidRows != 3 || preview.ErrorRows != 0 || preview.ConflictRows != 0 {
		t.Fatalf("跨持仓周期流水预检错误: %+v", preview.Rows)
	}
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	var positions []model.Position
	if err := common.DB.Where("user_id = ? AND symbol = ?", 1, "600010").Order("id ASC").Find(&positions).Error; err != nil {
		t.Fatal(err)
	}
	if len(positions) != 2 || positions[0].Status != model.PositionStatusClosed || positions[1].Status != model.PositionStatusHolding || positions[1].Quantity != 50 {
		t.Fatalf("再次买入必须建立新的单一账本持仓: %+v", positions)
	}
	var tradeCount int64
	common.DB.Model(&model.PositionTrade{}).Where("user_id = ? AND position_id IN ?", 1, []int64{positions[0].ID, positions[1].ID}).Count(&tradeCount)
	if tradeCount != 3 {
		t.Fatalf("跨周期应保留三条流水，得到 %d", tradeCount)
	}
	rolled, err := svc.Rollback(1, confirmed.ID)
	if err != nil || rolled.Status != model.ImportStatusRolledBack {
		t.Fatalf("跨周期批次回滚失败: %v %+v", err, rolled)
	}
	var positionCount int64
	common.DB.Model(&model.Position{}).Where("user_id = ? AND symbol = ?", 1, "600010").Count(&positionCount)
	if positionCount != 0 {
		t.Fatalf("回滚必须仅移除本批创建的两个持仓，剩余 %d", positionCount)
	}
}

func TestDataImportTradeExplicitPositionDoesNotHideLaterAmbiguity(t *testing.T) {
	resetImportTestData(t)
	positions := []model.Position{
		{UserID: 1, Symbol: "600012", Market: "cn", Name: "同标的一", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 10, BuyDate: "2026-07-01", Quantity: 100, TotalBuyCost: 1000, TotalBuyQty: 100, RemainingCost: 1000},
		{UserID: 1, Symbol: "600012", Market: "cn", Name: "同标的二", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 12, BuyDate: "2026-07-02", Quantity: 100, TotalBuyCost: 1200, TotalBuyQty: 100, RemainingCost: 1200},
	}
	for i := range positions {
		if err := common.DB.Create(&positions[i]).Error; err != nil {
			t.Fatal(err)
		}
		initial := buildInitialTrade(&positions[i])
		if err := common.DB.Create(&initial).Error; err != nil {
			t.Fatal(err)
		}
	}

	svc := NewDataImportService()
	csvText := "position_id,symbol,market,side,quantity,price,trade_date\n" +
		fmt.Sprintf("%d,600012,cn,buy,10,11,2026-07-03\n", positions[0].ID) +
		",600012,cn,sell,10,13,2026-07-04\n"
	uploaded, err := svc.Upload(1, model.ImportKindTrade, "ambiguous.csv", strings.NewReader(csvText))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	if preview.ValidRows != 1 || preview.ConflictRows != 1 || preview.Rows[1].ErrorCode != "ambiguous_position" {
		t.Fatalf("显式持仓 ID 不得替后续未指定行消除歧义: %+v", preview.Rows)
	}
}

func TestDataImportTradeEditBlocksRollback(t *testing.T) {
	resetImportTestData(t)
	position := model.Position{UserID: 1, Symbol: "000004", Market: "cn", Name: "测试", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 10, BuyDate: "2026-07-01", Quantity: 100, TotalBuyCost: 1000, TotalBuyQty: 100, RemainingCost: 1000}
	if err := common.DB.Create(&position).Error; err != nil {
		t.Fatal(err)
	}
	initial := buildInitialTrade(&position)
	if err := common.DB.Create(&initial).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewDataImportService()
	uploaded, err := svc.Upload(1, "trade", "edited-trade.csv", strings.NewReader("symbol,side,quantity,price,trade_date\n000004,buy,20,11,2026-07-02\n"))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	var imported model.PositionTrade
	if err := common.DB.Where("position_id = ? AND id <> ?", position.ID, initial.ID).First(&imported).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.PositionTrade{}).Where("id = ? AND user_id = ?", imported.ID, 1).Update("note", "人工修订流水").Error; err != nil {
		t.Fatal(err)
	}
	rolled, err := svc.Rollback(1, confirmed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Status != "conflict" || len(rolled.Conflicts) == 0 {
		t.Fatalf("人工修改本批流水后必须拒绝回滚: %+v", rolled)
	}
	var count int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", position.ID).Count(&count)
	if count != 2 {
		t.Fatalf("冲突回滚不得删除流水，得到 %d", count)
	}
}

func TestDataImportIncompleteAuditBlocksRollback(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	uploaded, err := svc.Upload(1, "position", "audit-gap.csv", strings.NewReader("symbol,price,quantity,trade_date\n600011,10,100,2026-08-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Where("batch_id = ? AND user_id = ?", confirmed.ID, 1).Delete(&model.ImportEffect{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rollback(1, confirmed.ID); err == nil || !strings.Contains(err.Error(), "审计事实不完整") {
		t.Fatalf("效果事实不完整时必须拒绝自动回滚: %v", err)
	}
	var count int64
	common.DB.Model(&model.Position{}).Where("user_id = ? AND symbol = ?", 1, "600011").Count(&count)
	if count != 1 {
		t.Fatalf("拒绝回滚不得删除业务数据，得到 %d", count)
	}
}

func TestDataImportRollbackRequiresEffectForEveryClaimedRow(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	uploaded, err := svc.Upload(1, model.ImportKindPosition, "audit-row-gap.csv", strings.NewReader(
		"symbol,price,quantity,trade_date\n600013,10,100,2026-08-01\n600014,11,100,2026-08-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	var secondClaim model.ImportRowClaim
	if err := common.DB.Where("batch_id = ? AND user_id = ?", confirmed.ID, 1).Order("row_number DESC").First(&secondClaim).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Where("batch_id = ? AND user_id = ? AND row_number = ?", confirmed.ID, 1, secondClaim.RowNumber).Delete(&model.ImportEffect{}).Error; err != nil {
		t.Fatal(err)
	}
	var remainingEffects int64
	common.DB.Model(&model.ImportEffect{}).Where("batch_id = ? AND user_id = ?", confirmed.ID, 1).Count(&remainingEffects)
	if remainingEffects < int64(confirmed.TotalRows) {
		t.Fatalf("用例必须覆盖旧的总数校验盲区，剩余效果=%d 总行数=%d", remainingEffects, confirmed.TotalRows)
	}
	if _, err := svc.Rollback(1, confirmed.ID); err == nil || !strings.Contains(err.Error(), "审计事实不完整") {
		t.Fatalf("任一已声明行缺少效果事实时必须拒绝回滚: %v", err)
	}
	var positionCount int64
	common.DB.Model(&model.Position{}).Where("user_id = ? AND symbol IN ?", 1, []string{"600013", "600014"}).Count(&positionCount)
	if positionCount != 2 {
		t.Fatalf("拒绝回滚不得产生部分删除，剩余持仓=%d", positionCount)
	}
}

func TestDataImportTradeDependencyFrozenBetweenPreviewAndConfirm(t *testing.T) {
	resetImportTestData(t)
	p := model.Position{UserID: 1, Symbol: "000003", Market: "cn", Name: "测试", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 10, BuyDate: "2026-07-01", Quantity: 100, TotalBuyCost: 1000, TotalBuyQty: 100, RemainingCost: 1000}
	common.DB.Create(&p)
	initial := buildInitialTrade(&p)
	common.DB.Create(&initial)
	svc := NewDataImportService()
	uploaded, _ := svc.Upload(1, "trade", "frozen.csv", strings.NewReader("symbol,side,quantity,price,trade_date\n000003,buy,20,11,2026-07-02\n"))
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	ps := NewPositionService(nil)
	if _, err := ps.AddTrade(1, p.ID, PositionTradeInput{Side: "buy", Price: 10.5, Quantity: 10, TradeDate: "2026-07-02"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version}); err == nil || !strings.Contains(err.Error(), "发生变化") {
		t.Fatalf("预检后账本变化必须拒绝确认: %v", err)
	}
	var count int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&count)
	if count != 2 {
		t.Fatalf("拒绝确认必须原子零写入，得到 %d 条流水", count)
	}
}

func TestDataImportWatchlistIsolationAndEditBlocksRollback(t *testing.T) {
	resetImportTestData(t)
	g1 := model.Watchlist{UserID: 1, Name: "本人"}
	g2 := model.Watchlist{UserID: 2, Name: "他人"}
	common.DB.Create(&g1)
	common.DB.Create(&g2)
	svc := NewDataImportService()
	uploaded, err := svc.Upload(1, "watchlist", "watch.csv", strings.NewReader("证券代码,交易市场,证券名称,备注\n600000,cn,浦发银行,观察\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Preview(1, uploaded.ID, ImportMappingInput{Version: uploaded.Version, Mapping: uploaded.Suggestions, TargetGroupID: g2.ID}); err == nil {
		t.Fatal("不能导入到他人分组")
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, g1.ID)
	confirmed, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version})
	if err != nil {
		t.Fatal(err)
	}
	var item model.WatchlistItem
	common.DB.Where("user_id = ?", 1).First(&item)
	common.DB.Model(&model.WatchlistItem{}).Where("id = ? AND user_id = ?", item.ID, 1).Update("note", "用户修改")
	rolled, err := svc.Rollback(1, confirmed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Status != "conflict" || len(rolled.Conflicts) == 0 {
		t.Fatalf("人工编辑后应拒绝自动回滚: %+v", rolled)
	}
}

func TestDataImportValidationBoundaries(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	if _, err := svc.Upload(1, "position", "gbk.csv", bytes.NewReader([]byte{0xff, 0xfe, 'a'})); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("异常编码必须拒绝: %v", err)
	}
	long := strings.Repeat("a", dataImportMaxField+1)
	if _, err := svc.Upload(1, "watchlist", "long.csv", strings.NewReader("symbol,note\n600000,"+long+"\n")); err == nil {
		t.Fatal("超长字段必须拒绝")
	}
	if _, err := svc.Upload(1, "position", "large.csv", strings.NewReader(strings.Repeat("x", dataImportMaxBytes+1))); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("超大文件必须拒绝: %v", err)
	}
	for _, value := range []string{"NaN", "Inf", "1e100"} {
		numeric, err := svc.Upload(1, "position", "bad-number-"+value+".csv", strings.NewReader("symbol,price,quantity,trade_date\n600000,"+value+",100,2026-08-01\n"))
		if err != nil {
			t.Fatal(err)
		}
		np := previewWithSuggestions(t, svc, 1, numeric, 0)
		if np.Rows[0].ErrorCode != "invalid_price" {
			t.Fatalf("异常数值 %s 必须逐行拒绝: %+v", value, np.Rows)
		}
	}
	var tooMany strings.Builder
	tooMany.WriteString("symbol,price,quantity,trade_date\n")
	for i := 0; i < dataImportMaxRows+1; i++ {
		tooMany.WriteString("600000,10,100,2026-08-01\n")
	}
	if _, err := svc.Upload(1, "position", "rows.csv", strings.NewReader(tooMany.String())); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("超行数必须整体拒绝: %v", err)
	}
	// 缺少明确方向时，预检逐行报错；绝不根据费用或数量猜测。
	uploaded, err := svc.Upload(1, "trade", "missing-side.csv", strings.NewReader("symbol,side,quantity,price,trade_date\n600000,,100,10,2026-08-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewWithSuggestions(t, svc, 1, uploaded, 0)
	if preview.ErrorRows != 1 || preview.Rows[0].ErrorCode != "missing_side" {
		t.Fatalf("缺方向应逐行报错: %+v", preview.Rows)
	}
	if _, err := svc.Confirm(context.Background(), 1, preview.ID, ImportConfirmInput{Version: preview.Version}); err == nil {
		t.Fatal("含错误行批次不得确认")
	}
	for _, tc := range []struct {
		name, row, code string
	}{
		{"missing-quantity", "600000,buy,,10,2026-08-01", "invalid_quantity"},
		{"missing-price", "600000,buy,100,,2026-08-01", "invalid_price"},
	} {
		batch, err := svc.Upload(1, "trade", tc.name+".csv", strings.NewReader("symbol,side,quantity,price,trade_date\n"+tc.row+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		checked := previewWithSuggestions(t, svc, 1, batch, 0)
		if checked.Rows[0].ErrorCode != tc.code {
			t.Fatalf("%s 必须逐行拒绝: %+v", tc.name, checked.Rows)
		}
	}

	manual, err := svc.Upload(1, "position", "manual-map.csv", strings.NewReader("证券,成本字段,股数字段,日期字段\n600010,11.2,300,2026-08-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	manualPreview, err := svc.Preview(1, manual.ID, ImportMappingInput{Version: manual.Version, Mapping: map[string]string{
		"symbol": "证券", "price": "成本字段", "quantity": "股数字段", "trade_date": "日期字段",
	}})
	if err != nil || manualPreview.ValidRows != 1 {
		t.Fatalf("手动列映射应生成有效冻结预检: %v %+v", err, manualPreview)
	}

	formula, err := svc.Upload(1, "position", "formula.csv", strings.NewReader("symbol,price,quantity,trade_date,note\n600000,10,100,2026-08-01,=1+1\n"))
	if err != nil {
		t.Fatal(err)
	}
	fp := previewWithSuggestions(t, svc, 1, formula, 0)
	if fp.Rows[0].ErrorCode != "formula_injection" {
		t.Fatalf("公式注入必须拒绝: %+v", fp.Rows)
	}
	formulaUnmapped, err := svc.Upload(1, "position", "formula-unmapped.csv", strings.NewReader("symbol,price,quantity,trade_date,ignored\n600002,10,100,2026-08-01,@SUM(A1)\n"))
	if err != nil {
		t.Fatal(err)
	}
	fup := previewWithSuggestions(t, svc, 1, formulaUnmapped, 0)
	if fup.Rows[0].ErrorCode != "formula_injection" {
		t.Fatalf("未映射列的公式内容也必须拒绝: %+v", fup.Rows)
	}
	if _, err := svc.Upload(1, "position", "formula-header.csv", strings.NewReader("symbol,price,quantity,trade_date,=cmd\n600003,10,100,2026-08-01,x\n")); err == nil || !strings.Contains(err.Error(), "表头") {
		t.Fatalf("公式表头必须在上传时拒绝: %v", err)
	}

	dup, _ := svc.Upload(1, "position", "dup.csv", strings.NewReader("symbol,price,quantity,trade_date\n600001,10,100,2026-08-01\n600001,10,100,2026-08-01\n"))
	dp := previewWithSuggestions(t, svc, 1, dup, 0)
	if dp.ConflictRows != 2 || dp.Rows[0].ErrorCode != "duplicate_row" || dp.Rows[1].ErrorCode != "duplicate_row" {
		t.Fatalf("重复行必须全部标冲突: %+v", dp.Rows)
	}

	bad, _ := svc.Upload(1, "position", "bad-symbol.csv", strings.NewReader("symbol,market,price,quantity,trade_date\nABC,cn,10,100,2026-08-01\n"))
	bp := previewWithSuggestions(t, svc, 1, bad, 0)
	if bp.Rows[0].ErrorCode != "invalid_symbol" {
		t.Fatalf("非法 symbol 必须拒绝: %+v", bp.Rows)
	}
	badMarket, _ := svc.Upload(1, "position", "bad-market.csv", strings.NewReader("symbol,market,price,quantity,trade_date\n600000,xx,10,100,2026-08-01\n"))
	bmp := previewWithSuggestions(t, svc, 1, badMarket, 0)
	if bmp.Rows[0].ErrorCode != "invalid_symbol" {
		t.Fatalf("非法 market 必须拒绝: %+v", bmp.Rows)
	}
}

func TestDataImportCrossFileRowClaimAndHistoryIsolation(t *testing.T) {
	resetImportTestData(t)
	svc := NewDataImportService()
	first, err := svc.Upload(1, "trade", "first.csv", strings.NewReader("symbol,market,side,quantity,price,trade_date,note\n600001,cn,buy,100,10,2026-08-01,观察\n"))
	if err != nil {
		t.Fatal(err)
	}
	firstPreview := previewWithSuggestions(t, svc, 1, first, 0)
	if _, err := svc.Confirm(context.Background(), 1, first.ID, ImportConfirmInput{Version: firstPreview.Version}); err != nil {
		t.Fatal(err)
	}
	// 文件正文不同，但规范化业务行相同，数据库级 claim 必须在预检时列冲突。
	second, err := svc.Upload(1, "trade", "second.csv", strings.NewReader("证券代码,市场,买卖方向,成交数量,成交价,成交日期,备注,未映射列\n600001,cn,buy,100,10,2026-08-01,观察,不同文件\n"))
	if err != nil {
		t.Fatal(err)
	}
	secondPreview := previewWithSuggestions(t, svc, 1, second, 0)
	if secondPreview.ConflictRows != 1 || secondPreview.Rows[0].ErrorCode != "previously_imported" {
		t.Fatalf("跨文件重复业务行未列冲突: %+v", secondPreview.Rows)
	}
	history, err := svc.List(1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].HeaderJSON != "" || history[0].MappingJSON != "" {
		t.Fatalf("历史应为本人轻量批次摘要: %+v", history)
	}
	other, err := svc.List(2, 20)
	if err != nil || len(other) != 0 {
		t.Fatalf("批次历史必须用户隔离: %v %+v", err, other)
	}
}

func TestDataImportModelsInAutoMigrateAndTemplateSafe(t *testing.T) {
	resetImportTestData(t)
	for _, table := range []string{"import_batches", "import_rows", "import_row_claims", "import_effects"} {
		if !common.DB.Migrator().HasTable(table) {
			t.Fatalf("%s 未进入 AllModels/AutoMigrate", table)
		}
	}
	if err := common.DB.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatalf("重复 AutoMigrate 必须幂等: %v", err)
	}
	data, name, err := NewDataImportService().Template("trade")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) || !strings.Contains(string(data), "买卖方向") || !strings.HasSuffix(name, ".csv") {
		t.Fatalf("模板输出错误: %s %s", name, string(data))
	}
}
