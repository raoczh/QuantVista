package datasource

import (
	"context"
	"errors"
	"testing"
	"time"
)

type rawDailyTestProvider struct {
	probeFakeAdapter
}

func (provider *rawDailyTestProvider) ProvidesForwardAdjustedDailyBars() bool { return false }

func TestManagerRejectsRawDailyBeforeCallingProvider(test *testing.T) {
	rawProvider := &rawDailyTestProvider{probeFakeAdapter{name: "sina", daily: func(context.Context) ([]Bar, error) {
		test.Fatal("unadjusted history must not be requested for adjusted analysis")
		return nil, nil
	}}}
	adjustedProvider := &probeFakeAdapter{name: "tencent", daily: func(context.Context) ([]Bar, error) {
		return []Bar{{TradeDate: "2026-10-30", Open: 10, High: 11, Low: 9, Close: 10}}, nil
	}}
	manager := NewManagerWithAdapters(rawProvider, adjustedProvider)
	bars, err := manager.GetDailyBars(test.Context(), "cn", "600000", 1)
	if err != nil || len(bars) != 1 || bars[0].Source != "tencent" {
		test.Fatalf("adjusted fallback failed: bars=%+v err=%v", bars, err)
	}
}

func TestManagerInternalBudgetPreservesStaleCandidate(test *testing.T) {
	candidate := &Quote{Symbol: "600000", Price: 10, Source: "eastmoney", DataTime: time.Now().Add(-time.Hour)}
	manager := NewManagerWithAdapters(
		&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) { return candidate, nil }},
		&probeFakeAdapter{name: "tencent", quote: func(ctx context.Context) (*Quote, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	)
	manager.totalBudget = 30 * time.Millisecond
	manager.sourceBudget = time.Second
	quote, fresh, err := manager.GetQuoteFresh(test.Context(), "cn", "600000", func(*Quote) bool { return false })
	if err != nil || fresh || quote != candidate {
		test.Fatalf("internal search timeout discarded a known stale quote: %+v %v %v", quote, fresh, err)
	}
}

func TestManagerCallerCancellationDoesNotReturnStaleSuccess(test *testing.T) {
	ctx, cancel := context.WithCancel(test.Context())
	defer cancel()
	manager := NewManagerWithAdapters(
		&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) { return &Quote{Price: 10}, nil }},
		&probeFakeAdapter{name: "tencent", quote: func(context.Context) (*Quote, error) {
			cancel()
			return nil, context.Canceled
		}},
	)
	quote, _, err := manager.GetQuoteFresh(ctx, "cn", "600000", func(*Quote) bool { return false })
	if quote != nil || !errors.Is(err, context.Canceled) {
		test.Fatalf("caller cancellation became a successful quote: %+v %v", quote, err)
	}
}

func TestManagerLongCallerDeadlineDoesNotExtendRouteBudget(test *testing.T) {
	manager := NewManagerWithAdapters(&probeFakeAdapter{name: "tencent", quote: func(ctx context.Context) (*Quote, error) {
		deadline, present := ctx.Deadline()
		if !present || time.Until(deadline) > 100*time.Millisecond {
			test.Error("source inherited the long caller deadline")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	manager.totalBudget = 30 * time.Millisecond
	ctx, cancel := context.WithTimeout(test.Context(), time.Hour)
	defer cancel()
	if _, err := manager.GetQuote(ctx, "cn", "600000"); !errors.Is(err, context.DeadlineExceeded) {
		test.Fatalf("route budget was not enforced: %v", err)
	}
}

type snapshotRouteTestProvider struct {
	probeFakeAdapter
	rows  []SpotRow
	err   error
	calls int
}

func (provider *snapshotRouteTestProvider) GetCNSpotSnapshot(context.Context) ([]SpotRow, error) {
	provider.calls++
	return provider.rows, provider.err
}

func TestManagerSnapshotFallbackPreservesSourceAndBlocksMassProbe(test *testing.T) {
	primary := &snapshotRouteTestProvider{probeFakeAdapter: probeFakeAdapter{name: "eastmoney"}, err: ErrUpstream}
	backup := &snapshotRouteTestProvider{probeFakeAdapter: probeFakeAdapter{name: "sina"}, rows: []SpotRow{{Symbol: "600000", Source: "sina_spot"}}}
	manager := NewManagerWithAdapters(primary, backup)
	rows, err := manager.GetCNSpotSnapshot(test.Context())
	if err != nil || len(rows) != 1 || rows[0].Source != "sina_spot" || backup.calls != 1 {
		test.Fatalf("snapshot fallback lost its provenance: %+v %v", rows, err)
	}
	result, err := manager.Probe(test.Context(), "sina", "spot_snapshot", "cn")
	if !errors.Is(err, ErrProbeNotAllowed) || result.Code != "USE_SYNC_JOB" || backup.calls != 1 {
		test.Fatalf("a sample probe must not trigger a market scan: %+v %v calls=%d", result, err, backup.calls)
	}
}

func TestEastMoneyDailyValidationRejectsIncompleteSeries(test *testing.T) {
	valid := "2026-10-30,10,10.5,11,9,100,105000,2,1,0.1,0.5"
	for _, invalid := range []string{
		"truncated", "2026-10-31,NaN,10,11,9,100,10000", "2026-10-31,10,12,11,9,100,10000",
		"2026-13-31,10,10,11,9,100,10000", "2026-10-31,10,10,11,9,bad,10000",
	} {
		if bars, err := parseEMKlinesChecked([]string{valid, invalid}); !errors.Is(err, ErrUpstream) || len(bars) != 0 {
			test.Errorf("bad row bypassed source fallback: %q bars=%+v err=%v", invalid, bars, err)
		}
	}
	if bars, err := parseEMKlinesChecked([]string{valid}); err != nil || len(bars) != 1 || bars[0].Source != "eastmoney" {
		test.Fatalf("valid daily history rejected: %+v %v", bars, err)
	}
}

func TestEastMoneyUsesCanonicalHostsWithoutNodeCycling(test *testing.T) {
	adapter, hosts := newTestEM(func(string) ([]byte, int, error) { return []byte(`{"rc":0,"data":{}}`), 200, nil })
	for _, endpoint := range []string{
		"https://2.push2his.eastmoney.com/api/qt/stock/kline/get",
		"https://99.push2.eastmoney.com/api/qt/clist/get",
	} {
		if _, _, err := adapter.get(test.Context(), endpoint, nil); err != nil {
			test.Fatal(err)
		}
	}
	if len(*hosts) != 2 || (*hosts)[0] != "push2his.eastmoney.com" || (*hosts)[1] != "push2.eastmoney.com" {
		test.Fatalf("numbered hosts were still treated as an independent healthy pool: %v", *hosts)
	}
}
