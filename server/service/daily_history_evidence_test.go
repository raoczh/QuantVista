package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func seedDailyHistoryEvidenceRows(test *testing.T, database *gorm.DB, symbol string, dates []string, updatedAt time.Time) {
	test.Helper()
	bars := wideGenBars(dates, 10)
	rows := make([]model.DailyBar, 0, len(bars))
	for _, candle := range bars {
		rows = append(rows, model.DailyBar{
			Market: "cn", Symbol: symbol, TradeDate: candle.TradeDate,
			Open: candle.Open, High: candle.High, Low: candle.Low, Close: candle.Close,
			Volume: candle.Volume, Source: "eastmoney", UpdatedAt: updatedAt,
		})
	}
	if err := database.CreateInBatches(rows, 200).Error; err != nil {
		test.Fatal(err)
	}
}

func TestDailyHistoryEvidenceCompletedMetadata(test *testing.T) {
	closeTime := time.Date(2026, 11, 4, 15, 5, 0, 0, datasource.ChinaMarketLocation())
	for _, scenario := range []struct {
		name      string
		tradeDate string
		updatedAt time.Time
		source    string
		verified  bool
	}{
		{name: "unknown timestamp", tradeDate: "2026-11-04", source: "eastmoney"},
		{name: "before close", tradeDate: "2026-11-04", updatedAt: closeTime.Add(-time.Second), source: "eastmoney"},
		{name: "completed boundary", tradeDate: "2026-11-04", updatedAt: closeTime, source: "eastmoney", verified: true},
		{name: "UTC timestamp", tradeDate: "2026-11-04", updatedAt: closeTime.UTC(), source: "tencent", verified: true},
		{name: "legacy raw source", tradeDate: "2026-11-04", updatedAt: closeTime, source: "sina"},
		{name: "adjusted Sina source", tradeDate: "2026-11-04", updatedAt: closeTime, source: "sina_qfq", verified: true},
		{name: "invalid trade date", tradeDate: "2026-02-30", updatedAt: closeTime, source: "eastmoney"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			row := model.DailyBar{Market: "cn", TradeDate: scenario.tradeDate, Source: scenario.source, UpdatedAt: scenario.updatedAt, CreatedAt: closeTime.Add(time.Hour)}
			if actual := isVerifiedCompletedDailyRow(row); actual != scenario.verified {
				test.Fatalf("completed evidence = %v, want %v: %+v", actual, scenario.verified, row)
			}
		})
	}
}

func TestDailyHistoryEvidenceUnknownCalendarPreservesOldFullHistory(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601101"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	oldEnd := time.Date(2026, 7, 7, 17, 0, 0, 0, now.Location())
	if err := database.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
		test.Fatal(err)
	}
	bars := wideGenBars(wideGenDates(250, oldEnd), 10)
	market := &MarketService{dailyNow: func() time.Time { return now }}
	err := market.persistDailyBarsWithState(test.Context(), "cn", symbol, bars, true)
	if !errors.Is(err, errDailyHistoryIncomplete) || !strings.Contains(err.Error(), "先回填日历") {
		test.Fatalf("unknown calendar certified old full history: %v", err)
	}
	state := stateOf(test, symbol)
	if state.InitStatus == "done" || state.BarsCount != 250 || barCount(test, symbol) != 250 {
		test.Fatalf("incomplete calendar lost useful committed rows or marked done: %+v", state)
	}
	// Even a known-closed current day cannot hide an unknown intervening tail.
	if err := database.Create(&model.TradingCalendar{Market: "cn", TradeDate: now.Format(time.DateOnly), IsOpen: false}).Error; err != nil {
		test.Fatal(err)
	}
	err = validateInitializedDailyHistory(database, "cn", symbol, 250, oldEnd.Format(time.DateOnly), now)
	if !errors.Is(err, errDailyHistoryIncomplete) || !strings.Contains(err.Error(), "先回填日历") {
		test.Fatalf("stale calendar tail certified old full history: %v", err)
	}
}

