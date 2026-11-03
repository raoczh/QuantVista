package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestTrackingIncompleteExpiryWaitsForRecovery(test *testing.T) {
	setupTestDB(test)
	seedTrackingReviewCalendar(test, "2026-09-01")
	batch, recommendation := seedLinkFixture(test, 8960, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	recommendation.DetailJSON = `{"valid_days":2,"take_profit":12,"stop_loss":9}`
	provider := &trackingReviewAdapter{bars: []datasource.Bar{bar("2026-09-02", 10, 11.1, 9.5, 11)}}
	tracking := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(provider)))
	if state, err := tracking.evaluateOne(test.Context(), batch, recommendation, "2026-09-01", nil, nil); err == nil || state != nil {
		test.Fatalf("an absent expiry price became a permanent return: state=%+v err=%v", state, err)
	}
	provider.bars = append(provider.bars, bar("2026-09-03", 11, 11.1, 8, 8.5))
	tracking = NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(provider)))
	state, err := tracking.evaluateOne(test.Context(), batch, recommendation, "2026-09-01", nil, nil)
	if err != nil || state == nil || state.Outcome != model.RecOutcomeStopLoss || state.ReturnPct != -15 {
		test.Fatalf("recovered history should settle using the actual missing date: state=%+v err=%v", state, err)
	}
}

func TestTrackingLaterHitRequiresKnownEarlierSuspension(test *testing.T) {
	setupTestDB(test)
	seedTrackingReviewCalendar(test, "2026-09-01")
	batch, recommendation := seedLinkFixture(test, 8961, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	recommendation.DetailJSON = `{"valid_days":5,"take_profit":12,"stop_loss":9}`
	provider := &trackingReviewAdapter{bars: []datasource.Bar{bar("2026-09-03", 10, 12.5, 9.5, 12)}}
	tracking := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(provider)))
	if state, err := tracking.evaluateOne(test.Context(), batch, recommendation, "2026-09-01", nil, nil); err == nil || state != nil {
		test.Fatalf("an earlier missing day could conceal a stop: state=%+v err=%v", state, err)
	}
	if err := common.DB.Create(&model.StockUniverseDaily{Market: "cn", Symbol: recommendation.Symbol, TradeDate: "2026-09-02", Suspended: true}).Error; err != nil {
		test.Fatal(err)
	}
	state, err := tracking.evaluateOne(test.Context(), batch, recommendation, "2026-09-01", nil, nil)
	if err != nil || state == nil || state.Outcome != model.RecOutcomeTakeProfit {
		test.Fatalf("confirmed suspension must not block an observed trigger: state=%+v err=%v", state, err)
	}
}

type completionSnapshotProvider struct {
	refusalTestAdapter
	name  string
	rows  []datasource.SpotRow
	calls int
}

func (provider *completionSnapshotProvider) Name() string { return provider.name }
func (provider *completionSnapshotProvider) GetCNSpotSnapshot(context.Context) ([]datasource.SpotRow, error) {
	provider.calls++
	return append([]datasource.SpotRow(nil), provider.rows...), nil
}

func TestWideSnapshotUnusablePrimaryFallsBackBeforeAnyPublication(test *testing.T) {
	setupTestDB(test)
	rows, date := wideTestSnapshot()
	primary := &completionSnapshotProvider{name: "eastmoney", rows: append([]datasource.SpotRow(nil), rows...)}
	for index := range primary.rows {
		primary.rows[index].DataTime = 0
	}
	backup := &completionSnapshotProvider{name: "sina", rows: rows}
	market := NewMarketService(datasource.NewManagerWithAdapters(primary, backup))
	log, err := market.syncMarketWideForDate(test.Context(), date)
	if err != nil || log == nil || log.Status != "success" || primary.calls != 1 || backup.calls != 1 {
		test.Fatalf("nonempty invalid primary blocked the backup: log=%+v err=%v calls=%d/%d", log, err, primary.calls, backup.calls)
	}
}

