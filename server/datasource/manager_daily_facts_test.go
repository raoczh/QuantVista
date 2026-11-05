package datasource

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

type dailyFactsTestAdapter struct {
	SohuAdapter
	calls int
	fetch func(context.Context, string, string, string, string) ([]DailyTradingFact, error)
}

func (adapter *dailyFactsTestAdapter) GetDailyTradingFacts(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error) {
	adapter.calls++
	return adapter.fetch(ctx, market, symbol, from, to)
}

func TestDailyFactsEnrichmentPreservesPricesKnownValuesAndInput(test *testing.T) {
	bars := []Bar{
		{TradeDate: "2025-07-16", Open: 9, High: 11, Low: 8, Close: 10, Volume: 100, Source: "tencent", Amount: 500, AmountSource: "eastmoney"},
		{TradeDate: "2025-07-15", Open: 19, High: 21, Low: 18, Close: 20, Volume: 200, Source: "sina_qfq", TurnoverRate: 2, TurnoverSource: "existing"},
		{TradeDate: "2025-07-14", Open: 29, High: 31, Low: 28, Close: 30, Volume: 300, Source: "", Amount: 700, TurnoverRate: 3},
	}
	original := append([]Bar(nil), bars...)
	adapter := &dailyFactsTestAdapter{fetch: func(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error) {
		if market != "cn" || symbol != "600000" || from != "2025-07-15" || to != "2025-07-16" {
			test.Fatalf("unexpected narrow window: %s %s %s %s", market, symbol, from, to)
		}
		return []DailyTradingFact{
			{Market: market, Symbol: symbol, TradeDate: from, Amount: 900, TurnoverRate: 99, Volume: 900000, Source: "sohu"},
			{Market: market, Symbol: symbol, TradeDate: to, Amount: 999, TurnoverRate: 1, Volume: 900000, Source: "sohu"},
		}, nil
	}}
	manager := NewManagerWithAdapters(adapter)
	result := manager.EnrichDailyTradingFacts(test.Context(), "cn", "600000", bars)
	want := append([]Bar(nil), original...)
	want[0].TurnoverRate, want[0].TurnoverSource = 1, "sohu"
	want[1].Amount, want[1].AmountSource = 900, "sohu"
	if !reflect.DeepEqual(result, want) || !reflect.DeepEqual(bars, original) || adapter.calls != 1 {
		test.Fatalf("enrichment changed prices/dates/volume/known values: result=%+v input=%+v", result, bars)
	}
	stat := healthStatFor(test, manager.HealthSnapshot(), "sohu", "daily_facts", "cn")
	if stat.Success != 1 || stat.Errors != 0 || stat.Observation != "success" {
		test.Fatalf("facts health was not recorded independently: %+v", stat)
	}
}

func TestDailyFactsEnrichmentKeepsUnknownPriceSource(test *testing.T) {
	bars := []Bar{{TradeDate: "2025-07-16", Open: 10, High: 10, Low: 10, Close: 10, Volume: 1}}
	adapter := &dailyFactsTestAdapter{fetch: func(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error) {
		return []DailyTradingFact{{Market: market, Symbol: symbol, TradeDate: from, Amount: 1000, Source: "sohu"}}, nil
	}}
	result := NewManagerWithAdapters(adapter).EnrichDailyTradingFacts(test.Context(), "cn", "600000", bars)
	if result[0].Source != "" || result[0].AmountSource != "sohu" || result[0].TurnoverSource != "" || result[0].TurnoverRate != 0 {
		test.Fatalf("unknown provenance or unavailable turnover invented: %+v", result)
	}
}

func TestDailyFactsEnrichmentRejectsWrongIdentityAndInvalidValues(test *testing.T) {
	bars := []Bar{{TradeDate: "2025-07-16", Open: 10, High: 10, Low: 10, Close: 10, Source: "sina_qfq"}}
	valid := DailyTradingFact{Market: "cn", Symbol: "600000", TradeDate: "2025-07-16", Amount: 1000, TurnoverRate: 1, Source: "sohu"}
	for name, mutate := range map[string]func(*DailyTradingFact){
		"wrong symbol":         func(fact *DailyTradingFact) { fact.Symbol = "000001" },
		"wrong market":         func(fact *DailyTradingFact) { fact.Market = "hk" },
		"wrong date":           func(fact *DailyTradingFact) { fact.TradeDate = "2025-07-15" },
		"invalid date":         func(fact *DailyTradingFact) { fact.TradeDate = "2025-02-30" },
		"negative amount":      func(fact *DailyTradingFact) { fact.Amount = -1 },
		"nan amount":           func(fact *DailyTradingFact) { fact.Amount = math.NaN() },
		"inf turnover":         func(fact *DailyTradingFact) { fact.TurnoverRate = math.Inf(1) },
		"unknown facts source": func(fact *DailyTradingFact) { fact.Source = "" },
	} {
		test.Run(name, func(test *testing.T) {
			invalid := valid
			mutate(&invalid)
			adapter := &dailyFactsTestAdapter{fetch: func(context.Context, string, string, string, string) ([]DailyTradingFact, error) {
				return []DailyTradingFact{invalid}, nil
			}}
			manager := NewManagerWithAdapters(adapter)
			if result := manager.EnrichDailyTradingFacts(test.Context(), "cn", "600000", bars); !reflect.DeepEqual(result, bars) {
				test.Fatalf("invalid facts changed prices: %+v", result)
			}
			if stat := healthStatFor(test, manager.HealthSnapshot(), "sohu", "daily_facts", "cn"); stat.Errors != 1 {
				test.Fatalf("invalid facts recorded as success: %+v", stat)
			}
		})
	}
	conflict := valid
	conflict.Amount++
	adapter := &dailyFactsTestAdapter{fetch: func(context.Context, string, string, string, string) ([]DailyTradingFact, error) {
		return []DailyTradingFact{valid, conflict}, nil
	}}
	if result := NewManagerWithAdapters(adapter).EnrichDailyTradingFacts(test.Context(), "cn", "600000", bars); !reflect.DeepEqual(result, bars) {
		test.Fatalf("conflicting facts changed prices: %+v", result)
	}
}

