package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func seedTradeStatReview(t *testing.T, userID int64) (model.PortfolioAccount, model.Position) {
	t.Helper()
	account := model.PortfolioAccount{UserID: userID, Kind: model.PortfolioKindReal, Name: "个人统计审查", Currency: "CNY", Status: model.PortfolioStatusActive}
	if err := common.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	position := model.Position{UserID: userID, AccountID: account.ID, Symbol: "600001", Market: "cn", Currency: "CNY", Status: model.PositionStatusClosed,
		BuyPrice: 10, SellPrice: 11, BuyDate: now.AddDate(0, 0, -3).Format("2006-01-02"), SellDate: now.Format("2006-01-02"),
		TotalBuyCost: 1000, TotalBuyQty: 100, TotalSellNet: 1100, RealizedPnl: 100}
	if err := common.DB.Create(&position).Error; err != nil {
		t.Fatal(err)
	}
	return account, position
}

func TestTradeStatsSeparatesCurrencyAndExcludesFutureClose(t *testing.T) {
	setupTestDB(t)
	account, original := seedTradeStatReview(t, 8940)
	foreign := original
	foreign.ID, foreign.Symbol, foreign.Market, foreign.Currency, foreign.RealizedPnl = 0, "AAPL", "us", "USD", 900
	future := original
	future.ID, future.RealizedPnl, future.SellDate = 0, 500, time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if err := common.DB.Create(&[]model.Position{foreign, future}).Error; err != nil {
		t.Fatal(err)
	}
	for _, window := range []string{"all", "30d"} {
		stats, err := (&PositionService{}).TradeStatsByAccount(t.Context(), account.UserID, account.ID, window)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Closed != 1 || stats.TotalRealizedPnl != 100 || !strings.Contains(strings.Join(stats.Notes, "；"), "币种") {
			t.Errorf("%s 统计不得把人民币、美元和未来平仓混算：closed=%d pnl=%v notes=%v", window, stats.Closed, stats.TotalRealizedPnl, stats.Notes)
		}
	}
}

func TestTradeStatsRequiresCompleteHoldingCalendar(t *testing.T) {
	setupTestDB(t)
	account, p := seedTradeStatReview(t, 8941)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: p.SellDate, IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := (&PositionService{}).TradeStatsByAccount(t.Context(), account.UserID, account.ID, "all")
	if err != nil {
		t.Fatal(err)
	}
	if stats.HoldSample != 0 || len(stats.ByHoldBucket) != 1 || !stats.ByHoldBucket[0].Unknown {
		t.Errorf("只覆盖平仓日的日历不能冒充完整持有天数：sample=%d days=%v buckets=%+v", stats.HoldSample, stats.AvgHoldTradeDays, stats.ByHoldBucket)
	}
}

func TestTradeStatsPropagatesCalendarReadFailure(t *testing.T) {
	setupTestDB(t)
	account, p := seedTradeStatReview(t, 8942)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: p.SellDate, IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	want := errors.New("本地统计日历读取失败")
	const callback = "review_trade_stat_calendar_error"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "trading_calendars" {
			tx.AddError(want)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	if _, err := (&PositionService{}).TradeStatsByAccount(t.Context(), account.UserID, account.ID, "all"); !errors.Is(err, want) {
		t.Fatalf("日历存储故障不应返回已计算的持有时长：%v", err)
	}
}

func TestTradeStatsLegacyReadAndCancellationDoNotWriteLedger(t *testing.T) {
	for _, cancelAfterRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "正常读取", true: "读取后取消"}[cancelAfterRead], func(t *testing.T) {
			setupTestDB(t)
			account, p := seedTradeStatReview(t, 8943)
			if err := common.DB.Model(&p).Updates(map[string]any{"total_buy_cost": 0, "total_buy_qty": 0, "total_sell_net": 0, "realized_pnl": 0, "quantity": 100}).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelAfterRead {
				const callback = "review_trade_stat_cancel_after_read"
				if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "positions" {
						cancel()
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			}
			stats, err := (&PositionService{}).TradeStatsByAccount(ctx, account.UserID, account.ID, "all")
			if cancelAfterRead {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("应传播取消：%v", err)
				}
			} else if err != nil || stats.Closed != 1 || stats.TotalRealizedPnl != 100 {
				t.Errorf("完整的旧记录仍应可只读验算：%+v %v", stats, err)
			}
			var count int64
			if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Errorf("读取个人统计写入了 %d 条交易流水", count)
			}
		})
	}
}

func TestMySQLTradeStatsKeepsClosedRowsInOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.Position{}, &model.PositionTrade{}, &model.TradingCalendar{}, &model.StockUniverseDaily{})
	account, p := seedTradeStatReview(t, 8944)
	read, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_trade_stat_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "positions" && first.CompareAndSwap(false, true) {
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
		stats *TradeStats
		err   error
	}
	done := make(chan reply, 1)
	go func() {
		stats, err := (&PositionService{}).TradeStatsByAccount(t.Context(), account.UserID, account.ID, "all")
		done <- reply{stats, err}
	}()
	select {
	case <-read:
	case result := <-done:
		t.Fatalf("未进入统计读取：%v", result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("未读到平仓记录")
	}
	if err := db.Model(&p).Updates(map[string]any{"status": model.PositionStatusHolding, "realized_pnl": 900}).Error; err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil || result.stats.Closed != 1 || result.stats.TotalRealizedPnl != 100 {
			t.Errorf("统计拼入了读取后已重新持仓的数据：%+v %v", result.stats, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("统计读取未完成")
	}
	after, err := (&PositionService{}).TradeStatsByAccount(t.Context(), account.UserID, account.ID, "all")
	if err != nil || after.Closed != 0 {
		t.Errorf("后续查询应看到已重新持仓：%+v %v", after, err)
	}
}

func TestTradeStatsUnknownLedgerIsExcludedAndDisclosed(t *testing.T) {
	setupTestDB(t)
	account, p := seedTradeStatReview(t, 8945)
	if err := common.DB.Model(&p).Updates(map[string]any{"total_buy_cost": 0, "quantity": 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PositionTrade{UserID: p.UserID, AccountID: p.AccountID, PositionID: p.ID, Side: "buy", Price: 10, Quantity: 200}).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := (&PositionService{}).TradeStatsByAccount(t.Context(), account.UserID, account.ID, "all")
	if err != nil || stats.Closed != 0 || !containsNote(stats.Notes, "账本") {
		t.Fatalf("已有明细但累计成本缺失时不能按旧单笔公式猜测：%+v %v", stats, err)
	}
}

func TestTradeStatsIndustryUsesCurrentMarketSnapshot(t *testing.T) {
	setupTestDB(t)
	account, p := seedTradeStatReview(t, 8946)
	for _, row := range []model.StockUniverseDaily{
		{Market: "cn", Symbol: p.Symbol, TradeDate: time.Now().AddDate(0, 0, -1).Format("2006-01-02"), Industry: "当前行业"},
		{Market: "us", Symbol: p.Symbol, TradeDate: p.SellDate, Industry: "其他市场"},
		{Market: "cn", Symbol: p.Symbol, TradeDate: time.Now().AddDate(0, 0, 1).Format("2006-01-02"), Industry: "未来行业"},
	} {
		if err := common.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	stats, err := (&PositionService{}).TradeStatsByAccount(t.Context(), account.UserID, account.ID, "all")
	if err != nil || len(stats.ByIndustry) != 1 || stats.ByIndustry[0].Label != "当前行业" {
		t.Fatalf("行业归属混入其他市场或未来快照：%+v %v", stats, err)
	}
}
