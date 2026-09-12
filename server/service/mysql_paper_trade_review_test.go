package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMySQLPaperTradeReviewCancelDuringAccountWait(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.PaperAccount{}, &model.PaperHolding{}, &model.PaperTrade{},
		&model.CorporateAction{}, &model.PaperCorpAdjust{}, &model.PortfolioSnapshot{})
	portfolio := model.PortfolioAccount{UserID: 1037, Kind: model.PortfolioKindPaper, Name: "本机取消回归", Status: model.PortfolioStatusActive}
	if err := db.Create(&portfolio).Error; err != nil {
		t.Fatal(err)
	}
	account := model.PaperAccount{UserID: 1037, AccountID: portfolio.ID, Cash: 100000, InitialCash: 100000}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	writer := db.Begin()
	if writer.Error != nil {
		t.Fatal(writer.Error)
	}
	defer writer.Rollback()
	if err := writer.Clauses(clause.Locking{Strength: "UPDATE"}).First(&portfolio, portfolio.ID).Error; err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	const hook = "review_paper_account_wait"
	if err := db.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, locking := tx.Statement.Clauses["FOR"]; tx.Statement.Table == "portfolio_accounts" && locking {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(hook) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := (&PaperService{}).TradeByAccount(ctx, 1037, portfolio.ID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 4.037, Quantity: 100})
		result <- err
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("未进入账户锁等待：%v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("没有进入交易行锁")
	}
	select {
	case err := <-result:
		t.Fatalf("交易没有等待已有账户锁：%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("取消后应保留取消原因：%v", err)
		}
	case <-time.After(700 * time.Millisecond):
		t.Error("取消后交易仍等待账户锁")
		writer.Rollback()
		select {
		case <-result:
		case <-time.After(3 * time.Second):
			t.Fatal("释放账户锁后交易未结束")
		}
	}
	var count int64
	if err := db.Model(&model.PaperTrade{}).Where("account_id = ?", portfolio.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("取消请求仍生成流水：count=%d", count)
	}
	var cash model.PaperAccount
	if err := db.First(&cash, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if cash.Cash != 100000 {
		t.Errorf("取消请求仍扣款：%v", cash.Cash)
	}
}
