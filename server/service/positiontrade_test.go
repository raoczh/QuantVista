package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// cleanLedgerTables 清空持仓账本相关表（内存库 cache=shared 测试间共享）。
func cleanLedgerTables(t *testing.T) {
	t.Helper()
	wipe := func() {
		for _, m := range []any{&model.Position{}, &model.PositionTrade{}, &model.PortfolioSnapshot{},
			&model.PaperAccount{}, &model.PaperHolding{}, &model.PaperTrade{},
			&model.CorporateAction{}, &model.PositionCorpAdjust{}, &model.PaperCorpAdjust{}} {
			common.DB.Where("1 = 1").Delete(m)
		}
	}
	wipe()
	t.Cleanup(wipe)
}

// seedHoldingWithLedger 建一笔带首笔 buy 流水的持仓（等价于走 Create）。
func seedHoldingWithLedger(t *testing.T, userID int64, symbol string, price, qty, fee, tax float64, buyDate string) model.Position {
	t.Helper()
	p := model.Position{
		UserID: userID, Symbol: symbol, Market: "cn", Name: symbol,
		PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY",
		BuyPrice: price, BuyDate: buyDate, Quantity: qty, BuyFee: fee, BuyTax: tax,
		TotalBuyCost: round4(price*qty + fee + tax), TotalBuyQty: qty,
	}
	p.RemainingCost = p.TotalBuyCost
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatalf("建持仓失败: %v", err)
	}
	trade := buildInitialTrade(&p)
	if err := common.DB.Create(&trade).Error; err != nil {
		t.Fatalf("建首笔流水失败: %v", err)
	}
	return p
}

// TestLedgerBuyWeightedCost 加权成本重算的手工验算 + 校验反例。
func TestLedgerBuyWeightedCost(t *testing.T) {
	// 1000 股 @10（费 5 税 1）再买 1000 股 @12（费 6 税 2）：
	// 加权成本 = (10*1000 + 12*1000) / 2000 = 11
	// 买入费 = 5+6 = 11、税 = 1+2 = 3
	// 累计买入成本 = 10*1000+5+1 + 12*1000+6+2 = 10006 + 12008 = 22014
	l := positionLedger{AvgCost: 10, Quantity: 1000, BuyFee: 5, BuyTax: 1,
		TotalBuyCost: 10006, TotalBuyQty: 1000, RemainingCost: 10006}
	got, err := ledgerBuy(l, 12, 1000, 6, 2)
	if err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	if got.AvgCost != 11 || got.Quantity != 2000 || got.BuyFee != 11 || got.BuyTax != 3 {
		t.Fatalf("加权成本/数量/费税错误: %+v", got)
	}
	if got.TotalBuyCost != 22014 || got.TotalBuyQty != 2000 {
		t.Fatalf("累计买入口径错误: %+v", got)
	}
	// 成本口径与 computeView 自洽：Cost = AvgCost*Qty + BuyFee + BuyTax = 22000+11+3 = 22014
	if c := got.AvgCost*got.Quantity + got.BuyFee + got.BuyTax; c != 22014 {
		t.Fatalf("持仓成本应与累计买入成本自洽（未卖出时），得到 %v", c)
	}
	// 非整数加权：1000@10 + 500@13 → (10000+6500)/1500 = 11
	l2, err := ledgerBuy(positionLedger{AvgCost: 10, Quantity: 1000, TotalBuyQty: 1000,
		TotalBuyCost: 10000, RemainingCost: 10000}, 13, 500, 0, 0)
	if err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	if l2.AvgCost != 11 || l2.Quantity != 1500 {
		t.Fatalf("非整倍加仓加权错误: %+v", l2)
	}
	for _, bad := range []struct {
		name            string
		price, qty, fee float64
	}{
		{"零价", 0, 100, 0},
		{"零量", 10, 0, 0},
		{"负费", 10, 100, -1},
	} {
		if _, err := ledgerBuy(l, bad.price, bad.qty, bad.fee, 0); err == nil {
			t.Errorf("%s 应被拒绝", bad.name)
		}
	}
}

// TestLedgerSellPartialAndOversell 部分卖出结转、费税分摊、卖超拒绝的手工验算。
func TestLedgerSellPartialAndOversell(t *testing.T) {
	// 建仓 2000 股 @11，买入费 11 税 3（承接上一测试的账面）。
	l := positionLedger{AvgCost: 11, Quantity: 2000, BuyFee: 11, BuyTax: 3,
		TotalBuyCost: 22014, TotalBuyQty: 2000, RemainingCost: 22014}

	// 卖 500 股 @13，卖出费 4 税 6：
	//   分摊买入费 = 11*500/2000 = 2.75、税 = 3*500/2000 = 0.75
	//   卖出部分成本 = 11*500 + 2.75 + 0.75 = 5503.5
	//   卖出净额 = 13*500 - 4 - 6 = 6490
	//   已实现 = 6490 - 5503.5 = 986.5
	after, realized, err := ledgerSell(l, 13, 500, 4, 6)
	if err != nil {
		t.Fatalf("减仓失败: %v", err)
	}
	if realized != 986.5 {
		t.Fatalf("部分卖出已实现盈亏应为 986.5，得到 %v", realized)
	}
	if after.Quantity != 1500 || after.AvgCost != 11 {
		t.Fatalf("减仓后数量应 1500、加权成本不变: %+v", after)
	}
	if after.BuyFee != 8.25 || after.BuyTax != 2.25 {
		t.Fatalf("剩余买入费税分摊错误: fee=%v tax=%v", after.BuyFee, after.BuyTax)
	}
	if after.TotalSellNet != 6490 || after.RealizedPnl != 986.5 {
		t.Fatalf("累计卖出/已实现错误: %+v", after)
	}
	// 剩余持仓成本 = 11*1500 + 8.25 + 2.25 = 16510.5；与「总买入 − 已结转成本」自洽：
	// 22014 − 5503.5 = 16510.5
	if c := round4(after.AvgCost*after.Quantity + after.BuyFee + after.BuyTax); c != 16510.5 {
		t.Fatalf("剩余成本与总账不自洽: %v", c)
	}

	// 卖超（1501 > 1500）必须拒绝，且账本不被改动。
	if _, _, err := ledgerSell(after, 13, 1501, 0, 0); err == nil {
		t.Fatal("卖出数量超过持仓必须拒绝")
	}
	// 清仓：剩余买入费税一次结清，数量与费税归零（不留舍入残渣）。
	closed, realized2, err := ledgerSell(after, 12, 1500, 0, 0)
	if err != nil {
		t.Fatalf("清仓失败: %v", err)
	}
	// 已实现 = 12*1500 − (11*1500 + 8.25 + 2.25) = 18000 − 16510.5 = 1489.5
	if realized2 != 1489.5 {
		t.Fatalf("清仓已实现应为 1489.5，得到 %v", realized2)
	}
	if closed.Quantity != 0 || closed.BuyFee != 0 || closed.BuyTax != 0 {
		t.Fatalf("清仓后当前持仓字段必须归零: %+v", closed)
	}
	if closed.RealizedPnl != round4(986.5+1489.5) {
		t.Fatalf("累计已实现应为两笔之和: %v", closed.RealizedPnl)
	}
	// 全程自洽：累计已实现 = 累计卖出净额 − 累计买入成本
	if diff := round4(closed.TotalSellNet - closed.TotalBuyCost); diff != closed.RealizedPnl {
		t.Fatalf("总账不自洽: 卖出净额-买入成本=%v，累计已实现=%v", diff, closed.RealizedPnl)
	}
	// 已无持仓再卖必须拒绝。
	if _, _, err := ledgerSell(closed, 12, 100, 0, 0); err == nil {
		t.Fatal("空仓再卖必须拒绝")
	}
}

