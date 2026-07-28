package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// B8 除权除息持仓调整的测试。覆盖：折算算式手工验算、确认/撤销状态机、
// 并发与重跑幂等、过期建议拒绝、越权拒绝、模拟盘自动调整。

// TestComputeCorpAdjust 折算算式逐项手工验算（纯函数，不碰库）。
func TestComputeCorpAdjust(t *testing.T) {
	// ① 10 转 10（transfer=10）：数量翻倍、成本减半——这正是「盈亏显示 -50%」的病灶。
	got, ok := computeCorpAdjust(1000, 20, 0, 10, 0)
	if !ok {
		t.Fatal("10 转 10 应可折算")
	}
	// 新数量 = 1000 × (1+10/10) = 2000；新成本 = (20×1000 − 0)/2000 = 10
	if got.QtyAfter != 2000 || got.CostAfter != 10 || got.CashDividend != 0 {
		t.Fatalf("10 转 10 折算错: %+v", got)
	}

	// ② 10 送 2 转 3 派 1.5（送转合计 5）：
	// 新数量 = 1000 × (1+5/10) = 1500
	// 现金 = 1.5 × 1000/10 = 150
	// 新成本 = (20×1000 − 150)/1500 = 19850/1500 = 13.2333…→ round4 13.2333
	got, ok = computeCorpAdjust(1000, 20, 2, 3, 1.5)
	if !ok {
		t.Fatal("送转派应可折算")
	}
	if got.QtyAfter != 1500 || got.CashDividend != 150 || got.CostAfter != 13.2333 {
		t.Fatalf("送转派折算错: %+v（期望 qty=1500 cash=150 cost=13.2333）", got)
	}

	// ③ 纯派息（无送转）：数量不变，成本按现金下降。
	// 新成本 = (10×500 − 0.5×500/10)/500 = (5000−25)/500 = 9.95
	got, ok = computeCorpAdjust(500, 10, 0, 0, 0.5)
	if !ok {
		t.Fatal("纯派息应可折算")
	}
	if got.QtyAfter != 500 || got.CostAfter != 9.95 || got.CashDividend != 25 {
		t.Fatalf("纯派息折算错: %+v", got)
	}

	// ④ 派息超过总成本：成本钉 0 不为负，现金如实保留。
	got, ok = computeCorpAdjust(100, 0.5, 0, 0, 10) // 现金 = 10×100/10 = 100 > 成本 50
	if !ok || got.CostAfter != 0 || got.CashDividend != 100 {
		t.Fatalf("成本不得为负: %+v ok=%v", got, ok)
	}

	// ⑤ 边界：无持仓 / 无成本 / 空方案一律不折算。
	if _, ok := computeCorpAdjust(0, 20, 0, 10, 0); ok {
		t.Fatal("零数量不应折算")
	}
	if _, ok := computeCorpAdjust(1000, 0, 0, 10, 0); ok {
		t.Fatal("零成本不应折算")
	}
	if _, ok := computeCorpAdjust(1000, 20, 0, 0, 0); ok {
		t.Fatal("空方案不应折算")
	}
}

// cleanCorpTables 清空本组测试涉及的表。setupTestDB 用的是
// `file::memory:?cache=shared`——**同一进程内所有测试共用一个库**，
// 不清表会让上一个用例落的公司行动撞唯一键（同 cleanExperimentTables 先例）。
func cleanCorpTables(t *testing.T) {
	t.Helper()
	for _, tbl := range []string{
		"corporate_actions", "restricted_releases", "ipo_subscriptions",
		"position_corp_adjusts", "paper_corp_adjusts",
		"positions", "position_trades", "paper_holdings", "paper_accounts", "paper_trades",
		"guard_events", "watchlist_items",
	} {
		if err := common.DB.Exec("DELETE FROM " + tbl).Error; err != nil {
			t.Fatalf("清表 %s 失败: %v", tbl, err)
		}
	}
}

