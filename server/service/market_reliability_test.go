package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestMarketReliabilitySharesDailyWindowWithoutRedis(test *testing.T) {
	setupTestDB(test)
	previousRedis := common.RDB
	common.RDB = nil
	test.Cleanup(func() { common.RDB = previousRedis })
	dates := wideGenDates(250, time.Now().AddDate(0, 0, -2))
	adapter := &reviewDailyAdapter{name: "tencent", bars: wideGenBars(dates, 10), calls: map[string]int{}}
	service := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	for _, limit := range []int{120, 210, 220} {
		bars, err := service.GetDailyBars(test.Context(), "cn", "600099", limit)
		if err != nil || len(bars) != limit || bars[len(bars)-1].TradeDate != dates[len(dates)-1] || bars[0].Source != "tencent" {
			test.Fatalf("daily window is not a correctly sourced tail: limit=%d count=%d err=%v", limit, len(bars), err)
		}
		bars[0].Close = 999
	}
	if adapter.calls["600099"] != 1 {
		test.Fatalf("chart/indicators/chips made duplicate upstream calls: %d", adapter.calls["600099"])
	}
	clean, err := service.GetDailyBars(test.Context(), "cn", "600099", 250)
	if err != nil || clean[0].Close != 10 {
		test.Fatalf("a caller mutated the cached series: %v", err)
	}
}

func TestMarketReliabilityTencentRebasePreservesKnownFacts(test *testing.T) {
	setupTestDB(test)
	const symbol = "600098"
	dates := wideGenDates(250, time.Now().AddDate(0, 0, -2))
	lastDate := dates[len(dates)-1]
	original := model.DailyBar{Symbol: symbol, Market: "cn", TradeDate: lastDate,
		Open: 20, High: 20, Low: 20, Close: 20, Volume: 1000, Amount: 200000, TurnoverRate: 1.25, Source: "eastmoney"}
	if err := common.DB.Create(&original).Error; err != nil {
		test.Fatal(err)
	}
	bars := wideGenBars(dates, 10)
	for index := range bars {
		bars[index].Source = "tencent"
		bars[index].Amount = 0
		bars[index].TurnoverRate = 0
	}
	adapter := &reviewDailyAdapter{name: "tencent", bars: bars, calls: map[string]int{}}
	service := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	if err := service.persistDailyBars(test.Context(), "cn", symbol, bars[len(bars)-1:]); err != nil {
		test.Fatalf("backup source could not rebuild the price basis: %v", err)
	}
	var restored model.DailyBar
	if err := common.DB.Where("symbol = ? AND trade_date = ?", symbol, lastDate).First(&restored).Error; err != nil {
		test.Fatal(err)
	}
	if barCount(test, symbol) != 250 || restored.Close != 10 || restored.Source != "tencent" ||
		restored.Amount != original.Amount || restored.TurnoverRate != original.TurnoverRate {
		test.Fatalf("rebase lost actual source or existing trading facts: %+v", restored)
	}
	if adapter.calls[symbol] != 1 {
		test.Fatalf("expected one full-history fetch, got %d", adapter.calls[symbol])
	}
}

func TestMarketReliabilitySnapshotPersistsActualSource(test *testing.T) {
	setupTestDB(test)
	const date = "2026-10-30"
	rows := []datasource.SpotRow{{Symbol: "600097", Name: "Test", Price: 10, Open: 10, High: 11, Low: 9,
		PrevClose: 10, Volume: 100, Amount: 100000, Source: "sina_spot"}}
	written, blocked, err := persistWideBarBatch(test.Context(), rows, date)
	if err != nil || written != 1 || len(blocked) != 0 {
		test.Fatalf("snapshot write failed: %d %v %v", written, blocked, err)
	}
	service := &MarketService{}
	if err := service.upsertUniverseDaily(rows, date, test.Context()); err != nil {
		test.Fatal(err)
	}
	var bar model.DailyBar
	var universe model.StockUniverseDaily
	if err := common.DB.Where("symbol = ?", "600097").First(&bar).Error; err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Where("symbol = ?", "600097").First(&universe).Error; err != nil {
		test.Fatal(err)
	}
	if bar.Source != "sina_spot" || universe.Source != "sina_spot" || universe.Industry != "" || universe.PETTM != 0 {
		test.Fatalf("snapshot provenance or unavailable fields fabricated: %+v %+v", bar, universe)
	}
	primaryRows := append([]datasource.SpotRow(nil), rows...)
	primaryRows[0].Source, primaryRows[0].Industry, primaryRows[0].PETTM = "eastmoney", "Banking", 5.5
	if err := service.upsertUniverseDaily(primaryRows, date, test.Context()); err != nil {
		test.Fatal(err)
	}
	if err := service.upsertUniverseDaily(rows, date, test.Context()); err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Where("symbol = ? AND trade_date = ?", "600097", date).First(&universe).Error; err != nil {
		test.Fatal(err)
	}
	if universe.Source != "sina_spot" || universe.Industry != "Banking" || universe.PETTM != 5.5 {
		test.Fatalf("backup erased same-day facts it cannot supply: %+v", universe)
	}
}

