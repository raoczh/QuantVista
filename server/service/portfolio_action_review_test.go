package service

import (
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestRealPortfolioViewsDoNotPriceUnconfirmedShareAction(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	p, _ := seedAdjustCase(t, 1060, now.Format("2006-01-02"), 0, 10, 0)
	account, err := ResolvePortfolioAccount(p.UserID, 0, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreatePortfolioCashFlow(p.UserID, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 100000, TradeDate: p.BuyDate, IdempotencyKey: "initial"}); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(p).Update("plan_stop_loss", 18).Error; err != nil {
		t.Fatal(err)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}}))
	positions := NewPositionService(market)
	rows, err := positions.ListByAccount(t.Context(), p.UserID, account.ID, "holding")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].QuoteOK || rows[0].BelowStopLoss || rows[0].ProfitAmount != 0 || !strings.Contains(rows[0].StaleReason, "送转") {
		t.Errorf("10 转 10 尚未确认时，不能显示 -50%% 浮亏或跌破旧止损价：%+v", rows)
	}
	risk := NewPortfolioRiskService(market, positions)
	view, err := risk.Overview(t.Context(), p.UserID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.TotalAssets.Status != RiskStatusUnavailable || view.PricedCount != 0 {
		t.Errorf("未确认送转的旧股数不能参与确定的资产汇总：%+v", view)
	}
	var count int64
	if err := common.DB.Model(&model.PositionCorpAdjust{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("读取不能自动生成或确认账务调整：count=%d err=%v", count, err)
	}
}

func TestPaperViewsDoNotPriceUnprocessedShareAction(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	account, _ := seedRiskReadAccount(t, 1061, model.PortfolioKindPaper, now.Format("2006-01-02"))
	if err := common.DB.Model(&model.PaperTrade{}).Where("account_id = ?", account.ID).Update("trade_date", previousDate(now.Format("2006-01-02"))).Error; err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{Symbol: "510300", Market: "cn", ReportDate: "2026-06-30", ExDate: now.Format("2006-01-02"), RecordDate: previousDate(now.Format("2006-01-02")), TransferRatio: 10, Progress: model.CorpActionProgressImplemented}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}}))
	svc := NewPaperService(market)
	view, err := svc.OverviewByAccount(t.Context(), account.UserID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Holdings) != 1 || view.Holdings[0].QuoteOK || !strings.Contains(view.ValuationNote, "送转") {
		t.Errorf("送转未入账不能以旧数量计算模拟浮盈和资产：%+v", view)
	}
	risk, err := NewPortfolioRiskService(market, nil).Overview(t.Context(), account.UserID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if risk.TotalAssets.Status != RiskStatusUnavailable || risk.PricedCount != 0 {
		t.Errorf("模拟风险汇总必须识别未入账送转：%+v", risk)
	}
	var audits int64
	if err := common.DB.Model(&model.PaperCorpAdjust{}).Count(&audits).Error; err != nil || audits != 0 {
		t.Fatalf("估值读取不能顺便执行公司行动：audits=%d err=%v", audits, err)
	}
}

func TestPaperSnapshotRechecksShareActionAfterQuotes(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	date := now.Format("2006-01-02")
	account, _ := seedRiskReadAccount(t, 1062, model.PortfolioKindPaper, date)
	if err := common.DB.Model(&model.PaperTrade{}).Where("account_id = ?", account.ID).Update("trade_date", previousDate(date)).Error; err != nil {
		t.Fatal(err)
	}
	inserted := false
	market := NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {
		if inserted {
			return
		}
		inserted = true
		action := model.CorporateAction{Symbol: "510300", Market: "cn", ReportDate: "2026-06-30", ExDate: date, RecordDate: previousDate(date), TransferRatio: 10, Progress: model.CorpActionProgressImplemented}
		if err := common.DB.Create(&action).Error; err != nil {
			t.Error(err)
		}
	}}))
	snap, err := NewPaperService(market).buildPaperSnapshotForAccount(t.Context(), account.UserID, account.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || !snap.Partial || snap.MissingCount != 1 || snap.MarketValue != 0 || !strings.Contains(snap.Note, "送转") {
		t.Fatalf("取行情期间新到的送转不能配旧股数保存为完整快照：inserted=%v snap=%+v", inserted, snap)
	}
}
