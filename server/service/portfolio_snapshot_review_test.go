package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func reviewSnapshotClock(t *testing.T) time.Time {
	t.Helper()
	now := time.Now().In(time.Local)
	yesterday := now.AddDate(0, 0, -1)
	for date, open := range map[string]bool{now.Format("2006-01-02"): true, yesterday.Format("2006-01-02"): true} {
		if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: open}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return now
}

func TestRealSnapshotUsesLedgerAfterQuoteFetch(t *testing.T) {
	setupTestDB(t)
	quoteTime := reviewSnapshotClock(t)
	date := quoteTime.Format("2006-01-02")
	p := seedHoldingWithLedger(t, 950, "600081", 10, 100, 0, 0, "2026-07-01")
	called := false
	svc := &PositionService{}
	svc.market = NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: quoteTime, hook: func() {
		if called {
			return
		}
		called = true
		if _, err := svc.AddTrade(950, p.ID, PositionTradeInput{Side: "buy", Price: 10, Quantity: 100, TradeDate: date}); err != nil {
			t.Error(err)
		}
	}}))
	snap, err := svc.buildRealSnapshot(context.Background(), 950, date)
	if err != nil {
		t.Fatal(err)
	}
	if !called || snap.Partial || snap.MarketValue != 2000 || snap.Cost != 2000 {
		t.Fatalf("取行情期间加仓后，应使用已提交的 200 股账本：%+v called=%v", snap, called)
	}
}

func TestPaperSnapshotUsesCashAndHoldingsAfterQuoteFetch(t *testing.T) {
	setupTestDB(t)
	quoteTime := reviewSnapshotClock(t)
	account, err := NewPortfolioAccountService().Create(951, PortfolioAccountInput{Name: "快照测试", Kind: model.PortfolioKindPaper})
	if err != nil {
		t.Fatal(err)
	}
	svc := &PaperService{}
	buy := func() error {
		_, err := svc.TradeByAccount(context.Background(), 951, account.ID, TradeInput{Symbol: "600082", Market: "cn", Side: "buy", Price: 10, Quantity: 100})
		return err
	}
	if err := buy(); err != nil {
		t.Fatal(err)
	}
	called := false
	svc.market = NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: quoteTime, hook: func() {
		if called {
			return
		}
		called = true
		if err := buy(); err != nil {
			t.Error(err)
		}
	}}))
	snap, err := svc.buildPaperSnapshotForAccount(context.Background(), 951, account.ID, quoteTime.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	var cash model.PaperAccount
	if err := common.DB.Where("user_id = ? AND account_id = ?", 951, account.ID).First(&cash).Error; err != nil {
		t.Fatal(err)
	}
	if !called || snap.Partial || snap.MarketValue != 2000 || snap.Cash != cash.Cash {
		t.Fatalf("快照现金和持仓应来自同一次最新账本读取：%+v cash=%v called=%v", snap, cash.Cash, called)
	}
}

