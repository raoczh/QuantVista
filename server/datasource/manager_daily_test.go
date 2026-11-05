package datasource

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDailyRouteTriesNewerBackupAfterNonemptyStalePrimary(test *testing.T) {
	primary := &probeFakeAdapter{name: "eastmoney", daily: func(context.Context) ([]Bar, error) {
		return []Bar{{TradeDate: "2026-09-01", Close: 10}}, nil
	}}
	backup := &probeFakeAdapter{name: "tencent", daily: func(context.Context) ([]Bar, error) {
		return []Bar{{TradeDate: "2026-09-02", Close: 11}}, nil
	}}
	manager := NewManagerWithAdapters(primary, backup)
	rows, fresh, err := manager.GetDailyBarsFresh(test.Context(), "cn", "600000", 5, "2026-09-02")
	if err != nil || !fresh || len(rows) != 1 || rows[0].Source != "tencent" || rows[0].Close != 11 {
		test.Fatalf("stale primary blocked newer adjusted history: %+v fresh=%v err=%v", rows, fresh, err)
	}
	rows, fresh, err = manager.GetDailyBarsFresh(test.Context(), "cn", "600000", 5, "2026-09-03")
	if err != nil || fresh || rows[0].Source != "tencent" || rows[0].TradeDate != "2026-09-02" {
		test.Fatalf("all-stale route must retain actual newest date: %+v fresh=%v err=%v", rows, fresh, err)
	}
}

func TestDailyRouteRetainsOldSeriesOnBudgetButNotCallerCancellation(test *testing.T) {
	primary := &probeFakeAdapter{name: "eastmoney", daily: func(context.Context) ([]Bar, error) {
		return []Bar{{TradeDate: "2026-09-01", Close: 10}}, nil
	}}
	manager := NewManagerWithAdapters(primary, &probeFakeAdapter{name: "tencent", daily: func(ctx context.Context) ([]Bar, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	manager.totalBudget = 15 * time.Millisecond
	manager.sourceBudget = time.Second
	rows, fresh, err := manager.GetDailyBarsFresh(test.Context(), "cn", "600000", 5, "2026-09-02")
	if err != nil || fresh || len(rows) != 1 {
		test.Fatalf("bounded search discarded known historical data: %+v %v %v", rows, fresh, err)
	}
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	rows, _, err = manager.GetDailyBarsFresh(ctx, "cn", "600000", 5, "2026-09-02")
	if !errors.Is(err, context.Canceled) || rows != nil {
		test.Fatalf("caller cancellation was ignored: %+v %v", rows, err)
	}
}

func TestDailyRouteTriesCompleteBackupAfterCurrentShortPrimary(test *testing.T) {
	const latestDate = "2026-09-04"
	primary := &probeFakeAdapter{name: "eastmoney", daily: func(context.Context) ([]Bar, error) {
		return []Bar{{TradeDate: latestDate, Close: 10}}, nil
	}}
	backup := &probeFakeAdapter{name: "tencent", daily: func(context.Context) ([]Bar, error) {
		return []Bar{
			{TradeDate: "2026-09-02", Close: 8},
			{TradeDate: "2026-09-03", Close: 9},
			{TradeDate: latestDate, Close: 10},
		}, nil
	}}
	manager := NewManagerWithAdapters(primary, backup)
	rows, fresh, err := manager.GetDailyBarsFresh(test.Context(), "cn", "600000", 3, latestDate)
	if err != nil || !fresh || len(rows) != 3 || rows[0].Source != "tencent" {
		test.Fatalf("a current date concealed truncated history: rows=%+v fresh=%v err=%v", rows, fresh, err)
	}
}

func TestDailyRoutePreservesRealShortHistoryWhenBackupsFail(test *testing.T) {
	primary := &probeFakeAdapter{name: "eastmoney", daily: func(context.Context) ([]Bar, error) {
		return []Bar{{TradeDate: "2026-09-04", Close: 10}}, nil
	}}
	backup := &probeFakeAdapter{name: "tencent", daily: func(context.Context) ([]Bar, error) {
		return nil, ErrUpstream
	}}
	rows, fresh, err := NewManagerWithAdapters(primary, backup).GetDailyBarsFresh(test.Context(), "cn", "600000", 250, "2026-09-04")
	if err != nil || !fresh || len(rows) != 1 || rows[0].Source != "eastmoney" {
		test.Fatalf("genuine short history should retain its actual date and count: %+v %v %v", rows, fresh, err)
	}
}
