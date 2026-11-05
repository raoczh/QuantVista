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

	"gorm.io/gorm"
)

func setupDailyBackfillRecoveryDB(test *testing.T) *gorm.DB {
	test.Helper()
	previous := common.DB
	setupTestDB(test)
	database := common.DB
	test.Cleanup(func() {
		waitForTestFactorRebuilds(test)
		common.DB = previous
	})
	return database
}

func seedDailyBackfillRecoveryCalendar(test *testing.T, database *gorm.DB, end time.Time, naturalDays int) []string {
	test.Helper()
	end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, end.Location())
	rows := make([]model.TradingCalendar, 0, naturalDays)
	var dates []string
	for day := end.AddDate(0, 0, -(naturalDays - 1)); !day.After(end); day = day.AddDate(0, 0, 1) {
		open := day.Weekday() != time.Saturday && day.Weekday() != time.Sunday
		date := day.Format(time.DateOnly)
		rows = append(rows, model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: open})
		if open {
			dates = append(dates, date)
		}
	}
	if err := database.CreateInBatches(rows, 200).Error; err != nil {
		test.Fatal(err)
	}
	return dates
}

type dailyBackfillRecoveryAdapter struct {
	planAdapter
	calls int
	limit int
}

func (adapter *dailyBackfillRecoveryAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	adapter.calls++
	adapter.limit = limit
	return adapter.planAdapter.GetDailyBars(ctx, market, symbol, limit)
}

func TestDailyBackfillRecoveryUniverseIncludesUntrackedAndDeduplicates(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, now, 2)
	if err := database.Create(&[]model.MarketSyncState{
		{Market: "cn", Symbol: "600001", InitStatus: "done"},
		{Market: "cn", Symbol: "600002", InitStatus: "done"},
	}).Error; err != nil {
		test.Fatal(err)
	}
	if err := database.Create(&[]model.Stock{{Market: "cn", Symbol: "600001"}, {Market: "cn", Symbol: "600003"}}).Error; err != nil {
		test.Fatal(err)
	}
	request := MaintenanceRequest{Market: "cn", Scope: DailyBackfillScopeMarketwide, From: dates[0], To: dates[1]}
	plan, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil {
		test.Fatal(err)
	}
	if plan.view.UniverseCount != 3 || len(plan.stocks) != 3 || plan.view.MissingCount != 6 || plan.stocks[1].Symbol != "600002" {
		test.Fatalf("market universe was replaced by visited stocks or duplicated: %+v", plan.view)
	}
	request.Scope = ""
	tracked, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil || tracked.view.Scope != DailyBackfillScopeTracked || tracked.view.TargetCount != 2 {
		test.Fatalf("legacy absent scope is not tracked: %+v %v", tracked, err)
	}
}

func TestDailyBackfillRecoveryMissingTargetsCapAfterCompletePrefix(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, datasource.ChinaMarketLocation())
	date := seedDailyBackfillRecoveryCalendar(test, database, now, 1)[0]
	states := make([]model.MarketSyncState, 0, syncMaxStocks+2)
	bars := make([]model.DailyBar, 0, syncMaxStocks)
	for index := 0; index < syncMaxStocks+2; index++ {
		symbol := fmt.Sprintf("%06d", 600000+index)
		states = append(states, model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "done"})
		if index < syncMaxStocks {
			bars = append(bars, model.DailyBar{Market: "cn", Symbol: symbol, TradeDate: date, Close: 10})
		}
	}
	if err := database.CreateInBatches(states, 200).Error; err != nil {
		test.Fatal(err)
	}
	if err := database.CreateInBatches(bars, 200).Error; err != nil {
		test.Fatal(err)
	}
	request := MaintenanceRequest{Scope: DailyBackfillScopeMarketwide, From: date, To: date}
	plan, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil || plan.view.TargetCount != 2 || plan.view.Capped || plan.stocks[0].Symbol != "600800" {
		test.Fatalf("complete prefix starved later universe stocks: %+v %v", plan, err)
	}
	if err := database.Where("market = ?", "cn").Delete(&model.DailyBar{}).Error; err != nil {
		test.Fatal(err)
	}
	plan, err = buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil || plan.view.TargetCount != syncMaxStocks || !plan.view.Capped || plan.view.EstimatedRequests != syncMaxStocks {
		test.Fatalf("missing target cap was not enforced: %+v %v", plan, err)
	}
}