// TestAddTradeEndToEnd 加仓/部分减仓/减到 0 自动平仓的端到端（含流水快照与汇总回写）。
func TestAddTradeEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	svc := &PositionService{}

	p := seedHoldingWithLedger(t, 1, "600000", 10, 1000, 5, 1, "2026-06-01")

	// 加仓 1000 @12（费 6 税 2）。
	got, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 12, Quantity: 1000, Fee: 6, Tax: 2, TradeDate: "2026-06-10",
	})
	if err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	if got.BuyPrice != 11 || got.Quantity != 2000 || got.BuyFee != 11 || got.BuyTax != 3 {
		t.Fatalf("加仓后汇总回写错误: price=%v qty=%v fee=%v tax=%v", got.BuyPrice, got.Quantity, got.BuyFee, got.BuyTax)
	}
	if got.Status != model.PositionStatusHolding {
		t.Fatalf("加仓不应改变状态: %s", got.Status)
	}

	// 部分减仓 500 @13（费 4 税 6）→ 已实现 986.5，仍持仓。
	got, err = svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 13, Quantity: 500, Fee: 4, Tax: 6, TradeDate: "2026-06-20",
	})
	if err != nil {
		t.Fatalf("减仓失败: %v", err)
	}
	if got.RealizedPnl != 986.5 || got.Quantity != 1500 {
		t.Fatalf("部分减仓结转错误: realized=%v qty=%v", got.RealizedPnl, got.Quantity)
	}
	if got.Status != model.PositionStatusHolding {
		t.Fatalf("部分减仓不得自动平仓: %s", got.Status)
	}
	// 持仓中的已实现盈亏必须进组合总览的 RealizedProfit（已落袋 ≠ 浮盈）。
	view := computeView(*got, 0, false)
	if view.RealizedPnl != 986.5 {
		t.Fatalf("视图应带出已实现盈亏: %+v", view.RealizedPnl)
	}

	// 卖超拒绝：还剩 1500，卖 1600 必须失败且账本不动。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 13, Quantity: 1600,
	}); err == nil {
		t.Fatal("卖超必须被拒绝")
	}
	var afterReject model.Position
	common.DB.First(&afterReject, p.ID)
	if afterReject.Quantity != 1500 || afterReject.RealizedPnl != 986.5 {
		t.Fatalf("被拒绝的卖超不得改动账本: %+v", afterReject)
	}
	var rejectTrades int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&rejectTrades)
	if rejectTrades != 3 {
		t.Fatalf("被拒绝的交易不得留下流水，应 3 条，得到 %d", rejectTrades)
	}

	// 减到 0：自动平仓 + 复盘字段写入。
	got, err = svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 12, Quantity: 1500, TradeDate: "2026-06-30",
		SellReason: "止盈离场", ReviewNote: "分三笔卖完", SellPlanned: "partial",
		AiVerdict: "mixed", LessonLearned: "分批比一次性好",
	})
	if err != nil {
		t.Fatalf("清仓失败: %v", err)
	}
	if got.Status != model.PositionStatusClosed {
		t.Fatalf("减到 0 应自动平仓: %s", got.Status)
	}
	if got.Quantity != 0 {
		t.Fatalf("平仓后当前持仓数量应为 0（累计买入看 TotalBuyQty）: %v", got.Quantity)
	}
	if got.TotalBuyQty != 2000 {
		t.Fatalf("累计买入数量应为 2000: %v", got.TotalBuyQty)
	}
	if got.SellPlanned != "partial" || got.AiVerdict != "mixed" || got.LessonLearned != "分批比一次性好" {
		t.Fatalf("清仓应沿用复盘字段: %+v", got)
	}
	wantRealized := round4(986.5 + 1489.5)
	if got.RealizedPnl != wantRealized {
		t.Fatalf("累计已实现应为 %v，得到 %v", wantRealized, got.RealizedPnl)
	}
	// 已平仓视图走账本口径（分批卖出下 SellPrice*Quantity 旧算式必然算错）。
	closedView := computeView(*got, 0, false)
	if closedView.ProfitAmount != wantRealized || closedView.Cost != got.TotalBuyCost {
		t.Fatalf("已平仓视图应走账本口径: profit=%v cost=%v", closedView.ProfitAmount, closedView.Cost)
	}

	// 已平仓后不得再加减仓。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 10, Quantity: 100,
	}); err == nil {
		t.Fatal("已平仓持仓不得再加仓")
	}

	// 流水明细：4 笔，QuantityAfter 逐笔正确。
	trades, err := svc.ListTrades(1, p.ID)
	if err != nil {
		t.Fatalf("流水查询失败: %v", err)
	}
	if len(trades) != 4 {
		t.Fatalf("应 4 笔流水，得到 %d", len(trades))
	}
	wantQtyAfter := []float64{1000, 2000, 1500, 0}
	for i, tr := range trades {
		if tr.QuantityAfter != wantQtyAfter[i] {
			t.Fatalf("第 %d 笔流水的 quantity_after 应为 %v，得到 %v", i+1, wantQtyAfter[i], tr.QuantityAfter)
		}
		if tr.UserID != 1 {
			t.Fatalf("流水必须带 user_id: %+v", tr)
		}
	}

	// 跨用户隔离：他人不能操作也看不到流水。
	if _, err := svc.AddTrade(99, p.ID, PositionTradeInput{Side: model.PositionTradeBuy, Price: 10, Quantity: 100}); err == nil {
		t.Fatal("跨用户加仓必须失败")
	}
	if _, err := svc.ListTrades(99, p.ID); err == nil {
		t.Fatal("跨用户查流水必须失败")
	}
}

