package datasource

import (
	"context"
	"os"
	"testing"
	"time"
)

// Opt-in, low-volume network checks. No database, background jobs or market scan.
func TestLiveFreeSources(test *testing.T) {
	if os.Getenv("LIVE_FREE_SOURCES") != "1" {
		test.Skip("set LIVE_FREE_SOURCES=1 for small public-source network checks")
	}
	for _, provider := range []Adapter{NewEastMoneyAdapter(), NewTencentAdapter()} {
		test.Run(provider.Name()+"_daily", func(test *testing.T) {
			ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
			defer cancel()
			bars, err := provider.GetDailyBars(ctx, "cn", "600000", 5)
			if err != nil || len(bars) == 0 {
				test.Fatalf("daily source unavailable: %v", err)
			}
			test.Logf("source=%s bars=%d latest=%s", provider.Name(), len(bars), bars[len(bars)-1].TradeDate)
		})
	}
	test.Run("daily_route", func(test *testing.T) {
		bars, err := DefaultManager().GetDailyBars(test.Context(), "cn", "600000", 5)
		if err != nil || len(bars) == 0 {
			test.Fatalf("daily fallback route unavailable: %v", err)
		}
		test.Logf("actual_source=%s bars=%d latest=%s", bars[0].Source, len(bars), bars[len(bars)-1].TradeDate)
	})
	test.Run("sina_min5", func(test *testing.T) {
		ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
		defer cancel()
		bars, err := NewSinaAdapter().GetMin5Bars(ctx, "cn", "600000", 3)
		if err != nil || len(bars) == 0 {
			test.Fatalf("five-minute source unavailable: %v", err)
		}
		test.Logf("source=sina bars=%d latest=%s", len(bars), bars[len(bars)-1].Time)
	})
	test.Run("cninfo_announcements", func(test *testing.T) {
		ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
		defer cancel()
		items, err := NewCNInfoAdapter().GetAnnouncements(ctx, "cn", "600000", 2)
		if err != nil || len(items) == 0 {
			test.Fatalf("official announcements unavailable: %v", err)
		}
		test.Logf("source=cninfo count=%d latest=%s", len(items), items[0].NoticeDate.Format(time.DateOnly))
	})
	test.Run("sina_news", func(test *testing.T) {
		ctx, cancel := context.WithTimeout(test.Context(), 15*time.Second)
		defer cancel()
		items, err := GetSinaFastNews(ctx, 3)
		if err != nil || len(items) == 0 {
			test.Fatalf("news source unavailable: %v", err)
		}
		test.Logf("source=sina count=%d latest=%s", len(items), items[0].PublishTime.Format(time.RFC3339))
	})
}

func TestLiveBoardFallback(test *testing.T) {
	if os.Getenv("LIVE_BOARD_FALLBACK") != "1" {
		test.Skip("set LIVE_BOARD_FALLBACK=1 for bounded board fallback checks")
	}
	for _, route := range []struct {
		name    string
		manager *Manager
	}{{"default", DefaultManager()}, {"sina_only", NewManagerWithAdapters(NewSinaAdapter())}} {
		for _, kind := range []string{"industry", "concept"} {
			test.Run(route.name+"/"+kind, func(test *testing.T) {
				manager := route.manager
				rows, err := manager.GetBoardHeat(test.Context(), kind)
				if err != nil || len(rows) == 0 {
					test.Fatalf("board route unavailable: %v", err)
				}
				test.Logf("kind=%s source=%s boards=%d source_timestamp=not_provided", kind, rows[0].Source, len(rows))
				stocks, err := manager.GetBoardConstituents(test.Context(), rows[0].Code, 1)
				if err != nil || len(stocks) != 1 {
					test.Fatalf("board constituent unavailable: %v", err)
				}
				test.Logf("board=%s symbol=%s quote_time=%s", rows[0].Code, stocks[0].Symbol, stocks[0].DataTime.Format(time.RFC3339))
			})
		}
	}
}

// A complete snapshot is deliberately separate from the small source probes.
// It is read-only but needs about two requests per hundred listed securities.
func TestLiveFreeSnapshot(test *testing.T) {
	if os.Getenv("LIVE_FREE_SNAPSHOT") != "1" {
		test.Skip("set LIVE_FREE_SNAPSHOT=1 for one complete Sina snapshot, without database writes")
	}
	ctx, cancel := context.WithTimeout(test.Context(), 4*time.Minute)
	defer cancel()
	started := time.Now()
	rows, err := NewSinaAdapter().GetCNSpotSnapshot(ctx)
	if err != nil || len(rows) == 0 {
		test.Fatalf("complete snapshot unavailable: rows=%d err=%v", len(rows), err)
	}
	active := 0
	for _, row := range rows {
		if row.Price > 0 && row.Volume > 0 && row.DataTime > 0 {
			active++
		}
	}
	test.Logf("source=sina_spot complete_rows=%d active_rows=%d elapsed=%s", len(rows), active, time.Since(started))
}