func TestDailyBackfillRecoveryUniverseHardLimit(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	states := make([]model.MarketSyncState, 0, maintenanceMaxUniverseRows+1)
	for index := 0; index <= maintenanceMaxUniverseRows; index++ {
		states = append(states, model.MarketSyncState{Market: "cn", Symbol: fmt.Sprintf("%06d", index)})
	}
	if err := database.CreateInBatches(states, 200).Error; err != nil {
		test.Fatal(err)
	}
	if _, err := dailyBackfillUniverseDB(database, "cn", DailyBackfillScopeMarketwide); err == nil || !strings.Contains(err.Error(), "7000") {
		test.Fatalf("oversized universe not rejected: %v", err)
	}
}

func TestDailyBackfillRecoveryDefaultWindowAndStaleTail(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, datasource.ChinaMarketLocation())
	seedDailyBackfillRecoveryCalendar(test, database, now, 400)
	request := MaintenanceRequest{Scope: DailyBackfillScopeMarketwide}
	_, dates, from, to, err := dailyBackfillOpenDatesAtDB(database, request, now)
	if err != nil || len(dates) != 250 || to != "2026-11-04" {
		test.Fatalf("default must establish 250 completed dates: %d %s %s %v", len(dates), from, to, err)
	}
	request.From, request.To = from, to
	_, explicitDates, _, _, err := dailyBackfillOpenDatesAtDB(database, request, now)
	if err != nil || len(explicitDates) != 250 {
		test.Fatalf("confirmed default dates cannot be executed explicitly: %v", err)
	}
	if err := database.Where("trade_date = ?", to).Delete(&model.TradingCalendar{}).Error; err != nil {
		test.Fatal(err)
	}
	for _, scope := range []string{DailyBackfillScopeMarketwide, DailyBackfillScopeTracked} {
		request = MaintenanceRequest{Scope: scope}
		if _, _, _, _, err := dailyBackfillOpenDatesAtDB(database, request, now); err == nil || !strings.Contains(err.Error(), "2026-11-04") {
			test.Fatalf("stale tail silently ended in the past for %s: %v", scope, err)
		}
		morning := time.Date(2026, 11, 4, 15, 4, 0, 0, now.Location())
		_, _, _, previous, err := dailyBackfillOpenDatesAtDB(database, request, morning)
		if err != nil || previous != "2026-11-03" {
			test.Fatalf("15:04 requested an unfinished current candle: %s %v", previous, err)
		}
	}
	request = MaintenanceRequest{Scope: DailyBackfillScopeMarketwide, From: "2026-11-02", To: "2026-11-03"}
	if _, _, _, _, err := dailyBackfillOpenDatesAtDB(database, request, now); err != nil {
		test.Fatalf("explicit old range should not require a newer calendar: %v", err)
	}
}

func TestDailyBackfillRecoveryInsufficientHistoryAndTrackedBounds(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, now, 100)
	if _, _, _, _, err := dailyBackfillOpenDatesAtDB(database, MaintenanceRequest{Scope: DailyBackfillScopeMarketwide}, now); err == nil || !strings.Contains(err.Error(), "250") {
		test.Fatalf("short calendar silently weakened long-window recovery: %v", err)
	}
	_, tracked, _, _, err := dailyBackfillOpenDatesAtDB(database, MaintenanceRequest{}, now)
	if err != nil || len(tracked) != 45 {
		test.Fatalf("tracked default compatibility lost: %d %v", len(tracked), err)
	}
	request := MaintenanceRequest{From: dates[0], To: dates[len(dates)-1]}
	if _, _, _, _, err := dailyBackfillOpenDatesAtDB(database, request, now); err == nil {
		test.Fatal("tracked explicit natural-day bound was widened")
	}
	request.Scope = DailyBackfillScopeMarketwide
	if _, _, _, _, err := dailyBackfillOpenDatesAtDB(database, request, now); err != nil {
		test.Fatalf("marketwide should permit explicitly shorter established history: %v", err)
	}
}