func TestWideSnapshotRejectsMixedActiveDatesButAllowsSuspension(test *testing.T) {
	now := time.Date(2026, 9, 4, 16, 10, 0, 0, datasource.ChinaMarketLocation())
	rows := []datasource.SpotRow{
		{Symbol: "600000", Price: 10, Volume: 100, DataTime: now.Add(-time.Hour).Unix()},
		{Symbol: "600001", Price: 11, Volume: 100, DataTime: now.AddDate(0, 0, -1).Unix()},
	}
	if err := validateWideSnapshotDate(rows, "2026-09-04", "2026-09-04", now); err == nil {
		test.Fatal("mixed active dates would become an apparently complete PIT universe")
	}
	rows[1].Volume = 0
	if err := validateWideSnapshotDate(rows, "2026-09-04", "2026-09-04", now); err != nil {
		test.Fatalf("an actual suspension may retain its last quote: %v", err)
	}
}

func TestMarketWideStartupCompletionRequiresSuccessfulTarget(test *testing.T) {
	setupPlanDB(test)
	date := "2026-09-04"
	for _, status := range []string{"processing", "canceled", "partial", "failed", "success"} {
		log := model.DataSyncLog{Task: JobKindSyncMarketWide, Market: "cn", Status: status, Succeeded: 1, RangeSummary: date + ".." + date}
		if err := common.DB.Create(&log).Error; err != nil {
			test.Fatal(err)
		}
		completed, err := hasCompletedMarketWideSync(common.DB, date)
		if err != nil || completed != (status == "success") {
			test.Fatalf("startup completion status=%s complete=%v err=%v", status, completed, err)
		}
	}
	if completed, err := hasCompletedMarketWideSync(common.DB, "2026-09-07"); err != nil || completed {
		test.Fatalf("log creation time cannot substitute for its actual target: %v %v", completed, err)
	}
}

func TestFactorRefreshCannotRepublishOldInputAfterSameDayRepair(test *testing.T) {
	setupTestDB(test)
	seedFactorBuildReview(test, common.DB, "done")
	old, err := RebuildFactorTable(test.Context(), "original")
	if err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Model(&model.DailyBar{}).Where("symbol = ?", "600081").Updates(map[string]any{"open": 11, "high": 11, "low": 11, "close": 11}).Error; err != nil {
		test.Fatal(err)
	}
	invalidateFactorTable()
	if err := publishFactorTable(old); !errors.Is(err, errFactorInputsChanged) {
		test.Fatalf("in-flight old input was republished: %v", err)
	}
	current, err := ensureFactorTable(test.Context())
	if err != nil || current == nil || current.TradeDate != old.TradeDate || current.Col("close")[0] != 11 {
		test.Fatalf("same-date repair left the current table stale: %+v %v", current, err)
	}
}