// TestRemainingCostMigrationUsesLedgerIdentity 升级前已有多笔流水的仓位不能再用四位均价
// 乘数量迁移余额。40 万股@1 + 60 万股@1.0001 的精确成本是 1,000,060，均价落库为
// 1.0001；若按均价反推会多出 40 元，清仓盈亏随之少 40 元。
func TestRemainingCostMigrationUsesLedgerIdentity(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	p := model.Position{
		UserID: 1, Symbol: "600099", Market: "cn", Status: model.PositionStatusHolding,
		PositionType: model.PositionTypeLongTerm, BuyPrice: 1.0001, BuyDate: "2026-01-01",
		Quantity: 1000000, TotalBuyCost: 1000060, TotalBuyQty: 1000000,
		RemainingCost: 0, // 模拟升级前尚无该列的行
	}
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	trades := []model.PositionTrade{
		{UserID: 1, PositionID: p.ID, Side: model.PositionTradeBuy, Price: 1, Quantity: 400000,
			TradeDate: "2026-01-01", AvgCostAfter: 1, QuantityAfter: 400000},
		{UserID: 1, PositionID: p.ID, Side: model.PositionTradeBuy, Price: 1.0001, Quantity: 600000,
			TradeDate: "2026-01-02", AvgCostAfter: 1.0001, QuantityAfter: 1000000},
	}
	if err := common.DB.Create(&trades).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PositionService{}
	closed, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 2, Quantity: 1000000, TradeDate: "2026-01-03",
	})
	if err != nil {
		t.Fatalf("清仓失败: %v", err)
	}
	if closed.RealizedPnl != 999940 {
		t.Fatalf("应按精确成本 1,000,060 结转，已实现应为 999,940，得到 %v", closed.RealizedPnl)
	}
}

func TestRemainingCostMigrationExcludesAdjustDividend(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	p := model.Position{
		UserID: 1, Symbol: "600097", Market: "cn", Status: model.PositionStatusHolding,
		PositionType: model.PositionTypeLongTerm, BuyPrice: 10, BuyDate: "2026-01-01",
		Quantity: 1000, TotalBuyCost: 10000, TotalBuyQty: 1000,
		RealizedPnl: 100, RemainingCost: 0,
	}
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	trades := []model.PositionTrade{
		{UserID: 1, PositionID: p.ID, Side: model.PositionTradeBuy, Price: 10, Quantity: 1000,
			TradeDate: "2026-01-01", AvgCostAfter: 10, QuantityAfter: 1000},
		{UserID: 1, PositionID: p.ID, Side: model.PositionTradeAdjust, RealizedPnl: 100,
			TradeDate: "2026-06-01", AvgCostBefore: 10, AvgCostAfter: 10,
			QuantityBefore: 1000, QuantityAfter: 1000},
	}
	if err := common.DB.Create(&trades).Error; err != nil {
		t.Fatal(err)
	}
	closed, err := (&PositionService{}).AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 11, Quantity: 1000, TradeDate: "2026-06-02",
	})
	if err != nil {
		t.Fatalf("清仓失败: %v", err)
	}
	if closed.RealizedPnl != 1100 {
		t.Fatalf("现金分红不能抬高剩余成本：分红 100 + 卖出收益 1000 应为 1100，得到 %v", closed.RealizedPnl)
	}
}

// TestAddTradeChronology 追加式账本必须按日期顺序录入；空日期固化为今天，未来日期拒绝。
func TestAddTradeChronology(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	todayTime := time.Now().In(time.Local)
	date := func(delta int) string { return todayTime.AddDate(0, 0, delta).Format("2006-01-02") }
	p := seedHoldingWithLedger(t, 1, "600098", 10, 100, 0, 0, date(-10))
	svc := &PositionService{}
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 12, Quantity: 50, TradeDate: date(-5),
	}); err != nil {
		t.Fatalf("正常减仓失败: %v", err)
	}
	var before model.Position
	common.DB.First(&before, p.ID)
	var countBefore int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&countBefore)
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 20, Quantity: 100, TradeDate: date(-7),
	}); err == nil {
		t.Fatal("早于最近流水的补录必须拒绝，否则展示顺序与结转顺序不一致")
	}
	var after model.Position
	common.DB.First(&after, p.ID)
	var countAfter int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&countAfter)
	if after.Quantity != before.Quantity || after.RealizedPnl != before.RealizedPnl || countAfter != countBefore {
		t.Fatalf("被拒绝的补录不得改变账本: before=%+v after=%+v trades=%d/%d",
			before, after, countBefore, countAfter)
	}
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 11, Quantity: 10, TradeDate: date(1),
	}); err == nil {
		t.Fatal("未来交易日期必须拒绝")
	}
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 11, Quantity: 10,
	}); err != nil {
		t.Fatalf("空日期应固化为今天并成功入账: %v", err)
	}
	var last model.PositionTrade
	common.DB.Where("position_id = ?", p.ID).Order("id DESC").First(&last)
	if last.TradeDate != date(0) {
		t.Fatalf("空日期应落为今天 %s，得到 %s", date(0), last.TradeDate)
	}
}

// TestCloseGoesThroughLedger 既有 Close 必须走同一流水逻辑：一次性平仓 = 卖出全部剩余，
// 已实现盈亏与旧算式（proceeds − 卖出费税 − 买入成本）逐分一致，且落一笔 sell 流水。
func TestCloseGoesThroughLedger(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	svc := &PositionService{}
	p := seedHoldingWithLedger(t, 1, "600001", 10, 1000, 5, 1, "2026-06-01")

	got, err := svc.Close(1, p.ID, CloseInput{
		SellPrice: 12, SellDate: "2026-06-20", SellFee: 6, SellTax: 2,
		SellReason: "到位", SellPlanned: "yes", AiVerdict: "right",
	})
	if err != nil {
		t.Fatalf("平仓失败: %v", err)
	}
	// 旧算式：12*1000 − 6 − 2 − (10*1000+5+1) = 11992 − 10006 = 1986
	if got.RealizedPnl != 1986 {
		t.Fatalf("平仓已实现应为 1986（与旧算式一致），得到 %v", got.RealizedPnl)
	}
	if got.Status != model.PositionStatusClosed || got.Quantity != 0 {
		t.Fatalf("平仓状态/数量错误: status=%s qty=%v", got.Status, got.Quantity)
	}
	view := computeView(*got, 0, false)
	if view.ProfitAmount != 1986 {
		t.Fatalf("视图盈亏应与旧算式一致: %v", view.ProfitAmount)
	}
	trades, _ := svc.ListTrades(1, p.ID)
	if len(trades) != 2 || trades[1].Side != model.PositionTradeSell || trades[1].RealizedPnl != 1986 {
		t.Fatalf("Close 必须落一笔 sell 流水并带已实现盈亏: %+v", trades)
	}
	// 重复平仓拒绝。
	if _, err := svc.Close(1, p.ID, CloseInput{SellPrice: 12}); err == nil {
		t.Fatal("重复平仓必须拒绝")
	}
}