func TestDailyFactsEnrichmentIsBoundedAndOptional(test *testing.T) {
	for _, scenario := range []string{"failure", "empty", "complete", "canceled", "caller deadline", "manager budget"} {
		test.Run(scenario, func(test *testing.T) {
			bars := []Bar{{TradeDate: "2025-07-16", Open: 10, High: 10, Low: 10, Close: 10, Source: "tencent"}}
			adapter := &dailyFactsTestAdapter{fetch: func(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error) {
				if scenario == "caller deadline" || scenario == "manager budget" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if scenario == "empty" {
					return nil, nil
				}
				return nil, errors.New("optional source unavailable")
			}}
			manager := NewManagerWithAdapters(adapter)
			ctx, cancel := context.WithCancel(test.Context())
			defer cancel()
			switch scenario {
			case "complete":
				bars[0].Amount, bars[0].TurnoverRate = 10, 1
			case "canceled":
				cancel()
			case "caller deadline":
				var cancelDeadline context.CancelFunc
				ctx, cancelDeadline = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancelDeadline()
			case "manager budget":
				manager.totalBudget = 30 * time.Millisecond
			}
			started := time.Now()
			result := manager.EnrichDailyTradingFacts(ctx, "cn", "600000", bars)
			if !reflect.DeepEqual(result, bars) || time.Since(started) > time.Second {
				test.Fatalf("optional failure erased bars or exceeded budget: %+v", result)
			}
			if (scenario == "complete" || scenario == "canceled") && adapter.calls != 0 {
				test.Fatal("unnecessary enrichment call")
			}
		})
	}
}

func TestDailyFactsEnrichmentUsesOneHistoricalTailWindow(test *testing.T) {
	latest := time.Date(2022, 11, 4, 0, 0, 0, 0, ChinaMarketLocation())
	cutoff := latest.AddDate(0, 0, -399).Format(time.DateOnly)
	bars := []Bar{
		{TradeDate: "2000-01-04", Source: "tencent"},
		{TradeDate: latest.Format(time.DateOnly), Source: "tencent"},
		{TradeDate: cutoff, Source: "tencent"},
		{TradeDate: "invalid", Source: "tencent"},
		{TradeDate: "2099-01-01", Source: "tencent"},
	}
	adapter := &dailyFactsTestAdapter{fetch: func(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error) {
		if from != cutoff || to != latest.Format(time.DateOnly) {
			test.Fatalf("history must use original tail dates: %s..%s", from, to)
		}
		return []DailyTradingFact{{Market: market, Symbol: symbol, TradeDate: to, Amount: 1000, Source: "sohu"}}, nil
	}}
	result := NewManagerWithAdapters(adapter).EnrichDailyTradingFacts(test.Context(), "cn", "600000", bars)
	if adapter.calls != 1 || result[0] != bars[0] || result[1].Amount != 1000 || result[2] != bars[2] || result[3] != bars[3] || result[4] != bars[4] {
		test.Fatalf("must not page, invent dates or copy facts across days: %+v", result)
	}
}

func TestDailyFactsProbeUsesOneBoundedSymbolAndIndependentHealth(test *testing.T) {
	ResetProbeLimiterForTest()
	test.Cleanup(ResetProbeLimiterForTest)
	adapter := &dailyFactsTestAdapter{fetch: func(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error) {
		fromDate, toDate, err := validateDailyFactsWindow(from, to, time.Now())
		if err != nil || symbol != probeSampleSymbol || market != "cn" || toDate.Sub(fromDate) > 13*24*time.Hour {
			test.Fatalf("unbounded facts probe: %s %s %s %s %v", market, symbol, from, to, err)
		}
		return []DailyTradingFact{{Market: market, Symbol: symbol, TradeDate: to, Amount: 1, Source: "sohu"}}, nil
	}}
	manager := NewManagerWithAdapters(adapter)
	result, err := manager.Probe(test.Context(), "sohu", "daily_facts", "cn")
	if err != nil || result.Outcome != ProbeSuccess || result.SampleCount != 1 || adapter.calls != 1 {
		test.Fatalf("facts probe failed: %+v %v", result, err)
	}
	for _, tuple := range []struct{ source, capability string }{{"sina", "daily_bars"}, {"sohu", "daily_facts"}} {
		stat := healthStatFor(test, NewHealthTracker().Snapshot(), tuple.source, tuple.capability, "cn")
		if !stat.Registered || !stat.Supported || stat.Observation != "unknown" || stat.CacheSemantics == "" || stat.QPSPolicy == "" {
			test.Fatalf("capability must start unknown, not successful: %+v", stat)
		}
	}
	if _, registered := LookupCapability("sohu", "daily_bars", "cn"); registered {
		test.Fatal("Sohu must not advertise adjusted prices")
	}
	if _, ok := DefaultManager().probeAdapter("sohu").(*SohuAdapter); !ok {
		test.Fatal("default manager lacks optional facts fallback")
	}
}
