package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestDataHealthMissingTodayDoesNotInvalidateCompletedHistory(test *testing.T) {
	now, dates := setupDataHealthReview(test)
	today := now.Format(time.DateOnly)
	if err := common.DB.Where("market = ? AND trade_date = ?", "cn", today).Delete(&model.TradingCalendar{}).Error; err != nil {
		test.Fatal(err)
	}
	previousDate := dates[len(dates)-2]
	if err := common.DB.Create(&model.MarketMoodDaily{Market: "cn", TradeDate: previousDate}).Error; err != nil {
		test.Fatal(err)
	}
	morning := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, now.Location())
	report, err := buildDataHealthReportContext(test.Context(), morning, 30)
	if err != nil {
		test.Fatal(err)
	}
	item := healthItemFor(test, report, "mood_pool")
	if item.ExpectedDate != previousDate || item.LagOpenDays != 0 || item.Status == "unknown" {
		test.Fatalf("today's unconfirmed session invalidated yesterday's coverage: %+v", item)
	}
	evening, err := buildDataHealthReportContext(test.Context(), now, 30)
	if err != nil {
		test.Fatal(err)
	}
	if item := healthItemFor(test, evening, "mood_pool"); item.ExpectedDate != "" || item.Status != "unknown" {
		test.Fatalf("unconfirmed today's close was guessed: %+v", item)
	}
	if calendar := healthItemFor(test, evening, "calendar"); calendar.Status == "unknown" {
		test.Fatalf("today's missing close polluted the independent historical calendar domain: %+v", calendar)
	}
}

func TestSyncBarsPlanRejectsUnknownCalendarInsteadOfEmptySuccess(test *testing.T) {
	setupPlanDB(test)
	request := MaintenanceRequest{Market: "cn", From: "2026-09-01", To: "2026-09-02"}
	if plan, err := buildSyncBarsPlanDB(common.DB, request); err == nil || plan != nil {
		test.Fatalf("missing calendar became a successful zero-gap plan: %+v %v", plan, err)
	}
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: request.From, IsOpen: true}).Error; err != nil {
		test.Fatal(err)
	}
	if plan, err := buildSyncBarsPlanDB(common.DB, request); err == nil || plan != nil {
		test.Fatalf("partial calendar silently skipped an unknown weekday: %+v %v", plan, err)
	}
}

func TestSyncBarsPlanReachesMissingStockBeyondCompletePrefix(test *testing.T) {
	setupPlanDB(test)
	date := "2026-09-01"
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}).Error; err != nil {
		test.Fatal(err)
	}
	stocks := make([]model.Stock, 0, syncMaxStocks+1)
	bars := make([]model.DailyBar, 0, syncMaxStocks)
	for index := 0; index <= syncMaxStocks; index++ {
		symbol := fmt.Sprintf("600%03d", index)
		stocks = append(stocks, model.Stock{Symbol: symbol, Market: "cn"})
		if index < syncMaxStocks {
			bars = append(bars, model.DailyBar{Symbol: symbol, Market: "cn", TradeDate: date, Close: 10})
		}
	}
	if err := common.DB.CreateInBatches(stocks, 200).Error; err != nil {
		test.Fatal(err)
	}
	if err := common.DB.CreateInBatches(bars, 200).Error; err != nil {
		test.Fatal(err)
	}
	plan, err := buildSyncBarsPlanDB(common.DB, MaintenanceRequest{Market: "cn", From: date, To: date})
	if err != nil || len(plan.stocks) != 1 || plan.stocks[0].Symbol != "600800" || plan.view.MissingCount != 1 {
		test.Fatalf("complete first 800 stocks starved the remaining missing stock: %+v %v", plan, err)
	}
}