// TestBackfillLegacyPositionLedger 旧持仓惰性补流水：幂等、并发安全、**绝不改变汇总值**。
func TestBackfillLegacyPositionLedger(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)

	// 旧数据形态：没有任何流水、四个新汇总列全为 0。
	legacyHolding := model.Position{
		UserID: 1, Symbol: "600002", Market: "cn", Name: "旧持仓", Status: model.PositionStatusHolding,
		PositionType: model.PositionTypeLongTerm, BuyPrice: 10, BuyDate: "2026-05-01",
		Quantity: 1000, BuyFee: 5, BuyTax: 1,
	}
	legacyClosed := model.Position{
		UserID: 1, Symbol: "600003", Market: "cn", Name: "旧平仓", Status: model.PositionStatusClosed,
		PositionType: model.PositionTypeLongTerm, BuyPrice: 10, BuyDate: "2026-05-01",
		Quantity: 1000, BuyFee: 5, BuyTax: 1,
		SellPrice: 12, SellDate: "2026-05-20", SellFee: 6, SellTax: 2,
	}
	if err := common.DB.Create(&legacyHolding).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&legacyClosed).Error; err != nil {
		t.Fatal(err)
	}
	// 补建前的展示值（旧算式），补建后必须一分不差。
	beforeHolding := computeView(legacyHolding, 11, true)
	beforeClosed := computeView(legacyClosed, 0, false)

	positions := []model.Position{legacyHolding, legacyClosed}
	if !backfillPositionLedgers(1, positions) {
		t.Fatal("首次应确有补建写入")
	}
	// 幂等：再跑不写、不新增流水。
	var reloaded []model.Position
	common.DB.Where("user_id = 1").Order("id").Find(&reloaded)
	if backfillPositionLedgers(1, reloaded) {
		t.Fatal("已补建过不应再写入（幂等）")
	}
	var tradeCount int64
	common.DB.Model(&model.PositionTrade{}).Count(&tradeCount)
	if tradeCount != 3 { // holding 1 笔 buy + closed 1 buy 1 sell
		t.Fatalf("补建流水应 3 条，得到 %d", tradeCount)
	}

	var afterHolding, afterClosed model.Position
	common.DB.First(&afterHolding, legacyHolding.ID)
	common.DB.First(&afterClosed, legacyClosed.ID)

	// **不改变任何既有汇总值**：当前持仓字段逐字不动。
	if afterHolding.BuyPrice != 10 || afterHolding.Quantity != 1000 ||
		afterHolding.BuyFee != 5 || afterHolding.BuyTax != 1 {
		t.Fatalf("补建不得改动买入字段: %+v", afterHolding)
	}
	if afterClosed.Quantity != 1000 || afterClosed.SellPrice != 12 {
		t.Fatalf("旧平仓记录的数量/卖价不得被改写（导出与展示依赖它）: %+v", afterClosed)
	}
	// 汇总列按等价算式回填。
	if afterHolding.TotalBuyCost != 10006 || afterHolding.TotalBuyQty != 1000 || afterHolding.RealizedPnl != 0 {
		t.Fatalf("holding 汇总回填错误: %+v", afterHolding)
	}
	if afterClosed.TotalBuyCost != 10006 || afterClosed.TotalSellNet != 11992 || afterClosed.RealizedPnl != 1986 {
		t.Fatalf("closed 汇总回填错误: %+v", afterClosed)
	}
	// 展示值不变（这是「绝不改变汇总值」的实质）。
	afterHoldingView := computeView(afterHolding, 11, true)
	afterClosedView := computeView(afterClosed, 0, false)
	if afterHoldingView.Cost != beforeHolding.Cost || afterHoldingView.ProfitAmount != beforeHolding.ProfitAmount {
		t.Fatalf("持仓中展示值被改变: before=%+v after=%+v", beforeHolding, afterHoldingView)
	}
	if afterClosedView.Cost != beforeClosed.Cost || afterClosedView.ProfitAmount != beforeClosed.ProfitAmount {
		t.Fatalf("已平仓展示值被改变: before=%v/%v after=%v/%v",
			beforeClosed.Cost, beforeClosed.ProfitAmount, afterClosedView.Cost, afterClosedView.ProfitAmount)
	}
}

// TestBackfillLedgerConcurrentIdempotent 并发补建幂等：N 个 goroutine 同时补同一持仓，
// 只能产生一笔流水（行锁 + 事务内二次确认）。
func TestBackfillLedgerConcurrentIdempotent(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	legacy := model.Position{
		UserID: 1, Symbol: "600004", Market: "cn", Status: model.PositionStatusHolding,
		PositionType: model.PositionTypeLongTerm, BuyPrice: 10, BuyDate: "2026-05-01", Quantity: 1000,
	}
	if err := common.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			backfillPositionLedgers(1, []model.Position{legacy})
		}()
	}
	wg.Wait()
	var n int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", legacy.ID).Count(&n)
	if n != 1 {
		t.Fatalf("并发补建应只产生 1 笔流水，得到 %d", n)
	}
	var after model.Position
	common.DB.First(&after, legacy.ID)
	if after.TotalBuyCost != 10000 || after.TotalBuyQty != 1000 {
		t.Fatalf("并发补建后汇总列错误: %+v", after)
	}
}

