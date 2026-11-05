package service

import (
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestHistoryIntegrityFailedRebaseBlocksFallbackWithoutUniverseState(test *testing.T) {
	setupTestDB(test)
	const symbol = "510300"
	series := seedRecoveredDailyHistory(test, symbol, 250, time.Now().AddDate(0, 0, -2))
	changed := append([]datasource.Bar(nil), series[220:]...)
	for candleIndex := range changed {
		changed[candleIndex].Open, changed[candleIndex].High = 5, 5
		changed[candleIndex].Low, changed[candleIndex].Close = 5, 5
		changed[candleIndex].Source = "tencent"
	}
	provider := &historyRecoveryAdapter{series: map[string][]datasource.Bar{symbol: changed}}
	market := NewMarketService(datasource.NewManagerWithAdapters(provider))
	if _, err := market.GetDailyBars(test.Context(), "cn", symbol, 250); !errors.Is(err, errBarBasisUnverified) {
		test.Fatalf("short rebase was accepted: %v", err)
	}
	var lock model.DailyBarWriteLock
	if err := common.DB.Where("market = ? AND symbol = ?", "cn", symbol).First(&lock).Error; err != nil || !lock.BasisUnverified {
		test.Fatalf("non-universe symbol lost its durable failure marker: lock=%+v err=%v", lock, err)
	}
	var universeCount int64
	common.DB.Model(&model.MarketSyncState{}).Where("market = ? AND symbol = ?", "cn", symbol).Count(&universeCount)
	if universeCount != 0 {
		test.Fatal("an ETF was inserted into the stock discovery universe")
	}
	provider.failure = datasource.ErrUpstream
	if bars, err := NewMarketService(datasource.NewManagerWithAdapters(provider)).GetDailyBars(test.Context(), "cn", symbol, 250); !errors.Is(err, errBarBasisUnverified) || len(bars) != 0 {
		test.Fatalf("a new service reused a known unverified ETF basis: rows=%d err=%v", len(bars), err)
	}
}

func TestHistoryIntegrityOnlineRebaseInvalidatesOtherConsumers(test *testing.T) {
	setupTestDB(test)
	const symbol = "600080"
	series := seedRecoveredDailyHistory(test, symbol, 250, time.Now().AddDate(0, 0, -2))
	reader := NewMarketService(datasource.NewManagerWithAdapters(&historyRecoveryAdapter{failure: datasource.ErrUpstream}))
	if bars, err := reader.GetDailyBars(test.Context(), "cn", symbol, 250); err != nil || bars[0].Close != 10 {
		test.Fatalf("initial cache setup failed: %v", err)
	}
	for candleIndex := range series {
		series[candleIndex].Open, series[candleIndex].High = 5, 5
		series[candleIndex].Low, series[candleIndex].Close = 5, 5
		series[candleIndex].Source = "tencent"
	}
	writer := NewMarketService(datasource.NewManagerWithAdapters(&historyRecoveryAdapter{series: map[string][]datasource.Bar{symbol: series}}))
	if _, err := writer.GetDailyBars(test.Context(), "cn", symbol, 250); err != nil {
		test.Fatal(err)
	}
	// No manual invalidation: the online rebase's committed transaction owns it.
	bars, err := reader.GetDailyBars(test.Context(), "cn", symbol, 250)
	if err != nil || len(bars) != 250 || bars[0].Close != 5 || bars[249].Close != 5 {
		test.Fatalf("another instance retained prices from before an online rebase: count=%d err=%v", len(bars), err)
	}
}

func TestHistoryIntegrityRebaseCannotReplaceNewInputWithOlderFallback(test *testing.T) {
	setupTestDB(test)
	const symbol = "600079"
	oldEnd := time.Now().AddDate(0, 0, -7)
	original := seedRecoveredDailyHistory(test, symbol, 250, oldEnd)
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).UpdateColumn("source", "sina_qfq").Error; err != nil {
		test.Fatal(err)
	}
	oldFallback := wideGenBars(wideGenDates(250, oldEnd.AddDate(0, 0, 1)), 10)
	for candleIndex := range oldFallback {
		oldFallback[candleIndex].Source = "sina_qfq"
	}
	newWindow := wideGenBars(wideGenDates(2, oldEnd.AddDate(0, 0, 5)), 12)
	for candleIndex := range newWindow {
		newWindow[candleIndex].Source = "tencent"
	}
	market := &MarketService{wide: &fakeWideSource{bars: map[string][]datasource.Bar{symbol: oldFallback}}}
	if err := market.persistDailyBars(test.Context(), "cn", symbol, newWindow); !errors.Is(err, errBarBasisUnverified) {
		test.Fatalf("an older re-fetch certified newer incompatible input: %v", err)
	}
	var latest model.DailyBar
	if err := common.DB.Where("market = ? AND symbol = ?", "cn", symbol).Order("trade_date DESC").First(&latest).Error; err != nil {
		test.Fatal(err)
	}
	if latest.TradeDate != original[249].TradeDate || latest.Close != 10 || barCount(test, symbol) != 250 {
		test.Fatalf("failed re-fetch modified the prior full history: %+v", latest)
	}
}

func TestHistoryIntegrityLegacyTimestampCannotCertifyCompletedCandle(test *testing.T) {
	setupTestDB(test)
	const symbol = "600078"
	series := seedRecoveredDailyHistory(test, symbol, 250, time.Now().AddDate(0, 0, -2))
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ? AND trade_date = ?", "cn", symbol, series[249].TradeDate).
		UpdateColumn("updated_at", nil).Error; err != nil {
		test.Fatal(err)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(&historyRecoveryAdapter{failure: datasource.ErrUpstream}))
	if bars, err := market.GetDailyBars(test.Context(), "cn", symbol, 250); err == nil || len(bars) != 0 {
		test.Fatalf("a migrated timestamp-unknown candle became a verified close: count=%d err=%v", len(bars), err)
	}
}