func TestDailyHistoryEvidenceFreshSourceEstablishesCurrentCalendar(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601102"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	if err := database.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
		test.Fatal(err)
	}
	market := &MarketService{dailyNow: func() time.Time { return now }}
	if err := market.persistDailyBarsWithState(test.Context(), "cn", symbol, wideGenBars(wideGenDates(250, now), 10), true); err != nil {
		test.Fatalf("confirmed current source could not establish calendar: %v", err)
	}
	if state := stateOf(test, symbol); state.InitStatus != "done" || state.BarsCount != 250 {
		test.Fatalf("verified current full history stayed pending: %+v", state)
	}
	var calendar model.TradingCalendar
	if err := database.Where("market = ? AND trade_date = ?", "cn", now.Format(time.DateOnly)).Take(&calendar).Error; err != nil || !calendar.IsOpen {
		test.Fatalf("committed current candle did not establish open-day evidence: %+v %v", calendar, err)
	}
}

func TestDailyHistoryEvidenceLegacyCountCannotCertifyShortRepair(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601103"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	seedDailyHistoryEvidenceRows(test, database, symbol, wideGenDates(250, now.AddDate(0, 0, -1)), now)
	// Normal Create populates UpdatedAt. Explicit SQL NULL reproduces upgraded
	// rows with no capture evidence, while leaving the misleading CreatedAt.
	if err := database.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).UpdateColumn("updated_at", nil).Error; err != nil {
		test.Fatal(err)
	}
	if err := database.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
		test.Fatal(err)
	}
	market := &MarketService{dailyNow: func() time.Time { return now }}
	err := market.persistDailyBarsWithState(test.Context(), "cn", symbol, wideGenBars([]string{now.Format(time.DateOnly)}, 10), true)
	if !errors.Is(err, errDailyHistoryIncomplete) || !strings.Contains(err.Error(), "已核验 1 根") {
		test.Fatalf("COUNT(*) certified unverified migrated history: %v", err)
	}
	state := stateOf(test, symbol)
	if state.InitStatus == "done" || state.BarsCount != 251 || barCount(test, symbol) != 251 {
		test.Fatalf("short repair lost progress or certified old timestamps: %+v", state)
	}
	var unknownCount int64
	if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND updated_at IS NULL", symbol).Count(&unknownCount).Error; err != nil || unknownCount != 250 {
		test.Fatalf("repair manufactured capture timestamps for old rows: count=%d err=%v", unknownCount, err)
	}
}

func TestDailyHistoryEvidenceVerifiedTailIgnoresOlderUnknownRows(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601104"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	dates := wideGenDates(251, now)
	seedDailyHistoryEvidenceRows(test, database, symbol, dates, now)
	seedDailyBackfillRecoveryCalendar(test, database, now, 1)
	if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", symbol, dates[0]).UpdateColumn("updated_at", nil).Error; err != nil {
		test.Fatal(err)
	}
	if err := validateInitializedDailyHistory(database, "cn", symbol, 251, dates[250], now); err != nil {
		test.Fatalf("unknown prefix poisoned a verified 250-row tail: %v", err)
	}
	if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", symbol, dates[249]).UpdateColumn("updated_at", nil).Error; err != nil {
		test.Fatal(err)
	}
	if err := validateInitializedDailyHistory(database, "cn", symbol, 251, dates[250], now); !errors.Is(err, errDailyHistoryIncomplete) {
		test.Fatalf("unknown row within the research tail was counted: %v", err)
	}
}

func TestDailyHistoryEvidenceExpiredRowsCannotCompleteWindow(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601105"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	seedDailyHistoryEvidenceRows(test, database, symbol, wideGenDates(250, now.AddDate(0, 0, -401)), now)
	seedDailyHistoryEvidenceRows(test, database, symbol, []string{now.Format(time.DateOnly)}, now)
	seedDailyBackfillRecoveryCalendar(test, database, now, 1)
	err := validateInitializedDailyHistory(database, "cn", symbol, 251, now.Format(time.DateOnly), now)
	if !errors.Is(err, errDailyHistoryIncomplete) || !strings.Contains(err.Error(), "已核验 1 根") {
		test.Fatalf("expired rows completed a retained research window: %v", err)
	}
}

