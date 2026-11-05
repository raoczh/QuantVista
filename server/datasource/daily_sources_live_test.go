package datasource

import (
	"context"
	"os"
	"testing"
	"time"
)

// Opt-in, one pass only: two symbols per source, no database or market scan.
// LIVE_DAILY_SOURCES=1 go test ./datasource -run '^TestLiveDailySources$' -count=1 -v
func TestLiveDailySources(test *testing.T) {
	if os.Getenv("LIVE_DAILY_SOURCES") != "1" {
		test.Skip("set LIVE_DAILY_SOURCES=1 for bounded Sina/Sohu public-source checks")
	}
	for _, symbol := range []string{"600000", "000001"} {
		test.Run("sina_qfq/"+symbol, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(test.Context(), 20*time.Second)
			defer cancel()
			bars, err := NewSinaAdapter().GetDailyBars(ctx, "cn", symbol, 400)
			if err != nil || len(bars) == 0 {
				test.Fatalf("Sina adjusted daily unavailable: symbol=%s err=%v", symbol, err)
			}
			latest := bars[len(bars)-1]
			if latest.Source != "sina_qfq" {
				test.Fatalf("wrong adjusted source: %+v", latest)
			}
			test.Logf("symbol=%s source=%s count=%d latest=%s close=%.2f volume_lots=%d", symbol, latest.Source, len(bars), latest.TradeDate, latest.Close, latest.Volume)
			for _, bar := range bars {
				if bar.TradeDate == "2025-07-15" || bar.TradeDate == "2025-07-16" {
					test.Logf("historical symbol=%s date=%s adjusted_close=%.2f volume_lots=%d", symbol, bar.TradeDate, bar.Close, bar.Volume)
				}
			}
		})
		test.Run("sohu_facts/"+symbol, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
			defer cancel()
			facts, err := NewSohuAdapter().GetDailyTradingFacts(ctx, "cn", symbol, "2025-07-14", "2025-07-18")
			if err != nil || len(facts) == 0 {
				test.Fatalf("Sohu daily facts unavailable: symbol=%s err=%v", symbol, err)
			}
			for _, fact := range facts {
				test.Logf("symbol=%s source=%s date=%s amount_cny=%.2f turnover_pct=%.2f volume_lots=%d", fact.Symbol, fact.Source, fact.TradeDate, fact.Amount, fact.TurnoverRate, fact.Volume)
			}
		})
	}
}