// seedAdjustCase 建一个「持仓 + 今日除权方案」的场景，返回持仓与方案。
func seedAdjustCase(t *testing.T, userID int64, exDate string, bonus, transfer, dividend float64) (*model.Position, *model.CorporateAction) {
	t.Helper()
	p := &model.Position{
		UserID: userID, Symbol: "600000", Market: "cn", Name: "浦发银行",
		PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding,
		BuyPrice: 20, Quantity: 1000, BuyDate: "2026-01-05",
		TotalBuyCost: 20000, TotalBuyQty: 1000,
	}
	if err := common.DB.Create(p).Error; err != nil {
		t.Fatalf("建持仓失败: %v", err)
	}
	if err := common.DB.Create(&model.PositionTrade{
		UserID: userID, PositionID: p.ID, Side: model.PositionTradeBuy,
		Price: 20, Quantity: 1000, TradeDate: "2026-01-05",
		AvgCostAfter: 20, QuantityAfter: 1000,
	}).Error; err != nil {
		t.Fatalf("建流水失败: %v", err)
	}
	a := &model.CorporateAction{
		Symbol: "600000", Market: "cn", Name: "浦发银行",
		ReportDate: "2025-12-31", ExDate: exDate, Progress: model.CorpActionProgressImplemented,
		BonusRatio: bonus, TransferRatio: transfer, DividendPretax: dividend,
		PlanProfile: "10转10派1.00元(含税)",
	}
	if err := common.DB.Create(a).Error; err != nil {
		t.Fatalf("建方案失败: %v", err)
	}
	return p, a
}

// TestGenerateCorpAdjustsIdempotent 建议生成幂等：重复扫描不产生重复行，
// 且**已确认/已忽略的行绝不会被拉回 pending**（DoNothing 而非 DoUpdates 的意义）。
func TestGenerateCorpAdjustsIdempotent(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	seedAdjustCase(t, 1, today, 0, 10, 1)

	n, err := GenerateCorpAdjusts(1, today)
	if err != nil || n != 1 {
		t.Fatalf("首次生成应为 1 条: n=%d err=%v", n, err)
	}
	n, err = GenerateCorpAdjusts(1, today)
	if err != nil || n != 0 {
		t.Fatalf("重复生成应为 0 条（幂等）: n=%d err=%v", n, err)
	}
	var cnt int64
	common.DB.Model(&model.PositionCorpAdjust{}).Where("user_id = ?", 1).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("应只有一行建议，got %d", cnt)
	}

	// 忽略后再扫描：不得被重新拉回 pending。
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	svc := &PositionService{}
	if _, err := svc.DismissCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatalf("忽略失败: %v", err)
	}
	if n, _ := GenerateCorpAdjusts(1, today); n != 0 {
		t.Fatalf("已忽略的建议不应被重新生成: n=%d", n)
	}
	after, _ := ListCorpAdjusts(1, "all")
	if len(after) != 1 || after[0].Status != model.CorpAdjustDismissed {
		t.Fatalf("已忽略状态被覆盖: %+v", after)
	}

	// 非除权日不生成。
	if n, _ := GenerateCorpAdjusts(1, "2020-01-01"); n != 0 {
		t.Fatalf("非除权日不应生成: n=%d", n)
	}
}

