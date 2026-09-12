package service

import (
	"context"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestPaperLargeQuantityKeepsFeesInRoundTripPnL(t *testing.T) {
	setupTestDB(t)
	checkPaperLargeQuantityRoundTrip(t)
}

func checkPaperLargeQuantityRoundTrip(t *testing.T) {
	t.Helper()
	for _, pieces := range [][]float64{{1000000}, {333333, 666667}} {
		svc := &PaperService{}
		account, err := NewPortfolioAccountService().Create(1020, PortfolioAccountInput{Name: "大数量成本核对", Kind: model.PortfolioKindPaper})
		if err != nil {
			t.Fatal(err)
		}
		buy, err := svc.TradeByAccount(context.Background(), 1020, account.ID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 0.01, Quantity: 1000000})
		if err != nil {
			t.Fatal(err)
		}
		pnl, fees := 0.0, buy.Fee
		for _, quantity := range pieces {
			sell, err := svc.TradeByAccount(context.Background(), 1020, account.ID, TradeInput{Symbol: "510300", Market: "cn", Side: "sell", Price: 0.01, Quantity: quantity})
			if err != nil {
				t.Fatal(err)
			}
			pnl += sell.RealizedPnl
			fees += sell.Fee
		}
		var cash model.PaperAccount
		if err := common.DB.Where("account_id = ?", account.ID).First(&cash).Error; err != nil {
			t.Fatal(err)
		}
		if math.Abs(pnl+fees) > 0.005 || math.Abs(pnl-(cash.Cash-cash.InitialCash)) > 0.005 {
			t.Errorf("等价买卖的净亏损必须等于全额费用，不能用截断均价漏掉买入佣金：pieces=%v pnl=%v cashChange=%v fees=%v", pieces, pnl, cash.Cash-cash.InitialCash, fees)
		}
	}
}

func TestPaperLegacyCostRecoveryDoesNotRewriteHistory(t *testing.T) {
	setupTestDB(t)
	svc := &PaperService{}
	account, err := svc.GetOrCreateAccount(1021)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TradeByAccount(context.Background(), 1021, account.AccountID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 0.01, Quantity: 1000000}); err != nil {
		t.Fatal(err)
	}
	var holding model.PaperHolding
	if err := common.DB.Where("account_id = ?", account.AccountID).First(&holding).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&holding).Update("remaining_cost", nil).Error; err != nil {
		t.Fatal(err)
	}
	holding.RemainingCost = nil
	if note, err := restorePaperHoldingCost(common.DB, &holding); err != nil || note != "" || paperHoldingCost(holding) != 10005 {
		t.Fatalf("完整流水应恢复丢失的手续费：note=%s err=%v cost=%v", note, err, paperHoldingCost(holding))
	}
	var stored model.PaperHolding
	if err := common.DB.First(&stored, holding.ID).Error; err != nil || stored.RemainingCost != nil {
		t.Fatalf("读取恢复不能回写存量：holding=%+v err=%v", stored, err)
	}
}

func TestPaperHistoricPnLDiscrepancyIsReportedWithoutRewrite(t *testing.T) {
	setupTestDB(t)
	svc := &PaperService{market: &MarketService{}}
	account, err := svc.GetOrCreateAccount(1022)
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"buy", "sell"} {
		trade, err := svc.TradeByAccount(context.Background(), 1022, account.AccountID, TradeInput{Symbol: "510300", Market: "cn", Side: side, Price: 0.01, Quantity: 1000000})
		if err != nil {
			t.Fatal(err)
		}
		if side == "sell" {
			if err := common.DB.Model(trade).Update("realized_pnl", -5).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	view, err := svc.OverviewByAccount(context.Background(), 1022, account.AccountID)
	if err != nil || view.RealizedUnavailableReason == "" {
		t.Fatalf("旧错误盈亏应标记待核验：view=%+v err=%v", view, err)
	}
	var stored model.PaperTrade
	if err := common.DB.Where("account_id = ? AND side = ?", account.AccountID, "sell").First(&stored).Error; err != nil || stored.RealizedPnl != -5 {
		t.Fatalf("审计不得改写旧已实现：trade=%+v err=%v", stored, err)
	}
}

func TestPaperShareActionKeepsPreciseRemainingCost(t *testing.T) {
	setupTestDB(t)
	svc := &PaperService{}
	account, err := svc.GetOrCreateAccount(1023)
	if err != nil {
		t.Fatal(err)
	}
	buy, err := svc.TradeByAccount(context.Background(), 1023, account.AccountID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 0.01, Quantity: 1000000})
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	if err := common.DB.Model(buy).Update("trade_date", previousDate(today)).Error; err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{Symbol: "510300", Market: "cn", ReportDate: "2025-12-31", RecordDate: previousDate(today), ExDate: today, TransferRatio: 10, Progress: model.CorpActionProgressImplemented}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	var holding model.PaperHolding
	if err := common.DB.Where("account_id = ?", account.AccountID).First(&holding).Error; err != nil {
		t.Fatal(err)
	}
	if !applyPaperCorpAdjust(holding, action) {
		t.Fatal("应执行一次送转")
	}
	if err := common.DB.First(&holding, holding.ID).Error; err != nil {
		t.Fatal(err)
	}
	if holding.Quantity != 2000000 || paperHoldingCost(holding) != 10005 {
		t.Fatalf("送转不得丢失买入佣金：%+v", holding)
	}
	sell, err := svc.TradeByAccount(context.Background(), 1023, account.AccountID, TradeInput{Symbol: "510300", Market: "cn", Side: "sell", Price: 0.005, Quantity: 2000000})
	if err != nil || sell.RealizedPnl != -10 {
		t.Fatalf("送转后平价卖出仍应亏全部佣金：sell=%+v err=%v", sell, err)
	}
}
