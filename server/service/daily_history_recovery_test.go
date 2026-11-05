package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

type historyRecoveryAdapter struct {
	recPreheatMarketAdapter
	mutex    sync.Mutex
	series   map[string][]datasource.Bar
	failure  error
	requests []int
}

func (adapter *historyRecoveryAdapter) Name() string { return "tencent" }

func (adapter *historyRecoveryAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	adapter.mutex.Lock()
	defer adapter.mutex.Unlock()
	adapter.requests = append(adapter.requests, limit)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if adapter.failure != nil {
		return nil, adapter.failure
	}
	bars := append([]datasource.Bar(nil), adapter.series[symbol]...)
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}
	return bars, nil
}

func (adapter *historyRecoveryAdapter) GetQuote(context.Context, string, string) (*datasource.Quote, error) {
	return nil, datasource.ErrNoData
}

func seedRecoveredDailyHistory(test *testing.T, symbol string, count int, lastDate time.Time) []datasource.Bar {
	test.Helper()
	series := wideGenBars(wideGenDates(count, lastDate), 10)
	rows := make([]model.DailyBar, 0, len(series))
	for candleIndex := range series {
		candle := &series[candleIndex]
		candle.Source = "eastmoney"
		rows = append(rows, model.DailyBar{
			Market: "cn", Symbol: symbol, TradeDate: candle.TradeDate,
			Open: candle.Open, High: candle.High, Low: candle.Low, Close: candle.Close,
			Volume: candle.Volume, Amount: candle.Amount, TurnoverRate: candle.TurnoverRate, Source: candle.Source,
		})
	}
	if err := common.DB.CreateInBatches(rows, 200).Error; err != nil {
		test.Fatal(err)
	}
	return series
}

func TestRecoveredHistoryRemainsReadableDuringSourceFailure(test *testing.T) {
	setupTestDB(test)
	const symbol = "600091"
	series := seedRecoveredDailyHistory(test, symbol, 251, time.Now().AddDate(0, 0, -2))
	adapter := &historyRecoveryAdapter{failure: datasource.ErrUpstream}
	market := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	bars, err := market.GetDailyBars(test.Context(), "cn", symbol, 250)
	if err != nil || len(bars) != 250 || bars[0] != series[1] {
		test.Fatalf("successful backfill was ignored during an upstream outage: count=%d err=%v", len(bars), err)
	}
	tracking := NewTrackingService(market)
	observed, referenceClose, err := tracking.symbolBarsAfter(test.Context(), "cn", symbol, series[248].TradeDate, series[248].TradeDate)
	if err != nil || len(observed) != 2 || referenceClose != 10 || observed[1].TradeDate != series[250].TradeDate {
		test.Fatalf("tracking did not consume repaired history: bars=%+v anchor=%v err=%v", observed, referenceClose, err)
	}
	if len(adapter.requests) != 1 {
		test.Fatalf("tracking bypassed the shared daily cache: requests=%v", adapter.requests)
	}
}

func TestRecoveredHistoryExtendsShortBackupAndRetainsTradingFacts(test *testing.T) {
	setupTestDB(test)
	const symbol = "600092"
	series := seedRecoveredDailyHistory(test, symbol, 251, time.Now().AddDate(0, 0, -2))
	shortSeries := append([]datasource.Bar(nil), series[221:]...)
	for candleIndex := range shortSeries {
		shortSeries[candleIndex].Source = "tencent"
		shortSeries[candleIndex].Amount = 0
		shortSeries[candleIndex].TurnoverRate = 0
	}
	adapter := &historyRecoveryAdapter{series: map[string][]datasource.Bar{symbol: shortSeries}}
	market := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	for _, limit := range []int{120, 210, 220, 250, 251} {
		bars, err := market.GetDailyBars(test.Context(), "cn", symbol, limit)
		if err != nil || len(bars) != limit {
			test.Fatalf("requested window %d shrank to upstream's 30 rows: count=%d err=%v", limit, len(bars), err)
		}
		last := bars[len(bars)-1]
		if last.Source != "tencent" || last.Amount != series[250].Amount || last.TurnoverRate != series[250].TurnoverRate {
			test.Fatalf("consumer lost preserved same-day trading facts: %+v", last)
		}
	}
	if len(adapter.requests) != 1 || adapter.requests[0] != 251 {
		test.Fatalf("recommendation's extra candle split the canonical cache: requests=%v", adapter.requests)
	}
}

