package service

import (
	"context"
	"fmt"
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
	got, ok := computeCorpAdjust(1000, 20, 1000, 0, 10, 0)
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
	// 现金已单独计入 realized_pnl，成本基不能再扣一次；新成本 = 20×1000/1500 = 13.3333…
	got, ok = computeCorpAdjust(1000, 20, 1000, 2, 3, 1.5)
	if !ok {
		t.Fatal("送转派应可折算")
	}
	if got.QtyAfter != 1500 || got.CashDividend != 150 || got.CostAfter != 13.3333 {
		t.Fatalf("送转派折算错: %+v（期望 qty=1500 cash=150 cost=13.3333）", got)
	}

	// ③ 纯派息（无送转）：数量与成本不变，现金只进已实现盈亏。
	got, ok = computeCorpAdjust(500, 10, 500, 0, 0, 0.5)
	if !ok {
		t.Fatal("纯派息应可折算")
	}
	if got.QtyAfter != 500 || got.CostAfter != 10 || got.CashDividend != 25 {
		t.Fatalf("纯派息折算错: %+v", got)
	}

	// ④ 大额派息也不改变成本基，现金如实单独保留。
	got, ok = computeCorpAdjust(100, 0.5, 100, 0, 0, 10) // 现金 = 10×100/10 = 100 > 成本 50
	if !ok || got.CostAfter != 0.5 || got.CashDividend != 100 {
		t.Fatalf("现金分红不得重复冲减成本: %+v ok=%v", got, ok)
	}

	// ⑤ 边界：无持仓 / 无成本 / 空方案一律不折算。
	if _, ok := computeCorpAdjust(0, 20, 1000, 0, 10, 0); ok {
		t.Fatal("零数量不应折算")
	}
	if _, ok := computeCorpAdjust(1000, 0, 1000, 0, 10, 0); ok {
		t.Fatal("零成本不应折算")
	}
	if _, ok := computeCorpAdjust(1000, 20, 1000, 0, 0, 0); ok {
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
		// D14~D16 新增：同进程共用一个内存库，带唯一键的新表必须一并清，
		// 否则上一个用例的行会撞 sell_reviews 的 (user, position, trigger, trade_date) 唯一键。
		"sell_reviews", "alert_rules", "alert_events", "daily_bars",
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
		TotalBuyCost: 20000, TotalBuyQty: 1000, RemainingCost: 20000,
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
		ReportDate: "2025-12-31", RecordDate: previousDate(exDate), ExDate: exDate,
		Progress:   model.CorpActionProgressImplemented,
		BonusRatio: bonus, TransferRatio: transfer, DividendPretax: dividend,
		PlanProfile: "10转10派1.00元(含税)",
	}
	if err := common.DB.Create(a).Error; err != nil {
		t.Fatalf("建方案失败: %v", err)
	}
	return p, a
}

func previousDate(date string) string {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return ""
	}
	return d.AddDate(0, 0, -1).Format("2006-01-02")
}

func localDateTime(t *testing.T, date string, hour int) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", date+" "+fmt.Sprintf("%02d:00", hour), time.Local)
	if err != nil {
		t.Fatalf("构造测试时间失败: %v", err)
	}
	return v
}