func TestDailyBackfillRecoveryListingEvidenceNotFirstSeen(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, datasource.ChinaMarketLocation())
	seedDailyBackfillRecoveryCalendar(test, database, now, 400)
	if err := database.Create(&[]model.MarketSyncState{
		{Market: "cn", Symbol: "600001", CreatedAt: now},
		{Market: "cn", Symbol: "600002", CreatedAt: now},
	}).Error; err != nil {
		test.Fatal(err)
	}
	if err := database.Create(&model.IpoSubscription{Kind: model.IpoKindStock, Code: "600001", ApplyDate: "2026-10-27", ListDate: "2026-11-04"}).Error; err != nil {
		test.Fatal(err)
	}
	plan, err := buildDailyBackfillRecoveryPlanAtDB(database, MaintenanceRequest{Scope: DailyBackfillScopeMarketwide}, now)
	if err != nil {
		test.Fatal(err)
	}
	if plan.view.MissingCount != 251 {
		test.Fatalf("new IPO needs one day, merely newly-seen stock still needs 250: %+v", plan.view)
	}
	if _, impossible := plan.missingKeys["600001@2026-11-03"]; impossible {
		test.Fatal("credible pre-listing date was treated as a missing candle")
	}
}

func TestDailyBackfillRecoveryScopeHashIsolatedAndRecheckedBeforeSource(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Now().In(datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, dailyBackfillCompletedCutoff(now), 4)
	if err := database.Create(&model.Stock{Market: "cn", Symbol: "600001"}).Error; err != nil {
		test.Fatal(err)
	}
	adapter := &dailyBackfillRecoveryAdapter{}
	service := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	request := MaintenanceRequest{Market: "cn", Scope: DailyBackfillScopeTracked, From: dates[0], To: dates[len(dates)-1]}
	tracked, err := service.PlanMaintenance(MaintenanceSyncBars, request)
	if err != nil {
		test.Fatal(err)
	}
	request.Scope = DailyBackfillScopeMarketwide
	marketwide, err := service.PlanMaintenance(MaintenanceSyncBars, request)
	if err != nil || marketwide.PlanHash == tracked.PlanHash {
		test.Fatalf("same targets must still have isolated scope hashes: %v", err)
	}
	request.PlanHash = tracked.PlanHash
	if _, err := service.RunSyncBarsPlan(test.Context(), request, SyncAudit{}); !errors.Is(err, ErrMaintenancePlanExpired) || adapter.calls != 0 {
		test.Fatalf("scope changed after authorization: calls=%d error=%v", adapter.calls, err)
	}
	request.PlanHash = marketwide.PlanHash
	if err := database.Create(&model.DailyBar{Market: "cn", Symbol: "600001", TradeDate: dates[0], Close: 10}).Error; err != nil {
		test.Fatal(err)
	}
	if _, err := service.RunSyncBarsPlan(test.Context(), request, SyncAudit{}); !errors.Is(err, ErrMaintenancePlanExpired) || adapter.calls != 0 {
		test.Fatalf("changed coverage did not invalidate confirmed plan: calls=%d error=%v", adapter.calls, err)
	}
	if _, err := service.PlanMaintenance(MaintenanceBackfillCalendar, request); err == nil {
		test.Fatal("non-daily task accepted a daily scope")
	}
}

