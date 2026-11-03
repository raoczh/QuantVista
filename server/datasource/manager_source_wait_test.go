package datasource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func sourceWaitTestAdapter(test *testing.T, source string) (Adapter, *httpSourceLimiter) {
	test.Helper()
	switch source {
	case "eastmoney":
		return NewEastMoneyAdapter(), replaceSourceLimiter(test, &emQuoteLimiter, 0)
	case "tencent":
		return NewTencentAdapter(), replaceSourceLimiter(test, &tencentLimiter, 0)
	case "sina":
		return NewSinaAdapter(), replaceSourceLimiter(test, &sinaLimiter, 0)
	default:
		test.Fatalf("unknown test source %q", source)
		return nil, nil
	}
}

func TestManagerLocalSourceWaitPreservesHealthAndFallback(test *testing.T) {
	for _, source := range []string{"eastmoney", "tencent", "sina"} {
		test.Run(source, func(test *testing.T) {
			adapter, limiter := sourceWaitTestAdapter(test, source)
			limiter.deferUntil(time.Now().Add(time.Hour))
			var calls atomic.Int32
			withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.WriteHeader(http.StatusServiceUnavailable)
			})
			manager := NewManagerWithAdapters(adapter)
			manager.totalBudget = 2 * time.Second
			manager.sourceBudget = 50 * time.Millisecond
			manager.health.RecordForMarket(source, "quote", "cn", outcomeError, 1)
			quote, err := manager.GetQuote(test.Context(), "cn", "600000")
			var localWait *localSourceWaitError
			if quote != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &localWait) || calls.Load() != 0 {
				test.Fatalf("wrapped adapter error lost pure admission cause: quote=%+v calls=%d err=%v", quote, calls.Load(), err)
			}
			row := probeHealthRow(test, manager, source, "quote", "cn")
			if row.Samples != 1 || row.Errors != 1 || row.Success != 0 || row.Empty != 0 {
				test.Fatalf("local wait changed existing health history: %+v", row)
			}
			manager.adapters = append(manager.adapters, &probeFakeAdapter{name: "fallback", quote: func(context.Context) (*Quote, error) {
				return &Quote{Symbol: "600000", Price: 10, Source: "fallback"}, nil
			}})
			quote, err = manager.GetQuote(test.Context(), "cn", "600000")
			row = probeHealthRow(test, manager, source, "quote", "cn")
			backup := probeHealthRow(test, manager, "fallback", "quote", "cn")
			if err != nil || quote == nil || quote.Source != "fallback" || row.Samples != 1 || row.Errors != 1 || backup.Success != 1 || calls.Load() != 0 {
				test.Fatalf("local wait blocked fallback or polluted health: quote=%+v primary=%+v backup=%+v calls=%d err=%v", quote, row, backup, calls.Load(), err)
			}
		})
	}
}

func TestManagerRealAttemptThenWaitStillRecordsFailure(test *testing.T) {
	for _, phase := range []string{"retry", "next_page"} {
		test.Run(phase, func(test *testing.T) {
			replaceSourceLimiter(test, &tencentLimiter, time.Hour)
			var calls atomic.Int32
			withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if phase == "retry" {
					writer.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fmt.Fprint(writer, `{}`)
			})
			adapter := &probeFakeAdapter{name: "tencent", quote: func(ctx context.Context) (*Quote, error) {
				_, _, err := doGetWithPolicy(ctx, "https://qt.gtimg.cn/first", nil, httpRequestPolicy{retryBackoff: time.Millisecond})
				if err == nil && phase == "next_page" {
					_, _, err = doGet(ctx, "https://qt.gtimg.cn/second", nil)
				}
				return nil, err
			}}
			manager := NewManagerWithAdapters(adapter)
			manager.totalBudget = 2 * time.Second
			manager.sourceBudget = 150 * time.Millisecond
			_, err := manager.GetQuote(test.Context(), "cn", "600000")
			var localWait *localSourceWaitError
			row := probeHealthRow(test, manager, "tencent", "quote", "cn")
			if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &localWait) || calls.Load() != 1 || row.Errors != 1 || row.Samples != 1 {
				test.Fatalf("real HTTP attempt was misclassified as pure queueing: calls=%d health=%+v err=%v", calls.Load(), row, err)
			}
		})
	}
}
