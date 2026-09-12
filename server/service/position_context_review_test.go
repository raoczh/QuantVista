package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestPositionCreateCanceledDuringQuoteKeepsLedger(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 12060
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	svc := NewPositionService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{hook: cancel})))
	_, err = svc.CreateByAccount(ctx, userID, account.ID, PositionInput{
		Symbol: "600079", Market: "cn", PositionType: model.PositionTypeLongTerm, BuyPrice: 10, BuyDate: "2026-08-01", Quantity: 100})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("行情读取期间取消后必须中止建仓：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.Position{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("取消后不能继续写入新持仓：count=%d err=%v", count, err)
	}
}

func TestMySQLPositionTradeCanceledWhileWaitingForAccount(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.Position{}, &model.PositionTrade{})
	const userID int64 = 12061
	account := model.PortfolioAccount{UserID: userID, Name: "本机取消测试", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	p := seedHoldingWithLedger(t, userID, "600079", 10, 100, 0, 0, "2026-08-01")
	if err := db.Model(&p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- db.Transaction(func(tx *gorm.DB) error {
			var current model.PortfolioAccount
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, account.ID).Error; err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-writerDone:
		t.Fatalf("持锁事务提前结束：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待账户锁超时")
	}
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	const callback = "review_position_cancel_wait"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_accounts" {
			waitingOnce.Do(func() { close(waiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	readerDone := make(chan error, 1)
	go func() {
		_, err := (&PositionService{}).AddTradeContext(ctx, userID, p.ID, PositionTradeInput{Side: "buy", Price: 10, Quantity: 100, TradeDate: "2026-08-02"})
		readerDone <- err
	}()
	select {
	case <-waiting:
	case err := <-readerDone:
		t.Fatalf("交易未进入账户锁等待：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("交易未发起账户锁读取")
	}
	cancel()
	select {
	case err := <-readerDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("等待期间取消必须返回取消原因：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消未中止账户锁等待")
	}
	unblock()
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&p, p.ID).Error; err != nil || p.Quantity != 100 {
		t.Fatalf("取消后不得改变持仓：p=%+v err=%v", p, err)
	}
	var count int64
	if err := db.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("取消后不得新增流水：count=%d err=%v", count, err)
	}
}
