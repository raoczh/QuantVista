package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestPositionCurrencyMustMatchQuoteMarket(t *testing.T) {
	for _, item := range [][2]string{{"USD", "cn"}, {"CNY", "us"}, {"USD", "hk"}} {
		if _, err := normalizeCurrency(item[0], item[1]); err == nil {
			t.Errorf("币种与行情市场不一致仍被接受：%v", item)
		}
	}
	if currency, err := normalizeCurrency("", "hk"); err != nil || currency != "HKD" {
		t.Fatalf("港股默认币种应保留：%s %v", currency, err)
	}
}

func TestRealSnapshotDoesNotAddForeignCurrencyAmounts(t *testing.T) {
	positions := []model.Position{
		{ID: 1, UserID: 1010, Market: "cn", Symbol: "600111", Currency: "CNY", Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100, RemainingCost: 1000, RealizedPnl: 100},
		{ID: 2, UserID: 1010, Market: "us", Symbol: "AAPL", Currency: "USD", Status: model.PositionStatusHolding, BuyPrice: 20, Quantity: 100, RemainingCost: 2000, RealizedPnl: 30},
	}
	quotes := map[string]FreshQuoteResult{
		QuoteKey("cn", "600111"): {Quote: &datasource.Quote{Price: 11}, Fresh: quoteFreshInfo{Status: freshStatusFresh}},
		QuoteKey("us", "AAPL"):   {Quote: &datasource.Quote{Price: 22}, Fresh: quoteFreshInfo{Status: freshStatusFresh}},
	}
	snap := realSnapshotFrom(1010, "2026-09-08", positions, quotes)
	if !snap.Partial || snap.MarketValue != 1100 || snap.Cost != 1000 || snap.RealizedCum != 100 {
		t.Fatalf("未换汇的 USD 不能加入 CNY 快照，且必须说明不完整：%+v", snap)
	}
}

func TestPaperTradeDoesNotSpendCNYAsUSD(t *testing.T) {
	setupTestDB(t)
	svc := &PaperService{}
	account, err := svc.GetOrCreateAccount(1011)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TradeByAccount(context.Background(), 1011, account.AccountID, TradeInput{Market: "us", Symbol: "AAPL", Side: "buy", Price: 10, Quantity: 100}); err == nil {
		t.Error("缺少汇率和外币资金账本时不能直接从 CNY 余额买入 USD 标的")
	}
	var stored model.PaperAccount
	if err := common.DB.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Cash != account.Cash {
		t.Errorf("拒绝前不得扣减现金：%v -> %v", account.Cash, stored.Cash)
	}
}

func TestRealCashRejectsUnconvertedForeignTrades(t *testing.T) {
	setupTestDB(t)
	p := &model.Position{UserID: 1012, Market: "us", Symbol: "AAPL", Currency: "USD", Status: model.PositionStatusHolding, BuyDate: "2026-08-01", BuyPrice: 10, Quantity: 100}
	if err := common.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	account := reviewPositionAccount(t, p)
	if err := common.DB.Create(&model.PortfolioCashFlow{UserID: p.UserID, AccountID: account.ID, Type: model.CashFlowDeposit, Amount: 10000, TradeDate: "2026-07-31", IdempotencyKey: "currency-deposit"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PositionTrade{UserID: p.UserID, AccountID: account.ID, PositionID: p.ID, Side: model.PositionTradeBuy, Price: 10, Quantity: 100, TradeDate: "2026-08-01"}).Error; err != nil {
		t.Fatal(err)
	}
	if cash, reason, err := realCashBalance(common.DB, p.UserID, account.ID, "2026-09-08"); err != nil || reason == "" {
		t.Fatalf("USD 交易不能直接从 CNY 入金扣除：cash=%v reason=%s err=%v", cash, reason, err)
	}
}

func TestForeignCurrencyHistoryIsFlaggedWithoutRewrite(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format("2006-01-02")
	before := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	p := &model.Position{UserID: 1013, Market: "us", Symbol: "AAPL", Currency: "USD", Status: model.PositionStatusClosed, BuyDate: previousDate(today)}
	if err := common.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	account := reviewPositionAccount(t, p)
	for _, date := range []string{before, today} {
		if err := common.DB.Create(&model.PortfolioSnapshot{UserID: p.UserID, AccountID: account.ID, Kind: model.SnapshotKindReal, TradeDate: date, MarketValue: 1234}).Error; err != nil {
			t.Fatal(err)
		}
	}
	curve, err := PortfolioCurveByAccount(p.UserID, account.ID, model.SnapshotKindReal, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(curve.Points) != 2 || curve.Points[0].Partial || !curve.Points[1].Partial {
		t.Fatalf("已有混币种快照不能继续作为完整净值，买入之前的点不受影响：%+v", curve)
	}
	var count int64
	if err := common.DB.Model(&model.PortfolioSnapshot{}).Where("account_id = ? AND partial = ?", account.ID, true).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("只读消费不得改写历史记录：count=%d err=%v", count, err)
	}
}

func TestPaperForeignHistoryMakesTotalsUnavailable(t *testing.T) {
	setupTestDB(t)
	svc := &PaperService{market: &MarketService{}}
	account, err := svc.GetOrCreateAccount(1014)
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PaperTrade{UserID: 1014, AccountID: account.AccountID, Symbol: "AAPL", Market: "us", Side: model.PaperSideSell, RealizedPnl: 50, TradeDate: "2026-08-01"}).Error; err != nil {
		t.Fatal(err)
	}
	view, err := svc.OverviewByAccount(context.Background(), 1014, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if reason, _ := body["currency_unavailable_reason"].(string); strings.TrimSpace(reason) == "" {
		t.Fatalf("外币历史已影响余额，总览必须明确金额不可用：%s", raw)
	}
}
