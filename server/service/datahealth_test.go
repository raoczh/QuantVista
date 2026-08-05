package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupDataHealthDB(t *testing.T) {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	models := []any{
		&model.TradingCalendar{}, &model.DailyBar{}, &model.MarketSyncState{}, &model.StockUniverseDaily{},
		&model.DataSyncLog{}, &model.MarketMoodDaily{}, &model.PopularityRank{}, &model.LhbEntry{},
		&model.IntradayFactorDaily{}, &model.News{}, &model.Announcement{},
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	common.DB = db
	if !db.Migrator().HasIndex(&model.DailyBar{}, "idx_bar_market_date") {
		t.Fatal("数据健康按 market+trade_date 查询必须有索引")
	}
}

func healthItemFor(t *testing.T, report *DataHealthReport, key string) DataHealthItem {
	t.Helper()
	for _, item := range report.Items {
		if item.Key == key {
			return item
		}
	}
	t.Fatalf("未找到健康域 %s", key)
	return DataHealthItem{}
}

func seedHealthCalendar(t *testing.T, now time.Time, openCount int) ([]string, string) {
	t.Helper()
	rows := make([]model.TradingCalendar, 0, openCount+50)
	opens := make([]string, 0, openCount)
	holiday := ""
	for d := now; len(opens) < openCount; d = d.AddDate(0, 0, -1) {
		date := d.Format("2006-01-02")
		isWeekend := d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
		isHoliday := len(opens) == 20 && !isWeekend && holiday == ""
		if isHoliday {
			holiday = date
		}
		isOpen := !isWeekend && !isHoliday
		rows = append(rows, model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: isOpen})
		if isOpen {
			opens = append(opens, date)
		}
	}
	if err := common.DB.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(opens)-1; i < j; i, j = i+1, j-1 {
		opens[i], opens[j] = opens[j], opens[i]
	}
	return opens, holiday
}

