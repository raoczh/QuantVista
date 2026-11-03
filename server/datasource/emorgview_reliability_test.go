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

func resetReportReliabilityClock(test *testing.T) {
	test.Helper()
	repMu.Lock()
	previousLast := repLast
	repLast = time.Time{}
	repMu.Unlock()
	test.Cleanup(func() {
		repMu.Lock()
		repLast = previousLast
		repMu.Unlock()
	})
}

func TestReportReliabilityEveryPhysicalAttemptIsThrottled(test *testing.T) {
	for _, failure := range []string{"503", "connection_closed"} {
		test.Run(failure, func(test *testing.T) {
			const interval = 40 * time.Millisecond
			const expectedAttempts = 3
			var calls atomic.Int32
			permits := make(chan time.Time, 16)
			server := withRepTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/warm" {
					fmt.Fprint(writer, `{}`)
					return
				}
				calls.Add(1)
				parameters := request.URL.Query()
				if parameters.Get("code") != "600519" || parameters.Get("pageNo") != "1" || parameters.Get("pageSize") != "100" ||
					parameters.Get("qType") != "0" || request.Header.Get("Referer") != "https://data.eastmoney.com/report/stock.jshtml" {
					test.Errorf("retry changed report query or headers: %v %v", parameters, request.Header)
				}
				repMu.Lock()
				permit := repLast
				repMu.Unlock()
				select {
				case permits <- permit:
				default:
					test.Error("report request storm exceeded observation buffer")
				}
				if failure == "connection_closed" {
					connection, _, err := writer.(http.Hijacker).Hijack()
					if err != nil {
						test.Errorf("hijack local connection: %v", err)
						return
					}
					_ = connection.Close()
					return
				}
				writer.WriteHeader(http.StatusServiceUnavailable)
			})
			repMinInterval = interval
			resetReportReliabilityClock(test)
			// Force the first EOF onto a reused connection to exercise hidden replay.
			if _, _, err := doGet(test.Context(), server.URL+"/warm", nil); err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(test.Context(), 4*time.Second)
			defer cancel()
			_, err := NewEastMoneyAdapter().GetStockReports(ctx, "600519", 365)
			if !errors.Is(err, ErrUpstream) || calls.Load() != expectedAttempts || len(permits) != expectedAttempts {
				test.Fatalf("report must have exactly three physical attempts: calls=%d permits=%d err=%v", calls.Load(), len(permits), err)
			}
			previous := <-permits
			for attempt := 1; attempt < expectedAttempts; attempt++ {
				current := <-permits
				if gap := current.Sub(previous); gap < interval {
					test.Fatalf("report physical retry %d bypassed its limiter: permit gap=%v", attempt, gap)
				}
				previous = current
			}
		})
	}
}

func TestReportReliabilityCancellationIsPreserved(test *testing.T) {
	ctx, cancel := context.WithCancel(test.Context())
	defer cancel()
	var calls atomic.Int32
	withRepTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		cancel()
		writer.WriteHeader(http.StatusServiceUnavailable)
	})
	resetReportReliabilityClock(test)
	_, err := NewEastMoneyAdapter().GetStockReports(ctx, "600519", 365)
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		test.Fatalf("report cancellation was wrapped away or retried: calls=%d err=%v", calls.Load(), err)
	}
}