func TestRecoveredHistoryCannotMaskFailedAdjustmentRebase(test *testing.T) {
	setupTestDB(test)
	const symbol = "600093"
	series := seedRecoveredDailyHistory(test, symbol, 250, time.Now().AddDate(0, 0, -2))
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "done", BarsCount: 250, LastBarDate: series[249].TradeDate}).Error; err != nil {
		test.Fatal(err)
	}
	truncated := append([]datasource.Bar(nil), series[220:]...)
	for candleIndex := range truncated {
		truncated[candleIndex].Open = 5
		truncated[candleIndex].High = 5
		truncated[candleIndex].Low = 5
		truncated[candleIndex].Close = 5
		truncated[candleIndex].Source = "tencent"
	}
	adapter := &historyRecoveryAdapter{series: map[string][]datasource.Bar{symbol: truncated}}
	market := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	if err := market.persistDailyBars(test.Context(), "cn", symbol, truncated); !errors.Is(err, errBarBasisUnverified) {
		test.Fatalf("short successful re-fetch was accepted as a complete replacement: %v", err)
	}
	if barCount(test, symbol) != 250 {
		test.Fatal("a truncated rebase deleted full history")
	}
	var first model.DailyBar
	if err := common.DB.Where("market = ? AND symbol = ?", "cn", symbol).Order("trade_date").First(&first).Error; err != nil || first.Close != 10 {
		test.Fatalf("rejected rebase changed the original series: row=%+v err=%v", first, err)
	}
	adapter.failure = datasource.ErrUpstream
	if bars, err := market.GetDailyBars(test.Context(), "cn", symbol, 250); !errors.Is(err, errBarBasisUnverified) || len(bars) != 0 {
		test.Fatalf("fallback reused a known unverified price basis: count=%d err=%v", len(bars), err)
	}
}

func TestRecoveredHistoryInvalidatesOtherServiceCaches(test *testing.T) {
	setupTestDB(test)
	const symbol = "600094"
	series := seedRecoveredDailyHistory(test, symbol, 250, time.Now().AddDate(0, 0, -3))
	adapter := &historyRecoveryAdapter{failure: datasource.ErrUpstream}
	reader := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	if _, err := reader.GetDailyBars(test.Context(), "cn", symbol, 250); err != nil {
		test.Fatal(err)
	}
	writer := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	newDate := time.Now().AddDate(0, 0, -2).Format(time.DateOnly)
	newSeries := append(series, datasource.Bar{TradeDate: newDate, Open: 10, High: 11, Low: 10, Close: 11, Source: "eastmoney"})
	if err := writer.persistDailyBars(test.Context(), "cn", symbol, newSeries); err != nil {
		test.Fatal(err)
	}
	writer.invalidateDailyBars("cn", symbol)
	updated, err := reader.GetDailyBars(test.Context(), "cn", symbol, 250)
	if err != nil || len(updated) != 250 || updated[249].TradeDate != newDate || updated[249].Close != 11 {
		test.Fatalf("background reader retained its pre-repair cache: count=%d err=%v", len(updated), err)
	}
}

func TestRecoveredHistoryNeverPersistsFormingCandle(test *testing.T) {
	setupTestDB(test)
	const symbol = "600095"
	now := time.Now().In(datasource.ChinaMarketLocation())
	marketOpen := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, datasource.ChinaMarketLocation())
	today := marketOpen.Format(time.DateOnly)
	series := wideGenBars([]string{marketOpen.AddDate(0, 0, -1).Format(time.DateOnly), today}, 10)
	for candleIndex := range series {
		series[candleIndex].Source = "tencent"
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(&historyRecoveryAdapter{series: map[string][]datasource.Bar{symbol: series}}))
	market.dailyNow = func() time.Time { return marketOpen }
	if bars, err := market.GetDailyBars(test.Context(), "cn", symbol, 250); err != nil || len(bars) != 2 {
		test.Fatalf("online chart lost its forming candle: bars=%+v err=%v", bars, err)
	}
	var todayCount int64
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ? AND trade_date = ?", "cn", symbol, today).Count(&todayCount).Error; err != nil || todayCount != 0 {
		test.Fatalf("forming candle entered completed daily history: count=%d err=%v", todayCount, err)
	}
}

