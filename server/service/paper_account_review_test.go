package service

import (
	"context"
	"math"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestPaperMissingCashAccountCreationReturnsSuccess(t *testing.T) {
	setupTestDB(t)
	portfolio := model.PortfolioAccount{UserID: 1026, Kind: model.PortfolioKindPaper, Name: "旧账户缺少现金记录", Status: model.PortfolioStatusActive}
	if err := common.DB.Create(&portfolio).Error; err != nil {
		t.Fatal(err)
	}
	account, err := (&PaperService{}).GetOrCreateAccountByID(1026, portfolio.ID)
	if err != nil || account == nil || account.Cash != model.PaperDefaultCash {
		t.Fatalf("补建成功后不能仍返回旧的 record not found：account=%+v err=%v", account, err)
	}
}

func TestPaperMissingCashCreationRechecksArchive(t *testing.T) {
	setupTestDB(t)
	portfolio := model.PortfolioAccount{UserID: 1027, Kind: model.PortfolioKindPaper, Name: "创建期间归档", Status: model.PortfolioStatusActive}
	if err := common.DB.Create(&portfolio).Error; err != nil {
		t.Fatal(err)
	}
	archived := false
	const callback = "review_missing_paper_account_archive"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "paper_accounts" || archived {
			return
		}
		archived = true
		archiveReviewAccount(t, portfolio)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	_, _ = (&PaperService{}).GetOrCreateAccountByID(1027, portfolio.ID)
	var count int64
	if err := common.DB.Model(&model.PaperAccount{}).Where("account_id = ?", portfolio.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !archived || count != 0 {
		t.Fatalf("归档后创建了新的现金记录：archived=%v count=%d", archived, count)
	}
}

func TestPaperOverviewCashAndHoldingsUseOneLedgerState(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	svc := NewPaperService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})))
	account, err := svc.GetOrCreateAccount(1028)
	if err != nil {
		t.Fatal(err)
	}
	traded := false
	const callback = "review_paper_overview_interleaved_trade"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "paper_accounts" || traded {
			return
		}
		traded = true
		if _, err := svc.TradeByAccount(context.Background(), 1028, account.AccountID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 10, Quantity: 1000}); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	view, err := svc.OverviewByAccount(context.Background(), 1028, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if !traded || math.Abs(view.TotalAssets-99995) > 0.005 {
		t.Fatalf("现金与持仓跨越一次买入，资产被凭空放大：traded=%v view=%+v", traded, view)
	}
}