// TestCorpAdjustUsesRecordDateEntitlement 只有登记日收盘时持有的数量享有送转派。
// 除权日新买入的份额已经按除权价成交，若再次折算会凭空造股、造现金。
func TestCorpAdjustUsesRecordDateEntitlement(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	recordDate, exDate := "2026-07-28", "2026-07-29"
	p := &model.Position{
		UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding,
		BuyPrice: 10, Quantity: 150, BuyDate: recordDate,
		TotalBuyCost: 1500, TotalBuyQty: 150,
	}
	if err := common.DB.Create(p).Error; err != nil {
		t.Fatalf("建持仓失败: %v", err)
	}
	trades := []model.PositionTrade{
		{UserID: 1, PositionID: p.ID, Side: model.PositionTradeBuy, Price: 10, Quantity: 100,
			TradeDate: recordDate, AvgCostAfter: 10, QuantityAfter: 100},
		{UserID: 1, PositionID: p.ID, Side: model.PositionTradeBuy, Price: 10, Quantity: 50,
			TradeDate: exDate, AvgCostAfter: 10, QuantityAfter: 150},
	}
	if err := common.DB.Create(&trades).Error; err != nil {
		t.Fatalf("建流水失败: %v", err)
	}
	a := &model.CorporateAction{
		Symbol: "600000", Market: "cn", ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: exDate, TransferRatio: 10, DividendPretax: 1,
		Progress: model.CorpActionProgressImplemented,
	}
	if err := common.DB.Create(a).Error; err != nil {
		t.Fatalf("建方案失败: %v", err)
	}

	n, err := GenerateCorpAdjusts(1, exDate)
	if err != nil || n != 1 {
		t.Fatalf("应生成一条按登记日数量计算的建议: n=%d err=%v", n, err)
	}
	rows, err := ListCorpAdjusts(1, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取建议失败: rows=%+v err=%v", rows, err)
	}
	adj := rows[0]
	// 当前 150 股中只有登记日的 100 股有权：送 100 股、派 10 元。
	// 现金单独进已实现盈亏，当前总成本 1500 不再扣现金，250 股的新成本为 6 元。
	if adj.EntitledQty != 100 || adj.QtyAfter != 250 || adj.CashDividend != 10 || adj.CostAfter != 6 {
		t.Fatalf("登记日有权数量折算错误: %+v", adj)
	}
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

func TestGenerateCorpAdjustsLookbackAndRefreshesPending(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	asOfTime := time.Now().In(time.Local)
	asOf := asOfTime.Format("2006-01-02")
	exDate := asOfTime.AddDate(0, 0, -20).Format("2006-01-02")
	recordDate := asOfTime.AddDate(0, 0, -21).Format("2006-01-02")
	buyDate := asOfTime.AddDate(0, 0, -30).Format("2006-01-02")
	p := &model.Position{
		UserID: 1, Symbol: "600020", Market: "cn", Name: "回补测试",
		PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding,
		BuyPrice: 10, Quantity: 100, BuyDate: buyDate,
		TotalBuyCost: 1000, TotalBuyQty: 100, RemainingCost: 1000,
	}
	if err := common.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PositionTrade{
		UserID: 1, PositionID: p.ID, Side: model.PositionTradeBuy,
		Price: 10, Quantity: 100, TradeDate: buyDate, AvgCostAfter: 10, QuantityAfter: 100,
	}).Error; err != nil {
		t.Fatal(err)
	}
	a := &model.CorporateAction{
		Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: exDate, TransferRatio: 10,
		Progress: model.CorpActionProgressImplemented,
	}
	if err := common.DB.Create(a).Error; err != nil {
		t.Fatal(err)
	}

	if n, err := GenerateCorpAdjusts(1, asOf); err != nil || n != 1 {
		t.Fatalf("20 天前漏跑的除权应在 30 天窗口内回补: n=%d err=%v", n, err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	if len(rows) != 1 || rows[0].QtyBefore != 100 || rows[0].QtyAfter != 200 {
		t.Fatalf("首次回补建议错误: %+v", rows)
	}
	adjustID := rows[0].ID

	// 建议尚未确认时用户又加仓。后买的 50 股不享有旧公司行动，但 pending 建议必须
	// 刷新当前账面，不能永久保留生成时的 100 股旧快照。
	if _, err := (&PositionService{}).AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 20, Quantity: 50, TradeDate: asOf,
	}); err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	if n, err := GenerateCorpAdjusts(1, asOf); err != nil || n != 0 {
		t.Fatalf("刷新既有 pending 不应计作新建: n=%d err=%v", n, err)
	}
	rows, _ = ListCorpAdjusts(1, model.CorpAdjustPending)
	if len(rows) != 1 || rows[0].ID != adjustID || rows[0].QtyBefore != 150 ||
		rows[0].EntitledQty != 100 || rows[0].QtyAfter != 250 || rows[0].CostAfter != 8 {
		t.Fatalf("pending 建议未按当前账面刷新: %+v", rows)
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

	// 账本：数量 1000→2000、成本 20×1000/2000=10、已实现 +100（现金分红）。
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Quantity != 2000 || after.BuyPrice != 10 || after.RealizedPnl != 100 ||
		after.RemainingCost != 20000 {
		t.Fatalf("账本折算错: qty=%v cost=%v remaining=%v realized=%v",
			after.Quantity, after.BuyPrice, after.RemainingCost, after.RealizedPnl)
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
		trade.AvgCostBefore != 20 || trade.AvgCostAfter != 10 {
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
	if back.Quantity != 1000 || back.BuyPrice != 20 || back.RealizedPnl != 0 ||
		back.RemainingCost != 20000 {
		t.Fatalf("撤销未回滚账本: qty=%v cost=%v remaining=%v realized=%v",
			back.Quantity, back.BuyPrice, back.RemainingCost, back.RealizedPnl)
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
	// 送转后的市值恰好等于原始投入时，最终收益应只剩现金分红 100 元。
	// 若确认时既冲减成本又把分红计入 realized，这里会错误变成 200 元。
	closed, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 10, Quantity: 2000, TradeDate: today,
	})
	if err != nil {
		t.Fatalf("折算后清仓失败: %v", err)
	}
	if closed.RealizedPnl != 100 || closed.RemainingCost != 0 {
		t.Fatalf("现金分红被重复计算或剩余成本未结清: realized=%v remaining=%v",
			closed.RealizedPnl, closed.RemainingCost)
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

// TestCorpAdjustBlocksClosingBeforeShareAdjustment 送转必须先于除权日卖出入账，
// 否则卖出会按旧成本结转，事后只改当前聚合态无法修复已实现盈亏。
func TestCorpAdjustBlocksClosingBeforeShareAdjustment(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1, today, 0, 10, 0)
	if _, err := GenerateCorpAdjusts(1, today); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	svc := &PositionService{}
	if _, err := svc.Close(1, p.ID, CloseInput{SellPrice: 22, SellDate: today}); err == nil {
		t.Fatal("到期送转未处理时必须阻止卖出，不能先按旧成本结转")
	}
	var unchanged model.Position
	common.DB.First(&unchanged, p.ID)
	if unchanged.Status != model.PositionStatusHolding || unchanged.Quantity != 1000 {
		t.Fatalf("被阻止的平仓不得改动账本: %+v", unchanged)
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatalf("先确认送转应成功: %v", err)
	}
	if _, err := svc.Close(1, p.ID, CloseInput{SellPrice: 11, SellDate: today}); err != nil {
		t.Fatalf("送转入账后应允许平仓: %v", err)
	}
}

func TestGenerateCorpAdjustsCreatesManualReviewForPostExSell(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	recordDate := previousDate(today)
	p := seedHoldingWithLedger(t, 1, "600018", 10, 100, 0, 0, recordDate)
	svc := &PositionService{}
	// 复刻旧版本已发生的错误顺序：公司行动尚未同步时先在除权日卖出。
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 5, Quantity: 50, TradeDate: today,
	}); err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{
		Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: today, TransferRatio: 10,
	}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	var before model.Position
	common.DB.First(&before, p.ID)
	if n, err := GenerateCorpAdjusts(1, today); err == nil || n != 1 {
		t.Fatalf("已有除权后卖出时应生成可操作的人工核对记录并显式报错: n=%d err=%v", n, err)
	}
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Quantity != before.Quantity || after.BuyPrice != before.BuyPrice ||
		after.RemainingCost != before.RemainingCost || after.RealizedPnl != before.RealizedPnl {
		t.Fatalf("拒绝补跑不得进一步污染账本: before=%+v after=%+v", before, after)
	}
	rows, err := ListCorpAdjusts(1, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 {
		t.Fatalf("人工核对记录应在持仓页可见: rows=%+v err=%v", rows, err)
	}
	adj := rows[0]
	if !adj.ManualReview || adj.ReviewReason == "" || adj.QtyAfter != before.Quantity ||
		adj.CostAfter != before.BuyPrice {
		t.Fatalf("不安全的送转只能生成保持当前账面的人工核对记录: %+v", adj)
	}
	if _, err := svc.ConfirmCorpAdjust(1, adj.ID); err == nil {
		t.Fatal("人工核对记录绝不能被确认成自动折算")
	}
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 5, Quantity: 10, TradeDate: today,
	}); err == nil {
		t.Fatal("人工核对记录未明确处理前仍应阻止后续卖出")
	}
	if _, err := svc.DismissCorpAdjust(1, adj.ID); err != nil {
		t.Fatalf("人工核对后应能显式忽略并解除拦截: %v", err)
	}
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 5, Quantity: 10, TradeDate: today,
	}); err != nil {
		t.Fatalf("人工核对记录被忽略后应允许继续交易: %v", err)
	}
}

