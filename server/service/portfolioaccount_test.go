package service

import (
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func cleanPortfolioAccountTables(t *testing.T) {
	t.Helper()
	for _, m := range []any{&model.PortfolioCashFlow{}, &model.TargetAllocationRevision{}, &model.PortfolioSnapshot{}, &model.PositionTrade{}, &model.PositionCorpAdjust{}, &model.Position{}, &model.PaperTrade{}, &model.PaperHolding{}, &model.PaperCorpAdjust{}, &model.PaperAccount{}, &model.PortfolioAccount{}} {
		common.DB.Where("1=1").Delete(m)
	}
	t.Cleanup(func() {
		for _, m := range []any{&model.PortfolioCashFlow{}, &model.TargetAllocationRevision{}, &model.PortfolioSnapshot{}, &model.PositionTrade{}, &model.PositionCorpAdjust{}, &model.Position{}, &model.PaperTrade{}, &model.PaperHolding{}, &model.PaperCorpAdjust{}, &model.PaperAccount{}, &model.PortfolioAccount{}} {
			common.DB.Where("1=1").Delete(m)
		}
	})
}

func TestPortfolioAccountDeleteAllowsOnlyEmptyAccount(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	svc := NewPortfolioAccountService()
	if _, err := svc.Create(100, PortfolioAccountInput{Name: "默认模拟账户", Kind: model.PortfolioKindPaper, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	empty, err := svc.Create(100, PortfolioAccountInput{Name: "空模拟账户", Kind: model.PortfolioKindPaper, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(100, empty.ID); err != nil {
		t.Fatalf("系统自动创建的空资金账户不应阻止删除: %v", err)
	}
	var paperCount int64
	common.DB.Model(&model.PaperAccount{}).Where("account_id = ?", empty.ID).Count(&paperCount)
	if paperCount != 0 {
		t.Fatalf("删除组合后不应残留模拟资金账户: %d", paperCount)
	}

	withFact, err := svc.Create(100, PortfolioAccountInput{Name: "有事实账户", Kind: model.PortfolioKindPaper, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PaperHolding{UserID: 100, AccountID: withFact.ID, Symbol: "600000", Market: "cn", Quantity: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(100, withFact.ID); err == nil {
		t.Fatal("已有持仓事实的账户必须拒绝删除")
	}
}

func TestPortfolioAccountIsolationAndKinds(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	svc := NewPortfolioAccountService()
	a1, err := svc.Create(101, PortfolioAccountInput{Name: "真实一", Kind: "real", Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := svc.Create(101, PortfolioAccountInput{Name: "真实二", Kind: "real", Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	p1, err := svc.Create(101, PortfolioAccountInput{Name: "模拟一", Kind: "paper", Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Create(202, PortfolioAccountInput{Name: "他人", Kind: "real", Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	pos := model.Position{UserID: 101, AccountID: a2.ID, Symbol: "600000", Market: "cn", Status: model.PositionStatusHolding}
	if err := common.DB.Create(&pos).Error; err != nil {
		t.Fatal(err)
	}
	if err := ValidatePositionAccount(101, a1.ID, pos.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("同用户不同账户不得串读: %v", err)
	}
	if _, err := PortfolioAccountByID(101, other.ID, ""); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("跨用户账户应表现为不存在: %v", err)
	}
	if _, err := PortfolioAccountByID(101, p1.ID, model.PortfolioKindReal); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("real/paper 不得混用: %v", err)
	}
	if _, err := svc.SetDefault(101, a2.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.List(101)
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, row := range rows {
		if row.Kind == model.PortfolioKindReal && row.IsDefault {
			defaults++
			if row.ID != a2.ID {
				t.Fatalf("默认切换错误: %+v", row)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("每种 kind 只能一个默认: %d", defaults)
	}
	if _, err := svc.Archive(101, a2.ID); err == nil {
		t.Fatal("默认账户不得归档")
	}
	if _, err := svc.Archive(101, a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePortfolioAccount(101, -1, model.PortfolioKindReal); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("非法 account_id 必须 fail-closed: %v", err)
	}
}

func TestCashFlowIdempotencyReversalAndValidation(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	account, err := NewPortfolioAccountService().Create(303, PortfolioAccountInput{Name: "现金流", Kind: "real", Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	in := CashFlowInput{Type: model.CashFlowDeposit, Amount: 10000, TradeDate: "2026-08-01", Note: "初始入金", IdempotencyKey: "deposit-1"}
	first, err := CreatePortfolioCashFlow(303, account.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreatePortfolioCashFlow(303, account.ID, in)
	if err != nil || second.ID != first.ID {
		t.Fatalf("重复请求必须幂等: first=%+v second=%+v err=%v", first, second, err)
	}
	if _, err := CreatePortfolioCashFlow(303, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: -1, TradeDate: "2026-08-01", IdempotencyKey: "bad"}); err == nil {
		t.Fatal("非法入金金额必须拒绝")
	}
	if _, err := CreatePortfolioCashFlow(303, account.ID, CashFlowInput{Type: model.CashFlowWithdrawal, Amount: -1, TradeDate: "bad", IdempotencyKey: "bad-date"}); err == nil {
		t.Fatal("非法日期必须拒绝")
	}
	future := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if _, err := CreatePortfolioCashFlow(303, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 1, TradeDate: future, IdempotencyKey: "future-date"}); err == nil {
		t.Fatal("未来现金流日期必须拒绝")
	}
	rev, err := ReversePortfolioCashFlow(303, account.ID, first.ID, "reverse-1", "录入错误")
	if err != nil {
		t.Fatal(err)
	}
	again, err := ReversePortfolioCashFlow(303, account.ID, first.ID, "reverse-2", "重复")
	if err != nil || again.ID != rev.ID {
		t.Fatalf("冲正必须幂等: %+v %+v %v", rev, again, err)
	}
	if rev.Amount != -first.Amount || rev.ReversalOfID == nil || *rev.ReversalOfID != first.ID {
		t.Fatalf("冲正事实错误: %+v", rev)
	}
	if err := common.DB.Model(first).Update("note", "篡改").Error; err == nil {
		t.Fatal("原现金流不可修改")
	}
	if err := common.DB.Delete(first).Error; err == nil {
		t.Fatal("原现金流不可删除")
	}
}

func TestArchivedPortfolioRejectsFactWrites(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	svc := NewPortfolioAccountService()
	if _, err := svc.Create(304, PortfolioAccountInput{Name: "默认账户", Kind: model.PortfolioKindReal, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	archived, err := svc.Create(304, PortfolioAccountInput{Name: "已归档账户", Kind: model.PortfolioKindReal, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Archive(304, archived.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := CreatePortfolioCashFlow(304, archived.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 100, TradeDate: "2026-08-01", IdempotencyKey: "archived"}); err == nil {
		t.Fatal("已归档账户不得新增现金流")
	}
	risk := NewPortfolioRiskService(&MarketService{}, NewPositionService(&MarketService{}))
	if _, err := risk.SaveTargets(304, archived.ID, []TargetAllocationItem{{Type: "symbol", Key: "600000", TargetWeightPct: 50, Enabled: true}}); err == nil {
		t.Fatal("已归档账户不得新增目标配置 revision")
	}
}

func TestStressAndRebalanceAreReadOnlyAndRevisionStable(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	account, err := NewPortfolioAccountService().Create(404, PortfolioAccountInput{Name: "只读模拟组合", Kind: model.PortfolioKindPaper, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	risk := NewPortfolioRiskService(&MarketService{}, NewPositionService(&MarketService{}))
	rev1, err := risk.SaveTargets(404, account.ID, []TargetAllocationItem{{Type: "symbol", Key: "600000", TargetWeightPct: 50, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := risk.SaveTargets(404, account.ID, []TargetAllocationItem{{Type: "symbol", Key: "600000", TargetWeightPct: 60, Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	countFacts := func() [4]int64 {
		var out [4]int64
		common.DB.Model(&model.Position{}).Where("user_id = ? AND account_id = ?", 404, account.ID).Count(&out[0])
		common.DB.Model(&model.PositionTrade{}).Where("user_id = ? AND account_id = ?", 404, account.ID).Count(&out[1])
		common.DB.Model(&model.PaperHolding{}).Where("user_id = ? AND account_id = ?", 404, account.ID).Count(&out[2])
		common.DB.Model(&model.PaperTrade{}).Where("user_id = ? AND account_id = ?", 404, account.ID).Count(&out[3])
		return out
	}
	before := countFacts()
	stress, err := risk.Stress(t.Context(), 404, account.ID, StressScenario{Type: "market", ShockPct: -10})
	if err != nil || !stress.ReadOnly {
		t.Fatalf("压力测试失败或非只读: out=%+v err=%v", stress, err)
	}
	draft, err := risk.Rebalance(t.Context(), 404, account.ID, rev1.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !draft.ReadOnly || draft.Revision != rev1.Revision || len(draft.Items) != 1 || draft.Items[0].TargetWeightPct != 50 {
		t.Fatalf("历史 revision 回放漂移或草案非只读: %+v", draft)
	}
	if after := countFacts(); after != before {
		t.Fatalf("压力测试/再平衡不得写入持仓或流水: before=%v after=%v", before, after)
	}
}

func TestRealCashBalanceCanReadLegacyHoldingWithoutWritingLedger(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	account, err := NewPortfolioAccountService().Create(407, PortfolioAccountInput{Name: "旧真实账户", Kind: model.PortfolioKindReal, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreatePortfolioCashFlow(407, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 10000, TradeDate: "2026-08-01", IdempotencyKey: "legacy-cash"}); err != nil {
		t.Fatal(err)
	}
	position := model.Position{UserID: 407, AccountID: account.ID, Symbol: "600000", Market: "cn", Status: model.PositionStatusHolding,
		BuyPrice: 10, BuyDate: "2026-08-01", Quantity: 100}
	if err := common.DB.Create(&position).Error; err != nil {
		t.Fatal(err)
	}
	cash, reason, err := realCashBalance(common.DB, 407, account.ID, "2026-08-02")
	if err != nil || reason != "" || cash != 9000 {
		t.Fatalf("旧持仓现金应按字段只读重建: cash=%v reason=%q err=%v", cash, reason, err)
	}
	var tradeCount int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", position.ID).Count(&tradeCount)
	if tradeCount != 0 {
		t.Fatalf("风险计算不得为旧持仓补写流水: %d", tradeCount)
	}
}

func TestRealStressAndRebalanceDoNotBackfillLedger(t *testing.T) {
	setupTestDB(t)
	cleanPortfolioAccountTables(t)
	account, err := NewPortfolioAccountService().Create(405, PortfolioAccountInput{Name: "只读真实组合", Kind: model.PortfolioKindReal, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	legacy := model.Position{UserID: 405, AccountID: account.ID, Symbol: "600000", Market: "cn", Name: "旧持仓", PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: "CNY", BuyPrice: 10, BuyDate: "2026-08-01", Quantity: 100}
	if err := common.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := CreatePortfolioCashFlow(405, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 10000, TradeDate: "2026-08-01", IdempotencyKey: "readonly-deposit"}); err != nil {
		t.Fatal(err)
	}
	risk := NewPortfolioRiskService(&MarketService{}, NewPositionService(&MarketService{}))
	if _, err := risk.SaveTargets(405, account.ID, []TargetAllocationItem{{Type: "symbol", Key: "600000", TargetWeightPct: 50, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	var beforeTrades int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", legacy.ID).Count(&beforeTrades)
	if _, err := risk.Stress(t.Context(), 405, account.ID, StressScenario{Type: "market", ShockPct: -10}); err != nil {
		t.Fatal(err)
	}
	if _, err := risk.Rebalance(t.Context(), 405, account.ID, 1); err != nil {
		t.Fatal(err)
	}
	var afterTrades int64
	common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", legacy.ID).Count(&afterTrades)
	var after model.Position
	if err := common.DB.First(&after, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if beforeTrades != 0 || afterTrades != 0 || after.TotalBuyCost != 0 || after.PeakPrice != 0 {
		t.Fatalf("只读风险路径不得惰性补流水或峰值: before=%d after=%d position=%+v", beforeTrades, afterTrades, after)
	}
}