func TestDailyHistoryEvidenceFullWindowRequiresTailEvidence(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601106"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	openDates := seedDailyBackfillRecoveryCalendar(test, database, now, 6)
	lastAvailable := now.AddDate(0, 0, -5)
	seedDailyHistoryEvidenceRows(test, database, symbol, wideGenDates(250, lastAvailable), now)
	if err := database.Create(&model.StockUniverseDaily{Market: "cn", Symbol: symbol, TradeDate: openDates[len(openDates)-1], Suspended: true}).Error; err != nil {
		test.Fatal(err)
	}
	if err := validateInitializedDailyHistory(database, "cn", symbol, 250, lastAvailable.Format(time.DateOnly), now); !errors.Is(err, errDailyHistoryIncomplete) {
		test.Fatalf("today's suspension concealed other missing completed sessions: %v", err)
	}
	for _, date := range openDates[1 : len(openDates)-1] {
		if err := database.Create(&model.StockUniverseDaily{Market: "cn", Symbol: symbol, TradeDate: date, Suspended: true}).Error; err != nil {
			test.Fatal(err)
		}
	}
	if err := validateInitializedDailyHistory(database, "cn", symbol, 250, lastAvailable.Format(time.DateOnly), now); err != nil {
		test.Fatalf("fully confirmed suspension tail blocked retained history: %v", err)
	}
}

func TestDailyHistoryEvidenceIPOCompletenessAcrossCalendarSegments(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, now, 120)
	listingDate := dates[0]
	first, _ := time.ParseInLocation(time.DateOnly, listingDate, now.Location())
	if known, err := knownCalendarRange(database, "cn", first, now); err != nil || known {
		test.Fatalf("HTTP calendar's 92-day cap was widened: known=%v err=%v", known, err)
	}
	for scenarioIndex, scenario := range []struct {
		name           string
		missingListing bool
		invalidListing bool
		missingDate    string
		unknownDate    string
		calendarGap    string
		preListing     bool
		suspended      bool
		complete       bool
	}{
		{name: "complete 120 natural days", complete: true},
		{name: "confirmed latest suspension", missingDate: dates[len(dates)-1], suspended: true, complete: true},
		{name: "unknown IPO", missingListing: true},
		{name: "invalid IPO application date", invalidListing: true},
		{name: "contradictory legacy pre-listing candle", preListing: true},
		{name: "unknown calendar inside second segment", calendarGap: dates[len(dates)-10]},
		{name: "missing middle candle", missingDate: dates[12]},
		{name: "unverified middle candle", unknownDate: dates[12]},
		{name: "coverage must reach target", missingDate: dates[len(dates)-1]},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			symbol := fmt.Sprintf("6012%02d", scenarioIndex)
			if !scenario.missingListing {
				applyDate := first.AddDate(0, 0, -7).Format(time.DateOnly)
				if scenario.invalidListing {
					applyDate = first.AddDate(0, 0, 1).Format(time.DateOnly)
				}
				if err := database.Create(&model.IpoSubscription{Kind: model.IpoKindStock, Code: symbol, ApplyDate: applyDate, ListDate: listingDate}).Error; err != nil {
					test.Fatal(err)
				}
			}
			availableDates := make([]string, 0, len(dates))
			for _, date := range dates {
				if date != scenario.missingDate {
					availableDates = append(availableDates, date)
				}
			}
			seedDailyHistoryEvidenceRows(test, database, symbol, availableDates, now)
			if scenario.unknownDate != "" {
				if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", symbol, scenario.unknownDate).UpdateColumn("updated_at", nil).Error; err != nil {
					test.Fatal(err)
				}
			}
			if scenario.preListing {
				previousDate := first.AddDate(0, 0, -1).Format(time.DateOnly)
				seedDailyHistoryEvidenceRows(test, database, symbol, []string{previousDate}, now)
				if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", symbol, previousDate).UpdateColumn("updated_at", nil).Error; err != nil {
					test.Fatal(err)
				}
			}
			if scenario.suspended {
				if err := database.Create(&model.StockUniverseDaily{Market: "cn", Symbol: symbol, TradeDate: scenario.missingDate, Suspended: true}).Error; err != nil {
					test.Fatal(err)
				}
			}
			if scenario.calendarGap != "" {
				if err := database.Where("market = ? AND trade_date = ?", "cn", scenario.calendarGap).Delete(&model.TradingCalendar{}).Error; err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() {
					if err := database.Create(&model.TradingCalendar{Market: "cn", TradeDate: scenario.calendarGap, IsOpen: true}).Error; err != nil {
						test.Error(err)
					}
				})
			}
			err := validateInitializedDailyHistory(database, "cn", symbol, len(availableDates), availableDates[len(availableDates)-1], now)
			if scenario.complete && err != nil {
				test.Fatalf("complete IPO proof was rejected: %v", err)
			}
			if !scenario.complete && !errors.Is(err, errDailyHistoryIncomplete) {
				test.Fatalf("incomplete IPO proof was accepted: %v", err)
			}
		})
	}
}