// TestUpdateCannotBypassLedger Update 不得绕过流水破坏成本/数量：
// 有加减仓流水后买入字段冻结；只有建仓一笔时允许修正且同步改写那笔流水。
func TestUpdateCannotBypassLedger(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	svc := &PositionService{}
	p := seedHoldingWithLedger(t, 1, "600005", 10, 1000, 5, 1, "2026-06-01")

	// 只有建仓一笔：允许修正录入错误，流水与汇总同步。
	if _, err := svc.Update(1, p.ID, PositionInput{
		Market: "cn", PositionType: model.PositionTypeLongTerm,
		BuyPrice: 10.5, BuyDate: "2026-06-01", Quantity: 1200, BuyFee: 6, BuyTax: 2,
	}); err != nil {
		t.Fatalf("单笔流水时应允许修正: %v", err)
	}
	trades, _ := svc.ListTrades(1, p.ID)
	if len(trades) != 1 || trades[0].Price != 10.5 || trades[0].Quantity != 1200 {
		t.Fatalf("修正应同步改写建仓流水: %+v", trades)
	}
	var afterFix model.Position
	common.DB.First(&afterFix, p.ID)
	if afterFix.TotalBuyCost != round4(10.5*1200+6+2) || afterFix.TotalBuyQty != 1200 {
		t.Fatalf("修正应同步汇总列: %+v", afterFix)
	}

	// 加一笔真实加仓后，买入字段冻结。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 12, Quantity: 800,
	}); err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	before := model.Position{}
	common.DB.First(&before, p.ID)
	_, err := svc.Update(1, p.ID, PositionInput{
		Market: "cn", PositionType: model.PositionTypeLongTerm,
		BuyPrice: 1, BuyDate: "2026-06-01", Quantity: 999999, BuyFee: 0, BuyTax: 0,
	})
	if err == nil {
		t.Fatal("有加减仓流水后不得直接改写买入价格/数量")
	}
	after := model.Position{}
	common.DB.First(&after, p.ID)
	if after.BuyPrice != before.BuyPrice || after.Quantity != before.Quantity ||
		after.TotalBuyCost != before.TotalBuyCost {
		t.Fatalf("被拒绝的编辑不得改动账本: before=%+v after=%+v", before, after)
	}
	// 但备注类字段仍可改（买入字段保持原值即视为未改）。
	if _, err := svc.Update(1, p.ID, PositionInput{
		Market: "cn", PositionType: model.PositionTypeShortTerm,
		BuyPrice: before.BuyPrice, BuyDate: before.BuyDate, Quantity: before.Quantity,
		BuyFee: before.BuyFee, BuyTax: before.BuyTax, UserNote: "改个备注",
	}); err != nil {
		t.Fatalf("不动买入字段时应允许编辑备注: %v", err)
	}
}

// TestDeletePositionCascadesTrades 删除持仓级联删流水（否则无主流水污染复盘统计）。
func TestDeletePositionCascadesTrades(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	svc := &PositionService{}
	p := seedHoldingWithLedger(t, 1, "600006", 10, 1000, 0, 0, "2026-06-01")
	if err := svc.Delete(1, p.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	var n int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&n)
	if n != 0 {
		t.Fatalf("流水应随持仓删除，残留 %d 条", n)
	}
}

// ---------- B6 复盘统计 ----------