func TestTrackingRecoveryOneGapDoesNotBlockHealthyStocksOrLaterBatches(test *testing.T) {
	setupTestDB(test)
	seedTrackingReviewCalendar(test, "2026-09-01")
	firstBatch, missing := seedLinkFixture(test, 8970, "600090", model.RecTypeShortTerm, model.RecStatusSuccess)
	laterBatch, later := seedLinkFixture(test, 8970, "600092", model.RecTypeShortTerm, model.RecStatusSuccess)
	firstBatch.CreatedAt = time.Date(2026, 9, 1, 16, 0, 0, 0, time.Local)
	laterBatch.CreatedAt = firstBatch.CreatedAt
	healthy := missing
	healthy.ID, healthy.Symbol = 0, "600091"
	for _, recommendation := range []*model.Recommendation{&missing, &healthy, &later} {
		recommendation.DetailJSON = `{"valid_days":2,"take_profit":12,"stop_loss":9}`
		if err := common.DB.Save(recommendation).Error; err != nil {
			test.Fatal(err)
		}
	}
	complete := []datasource.Bar{bar("2026-09-02", 10, 10.5, 9.5, 10), bar("2026-09-03", 10, 12.5, 9.5, 12)}
	provider := &historyRecoveryAdapter{series: map[string][]datasource.Bar{
		missing.Symbol: complete[1:], healthy.Symbol: complete, later.Symbol: complete,
	}}
	tracking := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(provider)))
	updated, err := tracking.refreshBatches(test.Context(), []model.RecommendationBatch{firstBatch, laterBatch})
	if err == nil || updated != 2 || !strings.Contains(err.Error(), missing.Symbol) {
		test.Fatalf("one missing date blocked other completed evaluations: updated=%d err=%v", updated, err)
	}
	var states []model.RecommendationStatus
	if err := common.DB.Order("recommendation_id").Find(&states).Error; err != nil {
		test.Fatal(err)
	}
	if len(states) != 2 {
		test.Fatalf("expected only the two safe settlements to be saved, got %d", len(states))
	}
	for _, state := range states {
		if state.RecommendationID == missing.ID || state.Outcome != model.RecOutcomeTakeProfit {
			test.Fatalf("missing evidence was settled or healthy result was lost: %+v", state)
		}
	}
}

func TestRecoveredHistoryFreshMaintenanceServesRecommendationWithoutRefetch(test *testing.T) {
	setupTestDB(test)
	const symbol = "600096"
	now := time.Date(2026, 9, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	seedRecoveredDailyHistory(test, symbol, 250, now)
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).
		Update("updated_at", now.Add(-time.Minute)).Error; err != nil {
		test.Fatal(err)
	}
	provider := &historyRecoveryAdapter{failure: datasource.ErrUpstream}
	market := NewMarketService(datasource.NewManagerWithAdapters(provider))
	market.dailyNow = func() time.Time { return now }
	bars, err := market.GetDailyBars(test.Context(), "cn", symbol, 251)
	if err != nil || len(bars) != 250 || len(provider.requests) != 0 {
		test.Fatalf("fresh repaired history needlessly depended on upstream again: count=%d calls=%v err=%v", len(bars), provider.requests, err)
	}
	completed, reason := recommendationCompletedBars(bars, candidate{Market: "cn", Symbol: symbol, QuoteAsOf: "2026-09-04 17:00"})
	if len(completed) != 250 || reason != "" {
		test.Fatalf("repaired history failed the actual recommendation input gate: count=%d reason=%s", len(completed), reason)
	}
}

func TestRecoveredHistoryInitializationDoesNotCertifyTruncatedResponse(test *testing.T) {
	setupTestDB(test)
	const symbol = "600097"
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
		test.Fatal(err)
	}
	series := wideGenBars(wideGenDates(45, time.Now().AddDate(0, 0, -2)), 10)
	for candleIndex := range series {
		series[candleIndex].Source = "tencent"
	}
	market := &MarketService{}
	if err := market.persistDailyBarsWithState(test.Context(), "cn", symbol, series, true); !errors.Is(err, errDailyHistoryIncomplete) {
		test.Fatalf("45 rows were accepted as a 250-session initialization: %v", err)
	}
	state := stateOf(test, symbol)
	if state.InitStatus != "pending" || state.BarsCount != 45 || barCount(test, symbol) != 45 {
		test.Fatalf("partial progress was lost or marked fully initialized: %+v", state)
	}
}