func TestWidePlanUsesSourceCloseWindowNotHealthGracePeriod(test *testing.T) {
	setupPlanDB(test)
	now := time.Date(2026, 9, 4, 16, 10, 0, 0, datasource.ChinaMarketLocation())
	// No today's calendar row: the source must confirm the requested target later.
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-09-03", IsOpen: true}).Error; err != nil {
		test.Fatal(err)
	}
	plan, err := buildWidePlanAtDB(common.DB, MaintenanceRequest{Market: "cn"}, now)
	if err != nil || plan.view.From != "2026-09-04" || plan.view.To != "2026-09-04" {
		test.Fatalf("16:10 close plan was blocked or targeted yesterday: %+v %v", plan, err)
	}
	if plan, err := buildWidePlanAtDB(common.DB, MaintenanceRequest{Market: "cn"}, now.Add(-6*time.Hour)); err == nil || plan != nil {
		test.Fatalf("intraday snapshot was advertised as completed daily data: %+v %v", plan, err)
	}
	var count int64
	common.DB.Model(&model.TradingCalendar{}).Where("trade_date = ?", "2026-09-04").Count(&count)
	if count != 0 {
		test.Fatal("dry-run wrote an assumed trading day")
	}
}

func TestWideSyncRejectsWrongDateBeforeWritingUniverse(test *testing.T) {
	setupPlanDB(test)
	rows, _ := wideTestSnapshot()
	service := &MarketService{wide: &fakeWideSource{snapshot: rows}}
	log, err := service.syncMarketWideForDate(test.Context(), "2026-07-08")
	if err == nil || log.Status != "failed" {
		test.Fatalf("wrong-date snapshot was published: %+v %v", log, err)
	}
	for _, table := range []any{&model.DailyBar{}, &model.MarketSyncState{}, &model.StockUniverseDaily{}, &model.TradingCalendar{}} {
		var count int64
		if err := common.DB.Model(table).Count(&count).Error; err != nil || count != 0 {
			test.Fatalf("rejected snapshot modified data: %T count=%d err=%v", table, count, err)
		}
	}
}

func TestWideSnapshotRejectsIntradayAndMissingClosingTime(test *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, datasource.ChinaMarketLocation())
	rows := []datasource.SpotRow{{Price: 10, Volume: 100, DataTime: now.Unix()}}
	if err := validateWideSnapshotDate(rows, "2026-09-04", "2026-09-04", now); err == nil {
		test.Fatal("current intraday data accepted as a full daily snapshot")
	}
	if err := validateWideSnapshotDate(rows, "2026-09-04", "2026-09-04", now.Add(7*time.Hour)); err == nil {
		test.Fatal("old morning snapshot became a closing snapshot after the wall clock advanced")
	}
}

func TestMarketWideAdminInitRetriesFailedWithoutRewritingDone(test *testing.T) {
	setupTestDB(test)
	states := []model.MarketSyncState{
		{Market: "cn", Symbol: "600001", InitStatus: "failed", FailCount: wideInitMaxFail},
		{Market: "cn", Symbol: "600002", InitStatus: "done", LastBarDate: "2026-09-01"},
	}
	if err := common.DB.Create(&states).Error; err != nil {
		test.Fatal(err)
	}
	source := &fakeWideSource{bars: map[string][]datasource.Bar{
		"600001": wideGenBars(wideGenDates(250, time.Date(2026, 9, 1, 16, 0, 0, 0, time.Local)), 10),
	}}
	service := &MarketService{wide: source, dailyNow: func() time.Time {
		return time.Date(2026, 9, 1, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	}}
	log, err := service.RunMarketWideInit(test.Context(), SyncAudit{TriggerSource: "admin"})
	if err != nil || log.Succeeded != 1 || source.barsCalls["600002"] != 0 || stateOf(test, "600001").InitStatus != "done" {
		test.Fatalf("manual recovery failed or rewrote a completed stock: %+v %v calls=%v", log, err, source.barsCalls)
	}
	if !strings.Contains(log.ParameterSummary, "retry_failed=1") {
		test.Fatalf("retry scope missing from audit: %+v", log)
	}
}

func TestMarketWideInitEmptyUniverseReportsPrerequisite(test *testing.T) {
	setupPlanDB(test)
	log, err := (&MarketService{}).RunMarketWideInit(context.Background(), SyncAudit{})
	if err == nil || log.Status != "failed" || !strings.Contains(err.Error(), "全市场") {
		test.Fatalf("missing universe was reported as successful initialization: %+v %v", log, err)
	}
	if errors.Is(err, context.Canceled) {
		test.Fatal("prerequisite failure is not cancellation")
	}
}