// TestTradeStatsManualCheck 复盘统计的手工验算：胜率/盈亏比/平均持有/分布/Top/教训，
// 以及「零亏损分母」「缺行业」「空样本」三处诚实表达。
func TestTradeStatsManualCheck(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	common.DB.Where("1 = 1").Delete(&model.StockUniverseDaily{})
	t.Cleanup(func() {
		common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
		common.DB.Where("1 = 1").Delete(&model.StockUniverseDaily{})
	})
	svc := &PositionService{}

	// 空样本：不给 0% 胜率，如实声明没有样本。
	empty, err := svc.TradeStats(context.Background(), 1, "all")
	if err != nil {
		t.Fatalf("空统计失败: %v", err)
	}
	if empty.Closed != 0 || empty.ProfitFactor != nil {
		t.Fatalf("空样本应零笔且盈亏比无定义: %+v", empty)
	}
	if !containsNote(empty.Notes, "没有已平仓记录") {
		t.Fatalf("空样本必须如实声明: %v", empty.Notes)
	}

	// 交易日历：2026-06-01 ~ 06-30 全部开市（简化验算）。
	for d := 1; d <= 30; d++ {
		common.DB.Exec("INSERT INTO trading_calendars (market, trade_date, is_open) VALUES ('cn', ?, 1)",
			fmt.Sprintf("2026-06-%02d", d))
	}
	// 宇宙快照只覆盖 600100（600200 行业未知 → 单列「行业未知」）。
	if err := common.DB.Create(&model.StockUniverseDaily{
		Symbol: "600100", Market: "cn", TradeDate: "2026-06-30", Industry: "白酒",
	}).Error; err != nil {
		t.Fatal(err)
	}

	// 三笔已平仓：赚 1000 / 亏 500 / 赚 200。
	seedClosed := func(symbol string, buy, sell, qty float64, buyDate, sellDate, reason, verdict, planned, lesson string) {
		buyCost := round4(buy * qty)
		sellNet := round4(sell * qty)
		p := model.Position{
			UserID: 1, Symbol: symbol, Market: "cn", Name: symbol, Status: model.PositionStatusClosed,
			PositionType: model.PositionTypeShortTerm, BuyPrice: buy, BuyDate: buyDate, Quantity: 0,
			SellPrice: sell, SellDate: sellDate, BuyReason: reason,
			AiVerdict: verdict, SellPlanned: planned, LessonLearned: lesson,
			TotalBuyCost: buyCost, TotalSellNet: sellNet, TotalBuyQty: qty,
			RealizedPnl: round4(sellNet - buyCost),
		}
		if err := common.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
		tr := model.PositionTrade{UserID: 1, PositionID: p.ID, Side: model.PositionTradeBuy,
			Price: buy, Quantity: qty, TradeDate: buyDate, QuantityAfter: qty, AvgCostAfter: buy}
		common.DB.Create(&tr)
	}
	// 600100：10→11，1000 股，持有 06-02→06-06 = 4 个交易日，赚 1000
	seedClosed("600100", 10, 11, 1000, "2026-06-02", "2026-06-06", "均线突破", "right", "yes", "")
	// 600200：10→9.5，1000 股，持有 06-02→06-25 = 23 个交易日，亏 500
	seedClosed("600200", 10, 9.5, 1000, "2026-06-02", "2026-06-25", "听消息", "wrong", "no", "别听消息买入")
	// 600100 第二笔：10→10.2，1000 股，无买卖日期 → 持有时长未知，赚 200
	seedClosed("600100", 10, 10.2, 1000, "", "", "", "", "", "复盘要写完整")

	st, err := svc.TradeStats(context.Background(), 1, "all")
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if st.Closed != 3 {
		t.Fatalf("样本应 3 笔: %d", st.Closed)
	}
	// 总已实现 = 1000 − 500 + 200 = 700
	if st.TotalRealizedPnl != 700 {
		t.Fatalf("总已实现应 700: %v", st.TotalRealizedPnl)
	}
	if st.WinCount != 2 || st.LossCount != 1 || st.WinRate != 66.67 {
		t.Fatalf("胜率验算错误: win=%d loss=%d rate=%v", st.WinCount, st.LossCount, st.WinRate)
	}
	// 平均盈利 = (1000+200)/2 = 600；平均亏损 = 500；盈亏比 = 1200/500 = 2.4
	if st.AvgWin != 600 || st.AvgLoss != 500 {
		t.Fatalf("平均盈亏错误: win=%v loss=%v", st.AvgWin, st.AvgLoss)
	}
	if st.ProfitFactor == nil || *st.ProfitFactor != 2.4 {
		t.Fatalf("盈亏比应为 2.4: %v", st.ProfitFactor)
	}
	// 平均持有交易日 = (4+23)/2 = 13.5（第三笔无日期不计入）
	if st.HoldSample != 2 || st.AvgHoldTradeDays != 13.5 {
		t.Fatalf("平均持有验算错误: sample=%d avg=%v", st.HoldSample, st.AvgHoldTradeDays)
	}
	if !containsNote(st.Notes, "未计入平均持有交易日") {
		t.Fatalf("缺日期样本必须如实声明: %v", st.Notes)
	}

	// 行业分布：白酒（2 笔，+1200）在前，「行业未知」恒排最后。
	if len(st.ByIndustry) != 2 {
		t.Fatalf("行业分布应 2 组: %+v", st.ByIndustry)
	}
	if st.ByIndustry[0].Label != "白酒" || st.ByIndustry[0].Trades != 2 || st.ByIndustry[0].RealizedPnl != 1200 {
		t.Fatalf("白酒组验算错误: %+v", st.ByIndustry[0])
	}
	last := st.ByIndustry[len(st.ByIndustry)-1]
	if !last.Unknown || last.Trades != 1 {
		t.Fatalf("行业未知必须单列且排最后: %+v", last)
	}
	if !containsNote(st.Notes, "行业未知") {
		t.Fatalf("缺行业必须如实声明: %v", st.Notes)
	}
	// 持有时长分档：4 日 → 2~5 交易日；23 日 → 21~60 交易日；缺日期 → 未知。
	if len(st.ByHoldBucket) != 3 {
		t.Fatalf("持有时长应 3 组: %+v", st.ByHoldBucket)
	}
	// AI 判断 / 是否按计划分布覆盖「未填」。
	if len(st.ByAiVerdict) != 3 || len(st.BySellPlanned) != 3 {
		t.Fatalf("AI/计划分布应各 3 组: %+v / %+v", st.ByAiVerdict, st.BySellPlanned)
	}
	// Top 榜：最赚第一为 +1000，最亏第一为 −500。
	if len(st.TopWinners) != 2 || st.TopWinners[0].RealizedPnl != 1000 {
		t.Fatalf("最赚榜错误: %+v", st.TopWinners)
	}
	if len(st.TopLosers) != 1 || st.TopLosers[0].RealizedPnl != -500 {
		t.Fatalf("最亏榜错误: %+v", st.TopLosers)
	}
	// 教训清单：2 条（第三笔与第二笔都填了）。
	if len(st.Lessons) != 2 {
		t.Fatalf("教训清单应 2 条: %+v", st.Lessons)
	}
	// 不与推荐归因混算的声明必须在。
	if !containsNote(st.Notes, "模型口径") {
		t.Fatalf("必须声明与模型口径不混算: %v", st.Notes)
	}

	// 零亏损分母：只留盈利笔时盈亏比无定义。
	common.DB.Where("symbol = ?", "600200").Delete(&model.Position{})
	noLoss, err := svc.TradeStats(context.Background(), 1, "all")
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if noLoss.LossCount != 0 || noLoss.ProfitFactor != nil {
		t.Fatalf("无亏损时盈亏比必须为 nil（不是 0 也不是 ∞）: %+v", noLoss.ProfitFactor)
	}
	if !containsNote(noLoss.Notes, "盈亏比无定义") {
		t.Fatalf("零亏损分母必须如实声明: %v", noLoss.Notes)
	}

	// 跨用户隔离：他人看不到本人样本。
	other, err := svc.TradeStats(context.Background(), 99, "all")
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if other.Closed != 0 {
		t.Fatalf("跨用户隔离失效: %+v", other)
	}
}

