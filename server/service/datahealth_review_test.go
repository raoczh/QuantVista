package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func setupDataHealthReview(t *testing.T) (time.Time, []string) {
	t.Helper()
	old := common.DB
	setupDataHealthDB(t)
	t.Cleanup(func() { common.DB = old })
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.Local)
	opens, _ := seedHealthCalendar(t, now, DataHealthMinDays)
	return now, opens
}

func TestDataHealthReviewReadFailureCannotProduceReport(t *testing.T) {
	now, _ := setupDataHealthReview(t)
	const callback = "review_datahealth_log_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "data_sync_logs" {
			tx.AddError(errors.New("本地故障注入：审计读取失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	db := common.DB
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	if report, err := buildDataHealthReportContext(t.Context(), now, DataHealthMinDays); report != nil || err == nil {
		t.Error("数据库读取失败不能返回一份成功的健康报告")
	}
}

func TestDataHealthReviewCanceledRead(t *testing.T) {
	now, _ := setupDataHealthReview(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if report, err := buildDataHealthReportContext(ctx, now, DataHealthMinDays); report != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的读取必须中止且不能返回成功快照: report=%v err=%v", report, err)
	}
}

func TestDataHealthReviewMissingPITIsPartial(t *testing.T) {
	now, dates := setupDataHealthReview(t)
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: "600001", LastBarDate: dates[len(dates)-1]}).Error; err != nil {
		t.Fatal(err)
	}
	for _, date := range dates {
		if err := common.DB.Create(&model.DailyBar{Market: "cn", Symbol: "600001", TradeDate: date, Close: 10}).Error; err != nil {
			t.Fatal(err)
		}
	}
	item := healthItemFor(t, buildDataHealthReport(now, DataHealthMinDays), "marketwide")
	if item.Status != "partial" || item.GapCalendar[len(item.GapCalendar)-1].Status != "partial" {
		t.Fatalf("缺少历史宇宙不能把估计分母报告为完整覆盖: %s %+v", item.Status, item.GapCalendar[len(item.GapCalendar)-1])
	}
}

func TestDataHealthReviewPartialFactorCoverage(t *testing.T) {
	now, dates := setupDataHealthReview(t)
	factorTableMu.Lock()
	old := factorTableCur
	factorTableCur = &FactorTable{TradeDate: dates[len(dates)-1], Symbols: []string{"600001", "600002"}, FreshCoverage: 0.5}
	factorTableMu.Unlock()
	t.Cleanup(func() { factorTableMu.Lock(); factorTableCur = old; factorTableMu.Unlock() })
	item := healthItemFor(t, buildDataHealthReport(now, DataHealthMinDays), "factor_table")
	if item.Status != "partial" || item.CoverageNumerator != 1 || item.GapCalendar[0].Status != "partial" {
		t.Fatalf("只有一半最新因子不能报告正常: %+v", item)
	}
}

func TestDataHealthReviewIgnoresFutureObservedDates(t *testing.T) {
	now, dates := setupDataHealthReview(t)
	for _, date := range []string{dates[len(dates)-5], now.AddDate(0, 0, 8).Format("2006-01-02")} {
		if err := common.DB.Create(&model.MarketMoodDaily{Market: "cn", TradeDate: date}).Error; err != nil {
			t.Fatal(err)
		}
	}
	item := healthItemFor(t, buildDataHealthReport(now, DataHealthMinDays), "mood_pool")
	if item.ObservedDate != dates[len(dates)-5] || item.Status == "ok" {
		t.Fatalf("未来数据不能掩盖真实落后: observed=%s status=%s", item.ObservedDate, item.Status)
	}
}

func TestDataHealthReviewWeekendNewsDoesNotEraseCoverage(t *testing.T) {
	now, dates := setupDataHealthReview(t)
	start, err := time.ParseInLocation("2006-01-02", dates[0], time.Local)
	if err != nil {
		t.Fatal(err)
	}
	for day := start; !day.After(now); day = day.AddDate(0, 0, 1) {
		if err := common.DB.Create(&model.News{Title: "本地新闻", PublishTime: day, Source: "test", URL: day.Format("2006-01-02"), ContentHash: day.Format("2006-01-02")}).Error; err != nil {
			t.Fatal(err)
		}
	}
	item := healthItemFor(t, buildDataHealthReport(now, DataHealthMinDays), "news")
	if item.CoverageNumerator != int64(len(dates)) {
		t.Fatalf("新闻自然日数量超交易日数不应清空覆盖: %d/%d", item.CoverageNumerator, item.CoverageDenominator)
	}
}

func TestMySQLDataHealthReviewSharesReadSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.TradingCalendar{}, &model.DailyBar{}, &model.MarketSyncState{}, &model.StockUniverseDaily{},
		&model.DataSyncLog{}, &model.MarketMoodDaily{}, &model.PopularityRank{}, &model.LhbEntry{}, &model.IntradayFactorDaily{}, &model.News{}, &model.Announcement{})
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.Local)
	dates, _ := seedHealthCalendar(t, now, DataHealthMinDays)
	for _, date := range dates {
		if err := db.Create(&model.DailyBar{Market: "cn", Symbol: "600001", TradeDate: date, Close: 10}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.StockUniverseDaily{Market: "cn", Symbol: "600001", TradeDate: date}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var changed atomic.Bool
	const callback = "review_datahealth_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if !strings.Contains(tx.Statement.SQL.String(), "JOIN stock_universe_dailies") || !changed.CompareAndSwap(false, true) {
			return
		}
		if err := db.Create(&model.StockUniverseDaily{Market: "cn", Symbol: "600002", TradeDate: dates[len(dates)-1]}).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	item := healthItemFor(t, buildDataHealthReport(now, DataHealthMinDays), "marketwide")
	if !changed.Load() || item.CoverageDenominator != int64(len(dates)) {
		t.Fatalf("健康报告混入另一读取时点的股票分母: changed=%v denominator=%d want=%d", changed.Load(), item.CoverageDenominator, len(dates))
	}
}