func TestMarketReliabilityFreshnessUsesMarketClock(test *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, datasource.ChinaMarketLocation())
	quoteTime := now.Add(-time.Minute).UTC()
	if result := quoteFreshness(quoteTime, now, marketStateTrading, "2026-10-30"); result.Status != freshStatusFresh {
		test.Fatalf("equivalent UTC and Shanghai instants were treated differently: %+v", result)
	}
	if result := quoteFreshness(now.Add(5*time.Hour), now, marketStateTrading, "2026-10-30"); result.Status != freshStatusStale {
		test.Fatalf("future price was treated as fresh: %+v", result)
	}
}

func TestMarketReliabilityAnnouncementRetryAndSource(test *testing.T) {
	setupTestDB(test)
	previousFetch := fetchAnnouncementFeed
	test.Cleanup(func() { fetchAnnouncementFeed = previousFetch })
	failed := true
	fetchAnnouncementFeed = func(context.Context, string, int) ([]datasource.EMAnnouncement, error) {
		if failed {
			return nil, errors.New("temporary source failure")
		}
		return []datasource.EMAnnouncement{{Symbol: "600096", Source: "cninfo", ArtCode: "cninfo:123", Title: "Official filing",
			NoticeDate: time.Now(), URL: "https://static.cninfo.com.cn/finalpage/2026-10-30/123.PDF"}}, nil
	}
	service := NewFinanceService()
	if _, err := service.ListAnnouncements(test.Context(), "600096", 20); err == nil {
		test.Fatal("source failure became an empty success")
	}
	service.annMu.Lock()
	attempt := service.annAttempts["600096"]
	if time.Until(attempt.retryAfter) > 61*time.Second {
		test.Error("transient failure suppressed retries for an hour")
	}
	attempt.retryAfter = time.Now().Add(-time.Second)
	service.annMu.Unlock()
	failed = false
	rows, err := service.ListAnnouncements(test.Context(), "600096", 20)
	if err != nil || len(rows) != 1 || rows[0].Source != "cninfo" {
		test.Fatalf("official source was not retried or persisted: %+v %v", rows, err)
	}
}

func TestMarketReliabilitySinaNewsSurvivesOtherFeedFailure(test *testing.T) {
	setupTestDB(test)
	service := NewNewsService()
	service.fetchEMFast = func(context.Context, string, int) ([]datasource.EMNewsItem, string, error) {
		return nil, "", errors.New("upstream unavailable")
	}
	published := time.Now().Truncate(time.Second)
	service.fetchSinaFast = func(context.Context, int) ([]datasource.EMNewsItem, error) {
		return []datasource.EMNewsItem{{SourceID: "sina-feed-test", Title: "Independent official news", Summary: "Original news text", PublishTime: published}}, nil
	}
	service.collectEMFast(test.Context())
	if count := service.collectSinaFast(test.Context()); count != 1 {
		test.Fatalf("backup news was not collected: %d", count)
	}
	if count := service.collectSinaFast(test.Context()); count != 0 {
		test.Fatalf("repeated news was duplicated: %d", count)
	}
	var news model.News
	if err := common.DB.Where("source_id = ?", "sina-feed-test").First(&news).Error; err != nil {
		test.Fatal(err)
	}
	if news.Source != "sina" || news.RelatedSymbols != "" || !news.PublishTime.Equal(published) {
		test.Fatalf("news source/time/association changed: %+v", news)
	}
}