func TestSystemSchedulerRetainsRebuildDuringRunningJob(test *testing.T) {
	resetJobOwnerArtifactTestDB(test)
	runtime := newJobRuntime(1, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer runtime.close()
	defer unblock()
	var revision atomic.Int64
	revision.Store(1)
	observed := make(chan int64, 4)
	handler := func(ctx context.Context, _ int64, _ bool, _ json.RawMessage) (DurableJobResult, error) {
		input := revision.Load()
		if input == 1 {
			close(entered)
			select {
			case <-ctx.Done():
				return DurableJobResult{}, ctx.Err()
			case <-release:
			}
		}
		observed <- input
		log := &model.DataSyncLog{Task: JobKindFactorRebuild, Market: "cn", Status: "success", Total: 1, Succeeded: 1}
		return DurableJobResult{Value: log, Status: model.JobStatusSuccess, Total: 1, Succeeded: 1}, nil
	}
	runtime.registerWithBinding(JobKindFactorRebuild, time.Minute, handler, dataSyncJobBinding(), false)
	request := DataSyncJobRequest{Version: 1, Market: "cn", TriggerSource: "system"}
	if _, err := startSystemDataSyncJob(runtime, JobKindFactorRebuild, nil, request); err != nil {
		test.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		test.Fatal("initial rebuild did not start")
	}
	originalDelay := systemScheduleRetryDelay
	systemScheduleRetryDelay = 10 * time.Millisecond
	test.Cleanup(func() { systemScheduleRetryDelay = originalDelay })
	revision.Store(2)
	for index := 0; index < 3; index++ {
		request.Reason = fmt.Sprintf("new inputs %d", index)
		scheduleSystemDataSyncJob(runtime, JobKindFactorRebuild, request)
	}
	unblock()
	for _, expected := range []int64{1, 2} {
		select {
		case actual := <-observed:
			if actual != expected {
				test.Fatalf("wrong rebuild input: %d instead of %d", actual, expected)
			}
		case <-time.After(3 * time.Second):
			test.Fatalf("rebuild input %d was never consumed", expected)
		}
	}
}

func TestFactorDateIgnoresIntradayAndFutureState(test *testing.T) {
	setupPlanDB(test)
	now := time.Date(2026, 9, 4, 11, 0, 0, 0, datasource.ChinaMarketLocation())
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: "600000", LastBarDate: "2026-09-04"}).Error; err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Create(&[]model.DailyBar{{Market: "cn", Symbol: "600000", TradeDate: "2026-09-03", Close: 10}, {Market: "cn", Symbol: "600000", TradeDate: "2026-09-04", Close: 11}}).Error; err != nil {
		test.Fatal(err)
	}
	if date, err := wideFreshDateAtDB(common.DB, now); err != nil || date != "2026-09-03" {
		test.Fatalf("forming daily candle became a closing factor target: %s %v", date, err)
	}
}

func TestIntradayIncompleteStockCannotBeHiddenByAnotherCompleteStock(test *testing.T) {
	setupTestDB(test)
	for _, symbol := range []string{"600901", "600902"} {
		if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol}).Error; err != nil {
			test.Fatal(err)
		}
	}
	recovered := false
	service := &IntradayService{fetchMin5: func(_ context.Context, _, symbol string, _ int) ([]datasource.Min5Bar, error) {
		bars := fullDay48("20260909", func(string) float64 { return 10 }, func(string) int64 { return 100 })
		if symbol == "600902" && !recovered {
			bars = bars[:45]
		}
		return bars, nil
	}}
	if count, err := service.SyncIntradayFactors(test.Context(), []string{"2026-09-09"}); err == nil || count != 1 {
		test.Fatalf("an incomplete stock allowed the market cursor to advance: count=%d err=%v", count, err)
	}
	recovered = true
	if count, err := service.SyncIntradayFactors(test.Context(), []string{"2026-09-09"}); err != nil || count != 2 {
		test.Fatalf("retry failed to fill the missing stock: count=%d err=%v", count, err)
	}
}

func TestAnalysisFinalQuoteDisablesOldEntryWithoutChangingFrozenPrices(test *testing.T) {
	service, _, _ := analysisSnapshotReviewEnv(test, "", 12)
	frozen := &ResearchPricePlan{Status: "ready", BuyLow: 10, BuyHigh: 11, ReferencePrice: 10.5, Reasons: []string{"original"}}
	result := &AnalysisResult{TradePlan: &tradePlan{BuyLow: 10, BuyHigh: 11, StopPrice: 9, TargetPrice: 14,
		PricePlan: frozen, Position: &positionAdvice{PositionPct: 50}}}
	service.recheckAnalysisTradePlan(test.Context(), AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "cn", Symbol: "510300"}, result)
	if !result.TradePlan.NoPlan || result.TradePlan.Position.PositionPct != 0 || result.TradePlan.PricePlan.Status != "unavailable" {
		test.Fatalf("final quote above the buy zone retained an executable plan: %+v", result.TradePlan)
	}
	if frozen.Status != "ready" || frozen.BuyHigh != 11 || len(frozen.Reasons) != 1 || result.TradePlan.BuyHigh != 11 {
		test.Fatalf("execution recheck mutated the frozen research input: %+v", frozen)
	}
}