func TestDailyHistoryEvidencePropagatesCancellationAndDatabaseErrors(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	seedDailyBackfillRecoveryCalendar(test, database, now, 1)
	canceledContext, cancel := context.WithCancel(test.Context())
	cancel()
	if err := validateInitializedDailyHistory(database.WithContext(canceledContext), "cn", "601301", 250, now.Format(time.DateOnly), now); !errors.Is(err, context.Canceled) {
		test.Fatalf("cancellation was converted to successful readiness: %v", err)
	}
	databaseFailure := errors.New("daily history evidence database failure")
	callbackName := "test:daily_history_evidence_read_failure"
	if err := database.Callback().Query().Before("gorm:query").Register(callbackName, func(query *gorm.DB) {
		if query.Statement.Table == "daily_bars" {
			query.AddError(databaseFailure)
		}
	}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { database.Callback().Query().Remove(callbackName) })
	if err := validateInitializedDailyHistory(database, "cn", "601301", 250, now.Format(time.DateOnly), now); !errors.Is(err, databaseFailure) {
		test.Fatalf("database read error was converted to readiness or incomplete proof: %v", err)
	}
}

func TestDailyHistoryEvidenceBackfillTargetsUnverifiedExistingRows(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601401"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, now, 8)
	seedDailyHistoryEvidenceRows(test, database, symbol, dates, now)
	if err := database.Create(&model.Stock{Market: "cn", Symbol: symbol}).Error; err != nil {
		test.Fatal(err)
	}
	request := MaintenanceRequest{From: dates[0], To: dates[len(dates)-1]}
	complete, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil || complete.view.TargetCount != 0 {
		test.Fatalf("verified history was not skipped: %+v %v", complete, err)
	}
	preCloseDate, err := time.ParseInLocation(time.DateOnly, dates[2], now.Location())
	if err != nil {
		test.Fatal(err)
	}
	updates := []any{nil, time.Time{}, preCloseDate.Add(15*time.Hour + 4*time.Minute)}
	for dateIndex, updatedAt := range updates {
		if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", symbol, dates[dateIndex]).UpdateColumn("updated_at", updatedAt).Error; err != nil {
			test.Fatal(err)
		}
	}
	if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", symbol, dates[3]).UpdateColumn("source", "sina").Error; err != nil {
		test.Fatal(err)
	}
	// Suspensions have neither UpdatedAt nor Source. Their query must continue
	// to use only symbol/date and the explicit suspended fact.
	if err := database.Create(&model.StockUniverseDaily{Market: "cn", Symbol: symbol, TradeDate: dates[4], Suspended: true}).Error; err != nil {
		test.Fatal(err)
	}
	plan, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil {
		test.Fatal(err)
	}
	if plan.view.TargetCount != 1 || plan.view.MissingCount != 4 || plan.view.SuspendedCount != 1 || !strings.Contains(plan.view.DifferenceSummary, "缺失/待核验") {
		test.Fatalf("existing rows concealed repair targets: %+v", plan.view)
	}
	if plan.view.PlanHash == complete.view.PlanHash {
		test.Fatal("unverified coverage did not invalidate the confirmed plan")
	}
	for _, date := range dates[:4] {
		if _, missing := plan.missingKeys[symbol+"@"+date]; !missing {
			test.Fatalf("unverified existing row %s was not targeted", date)
		}
	}
	saved, remaining, err := dailyBackfillPersistedCoverageDB(database, model.Stock{Market: "cn", Symbol: symbol}, dates[:4])
	if err != nil || saved != 0 || remaining != 4 {
		test.Fatalf("unverified rows were reported as persisted coverage: %d %d %v", saved, remaining, err)
	}
	var unknownCount int64
	if err := database.Model(&model.DailyBar{}).Where("symbol = ? AND updated_at IS NULL", symbol).Count(&unknownCount).Error; err != nil || unknownCount != 1 {
		test.Fatalf("dry run rewrote migrated timestamp evidence: %d %v", unknownCount, err)
	}
}