func TestRecoveredHistoryInitializationAcceptsVerifiedNewListing(test *testing.T) {
	setupTestDB(test)
	const symbol = "600098"
	now := time.Date(2026, 9, 4, 17, 0, 0, 0, datasource.ChinaMarketLocation())
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Create(&model.IpoSubscription{Kind: model.IpoKindStock, Code: symbol, ApplyDate: "2026-08-28", ListDate: "2026-09-03"}).Error; err != nil {
		test.Fatal(err)
	}
	series := wideGenBars([]string{"2026-09-03", "2026-09-04"}, 10)
	for candleIndex := range series {
		series[candleIndex].Source = "tencent"
	}
	market := &MarketService{dailyNow: func() time.Time { return now }}
	if err := market.persistDailyBarsWithState(test.Context(), "cn", symbol, series, true); err != nil {
		test.Fatalf("an observed two-day listing was required to invent 250 sessions: %v", err)
	}
	if state := stateOf(test, symbol); state.InitStatus != "done" || state.BarsCount != 2 {
		test.Fatalf("verified short listing was not initialized: %+v", state)
	}
}

func TestRecoveredHistorySourceAlgorithmSwitchReplacesWholeSeries(test *testing.T) {
	setupTestDB(test)
	const symbol = "600099"
	series := seedRecoveredDailyHistory(test, symbol, 250, time.Now().AddDate(0, 0, -2))
	for candleIndex := range series {
		series[candleIndex].Source = "sina_qfq"
	}
	provider := &fakeWideSource{bars: map[string][]datasource.Bar{symbol: series}}
	market := &MarketService{wide: provider}
	// All recent closes match: source-family changes must not depend on a
	// numeric mismatch that may first appear outside the requested tail.
	if err := market.persistDailyBars(test.Context(), "cn", symbol, series[249:]); err != nil {
		test.Fatal(err)
	}
	var nonSinaCount int64
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ? AND source <> ?", "cn", symbol, "sina_qfq").Count(&nonSinaCount).Error; err != nil {
		test.Fatal(err)
	}
	if barCount(test, symbol) != 250 || nonSinaCount != 0 || provider.barsCalls[symbol] != 1 {
		test.Fatalf("source switch mixed incompatible adjustment methods: rows=%d other=%d calls=%d", barCount(test, symbol), nonSinaCount, provider.barsCalls[symbol])
	}
}

func TestRecoveredHistoryTradingFactsStayDateSpecificAndKeepProvenance(test *testing.T) {
	setupTestDB(test)
	const symbol = "600088"
	series := seedRecoveredDailyHistory(test, symbol, 2, time.Now().AddDate(0, 0, -2))
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).
		Updates(map[string]any{"amount_source": "eastmoney", "turnover_source": "eastmoney"}).Error; err != nil {
		test.Fatal(err)
	}
	series[0].Amount, series[0].AmountSource = 700000, "sohu"
	series[0].TurnoverRate, series[0].TurnoverSource = 2, "sohu"
	series[1].Amount, series[1].TurnoverRate = 0, 0
	for candleIndex := range series {
		series[candleIndex].Source = "tencent"
	}
	market := &MarketService{}
	if err := market.persistDailyBars(test.Context(), "cn", symbol, series); err != nil {
		test.Fatal(err)
	}
	var stored []model.DailyBar
	if err := common.DB.Where("market = ? AND symbol = ?", "cn", symbol).Order("trade_date").Find(&stored).Error; err != nil {
		test.Fatal(err)
	}
	if stored[0].Amount != 700000 || stored[0].AmountSource != "sohu" || stored[0].TurnoverSource != "sohu" || stored[0].Source != "tencent" {
		test.Fatalf("independent amount facts replaced or mislabeled prices: %+v", stored[0])
	}
	if stored[1].Amount != 1000000 || stored[1].TurnoverRate != 1 || stored[1].AmountSource != "eastmoney" {
		test.Fatalf("one nonempty row erased another day's known facts: %+v", stored[1])
	}
}