func TestDataHealthBoundedGapCalendarHolidaySuspensionAndClasses(t *testing.T) {
	setupDataHealthDB(t)
	now := time.Date(2026, 8, 5, 18, 0, 0, 0, time.Local)
	opens, holiday := seedHealthCalendar(t, now, 70)
	window := opens[len(opens)-DataHealthMaxDays:]
	states := []model.MarketSyncState{
		{Symbol: "600001", Market: "cn", Name: "甲", InitStatus: "done", LastBarDate: window[len(window)-1]},
		{Symbol: "600002", Market: "cn", Name: "乙", InitStatus: "done", LastBarDate: window[len(window)-1]},
		{Symbol: "600003", Market: "cn", Name: "停牌", InitStatus: "done", LastBarDate: window[len(window)-2]},
	}
	if err := common.DB.Create(&states).Error; err != nil {
		t.Fatal(err)
	}
	for i, date := range window {
		universe := []model.StockUniverseDaily{
			{TradeDate: date, Symbol: "600001", Market: "cn", Name: "甲"},
			{TradeDate: date, Symbol: "600002", Market: "cn", Name: "乙"},
			{TradeDate: date, Symbol: "600003", Market: "cn", Name: "停牌", Suspended: i == len(window)-1},
		}
		if err := common.DB.Create(&universe).Error; err != nil {
			t.Fatal(err)
		}
		bars := []model.DailyBar{{Symbol: "600001", Market: "cn", TradeDate: date, Close: 10}}
		if i == len(window)-1 {
			bars = append(bars, model.DailyBar{Symbol: "600002", Market: "cn", TradeDate: date, Close: 11})
			// 即使库中残留停牌标的当日 bar，也不能用它掩盖活跃宇宙缺口或抬高分子。
			bars = append(bars, model.DailyBar{Symbol: "600003", Market: "cn", TradeDate: date, Close: 12})
		}
		if err := common.DB.Create(&bars).Error; err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			common.DB.Create(&model.MarketMoodDaily{Market: "cn", TradeDate: date})
		}
	}
	common.DB.Create(&model.DataSyncLog{Task: "sync_market_wide", Market: "cn", Status: "failed", Message: "上游限流", CreatedAt: now.Add(-time.Hour)})

	report := buildDataHealthReport(now, 999)
	if report.WindowDays != DataHealthMaxDays || len(report.Items) == 0 || report.QueryHardMax != DataHealthMaxDays {
		t.Fatalf("窗口未钳制到 60: %+v", report)
	}
	wide := healthItemFor(t, report, "marketwide")
	if len(wide.GapCalendar) != DataHealthMaxDays || wide.RecoveryClass != "backfillable" {
		t.Fatalf("全市场缺口日历不完整: %+v", wide)
	}
	for _, day := range wide.GapCalendar {
		if day.Date == holiday {
			t.Fatalf("休市日 %s 不得进入业务覆盖分母", holiday)
		}
	}
	last := wide.GapCalendar[len(wide.GapCalendar)-1]
	if last.Status != "covered" || last.Expected != 2 || last.Observed != 2 || last.Suspended != 1 {
		t.Fatalf("停牌必须从当日分母排除: %+v", last)
	}
	if wide.Status != "partial" || wide.CoverageDenominator <= 0 || wide.CoverageNumerator >= wide.CoverageDenominator {
		t.Fatalf("历史部分覆盖应明确 partial 且带分母: %+v", wide)
	}
	if wide.RecentFailure == nil || wide.RecentFailure.Message != "上游限流" {
		t.Fatalf("最近失败摘要缺失: %+v", wide.RecentFailure)
	}
	mood := healthItemFor(t, report, "mood_pool")
	if mood.RecoveryClass != "unrecoverable" {
		t.Fatalf("不可回溯域分类错误: %+v", mood)
	}
	lhb := healthItemFor(t, report, "lhb")
	if lhb.RecoveryClass != "unknown" || len(lhb.GapCalendar) == 0 || lhb.GapCalendar[0].Status != "unknown" {
		t.Fatalf("稀疏事件零行必须是 unknown，不得伪报 missing: %+v", lhb)
	}
	calendar := healthItemFor(t, report, "calendar")
	foundClosed := false
	for _, day := range calendar.GapCalendar {
		if day.Date == holiday && day.Status == "closed" {
			foundClosed = true
		}
	}
	if !foundClosed {
		t.Fatalf("日历域应把休市日显示为 closed: holiday=%s calendar=%+v", holiday, calendar.GapCalendar)
	}
}

func TestDataHealthEmptyTablesAndMinimumWindow(t *testing.T) {
	setupDataHealthDB(t)
	now := time.Date(2026, 8, 5, 18, 0, 0, 0, time.Local)
	seedHealthCalendar(t, now, 35)
	report := buildDataHealthReport(now, 1)
	if report.WindowDays != DataHealthMinDays {
		t.Fatalf("请求过小应钳到 30，got %d", report.WindowDays)
	}
	wide := healthItemFor(t, report, "marketwide")
	if wide.Status != "empty" || wide.CoverageDenominator != 0 {
		t.Fatalf("空宇宙/空日线应诚实显示 empty: %+v", wide)
	}
	mood := healthItemFor(t, report, "mood_pool")
	if mood.Status != "empty" || mood.CoverageDenominator != int64(DataHealthMinDays) {
		t.Fatalf("空表应带明确分母: %+v", mood)
	}
	intraday := healthItemFor(t, report, "intraday")
	oldUnrecoverable := false
	for _, day := range intraday.GapCalendar {
		if day.RecoveryClass == "unrecoverable" {
			oldUnrecoverable = true
			break
		}
	}
	if !oldUnrecoverable {
		t.Fatalf("盘中因子超 18 日缺口应不可回溯: %+v", intraday.GapCalendar)
	}
	if got := fmt.Sprintf("%d/%d", mood.CoverageNumerator, mood.CoverageDenominator); got != "0/30" {
		t.Fatalf("空表覆盖摘要错误: %s", got)
	}
}
