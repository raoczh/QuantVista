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

func TestDailyBarsCacheCannotCarryFormingCandleAcrossClose(test *testing.T) {
	beforeClose := time.Date(2026, 9, 4, 14, 59, 0, 0, datasource.ChinaMarketLocation())
	forming := dailyBarPolicyAt("cn", beforeClose, true, "2026-09-03")
	closing := dailyBarPolicyAt("cn", beforeClose.Add(2*time.Minute), true, "2026-09-03")
	if forming.cacheScope == closing.cacheScope || forming.cacheTTL > time.Minute || closing.expectedDate != "2026-09-04" {
		test.Fatalf("forming candle reused as closing signal: before=%+v after=%+v", forming, closing)
	}
	nextDay := dailyBarPolicyAt("cn", beforeClose.AddDate(0, 0, 3), true, "2026-09-04")
	if forming.cacheScope == nextDay.cacheScope {
		test.Fatal("daily history cache did not separate trading dates")
	}
}

func TestTrackingFreshQuoteSupersedesCachedTodayWithoutReplacingHistory(test *testing.T) {
	now := time.Date(2026, 9, 4, 10, 30, 0, 0, datasource.ChinaMarketLocation())
	bars := []datasource.Bar{{TradeDate: "2026-09-03", Close: 10}, {TradeDate: "2026-09-04", Close: 10.1}}
	remaining := dropSupersededTrackingDay(bars, now, "2026-09-02", now)
	if len(remaining) != 1 || remaining[0].TradeDate != "2026-09-03" {
		test.Fatalf("cached today prevented fresh quote replacement: %+v", remaining)
	}
	postClose := now.Add(6 * time.Hour)
	if got := dropSupersededTrackingDay(bars, postClose, "2026-09-02", now); len(got) != 2 {
		test.Fatal("morning quote must not displace completed closing history")
	}
}

func TestTrackingCalendarCanRecoverFromBenchmarkWithoutGuessingFuture(test *testing.T) {
	setupPlanDB(test)
	bars := []datasource.Bar{{TradeDate: "2026-09-04", Close: 3000}, {TradeDate: "2026-09-07", Close: 3010}}
	if err := persistTrackingCalendar(test.Context(), "cn", bars); err != nil {
		test.Fatal(err)
	}
	dates, err := trackingTradeDates(test.Context(), "cn", "2026-09-04", "2026-09-07")
	if err != nil || len(dates) != 1 || dates[0] != "2026-09-07" {
		test.Fatalf("benchmark did not recover known market day axis: %v %v", dates, err)
	}
	if _, err := trackingTradeDates(test.Context(), "cn", "2026-09-04", "2026-09-08"); err == nil {
		test.Fatal("benchmark window was extended into an unobserved weekday")
	}
}

func TestAnalysisCompactionKeepsProvenanceAndUnknownSourceTimes(test *testing.T) {
	quoteTime := time.Date(2026, 9, 4, 10, 30, 0, 0, datasource.ChinaMarketLocation())
	indices := compactIndices([]datasource.Index{{Code: "000001", Source: "tencent", Price: 3000, DataTime: quoteTime}, {Code: "399001", Source: "sina"}})
	if indices[0]["source_data_time"] != "2026-09-04 10:30:00" || indices[0]["source"] != "tencent" || indices[1]["source_time_status"] != "unknown" {
		test.Fatalf("AI snapshot lost real source time: %+v", indices)
	}
	sectors := []datasource.SectorRank{{Code: "sina:new_bank", Source: "sina", Name: "Banks"}}
	compacted := compactSectors(sectors, 10)
	if compacted[0]["code"] != "sina:new_bank" || compacted[0]["source"] != "sina" || sectorEnrichmentMatchesSource(sectors) {
		test.Fatalf("different classifications would be silently mixed: %+v", compacted)
	}
	service := &AnalysisService{market: NewMarketService(datasource.NewManagerWithAdapters())}
	if snapshot, err := service.buildMarketContext(test.Context(), "cn"); err == nil || snapshot != nil {
		test.Fatalf("AI analysis accepted an entirely unavailable market: %+v %v", snapshot, err)
	}
}

type researchAnnouncementTestProvider struct {
	refusalTestAdapter
	announcements []datasource.EMAnnouncement
	failure       error
	calls         int
}

func (provider *researchAnnouncementTestProvider) Name() string { return "announcement-test" }
func (provider *researchAnnouncementTestProvider) GetAnnouncements(context.Context, string, string, int) ([]datasource.EMAnnouncement, error) {
	provider.calls++
	return provider.announcements, provider.failure
}

func TestResearchAnnouncementsRefreshInventoryAndRetainFailure(test *testing.T) {
	setupTestDB(test)
	if err := common.DB.Create(&model.Announcement{Symbol: "600000", Market: "cn", ArtCode: "old", Title: "Old notice", NoticeDate: "2026-09-01", Source: "eastmoney"}).Error; err != nil {
		test.Fatal(err)
	}
	provider := &researchAnnouncementTestProvider{announcements: []datasource.EMAnnouncement{{Symbol: "600000", ArtCode: "new", Title: "New notice", Source: "cninfo", NoticeDate: time.Date(2026, 9, 2, 0, 0, 0, 0, time.Local)}}}
	market := NewMarketService(datasource.NewManagerWithAdapters(provider))
	rows, err := market.researchAnnouncementBriefs(test.Context(), "600000", 5)
	if err != nil || len(rows) != 2 || rows[0].Title != "New notice" || rows[0].Source != "cninfo" {
		test.Fatalf("AI research used only old inventory: %+v %v", rows, err)
	}
	_, _ = market.researchAnnouncementBriefs(test.Context(), "600000", 5)
	if provider.calls != 1 {
		test.Fatalf("repeated research bypassed refresh coalescing/cooldown: %d", provider.calls)
	}
	provider.failure = errors.New("offline")
	failingMarket := NewMarketService(datasource.NewManagerWithAdapters(provider))
	rows, err = failingMarket.researchAnnouncementBriefs(test.Context(), "600000", 5)
	if err == nil || len(rows) != 2 {
		test.Fatalf("history must remain visible while refresh failure remains explicit: %+v %v", rows, err)
	}
}