func TestDailyHistoryEvidenceUnverifiedPreListingRowInvalidatesExclusion(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const symbol = "601402"
	now := time.Date(2026, 11, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, now, 3)
	if err := database.Create(&model.Stock{Market: "cn", Symbol: symbol}).Error; err != nil {
		test.Fatal(err)
	}
	if err := database.Create(&model.IpoSubscription{Kind: model.IpoKindStock, Code: symbol, ApplyDate: dates[0], ListDate: dates[2]}).Error; err != nil {
		test.Fatal(err)
	}
	seedDailyHistoryEvidenceRows(test, database, symbol, dates[:1], now)
	if err := database.Model(&model.DailyBar{}).Where("symbol = ?", symbol).UpdateColumn("updated_at", nil).Error; err != nil {
		test.Fatal(err)
	}
	plan, err := buildDailyBackfillRecoveryPlanAtDB(database, MaintenanceRequest{From: dates[0], To: dates[2]}, now)
	if err != nil || plan.view.MissingCount != 3 {
		test.Fatalf("unverified but present pre-listing evidence was discarded: %+v %v", plan, err)
	}
}

func TestDailyHistoryEvidenceBackfillRepairsBlockedBasis(test *testing.T) {
	database := setupDailyBackfillRecoveryDB(test)
	const blockedSymbol = "601403"
	const healthySymbol = "601404"
	now := time.Now().In(datasource.ChinaMarketLocation())
	dates := seedDailyBackfillRecoveryCalendar(test, database, dailyBackfillCompletedCutoff(now), 400)
	dates = dates[len(dates)-wideBarLimit:]
	for _, symbol := range []string{blockedSymbol, healthySymbol} {
		seedDailyHistoryEvidenceRows(test, database, symbol, dates, now)
		if err := database.Create(&model.Stock{Market: "cn", Symbol: symbol}).Error; err != nil {
			test.Fatal(err)
		}
	}
	request := MaintenanceRequest{Market: "cn", Scope: DailyBackfillScopeMarketwide, From: dates[0], To: dates[len(dates)-1]}
	completePlan, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil || completePlan.view.TargetCount != 0 {
		test.Fatalf("verified history was not skipped: %+v %v", completePlan, err)
	}
	adapter := &dailyBackfillRecoveryAdapter{planAdapter: planAdapter{bars: map[string][]datasource.Bar{
		blockedSymbol: wideGenBars(dates, 5),
	}}}
	marketService := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	// No universe state is required: the durable lock must also protect a
	// visited stock whose old rows still have valid completion timestamps.
	if err := marketService.markStatePending(test.Context(), "cn", blockedSymbol); err != nil {
		test.Fatal(err)
	}
	blockedPlan, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil {
		test.Fatal(err)
	}
	if blockedPlan.view.TargetCount != 1 || blockedPlan.view.MissingCount != len(dates) || blockedPlan.view.ExistingCount != 0 || blockedPlan.stocks[0].Symbol != blockedSymbol {
		test.Fatalf("blocked price basis was advertised as complete: %+v", blockedPlan.view)
	}
	if blockedPlan.view.PlanHash == completePlan.view.PlanHash {
		test.Fatal("price-basis failure did not invalidate the confirmed plan")
	}
	stock := model.Stock{Market: "cn", Symbol: blockedSymbol}
	savedCount, remainingCount, err := dailyBackfillPersistedCoverageDB(database, stock, dates)
	if err != nil || savedCount != 0 || remainingCount != len(dates) {
		test.Fatalf("unusable old prices were counted as repaired: saved=%d remaining=%d err=%v", savedCount, remainingCount, err)
	}
	request.PlanHash = blockedPlan.view.PlanHash
	log, err := marketService.RunSyncBarsPlan(test.Context(), request, SyncAudit{})
	if err != nil || log.Status != "success" || log.Succeeded != 1 || log.Failed != 0 {
		test.Fatalf("full-window repair did not complete: log=%+v err=%v", log, err)
	}
	savedCount, remainingCount, err = dailyBackfillPersistedCoverageDB(database, stock, dates)
	if err != nil || savedCount != len(dates) || remainingCount != 0 {
		test.Fatalf("repaired basis stayed unavailable: saved=%d remaining=%d err=%v", savedCount, remainingCount, err)
	}
	repairedPlan, err := buildDailyBackfillRecoveryPlanAtDB(database, request, now)
	if err != nil || repairedPlan.view.TargetCount != 0 {
		test.Fatalf("repaired history was still selected: %+v %v", repairedPlan, err)
	}
	stored, err := marketService.readStoredDailyHistory(test.Context(), "cn", blockedSymbol, wideBarLimit, now, true)
	if err != nil || len(stored) != wideBarLimit || stored[0].Close != 5 {
		test.Fatalf("local consumers cannot use the repaired history: count=%d err=%v", len(stored), err)
	}
}
