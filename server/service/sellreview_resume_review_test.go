package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestSellReviewIgnoresFutureAndForeignFacts(t *testing.T) {
	setupTestDB(t)
	const today = "2026-09-10"
	p := seedHoldingWithPeak(t, 14110, "600061", "事实边界", 10, 100, 10, today)
	seedGuardReviewEvents(t, today)
	n, err := (&SellReviewService{}).evaluateSellReviewsForUser(t.Context(), p.UserID, today, mustAddDays(today, -2))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ListSellReviews(p.UserID, "all")
	if err != nil || n != 2 || len(rows) != 2 {
		t.Fatalf("本日有效预告和龙虎榜应各一条：n=%d rows=%+v err=%v", n, rows, err)
	}
	for _, row := range rows {
		if row.TradeDate != today {
			t.Errorf("未来事实不能成为当日卖出依据：%+v", row)
		}
	}
}

func TestSellReviewFutureBarsCannotHideCurrentBreak(t *testing.T) {
	setupTestDB(t)
	const today = "2026-09-10"
	p := seedHoldingWithPeak(t, 14111, "600061", "均线边界", 10, 100, 10, today)
	var bars []model.DailyBar
	for i := -24; i <= 1; i++ {
		price := 10.0
		if i >= 0 {
			price = 9
		}
		bars = append(bars, model.DailyBar{Symbol: p.Symbol, Market: p.Market, TradeDate: mustAddDays(today, i), Open: price, High: price, Low: price, Close: price, Source: "eastmoney"})
	}
	if err := common.DB.Create(&bars).Error; err != nil {
		t.Fatal(err)
	}
	n, err := (&SellReviewService{}).evaluateSellReviewsForUser(t.Context(), p.UserID, today, mustAddDays(today, -2))
	if err != nil || n != 1 {
		t.Fatalf("未来日线不能挡住当日刚跌破均线：n=%d err=%v", n, err)
	}
}

func TestSellReviewCrossCurrencyDoesNotCalculateProfit(t *testing.T) {
	p := model.Position{Market: "cn", Currency: "USD", BuyPrice: 10, Quantity: 100}
	detail, pct := composeSellReviewDetail("公告事件", p, 20, true)
	if pct != 0 || !strings.Contains(detail, "币种") || strings.Contains(detail, "浮动盈利") || strings.Contains(detail, "成本 10.00 元") {
		t.Fatalf("不同币种不能直接相减计算盈亏：pct=%v detail=%s", pct, detail)
	}
}

func TestSellReviewCanceledBeforeCommitCannotWrite(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format("2006-01-02")
	p := seedHoldingWithPeak(t, 14112, "600061", "取消复核", 10, 100, 10, today)
	if err := common.DB.Create(&model.RestrictedRelease{Symbol: p.Symbol, Market: p.Market, FreeDate: today, FreeShares: 1000, FreeType: "测试"}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const callback = "review_sell_cancel_commit"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "sell_reviews" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
	n, err := (&SellReviewService{}).evaluateSellReviewsForUser(ctx, p.UserID, today, mustAddDays(today, -2))
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("取消必须中止复核写入并返回原因：n=%d err=%v", n, err)
	}
	var count int64
	if err := common.DB.Model(&model.SellReview{}).Where("user_id = ?", p.UserID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("取消不能留下新的卖出复核：count=%d err=%v", count, err)
	}
}

func TestSellReviewStatusPreservesReadFailure(t *testing.T) {
	setupTestDB(t)
	forced := errors.New("review storage unavailable")
	const callback = "review_sell_status_read_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "sell_reviews" {
			tx.AddError(forced)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	if _, err := SetSellReviewStatus(14113, 1, model.SellReviewStatusResolved); !errors.Is(err, forced) {
		t.Fatalf("存储故障不能伪装成复核不存在：%v", err)
	}
}

func TestSellReviewUnconfirmedSharesCannotShowProfit(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	pinCalendarTo(t, today)
	p, _ := seedAdjustCase(t, 14114, today, 0, 10, 0)
	svc := &SellReviewService{market: NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}}))}
	n, err := svc.evaluateSellReviewsForUser(t.Context(), p.UserID, today, mustAddDays(today, -2))
	if err != nil || n != 1 {
		t.Fatalf("送转事实仍应生成一次复核：n=%d err=%v", n, err)
	}
	rows, err := ListSellReviews(p.UserID, "all")
	if err != nil || len(rows) != 1 || rows[0].QuoteOK || rows[0].ProfitPct != 0 || !strings.Contains(rows[0].Detail, "送转尚未处理") {
		t.Fatalf("未确认送转不能用除权后价格除以旧成本报告亏损：rows=%+v err=%v", rows, err)
	}
}

func TestMySQLSellReviewFactsReadOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.Position{}, &model.PositionTrade{}, &model.PositionCorpAdjust{},
		&model.CorporateAction{}, &model.RestrictedRelease{}, &model.EarningsForecast{}, &model.LhbEntry{}, &model.DailyBar{}, &model.SellReview{})
	const today = "2026-09-10"
	p := seedHoldingWithPeak(t, 14115, "600061", "同步交错", 10, 100, 10, today)
	release := model.RestrictedRelease{Symbol: p.Symbol, Market: "cn", FreeDate: today, FreeShares: 1000, FreeType: "测试"}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review_sell_snapshot_writer"
	wrote := false
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "restricted_releases" || wrote || tx.Error != nil {
			return
		}
		wrote = true
		err := db.Transaction(func(writer *gorm.DB) error {
			if err := writer.Delete(&release).Error; err != nil {
				return err
			}
			return writer.Create(&model.EarningsForecast{Symbol: p.Symbol, Market: "cn", ReportDate: "2026-06-30", NoticeDate: today, PredictType: "首亏"}).Error
		})
		if err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	n, err := (&SellReviewService{}).evaluateSellReviewsForUser(t.Context(), p.UserID, today, mustAddDays(today, -2))
	if !wrote || err != nil || n != 1 {
		t.Fatalf("扫描不能组合从未同时存在的解禁与预告：wrote=%v n=%d err=%v", wrote, n, err)
	}
}

func TestSellReviewStatusCannotSucceedAfterConcurrentDeletion(t *testing.T) {
	setupTestDB(t)
	row := model.SellReview{UserID: 14116, PositionID: 1, Symbol: "600061", Market: "cn", TradeDate: "2026-09-11", Trigger: model.SellReviewLift, Status: model.SellReviewStatusOpen}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review_sell_status_deleted"
	deleted := false
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "sell_reviews" && tx.Error == nil && !deleted {
			deleted = true
			if err := common.DB.Delete(&row).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	if got, err := SetSellReviewStatus(row.UserID, row.ID, model.SellReviewStatusResolved); !deleted || err == nil || got != nil {
		t.Fatalf("记录已被删除不能返回已复核成功：deleted=%v got=%+v err=%v", deleted, got, err)
	}
}