func TestDailyBackfillRecoveryCountsPersistedDatesNotSourceLength(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Now().In(datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, dailyBackfillCompletedCutoff(now), 6)
	dates = dates[len(dates)-2:]
	stock := model.Stock{Market: "cn", Symbol: "600091"}
	// This symbol was initialized for discovery but has never been user-visited.
	if err := database.Create(&model.MarketSyncState{Market: stock.Market, Symbol: stock.Symbol, InitStatus: "done"}).Error; err != nil {
		test.Fatal(err)
	}
	adapter := &dailyBackfillRecoveryAdapter{planAdapter: planAdapter{bars: map[string][]datasource.Bar{
		stock.Symbol: {
			{TradeDate: dates[0], Open: 10, High: 10, Low: 10, Close: 10, Source: "test"},
			{TradeDate: dates[0], Open: 10, High: 10, Low: 10, Close: 10, Source: "test"},
		},
	}}}
	service := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	request := MaintenanceRequest{Scope: DailyBackfillScopeMarketwide, From: dates[0], To: dates[1]}
	plan, err := service.PlanMaintenance(MaintenanceSyncBars, request)
	if err != nil {
		test.Fatal(err)
	}
	request.PlanHash = plan.PlanHash
	// Duplicate source dates must not count twice against two distinct gaps.
	log, err := service.RunSyncBarsPlan(test.Context(), request, SyncAudit{})
	if err != nil || log.Status != "partial" || log.Succeeded != 0 || log.Failed != 1 || !strings.Contains(log.Message, "保存 1，剩余 1") {
		test.Fatalf("source length became success despite missing persisted day: %+v %v", log, err)
	}
	if adapter.limit < 250 {
		test.Fatalf("short missing window weakened adjustment reference: %d", adapter.limit)
	}
	saved, remaining, err := dailyBackfillPersistedCoverageDB(database, stock, dates)
	if err != nil || saved != 1 || remaining != 1 {
		test.Fatalf("actual coverage mismatch: %d %d %v", saved, remaining, err)
	}
}

func TestDailyBackfillRecoveryFetchWindowReachesOldDatesAndPreservesCompletedOnly(test *testing.T) {
	now := time.Date(2026, 11, 4, 15, 4, 0, 0, datasource.ChinaMarketLocation())
	limit, err := dailyBackfillFetchLimitAt([]string{"2026-11-03"}, now)
	if err != nil || limit < 251 {
		test.Fatalf("unfinished current row can consume the 250-day reference: %d %v", limit, err)
	}
	limit, err = dailyBackfillFetchLimitAt([]string{now.AddDate(0, 0, -399).Format(time.DateOnly)}, now)
	if err != nil || limit < 400 || limit > 1000 {
		test.Fatalf("old explicit date cannot be reached safely: %d %v", limit, err)
	}
	bars := dailyBackfillCompletedBars([]datasource.Bar{{TradeDate: "2026-11-03"}, {TradeDate: "2026-11-04"}}, "cn", now)
	if len(bars) != 1 || bars[0].TradeDate != "2026-11-03" {
		test.Fatalf("unfinished today candle was treated as completed: %+v", bars)
	}
}

func TestDailyBackfillRecoveryLegacyStaleSourceKeepsProgressWithoutSuccess(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Now().In(datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, dailyBackfillCompletedCutoff(now), 8)
	previousDate, expectedDate := dates[len(dates)-2], dates[len(dates)-1]
	stock := model.Stock{Market: "cn", Symbol: "600081"}
	if err := database.Create(&stock).Error; err != nil {
		test.Fatal(err)
	}
	adapter := &dailyBackfillRecoveryAdapter{planAdapter: planAdapter{bars: map[string][]datasource.Bar{
		stock.Symbol: {{TradeDate: previousDate, Open: 10, High: 10, Low: 10, Close: 10, Source: "test"}},
	}}}
	service := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	previousCursor := syncCursor.Load()
	syncCursor.Store(0)
	test.Cleanup(func() { syncCursor.Store(previousCursor) })
	log, err := service.SyncTrackedDailyBars(test.Context(), "cn", 120)
	if err != nil || log.Status != "partial" || log.Succeeded != 0 || log.Failed != 1 || !strings.Contains(log.Message, expectedDate) {
		test.Fatalf("stale legacy source incorrectly marked complete: %+v %v", log, err)
	}
	saved, _, err := dailyBackfillPersistedCoverageDB(database, stock, []string{previousDate})
	if err != nil || saved != 1 {
		test.Fatalf("valid historical progress was discarded: %d %v", saved, err)
	}
	if err := database.Create(&model.StockUniverseDaily{Market: "cn", Symbol: stock.Symbol, TradeDate: expectedDate, Suspended: true}).Error; err != nil {
		test.Fatal(err)
	}
	if err := dailyBackfillVerifyCompletedDB(database, stock, expectedDate, false); err != nil {
		test.Fatalf("confirmed suspension should explain absent completed candle: %v", err)
	}
}