func TestSnapshotCannotWriteArchivedAccountAfterQuoteFetch(t *testing.T) {
	setupTestDB(t)
	quoteTime := reviewSnapshotClock(t)
	accounts := NewPortfolioAccountService()
	if _, err := accounts.Create(952, PortfolioAccountInput{Name: "默认", Kind: model.PortfolioKindReal}); err != nil {
		t.Fatal(err)
	}
	account, err := accounts.Create(952, PortfolioAccountInput{Name: "采集时归档", Kind: model.PortfolioKindReal})
	if err != nil {
		t.Fatal(err)
	}
	p := seedHoldingWithLedger(t, 952, "600083", 10, 100, 0, 0, "2026-07-01")
	if err := common.DB.Model(&p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	called := false
	svc := NewPositionService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: quoteTime, hook: func() {
		if called {
			return
		}
		called = true
		if _, err := accounts.Archive(952, account.ID); err != nil {
			t.Error(err)
		}
	}})))
	if count := RunPortfolioSnapshots(context.Background(), svc, nil, quoteTime.Format("2006-01-02")); count != 0 {
		t.Errorf("归档后不应新增快照：%d", count)
	}
	var count int64
	if err := common.DB.Model(&model.PortfolioSnapshot{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !called || count != 0 {
		t.Fatalf("归档期间仍产生了快照：count=%d called=%v", count, called)
	}
}

func TestRealSnapshotDoesNotValueUnconfirmedShareAction(t *testing.T) {
	setupTestDB(t)
	quoteTime := reviewSnapshotClock(t)
	date := quoteTime.Format("2006-01-02")
	p, _ := seedAdjustCase(t, 953, date, 0, 10, 0)
	svc := NewPositionService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: quoteTime, hook: func() {}})))
	// 尚未生成调整建议，也必须识别已到期的公司行动。
	snap, err := svc.buildRealSnapshot(context.Background(), 953, date)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Partial || snap.MissingCount != 1 || snap.MarketValue != 0 || snap.PositionCount != 1 {
		t.Errorf("未确认送转时旧股数乘除权后股价不能成为完整净值：%+v", snap)
	}
	if _, err := GenerateCorpAdjusts(953, date); err != nil {
		t.Fatal(err)
	}
	rows, err := ListCorpAdjusts(953, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 {
		t.Fatalf("建议生成失败：%+v %v", rows, err)
	}
	if _, err := svc.ConfirmCorpAdjust(953, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	snap, err = svc.buildRealSnapshot(context.Background(), 953, date)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Partial || snap.MarketValue != 20000 || snap.Cost != 20000 {
		t.Fatalf("送转确认后应按 2000 股和原始成本估值：%+v position=%d", snap, p.ID)
	}
}

func TestLedgerChangesInvalidateOnlyAffectedSnapshotDates(t *testing.T) {
	setupTestDB(t)
	for index, action := range []string{"add", "edit", "delete", "note"} {
		t.Run(action, func(t *testing.T) {
			uid := int64(960 + index)
			p := seedHoldingWithLedger(t, uid, "600084", 10, 100, 0, 0, "2026-09-02")
			account, err := ResolvePortfolioAccount(uid, 0, model.PortfolioKindReal)
			if err != nil {
				t.Fatal(err)
			}
			for _, date := range []string{"2026-09-01", "2026-09-02", "2026-09-03"} {
				if err := common.DB.Create(&model.PortfolioSnapshot{UserID: uid, AccountID: account.ID, Kind: model.PortfolioKindReal, TradeDate: date, MarketValue: 1000}).Error; err != nil {
					t.Fatal(err)
				}
			}
			svc := &PositionService{}
			switch action {
			case "add":
				_, err = svc.AddTrade(uid, p.ID, PositionTradeInput{Side: "buy", Price: 10, Quantity: 100, TradeDate: "2026-09-03"})
			case "edit", "note":
				price := p.BuyPrice
				if action == "edit" {
					price = 11
				}
				_, err = svc.Update(uid, p.ID, PositionInput{BuyPrice: price, Quantity: p.Quantity, BuyDate: p.BuyDate, PositionType: p.PositionType, UserNote: "修改备注"})
			case "delete":
				err = svc.Delete(uid, p.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			var snapshots []model.PortfolioSnapshot
			if err := common.DB.Where("account_id = ?", account.ID).Order("trade_date").Find(&snapshots).Error; err != nil {
				t.Fatal(err)
			}
			for _, snap := range snapshots {
				want := action != "note" && snap.TradeDate >= "2026-09-02"
				if action == "add" {
					want = snap.TradeDate >= "2026-09-03"
				}
				if snap.Partial != want || (want && snap.Note == "") {
					t.Errorf("%s 后历史快照可用性错误：%+v wantPartial=%v", action, snap, want)
				}
			}
		})
	}
}

func TestSnapshotInvalidationFailureRollsBackLedger(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithLedger(t, 964, "600085", 10, 100, 0, 0, "2026-09-01")
	account, err := ResolvePortfolioAccount(964, 0, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PortfolioSnapshot{UserID: 964, AccountID: account.ID, Kind: model.PortfolioKindReal, TradeDate: "2026-09-02", MarketValue: 1000}).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review:reject_snapshot_invalidation"
	if err := common.DB.Callback().Update().Before("gorm:update").Register(callback, func(db *gorm.DB) {
		if db.Statement.Table == "portfolio_snapshots" {
			db.AddError(errors.New("注入快照更新失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Update().Remove(callback) })
	_, err = (&PositionService{}).AddTrade(964, p.ID, PositionTradeInput{Side: "buy", Price: 10, Quantity: 100, TradeDate: "2026-09-02"})
	if err == nil {
		t.Error("快照失效处理失败，账本事务也应回滚")
	}
	var after model.Position
	if err := common.DB.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Quantity != 100 || after.TotalBuyCost != 1000 {
		t.Fatalf("回滚后账本仍被改变：%+v", after)
	}
}
