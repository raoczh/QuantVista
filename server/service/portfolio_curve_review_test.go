package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestPortfolioCurveContextStopsCanceledRead(t *testing.T) {
	setupTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	view, err := PortfolioCurveByAccountContext(ctx, 8930, 1, model.PortfolioKindReal, 90)
	if !errors.Is(err, context.Canceled) || view != nil {
		t.Fatalf("曲线查询必须传播请求取消：%+v %v", view, err)
	}
}

func TestPortfolioCurveExcludesFutureAndOutOfWindowSnapshots(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	for i, kind := range []string{model.PortfolioKindReal, model.PortfolioKindPaper} {
		account := model.PortfolioAccount{UserID: int64(8930 + i), Kind: kind, Name: "曲线窗口", Status: model.PortfolioStatusActive}
		if err := common.DB.Create(&account).Error; err != nil {
			t.Fatal(err)
		}
		for _, offset := range []int{-91, -90, 0, 1} {
			row := model.PortfolioSnapshot{UserID: account.UserID, AccountID: account.ID, Kind: kind,
				TradeDate: now.AddDate(0, 0, offset).Format("2006-01-02"), MarketValue: 1000, Cash: 500}
			if err := common.DB.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
		}
		view, err := PortfolioCurveByAccount(account.UserID, account.ID, kind, 90)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Points) != 2 || view.Points[0].TradeDate != now.AddDate(0, 0, -90).Format("2006-01-02") || view.Points[1].TradeDate != now.Format("2006-01-02") {
			t.Errorf("%s 曲线只能包含回看下界至今天的真实快照：%+v", kind, view.Points)
		}
		other, err := PortfolioCurveByAccount(account.UserID+100, account.ID, kind, 90)
		if err != nil || len(other.Points) != 0 {
			t.Errorf("曲线读取必须按用户和账户隔离：%+v %v", other, err)
		}
	}
}

func TestMySQLPortfolioCurveKeepsSnapshotAndCurrencyReadTogether(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.PortfolioSnapshot{}, &model.Position{}, &model.PositionTrade{})
	account := model.PortfolioAccount{UserID: 8932, Kind: model.PortfolioKindReal, Name: "曲线读取快照", Status: model.PortfolioStatusActive}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	date := time.Now().Format("2006-01-02")
	snapshot := model.PortfolioSnapshot{UserID: account.UserID, AccountID: account.ID, Kind: account.Kind, TradeDate: date, MarketValue: 1000}
	if err := db.Create(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_mysql_portfolio_curve_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_snapshots" && first.CompareAndSwap(false, true) {
			close(read)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
				tx.AddError(tx.Statement.Context.Err())
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	type reply struct {
		view *PortfolioCurveView
		err  error
	}
	done := make(chan reply, 1)
	go func() {
		view, err := PortfolioCurveByAccount(account.UserID, account.ID, account.Kind, 90)
		done <- reply{view, err}
	}()
	select {
	case <-read:
	case result := <-done:
		t.Fatalf("未读取快照：%v", result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待曲线读取超时")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model.Position{UserID: account.UserID, AccountID: account.ID, Symbol: "AAPL", Market: "us", Currency: "USD", BuyDate: date}).Error; err != nil {
			return err
		}
		return tx.Model(&snapshot).Updates(map[string]any{"market_value": 500, "partial": true}).Error
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.view.Points) != 1 || result.view.Points[0].Partial || result.view.Points[0].MarketValue != 1000 {
			t.Errorf("曲线把旧快照和新币种记录拼在一起：%+v", result.view)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("曲线查询未结束")
	}
	after, err := PortfolioCurveByAccount(account.UserID, account.ID, account.Kind, 90)
	if err != nil || len(after.Points) != 1 || !after.Points[0].Partial || after.Points[0].MarketValue != 500 {
		t.Errorf("后续读取应看到已提交的快照与币种状态：%+v %v", after, err)
	}
}
