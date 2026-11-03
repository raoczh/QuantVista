package service

import (
	"context"
	"testing"
	"time"

	"quantvista/datasource"
)

func TestResearchMarketFreshnessStripsStaleNumbersFromEvidence(test *testing.T) {
	now := quoteReviewTime(test, "2026-07-17 11:00:00")
	judge := func(observed time.Time) quoteFreshInfo {
		return quoteFreshness(observed, now, marketStateTrading, "2026-07-17")
	}
	snapshot := map[string]any{
		"indices": compactIndices([]datasource.Index{
			{Code: "old", Name: "Old index", Source: "old-source", Price: 8792.31, DataTime: now.AddDate(0, -1, 0)},
			{Code: "fresh", Name: "Fresh index", Source: "current-source", Price: 3123.45, DataTime: now.Add(-time.Minute)},
		}),
		"gainers":   []map[string]any{{"symbol": "600001", "price": 871.23, "source": "old-rank", "source_data_time": "2026-07-16 15:00:00"}},
		"actives":   compactRanks([]datasource.StockRank{{Symbol: "600002", Price: 777.31, Source: "unknown-rank"}}, 6),
		"breadth":   &datasource.Breadth{Advances: 8743, TradeDate: "2026-07-16", Source: "breadth-source", DataTime: now},
		"fund_flow": &datasource.MarketFundFlow{MainNet: 98731, Source: "flow-source", DataTime: now},
		"mood":      map[string]any{"trade_date": "2026-07-16", "limit_up_count": 951},
		"sectors":   compactSectors([]datasource.SectorRank{{Code: "sina:bank", Name: "Banks", Source: "sina", ChangePct: 2.5}}, 10),
	}
	if !applyResearchMarketFreshness(snapshot, judge) {
		test.Fatal("valid current index was discarded with its stale peers")
	}
	values := snapshotLabeledValues(snapshot, nil)
	for _, forbidden := range []float64{8792.31, 871.23, 777.31, 8743, 98731, 951} {
		if labeledHas(values, forbidden) {
			test.Fatalf("unverified value %v still entered AI evidence", forbidden)
		}
	}
	if !labeledHas(values, 3123.45) {
		test.Fatal("current index disappeared from evidence")
	}
	oldIndex := snapshot["indices"].([]map[string]any)[0]
	if oldIndex["source"] != "old-source" || oldIndex["source_data_time"] == nil || oldIndex["freshness_status"] != freshStatusStale {
		test.Fatalf("stale index lost its provenance: %+v", oldIndex)
	}
	sector := snapshot["sectors"].([]map[string]any)[0]
	if sector["source_time_status"] != freshStatusUnknown || sector["freshness_status"] != freshStatusUnknown || sector["source_data_time"] != nil {
		test.Fatalf("sector capture time was invented as market time: %+v", sector)
	}
	if len(snapshot["data_deficiencies"].([]string)) < 5 || snapshot["freshness_note"] == nil {
		test.Fatalf("rejected blocks were not declared: %+v", snapshot)
	}
}

type sourceMarketReviewAdapter struct {
	refusalTestAdapter
	indices []datasource.Index
	sectors []datasource.SectorRank
}

func (adapter sourceMarketReviewAdapter) GetIndices(context.Context, string) ([]datasource.Index, error) {
	return adapter.indices, nil
}

func (adapter sourceMarketReviewAdapter) GetSectorRanking(context.Context, string, int) ([]datasource.SectorRank, error) {
	return adapter.sectors, nil
}

func TestResearchMarketAndSectorApplyProgrammaticTimeGate(test *testing.T) {
	setupTestDB(test)
	now := quoteReviewTime(test, "2026-07-17 11:00:00")
	adapter := sourceMarketReviewAdapter{
		indices: []datasource.Index{{Code: "old", Price: 8792.31, Source: "fixture", DataTime: now.AddDate(0, -1, 0)}},
		sectors: []datasource.SectorRank{{Code: "sina:bank", Name: "Banks", Source: "sina", ChangePct: 2.5}},
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	market.quoteNow = func() time.Time { return now }
	analysis := &AnalysisService{market: market}
	for _, build := range []func() (*analysisContext, error){
		func() (*analysisContext, error) { return analysis.buildMarketContext(test.Context(), "cn") },
		func() (*analysisContext, error) { return analysis.buildSectorContext(test.Context(), "cn", "Banks") },
	} {
		result, err := build()
		if err != nil || result == nil {
			test.Fatalf("unknown-time sectors should remain explicitly limited observations: %v", err)
		}
		if labeledHas(snapshotLabeledValues(result.Snapshot, nil), 8792.31) {
			test.Fatal("module builder bypassed stale index filtering")
		}
		if result.Snapshot["data_deficiencies"] == nil || result.Snapshot["unavailable"] == nil {
			test.Fatal("module builder lost freshness or upstream failure declarations")
		}
	}
	if overview := market.GetOverview(test.Context(), "cn"); overview.Indices[0].Price != 8792.31 {
		test.Fatal("AI filtering mutated the shared presentation cache")
	}
	adapter.sectors = nil
	market = NewMarketService(datasource.NewManagerWithAdapters(adapter))
	market.quoteNow = func() time.Time { return now }
	analysis = &AnalysisService{market: market}
	if result, err := analysis.buildMarketContext(test.Context(), "cn"); result != nil || err == nil {
		test.Fatalf("all-unverified market should stop before a model call: %+v %v", result, err)
	}
}