// TestCorpAdjustConfirmRevert 确认 → 账本改写 → 撤销 → 账本回滚的完整状态机。
func TestCorpAdjustConfirmRevert(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	p, action := seedAdjustCase(t, 1, today, 0, 10, 1) // 10 转 10 派 1
	if _, err := GenerateCorpAdjusts(1, today); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	if len(rows) != 1 {
		t.Fatalf("应有 1 条待确认: %+v", rows)
	}
	adj := rows[0]
	if adj.CorporateActionID != action.ID {
		t.Fatalf("建议未绑定来源公司行动: %+v", adj)
	}

	svc := &PositionService{}
	out, err := svc.ConfirmCorpAdjust(1, adj.ID)
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if out.Status != model.CorpAdjustConfirmed || out.TradeID == 0 || out.ConfirmedAt == nil {
		t.Fatalf("确认后状态不对: %+v", out)
	}

	// 账本：数量 1000→2000、成本 (20×1000−100)/2000=9.95、已实现 +100（现金分红）。
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Quantity != 2000 || after.BuyPrice != 9.95 || after.RealizedPnl != 100 {
		t.Fatalf("账本折算错: qty=%v cost=%v realized=%v", after.Quantity, after.BuyPrice, after.RealizedPnl)
	}
	// **TotalBuyCost/TotalBuyQty 必须不变**——送转不是又买了一次。
	if after.TotalBuyCost != 20000 || after.TotalBuyQty != 1000 {
		t.Fatalf("累计买入口径被送转污染: cost=%v qty=%v", after.TotalBuyCost, after.TotalBuyQty)
	}

	// 流水：side=adjust，显式记录来源与前后账面（不靠 note）。
	var trade model.PositionTrade
	if err := common.DB.First(&trade, out.TradeID).Error; err != nil {
		t.Fatalf("adjust 流水缺失: %v", err)
	}
	if trade.Side != model.PositionTradeAdjust {
		t.Fatalf("流水方向应为 adjust: %q", trade.Side)
	}
	if trade.CorporateActionID != action.ID || trade.AdjustID != adj.ID {
		t.Fatalf("流水未显式绑定来源: action=%d adjust=%d", trade.CorporateActionID, trade.AdjustID)
	}
	if trade.QuantityBefore != 1000 || trade.QuantityAfter != 2000 ||
		trade.AvgCostBefore != 20 || trade.AvgCostAfter != 9.95 {
		t.Fatalf("流水前后账面错: %+v", trade)
	}
	if trade.RealizedPnl != 100 {
		t.Fatalf("现金分红应计入流水已实现盈亏: %v", trade.RealizedPnl)
	}

	// 重复确认拒绝。
	if _, err := svc.ConfirmCorpAdjust(1, adj.ID); err == nil {
		t.Fatal("重复确认应被拒绝")
	}
	// 越权确认拒绝（他人 user_id）。
	if _, err := svc.ConfirmCorpAdjust(2, adj.ID); err == nil {
		t.Fatal("越权确认应被拒绝")
	}
	// 已确认不能直接忽略。
	if _, err := svc.DismissCorpAdjust(1, adj.ID); err == nil {
		t.Fatal("已确认的调整不应能直接忽略")
	}

	// 撤销：账本回滚、流水删除、状态 reverted。
	rev, err := svc.RevertCorpAdjust(1, adj.ID)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if rev.Status != model.CorpAdjustReverted || rev.TradeID != 0 || rev.RevertedAt == nil {
		t.Fatalf("撤销后状态不对: %+v", rev)
	}
	var back model.Position
	common.DB.First(&back, p.ID)
	if back.Quantity != 1000 || back.BuyPrice != 20 || back.RealizedPnl != 0 {
		t.Fatalf("撤销未回滚账本: qty=%v cost=%v realized=%v", back.Quantity, back.BuyPrice, back.RealizedPnl)
	}
	var n int64
	common.DB.Model(&model.PositionTrade{}).Where("side = ?", model.PositionTradeAdjust).Count(&n)
	if n != 0 {
		t.Fatalf("撤销未删除 adjust 流水，剩 %d 条", n)
	}
	// 重复撤销拒绝。
	if _, err := svc.RevertCorpAdjust(1, adj.ID); err == nil {
		t.Fatal("重复撤销应被拒绝")
	}
	// 撤销后可再次确认（撤销是「先不调」，不是永久拒绝）。
	if _, err := svc.ConfirmCorpAdjust(1, adj.ID); err != nil {
		t.Fatalf("撤销后应可再次确认: %v", err)
	}
}

// TestCorpAdjustRevertBlockedByLaterTrade 折算之后有新交易时，撤销必须被明确拒绝
// （否则回滚会把后续交易的账面一并改坏）。
func TestCorpAdjustRevertBlockedByLaterTrade(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1, today, 0, 10, 0)
	if _, err := GenerateCorpAdjusts(1, today); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	svc := &PositionService{}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	// 折算后再加仓一笔。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 12, Quantity: 500, TradeDate: today,
	}); err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	if _, err := svc.RevertCorpAdjust(1, rows[0].ID); err == nil {
		t.Fatal("折算后有新交易时撤销必须被拒绝")
	}
}

