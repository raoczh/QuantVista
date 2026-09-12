package service

import (
	"context"
	"errors"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestPaperTradeReviewCanceledBeforeSubmission(t *testing.T) {
	setupTestDB(t)
	svc := &PaperService{}
	account, err := svc.GetOrCreateAccount(1035)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = svc.TradeByAccount(ctx, 1035, account.AccountID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 4.037, Quantity: 100})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("已取消请求不能继续按指定价成交：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.PaperTrade{}).Where("account_id = ?", account.AccountID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("已取消请求新增了 %d 条流水", count)
	}
}

func TestPaperTradeReviewCanceledDuringLedgerRead(t *testing.T) {
	setupTestDB(t)
	svc := &PaperService{}
	account, err := svc.GetOrCreateAccount(1036)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const hook = "review_paper_cancel_after_holding"
	if err := common.DB.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "paper_holdings" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	_, err = svc.TradeByAccount(ctx, 1036, account.AccountID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 4.037, Quantity: 100})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("账本读取期间取消也必须阻止后续写入：%v", err)
	}
	var stored model.PaperAccount
	if err := common.DB.Where("account_id = ?", account.AccountID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Cash != account.Cash {
		t.Errorf("取消后仍扣减现金：before=%v after=%v", account.Cash, stored.Cash)
	}
}