func containsNote(notes []string, sub string) bool {
	for _, n := range notes {
		if len(sub) > 0 && len(n) >= len(sub) && contains(n, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestTradeStatsRangeFilter 窗口过滤与非法参数回落。
func TestTradeStatsRangeFilter(t *testing.T) {
	if got, days, ok := normalizeTradeStatRange("90d"); got != "90d" || days != 90 || !ok {
		t.Fatalf("90d 解析错误: %s %d %v", got, days, ok)
	}
	if got, days, ok := normalizeTradeStatRange(""); got != tradeStatRangeAll || days != 0 || !ok {
		t.Fatalf("空值应为全部历史: %s %d %v", got, days, ok)
	}
	if got, days, ok := normalizeTradeStatRange("十年"); got != tradeStatRangeAll || days != 0 || ok {
		t.Fatalf("非法值应回落 all 并标记: %s %d %v", got, days, ok)
	}
	if k, l := holdBucketOf(1); k != "d1" || l == "" {
		t.Fatalf("1 日档错误: %s %s", k, l)
	}
	if k, _ := holdBucketOf(5); k != "d2_5" {
		t.Fatalf("5 日应属 2~5 档: %s", k)
	}
	if k, _ := holdBucketOf(61); k != "d60p" {
		t.Fatalf("61 日应属 60+ 档: %s", k)
	}
}

// ---------- B7 资产快照 ----------

func freshQuote(price float64) FreshQuoteResult {
	return FreshQuoteResult{
		Quote: &datasource.Quote{Price: price},
		Fresh: quoteFreshInfo{Status: freshStatusFresh},
	}
}

func staleQuote(price float64) FreshQuoteResult {
	return FreshQuoteResult{
		Quote: &datasource.Quote{Price: price},
		Fresh: quoteFreshInfo{Status: "stale"},
	}
}

// TestRealSnapshotPartialFailClosed 快照定价 fail-closed：stale/缺失标的不进市值**也不进
// 成本**，快照标 partial 并记缺口数与说明；已平仓持仓只贡献累计已实现盈亏。
func TestRealSnapshotPartialFailClosed(t *testing.T) {
	positions := []model.Position{
		{Symbol: "600100", Market: "cn", Status: model.PositionStatusHolding,
			BuyPrice: 10, Quantity: 1000, BuyFee: 5, BuyTax: 1, RealizedPnl: 100},
		{Symbol: "600200", Market: "cn", Status: model.PositionStatusHolding,
			BuyPrice: 20, Quantity: 500},
		{Symbol: "600300", Market: "cn", Status: model.PositionStatusHolding,
			BuyPrice: 30, Quantity: 100},
		{Symbol: "600400", Market: "cn", Status: model.PositionStatusClosed,
			BuyPrice: 5, Quantity: 0, RealizedPnl: 250},
	}
	quotes := map[string]FreshQuoteResult{
		QuoteKey("cn", "600100"): freshQuote(11),
		QuoteKey("cn", "600200"): staleQuote(21), // stale：不得拿旧价冒充
		// 600300 完全取不到
	}
	snap := realSnapshotFrom(7, "2026-07-28", positions, quotes)
	if snap.PositionCount != 3 {
		t.Fatalf("持仓笔数应 3（已平仓不计）: %d", snap.PositionCount)
	}
	if snap.MarketValue != 11000 {
		t.Fatalf("市值只算 fresh 的 600100=11000: %v", snap.MarketValue)
	}
	if snap.Cost != 10006 {
		t.Fatalf("成本也只算 fresh 的（否则浮亏=−成本）: %v", snap.Cost)
	}
	if snap.UnrealizedPnl != 994 {
		t.Fatalf("浮盈应 11000−10006=994: %v", snap.UnrealizedPnl)
	}
	if !snap.Partial || snap.MissingCount != 2 || snap.Note == "" {
		t.Fatalf("必须标 partial + 缺口数 + 说明: %+v", snap)
	}
	// 累计已实现含已平仓（100 + 250 = 350）。
	if snap.RealizedCum != 350 {
		t.Fatalf("累计已实现应 350: %v", snap.RealizedCum)
	}

	// 全部 fresh 时不得标 partial。
	full := realSnapshotFrom(7, "2026-07-28", positions[:1], quotes)
	if full.Partial || full.MissingCount != 0 || full.Note != "" {
		t.Fatalf("全部定价成功不应标 partial: %+v", full)
	}
}

// TestPaperSnapshotFailClosed 模拟盘快照同样 fail-closed（不用成本兜底估值，
// 否则停牌期会画出一条平直的假净值）。
func TestPaperSnapshotFailClosed(t *testing.T) {
	acc := model.PaperAccount{UserID: 7, InitialCash: 100000, Cash: 40000}
	holdings := []model.PaperHolding{
		{Symbol: "600100", Market: "cn", Quantity: 1000, AvgCost: 10},
		{Symbol: "600200", Market: "cn", Quantity: 500, AvgCost: 20},
	}
	quotes := map[string]FreshQuoteResult{QuoteKey("cn", "600100"): freshQuote(12)}
	snap := paperSnapshotFrom(7, "2026-07-28", acc, holdings, quotes, 1234)
	if snap.Cash != 40000 || snap.PositionCount != 2 {
		t.Fatalf("现金/笔数错误: %+v", snap)
	}
	if snap.MarketValue != 12000 || snap.Cost != 10000 || snap.UnrealizedPnl != 2000 {
		t.Fatalf("市值/成本/浮盈错误（stale 标的不得按成本顶上）: %+v", snap)
	}
	if !snap.Partial || snap.MissingCount != 1 {
		t.Fatalf("必须标 partial: %+v", snap)
	}
	if snap.RealizedCum != 1234 {
		t.Fatalf("累计已实现应透传: %v", snap.RealizedCum)
	}
}

// TestPortfolioSnapshotUpsertAndCurve upsert 幂等（同日重跑覆盖不重复点）+ 曲线读取
// （用户隔离、日期升序、days 有界、partial 计数与声明）。
func TestPortfolioSnapshotUpsertAndCurve(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)

	base := &model.PortfolioSnapshot{
		UserID: 1, Kind: model.SnapshotKindReal, TradeDate: "2026-07-27",
		MarketValue: 10000, Cost: 9000, UnrealizedPnl: 1000, RealizedCum: 500, PositionCount: 2,
	}
	if err := upsertPortfolioSnapshot(base); err != nil {
		t.Fatalf("首次落库失败: %v", err)
	}
	// 同日重跑：覆盖而非新增。
	base2 := &model.PortfolioSnapshot{
		UserID: 1, Kind: model.SnapshotKindReal, TradeDate: "2026-07-27",
		MarketValue: 10500, Cost: 9000, UnrealizedPnl: 1500, RealizedCum: 500, PositionCount: 2,
		Partial: true, MissingCount: 1, Note: "1 笔无有效行情",
	}
	if err := upsertPortfolioSnapshot(base2); err != nil {
		t.Fatalf("重跑 upsert 失败: %v", err)
	}
	var n int64
	common.DB.Model(&model.PortfolioSnapshot{}).Where("user_id = 1 AND kind = ?", model.SnapshotKindReal).Count(&n)
	if n != 1 {
		t.Fatalf("同 (user,kind,date) 必须唯一，得到 %d 行", n)
	}
	var stored model.PortfolioSnapshot
	common.DB.Where("user_id = 1").First(&stored)
	if stored.MarketValue != 10500 || !stored.Partial || stored.MissingCount != 1 {
		t.Fatalf("重跑应覆盖为最新值: %+v", stored)
	}

	// 另一日 + 另一账户类型 + 另一用户。
	if err := upsertPortfolioSnapshot(&model.PortfolioSnapshot{
		UserID: 1, Kind: model.SnapshotKindReal, TradeDate: "2026-07-28",
		MarketValue: 11000, Cost: 9000, UnrealizedPnl: 2000, RealizedCum: 500, PositionCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := upsertPortfolioSnapshot(&model.PortfolioSnapshot{
		UserID: 1, Kind: model.SnapshotKindPaper, TradeDate: "2026-07-28",
		MarketValue: 60000, Cash: 40000, Cost: 55000, UnrealizedPnl: 5000, PositionCount: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := upsertPortfolioSnapshot(&model.PortfolioSnapshot{
		UserID: 2, Kind: model.SnapshotKindReal, TradeDate: "2026-07-28", MarketValue: 999,
	}); err != nil {
		t.Fatal(err)
	}

	curve, err := PortfolioCurve(1, model.SnapshotKindReal, 30)
	if err != nil {
		t.Fatalf("曲线查询失败: %v", err)
	}
	if len(curve.Points) != 2 {
		t.Fatalf("real 曲线应 2 点（跨用户/跨 kind 不混）: %d", len(curve.Points))
	}
	if curve.Points[0].TradeDate != "2026-07-27" || curve.Points[1].TradeDate != "2026-07-28" {
		t.Fatalf("必须日期升序: %+v", curve.Points)
	}
	if curve.PartialCount != 1 || !containsNote(curve.Notes, "partial") {
		t.Fatalf("partial 必须计数并声明: count=%d notes=%v", curve.PartialCount, curve.Notes)
	}
	// real 无现金概念：总资产=市值。
	if curve.Points[1].TotalAssets != 11000 {
		t.Fatalf("real 总资产应等于市值: %v", curve.Points[1].TotalAssets)
	}
	paperCurve, err := PortfolioCurve(1, model.SnapshotKindPaper, 30)
	if err != nil {
		t.Fatalf("模拟盘曲线失败: %v", err)
	}
	if len(paperCurve.Points) != 1 || paperCurve.Points[0].TotalAssets != 100000 {
		t.Fatalf("paper 总资产应为现金+市值=100000: %+v", paperCurve.Points)
	}
	// days 上界与非法 kind。
	if c, _ := PortfolioCurve(1, model.SnapshotKindReal, 99999); c.Days != curveMaxDays {
		t.Fatalf("days 必须有界: %d", c.Days)
	}
	if _, err := PortfolioCurve(1, "bogus", 30); err == nil {
		t.Fatal("非法 kind 必须报错")
	}
	// 空数据如实声明。
	emptyCurve, _ := PortfolioCurve(1234, model.SnapshotKindReal, 30)
	if len(emptyCurve.Points) != 0 || !containsNote(emptyCurve.Notes, "暂无快照") {
		t.Fatalf("空曲线必须如实声明: %+v", emptyCurve)
	}
}

// TestRunPortfolioSnapshotsIdempotent job 端到端幂等：为有持仓/有模拟账户的用户各落一条，
// 重跑不产生重复行。用「无持仓中标的」的场景避免依赖真实行情源。
func TestRunPortfolioSnapshotsIdempotent(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	// 用户 1：仅已平仓持仓（累计已实现要继续画进曲线）。
	if err := common.DB.Create(&model.Position{
		UserID: 1, Symbol: "600100", Market: "cn", Status: model.PositionStatusClosed,
		BuyPrice: 10, Quantity: 0, TotalBuyCost: 10000, TotalSellNet: 11000, RealizedPnl: 1000,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 用户 2：仅模拟账户（无持仓）。
	if err := common.DB.Create(&model.PaperAccount{UserID: 2, InitialCash: 100000, Cash: 100000}).Error; err != nil {
		t.Fatal(err)
	}
	posSvc := &PositionService{}
	paperSvc := &PaperService{}
	if n := RunPortfolioSnapshots(context.Background(), posSvc, paperSvc, "2026-07-28"); n != 2 {
		t.Fatalf("应落 2 条快照（real+paper），得到 %d", n)
	}
	if n := RunPortfolioSnapshots(context.Background(), posSvc, paperSvc, "2026-07-28"); n != 2 {
		t.Fatalf("重跑仍应处理 2 条（幂等 upsert），得到 %d", n)
	}
	var total int64
	common.DB.Model(&model.PortfolioSnapshot{}).Count(&total)
	if total != 2 {
		t.Fatalf("重跑不得产生重复行，得到 %d", total)
	}
	var realSnap model.PortfolioSnapshot
	common.DB.Where("user_id = 1 AND kind = ?", model.SnapshotKindReal).First(&realSnap)
	if realSnap.RealizedCum != 1000 || realSnap.PositionCount != 0 {
		t.Fatalf("已平仓用户快照应只带累计已实现: %+v", realSnap)
	}
	var paperSnap model.PortfolioSnapshot
	common.DB.Where("user_id = 2 AND kind = ?", model.SnapshotKindPaper).First(&paperSnap)
	if paperSnap.Cash != 100000 || paperSnap.Partial {
		t.Fatalf("空持仓模拟盘快照应只有现金且不标 partial: %+v", paperSnap)
	}
}

func TestSnapshotUserIDsPropagatesQueryErrors(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	t.Cleanup(func() {
		if err := common.DB.AutoMigrate(&model.Position{}, &model.PaperAccount{}); err != nil {
			t.Errorf("恢复快照候选表失败: %v", err)
		}
	})
	if err := common.DB.Migrator().DropTable(&model.Position{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshotUserIDs(); err == nil {
		t.Fatal("持仓表查询失败不得静默等价为零候选用户")
	}
	if n := RunPortfolioSnapshots(context.Background(), &PositionService{}, &PaperService{}, "2026-07-28"); n != 0 {
		t.Fatalf("持仓候选查询失败时本轮必须中止，得到 %d 条", n)
	}
	if err := common.DB.AutoMigrate(&model.Position{}); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Migrator().DropTable(&model.PaperAccount{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshotUserIDs(); err == nil {
		t.Fatal("模拟账户表查询失败不得静默等价为零候选用户")
	}
	if n := RunPortfolioSnapshots(context.Background(), &PositionService{}, &PaperService{}, "2026-07-28"); n != 0 {
		t.Fatalf("模拟账户候选查询失败时本轮必须中止，得到 %d 条", n)
	}
	if err := common.DB.AutoMigrate(&model.PaperAccount{}); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotBackfillsLegacyLedger 从未打开过持仓页的老用户：快照必须先补建账本再算，
// 否则 RealizedPnl 全为 0，曲线的「累计已实现」会一路是 0——不是他没赚过，是账本没补建。
func TestSnapshotBackfillsLegacyLedger(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	// 旧数据形态：无流水、四个汇总列全 0（旧算式盈亏 = 12*1000−6−2−(10*1000+5+1) = 1986）。
	if err := common.DB.Create(&model.Position{
		UserID: 5, Symbol: "600100", Market: "cn", Status: model.PositionStatusClosed,
		BuyPrice: 10, BuyDate: "2026-05-01", Quantity: 1000, BuyFee: 5, BuyTax: 1,
		SellPrice: 12, SellDate: "2026-05-20", SellFee: 6, SellTax: 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	posSvc := &PositionService{}
	snap, err := posSvc.buildRealSnapshot(context.Background(), 5, "2026-07-28")
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	if snap.RealizedCum != 1986 {
		t.Fatalf("快照应先补建账本再算累计已实现（期望 1986）: %v", snap.RealizedCum)
	}
	if snap.PositionCount != 0 || snap.Partial {
		t.Fatalf("已平仓不计入持仓笔数且无缺口: %+v", snap)
	}
}