func TestClosedShareActionCreatesManualReview(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	recordDate := previousDate(today)
	p := seedHoldingWithLedger(t, 1, "600027", 10, 100, 0, 0, recordDate)
	svc := &PositionService{}
	if _, err := svc.Close(1, p.ID, CloseInput{SellPrice: 10, SellDate: today}); err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{
		Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: today, TransferRatio: 10,
	}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := GenerateCorpAdjusts(1, today); err == nil || n != 1 {
		t.Fatalf("已平仓送转应生成不可自动确认的人工核对记录: n=%d err=%v", n, err)
	}
	rows, err := ListCorpAdjusts(1, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 || !rows[0].ManualReview || rows[0].ReviewReason == "" {
		t.Fatalf("已平仓送转人工核对记录错误: rows=%+v err=%v", rows, err)
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err == nil {
		t.Fatal("已平仓送转人工核对记录不能自动确认")
	}
}

func TestCorpAdjustRequiresEarlierShareActionBeforeLaterDividend(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now().In(time.Local)
	d1Ex := now.AddDate(0, 0, -2).Format("2006-01-02")
	d1Record := now.AddDate(0, 0, -3).Format("2006-01-02")
	d2Ex := now.Format("2006-01-02")
	d2Record := now.AddDate(0, 0, -1).Format("2006-01-02")
	p := seedHoldingWithLedger(t, 1, "600028", 10, 100, 0, 0,
		now.AddDate(0, 0, -10).Format("2006-01-02"))
	actions := []model.CorporateAction{
		{Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-06-30",
			RecordDate: d1Record, ExDate: d1Ex, TransferRatio: 10},
		{Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-12-31",
			RecordDate: d2Record, ExDate: d2Ex, DividendPretax: 1},
	}
	if err := common.DB.Create(&actions).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := GenerateCorpAdjusts(1, d2Ex); err != nil || n != 2 {
		t.Fatalf("生成两次公司行动失败: n=%d err=%v", n, err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	if len(rows) != 2 || rows[0].CorporateActionID != actions[0].ID {
		t.Fatalf("待确认建议应按最早除权日优先: %+v", rows)
	}
	svc := &PositionService{}
	if _, err := svc.ConfirmCorpAdjust(1, rows[1].ID); err == nil {
		t.Fatal("后续纯分红也必须等待会改变其登记日权益的前序送转")
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatalf("确认前序送转失败: %v", err)
	}
	if _, err := GenerateCorpAdjusts(1, d2Ex); err != nil {
		t.Fatalf("前序确认后刷新后续建议失败: %v", err)
	}
	rows, _ = ListCorpAdjusts(1, model.CorpAdjustPending)
	if len(rows) != 1 || rows[0].EntitledQty != 200 || rows[0].CashDividend != 20 {
		t.Fatalf("后续分红必须按前序送转后的 200 股权益刷新: %+v", rows)
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatalf("确认后续分红失败: %v", err)
	}
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Quantity != 200 || after.RealizedPnl != 20 {
		t.Fatalf("顺序入账结果错误: %+v", after)
	}
}

func TestCorpAdjustTwoShareActionsRefreshSequentially(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now().In(time.Local)
	d1Ex := now.AddDate(0, 0, -2).Format("2006-01-02")
	d2Ex := now.Format("2006-01-02")
	p := seedHoldingWithLedger(t, 1, "600029", 10, 100, 0, 0,
		now.AddDate(0, 0, -10).Format("2006-01-02"))
	actions := []model.CorporateAction{
		{Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-06-30",
			RecordDate: previousDate(d1Ex), ExDate: d1Ex, TransferRatio: 10},
		{Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-12-31",
			RecordDate: previousDate(d2Ex), ExDate: d2Ex, TransferRatio: 10},
	}
	if err := common.DB.Create(&actions).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCorpAdjusts(1, d2Ex); err != nil {
		t.Fatal(err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	svc := &PositionService{}
	if _, err := svc.ConfirmCorpAdjust(1, rows[1].ID); err == nil {
		t.Fatal("两次送转不得倒序确认")
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCorpAdjusts(1, d2Ex); err != nil {
		t.Fatal(err)
	}
	rows, _ = ListCorpAdjusts(1, model.CorpAdjustPending)
	if len(rows) != 1 || rows[0].QtyBefore != 200 || rows[0].EntitledQty != 200 || rows[0].QtyAfter != 400 {
		t.Fatalf("第二次送转未按第一次后的权益刷新: %+v", rows)
	}
	if _, err := svc.ConfirmCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Quantity != 400 || after.BuyPrice != 2.5 {
		t.Fatalf("两次 10 转 10 应从 100 股变为 400 股: %+v", after)
	}
}

func TestPendingShareAdjustDoesNotExpireFromTradeGate(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now().In(time.Local)
	exDate := now.AddDate(0, 0, -(corpAdjustLookbackDays + 1)).Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1, exDate, 0, 10, 0)
	if n, err := GenerateCorpAdjusts(1, exDate); err != nil || n != 1 {
		t.Fatalf("生成历史待处理送转失败: n=%d err=%v", n, err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	svc := &PositionService{}
	today := now.Format("2006-01-02")
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 10, Quantity: 100, TradeDate: today,
	}); err == nil {
		t.Fatal("超过 30 天但仍 pending 的送转不能自动退出交易闸门")
	}
	if _, err := svc.DismissCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeSell, Price: 10, Quantity: 100, TradeDate: today,
	}); err != nil {
		t.Fatalf("明确忽略后应解除历史送转拦截: %v", err)
	}
}

func TestClosedCashDividendCanConfirmAndRevert(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1, today, 0, 0, 1)
	svc := &PositionService{}
	if _, err := svc.Close(1, p.ID, CloseInput{SellPrice: 20, SellDate: today}); err != nil {
		t.Fatalf("纯派息不影响卖出成本，应允许先平仓: %v", err)
	}
	if n, err := GenerateCorpAdjusts(1, today); err != nil || n != 1 {
		t.Fatalf("已平仓但登记日有权的纯现金分红仍应生成建议: n=%d err=%v", n, err)
	}
	rows, err := ListCorpAdjusts(1, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 || rows[0].CashDividend != 100 || rows[0].QtyAfter != 0 {
		t.Fatalf("已平仓现金分红建议错误: rows=%+v err=%v", rows, err)
	}
	confirmed, err := svc.ConfirmCorpAdjust(1, rows[0].ID)
	if err != nil {
		t.Fatalf("已平仓纯现金分红应可确认: %v", err)
	}
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Status != model.PositionStatusClosed || after.Quantity != 0 || after.RealizedPnl != 100 {
		t.Fatalf("现金分红只能增加已实现收益，不得复活持仓: %+v", after)
	}
	var trade model.PositionTrade
	common.DB.First(&trade, confirmed.TradeID)
	if trade.Side != model.PositionTradeAdjust || trade.Quantity != 0 || trade.RealizedPnl != 100 {
		t.Fatalf("已平仓现金分红应落零数量审计流水: %+v", trade)
	}
	if _, err := svc.RevertCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatalf("已平仓纯现金分红应可撤销: %v", err)
	}
	common.DB.First(&after, p.ID)
	if after.Status != model.PositionStatusClosed || after.RealizedPnl != 0 {
		t.Fatalf("撤销后应只回退现金收益: %+v", after)
	}
}

func TestCorpAdjustUsesExactRemainingCost(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	recordDate := previousDate(today)
	p := model.Position{
		UserID: 1, Symbol: "600019", Market: "cn", Status: model.PositionStatusHolding,
		PositionType: model.PositionTypeLongTerm, BuyDate: "2026-01-01",
		BuyPrice: 1.0001, Quantity: 1000000, TotalBuyCost: 1000060,
		TotalBuyQty: 1000000, RemainingCost: 1000060,
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
	action := model.CorporateAction{
		Symbol: p.Symbol, Market: p.Market, ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: today, TransferRatio: 10,
	}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := GenerateCorpAdjusts(1, today); err != nil || n != 1 {
		t.Fatalf("生成失败: n=%d err=%v", n, err)
	}
	rows, _ := ListCorpAdjusts(1, model.CorpAdjustPending)
	// 精确价格成本 1,000,060 / 2,000,000 = 0.50003 -> 0.5000；若误用
	// 四位 BuyPrice×Quantity，会得到 0.50005 -> 0.5001。
	if len(rows) != 1 || rows[0].CostAfter != 0.5 {
		t.Fatalf("送转后均价必须由精确剩余成本计算: %+v", rows)
	}
	if _, err := (&PositionService{}).ConfirmCorpAdjust(1, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	var after model.Position
	common.DB.First(&after, p.ID)
	if after.Quantity != 2000000 || after.BuyPrice != 0.5 || after.RemainingCost != 1000060 {
		t.Fatalf("送转不得丢失精确成本余额: %+v", after)
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
	recordDate := previousDate(today)
	h := &model.PaperHolding{UserID: 7, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Quantity: 150, AvgCost: 20}
	if err := common.DB.Create(h).Error; err != nil {
		t.Fatalf("建模拟持仓失败: %v", err)
	}
	// 登记日持有 100 股，除权日又买 50 股；后 50 股没有本次权益。
	seedTrades := []model.PaperTrade{
		{UserID: 7, Symbol: "600000", Market: "cn", Side: model.PaperSideBuy, Price: 20,
			Quantity: 100, Amount: 2000, TradeDate: recordDate, CreatedAt: localDateTime(t, recordDate, 14)},
		{UserID: 7, Symbol: "600000", Market: "cn", Side: model.PaperSideBuy, Price: 20,
			Quantity: 50, Amount: 1000, TradeDate: today, CreatedAt: localDateTime(t, today, 10)},
	}
	if err := common.DB.Create(&seedTrades).Error; err != nil {
		t.Fatalf("建模拟流水失败: %v", err)
	}
	a := &model.CorporateAction{Symbol: "600000", Market: "cn", Name: "浦发银行",
		ReportDate: "2025-12-31", RecordDate: recordDate, ExDate: today,
		BonusRatio: 0, TransferRatio: 10, DividendPretax: 1,
		PlanProfile: "10转10派1.00元(含税)"}
	if err := common.DB.Create(a).Error; err != nil {
		t.Fatalf("建方案失败: %v", err)
	}

	if n := RunPaperCorpAdjust(); n != 1 {
		t.Fatalf("首轮应调整 1 笔，got %d", n)
	}
	var after model.PaperHolding
	common.DB.First(&after, h.ID)
	// 只有 100 股有权：当前 150 + 转增 100 = 250；总成本 3000 / 250 = 12。
	if after.Quantity != 250 || after.AvgCost != 12 {
		t.Fatalf("模拟盘折算错: qty=%v cost=%v", after.Quantity, after.AvgCost)
	}
	var acc2 model.PaperAccount
	common.DB.First(&acc2, acc.ID)
	if acc2.Cash != 50010 {
		t.Fatalf("现金分红未按登记日数量入账: %v（期望 50010）", acc2.Cash)
	}
	var trades []model.PaperTrade
	common.DB.Where("side = ?", model.PaperSideAdjust).Find(&trades)
	if len(trades) != 1 || trades[0].RealizedPnl != 10 || trades[0].Quantity != 100 ||
		trades[0].TradeDate != today {
		t.Fatalf("模拟盘审计流水错: %+v", trades)
	}
	realized, err := paperRealizedPnl(common.DB, 7)
	if err != nil || realized != 10 {
		t.Fatalf("模拟盘累计已实现必须包含 adjust 现金分红: realized=%v err=%v", realized, err)
	}

	// 重跑：按 (user, action) 唯一键幂等，不重复调整也不重复发钱。
	if n := RunPaperCorpAdjust(); n != 0 {
		t.Fatalf("重跑应为 0 笔（幂等），got %d", n)
	}
	common.DB.First(&after, h.ID)
	common.DB.First(&acc2, acc.ID)
	if after.Quantity != 250 || acc2.Cash != 50010 {
		t.Fatalf("重跑污染了账面: qty=%v cash=%v", after.Quantity, acc2.Cash)
	}
	var audits int64
	common.DB.Model(&model.PaperCorpAdjust{}).Count(&audits)
	if audits != 1 {
		t.Fatalf("审计行应只有 1 条，got %d", audits)
	}
}

func TestPaperCorpAdjustAlreadyProcessedThenClosedIsIdempotent(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	recordDate := previousDate(today)
	acc := model.PaperAccount{UserID: 17, InitialCash: 10000, Cash: 9000}
	holding := model.PaperHolding{UserID: 17, Symbol: "600030", Market: "cn",
		Quantity: 100, AvgCost: 10}
	if err := common.DB.Create(&acc).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&holding).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PaperTrade{
		UserID: 17, Symbol: holding.Symbol, Market: holding.Market, Side: model.PaperSideBuy,
		Price: 10, Quantity: 100, Amount: 1000, TradeDate: recordDate,
	}).Error; err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{
		Symbol: holding.Symbol, Market: holding.Market, ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: today, TransferRatio: 10,
	}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	if n := RunPaperCorpAdjust(); n != 1 {
		t.Fatalf("首次模拟盘送转应执行一笔: %d", n)
	}
	// 模拟已成功折算后又清仓；下一轮不应再把它当成“除权后先卖、未折算”。
	if err := common.DB.Where("id = ?", holding.ID).Delete(&model.PaperHolding{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PaperTrade{
		UserID: 17, Symbol: holding.Symbol, Market: holding.Market, Side: model.PaperSideSell,
		Price: 5, Quantity: 200, Amount: 1000, TradeDate: today,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if n := RunPaperCorpAdjust(); n != 0 {
		t.Fatalf("已折算后清仓的送转重跑应安静幂等: %d", n)
	}
	var audits int64
	common.DB.Model(&model.PaperCorpAdjust{}).Where("user_id = ? AND corporate_action_id = ?", 17, action.ID).Count(&audits)
	if audits != 1 {
		t.Fatalf("已折算后清仓不得重复落审计: %d", audits)
	}
}

func TestPaperClosedCashDividendBackfill(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	recordDate := previousDate(today)
	acc := model.PaperAccount{UserID: 8, InitialCash: 100000, Cash: 50000}
	if err := common.DB.Create(&acc).Error; err != nil {
		t.Fatal(err)
	}
	trades := []model.PaperTrade{
		{UserID: 8, Symbol: "600020", Market: "cn", Name: "现金分红样本",
			Side: model.PaperSideBuy, Price: 20, Quantity: 100, TradeDate: recordDate},
		{UserID: 8, Symbol: "600020", Market: "cn", Name: "现金分红样本",
			Side: model.PaperSideSell, Price: 20, Quantity: 100, TradeDate: today},
	}
	if err := common.DB.Create(&trades).Error; err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{
		Symbol: "600020", Market: "cn", Name: "现金分红样本", ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: today, DividendPretax: 1,
	}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	if n := RunPaperCorpAdjust(); n != 1 {
		t.Fatalf("已清仓纯现金分红应补发 1 笔，得到 %d", n)
	}
	var after model.PaperAccount
	common.DB.First(&after, acc.ID)
	if after.Cash != 50010 {
		t.Fatalf("已清仓现金分红未到账: %v", after.Cash)
	}
	var holdingCount int64
	common.DB.Model(&model.PaperHolding{}).Where("user_id = ?", acc.UserID).Count(&holdingCount)
	if holdingCount != 0 {
		t.Fatalf("纯现金补发不得复活已清仓 holding，得到 %d 条", holdingCount)
	}
	var adjust model.PaperTrade
	if err := common.DB.Where("user_id = ? AND side = ?", acc.UserID, model.PaperSideAdjust).
		First(&adjust).Error; err != nil || adjust.Quantity != 0 || adjust.RealizedPnl != 10 {
		t.Fatalf("应落零数量现金审计流水: %+v err=%v", adjust, err)
	}
	if n := RunPaperCorpAdjust(); n != 0 {
		t.Fatalf("补发重跑必须幂等，得到 %d", n)
	}
}

func TestPaperShareActionRunsBeforeSell(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	recordDate := previousDate(today)
	acc := model.PaperAccount{UserID: 9, InitialCash: 100000, Cash: 98000}
	holding := model.PaperHolding{
		UserID: 9, Symbol: "600021", Market: "cn", Name: "送转样本", Quantity: 100, AvgCost: 20,
	}
	if err := common.DB.Create(&acc).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&holding).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PaperTrade{
		UserID: 9, Symbol: holding.Symbol, Market: holding.Market, Name: holding.Name,
		Side: model.PaperSideBuy, Price: 20, Quantity: 100, Amount: 2000, TradeDate: recordDate,
	}).Error; err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{
		Symbol: holding.Symbol, Market: holding.Market, ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: today, TransferRatio: 10,
	}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PaperService{}
	trade, err := svc.Trade(context.Background(), 9, TradeInput{
		Symbol: holding.Symbol, Market: holding.Market, Side: model.PaperSideSell,
		Price: 10, Quantity: 50,
	})
	if err != nil {
		t.Fatalf("模拟盘应先自动送转再卖出: %v", err)
	}
	var after model.PaperHolding
	common.DB.First(&after, holding.ID)
	if after.Quantity != 150 || after.AvgCost != 10 {
		t.Fatalf("应先 100→200@10 再卖 50，得到 %+v", after)
	}
	if trade.RealizedPnl != -5.25 {
		t.Fatalf("卖出应按送转后成本结转（仅亏费税 5.25），得到 %v", trade.RealizedPnl)
	}
	var audits int64
	common.DB.Model(&model.PaperCorpAdjust{}).Where("user_id = ?", 9).Count(&audits)
	if audits != 1 {
		t.Fatalf("卖出前必须已有送转审计，得到 %d", audits)
	}
}

func TestPaperHistoricalPostExSellRejectsShareBackfill(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	recordDate := previousDate(today)
	acc := model.PaperAccount{UserID: 10, InitialCash: 100000, Cash: 100000}
	if err := common.DB.Create(&acc).Error; err != nil {
		t.Fatal(err)
	}
	trades := []model.PaperTrade{
		{UserID: 10, Symbol: "600022", Market: "cn", Side: model.PaperSideBuy,
			Price: 10, Quantity: 100, TradeDate: recordDate},
		{UserID: 10, Symbol: "600022", Market: "cn", Side: model.PaperSideSell,
			Price: 5, Quantity: 100, TradeDate: today},
	}
	if err := common.DB.Create(&trades).Error; err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{
		Symbol: "600022", Market: "cn", ReportDate: "2025-12-31",
		RecordDate: recordDate, ExDate: today, TransferRatio: 10,
	}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	if n := RunPaperCorpAdjust(); n != 0 {
		t.Fatalf("除权后已清仓的送转不能倒序套当前聚合态，得到 %d 笔", n)
	}
	if err := ensurePaperCorpAdjustBeforeTrade(10, action.Symbol, action.Market, today); err == nil {
		t.Fatal("历史送转已先卖出时，后续交易必须显式报账本需核对")
	}
	var audits int64
	common.DB.Model(&model.PaperCorpAdjust{}).Where("user_id = ?", 10).Count(&audits)
	if audits != 0 {
		t.Fatalf("不安全补跑不得留下已执行审计，得到 %d", audits)
	}
}