// TestCorpAdjustStaleRejected 建议生成后持仓变动 → 折算基数失效 → 确认必须被拒绝。
func TestCorpAdjustStaleRejected(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1, today, 0, 10, 0)
	if _, err := GenerateCorpAdjusts(1, today); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	svc := &PositionService{}
	// 建议生成后又加了一笔仓：QtyBefore/CostBefore 已对不上。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 25, Quantity: 500, TradeDate: today,
	}); err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err == nil {
		t.Fatal("过期建议（持仓已变动）确认必须被拒绝")
	}
	// 账本未被污染。
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Quantity != 1500 {
		t.Fatalf("被拒绝的确认不应改动账本: qty=%v", after.Quantity)
	}
}

// TestCorpAdjustClosedPositionRejected 已平仓持仓不能折算（无仓可调）。
func TestCorpAdjustClosedPositionRejected(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1, today, 0, 10, 0)
	if _, err := GenerateCorpAdjusts(1, today); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	svc := &PositionService{}
	if _, err := svc.Close(1, p.ID, CloseInput{SellPrice: 22, SellDate: today}); err != nil {
		t.Fatalf("平仓失败: %v", err)
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err == nil {
		t.Fatal("已平仓持仓不应能确认折算")
	}
}

// TestPaperCorpAdjustAuto 模拟盘自动折算：执行正确 + 按 action 唯一键重跑不重复。
func TestPaperCorpAdjustAuto(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	acc := &model.PaperAccount{UserID: 7, InitialCash: 100000, Cash: 50000}
	if err := common.DB.Create(acc).Error; err != nil {
		t.Fatalf("建账户失败: %v", err)
	}
	h := &model.PaperHolding{UserID: 7, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Quantity: 1000, AvgCost: 20}
	if err := common.DB.Create(h).Error; err != nil {
		t.Fatalf("建模拟持仓失败: %v", err)
	}
	a := &model.CorporateAction{Symbol: "600000", Market: "cn", Name: "浦发银行",
		ReportDate: "2025-12-31", ExDate: today, BonusRatio: 0, TransferRatio: 10, DividendPretax: 1,
		PlanProfile: "10转10派1.00元(含税)"}
	if err := common.DB.Create(a).Error; err != nil {
		t.Fatalf("建方案失败: %v", err)
	}

	if n := RunPaperCorpAdjust(); n != 1 {
		t.Fatalf("首轮应调整 1 笔，got %d", n)
	}
	var after model.PaperHolding
	common.DB.First(&after, h.ID)
	if after.Quantity != 2000 || after.AvgCost != 9.95 {
		t.Fatalf("模拟盘折算错: qty=%v cost=%v", after.Quantity, after.AvgCost)
	}
	var acc2 model.PaperAccount
	common.DB.First(&acc2, acc.ID)
	if acc2.Cash != 50100 {
		t.Fatalf("现金分红未入账: %v（期望 50100）", acc2.Cash)
	}
	var trades []model.PaperTrade
	common.DB.Where("side = ?", model.PaperSideAdjust).Find(&trades)
	if len(trades) != 1 || trades[0].RealizedPnl != 100 || trades[0].Quantity != 1000 {
		t.Fatalf("模拟盘审计流水错: %+v", trades)
	}

	// 重跑：按 (user, action) 唯一键幂等，不重复调整也不重复发钱。
	if n := RunPaperCorpAdjust(); n != 0 {
		t.Fatalf("重跑应为 0 笔（幂等），got %d", n)
	}
	common.DB.First(&after, h.ID)
	common.DB.First(&acc2, acc.ID)
	if after.Quantity != 2000 || acc2.Cash != 50100 {
		t.Fatalf("重跑污染了账面: qty=%v cash=%v", after.Quantity, acc2.Cash)
	}
	var audits int64
	common.DB.Model(&model.PaperCorpAdjust{}).Count(&audits)
	if audits != 1 {
		t.Fatalf("审计行应只有 1 条，got %d", audits)
	}
}
