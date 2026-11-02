package datasource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func resetDCReliabilityClock(test *testing.T) {
	test.Helper()
	dcMu.Lock()
	previousLast := dcLast
	dcLast = time.Time{}
	dcMu.Unlock()
	test.Cleanup(func() {
		dcMu.Lock()
		dcLast = previousLast
		dcMu.Unlock()
	})
}

func TestDCReliabilityEveryPhysicalRetryIsThrottled(test *testing.T) {
	for _, source := range []string{"datacenter", "finance"} {
		for _, failure := range []string{"503", "connection_closed"} {
			test.Run(source+"/"+failure, func(test *testing.T) {
				const interval = 40 * time.Millisecond
				var calls atomic.Int32
				permits := make(chan time.Time, 20)
				server := withDcTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/warm" {
						fmt.Fprint(writer, `{}`)
						return
					}
					calls.Add(1)
					dcMu.Lock()
					permitTime := dcLast
					dcMu.Unlock()
					permits <- permitTime
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
				}, interval)
				resetDCReliabilityClock(test)
				// Reuse a live connection so the first EOF also exercises Transport's
				// implicit retry, not just the explicit retry loop.
				if _, _, err := doGet(context.Background(), server.URL+"/warm", nil); err != nil {
					test.Fatal(err)
				}
				var err error
				if source == "finance" {
					_, err = finGet(context.Background(), server.URL)
				} else {
					iterator := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
					_, err = iterator.Next(context.Background())
				}
				if !errors.Is(err, ErrUpstream) || calls.Load() != httpMaxAttempts {
					test.Fatalf("physical attempts must be bounded at %d: calls=%d err=%v", httpMaxAttempts, calls.Load(), err)
				}
				previous := <-permits
				for attempt := 1; attempt < httpMaxAttempts; attempt++ {
					current := <-permits
					if gap := current.Sub(previous); gap < interval {
						test.Fatalf("physical retry %d bypassed dcThrottle: permit gap=%v", attempt, gap)
					}
					previous = current
				}
			})
		}
	}
}

func TestDCReliabilityFinanceAndReportsShareThrottle(test *testing.T) {
	const interval = 20 * time.Millisecond
	permits := make(chan time.Time, 20)
	server := withDcTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		dcMu.Lock()
		permitTime := dcLast
		dcMu.Unlock()
		permits <- permitTime
		writer.WriteHeader(http.StatusServiceUnavailable)
	}, interval)
	resetDCReliabilityClock(test)
	results := make(chan error, 2)
	go func() {
		iterator := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
		_, err := iterator.Next(context.Background())
		results <- err
	}()
	go func() {
		_, err := finGet(context.Background(), server.URL)
		results <- err
	}()
	for request := 0; request < 2; request++ {
		if err := <-results; !errors.Is(err, ErrUpstream) {
			test.Fatalf("expected failed upstream response: %v", err)
		}
	}
	if len(permits) != 2*httpMaxAttempts {
		test.Fatalf("nested retries inflated request count: %d", len(permits))
	}
	previous := <-permits
	for attempt := 1; attempt < 2*httpMaxAttempts; attempt++ {
		current := <-permits
		if current.Sub(previous) < interval {
			test.Fatalf("F10 and datacenter did not share permits: %v", current.Sub(previous))
		}
		previous = current
	}
}

func TestDCReliabilityCancellationIsPreserved(test *testing.T) {
	for _, source := range []string{"datacenter", "finance"} {
		test.Run(source, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			server := withDcTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				cancel()
				writer.WriteHeader(http.StatusServiceUnavailable)
			}, time.Millisecond)
			var err error
			if source == "finance" {
				_, err = finGet(ctx, server.URL)
			} else {
				iterator := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
				_, err = iterator.Next(ctx)
			}
			if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
				test.Fatalf("cancellation must not be wrapped away or retried: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestF10ReliabilityBusinessErrorsAreNotEmptyData(test *testing.T) {
	previousInterval := dcMinInterval
	dcMinInterval = 0
	test.Cleanup(func() { dcMinInterval = previousInterval })
	for _, payload := range []string{
		`{"success":false,"code":500,"result":null}`,
		`{"code":500,"result":null}`,
		`<html>verification required</html>`,
		`{"data":`,
	} {
		test.Run(payload, func(test *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				fmt.Fprint(writer, payload)
			}))
			defer server.Close()
			_, err := finGet(context.Background(), server.URL)
			if !errors.Is(err, ErrUpstream) || errors.Is(err, ErrNoData) || calls.Load() != 1 {
				test.Fatalf("business failure must not become no-data success: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestDCReliabilityNoDataOnTailEndsPaging(test *testing.T) {
	var calls atomic.Int32
	withDcTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(writer, dcPage(2, `{"VALUE":1}`))
			return
		}
		fmt.Fprint(writer, `{"success":false,"code":9201,"result":null}`)
	}, time.Millisecond)
	iterator := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
	if rows, err := iterator.Next(context.Background()); err != nil || len(rows) != 1 {
		test.Fatalf("first page failed: rows=%v err=%v", rows, err)
	}
	for repeat := 0; repeat < 2; repeat++ {
		if rows, err := iterator.Next(context.Background()); err != nil || rows != nil {
			test.Fatalf("tail no-data should end cleanly: rows=%v err=%v", rows, err)
		}
	}
	if calls.Load() != 2 {
		test.Fatalf("tail response was retried: %d", calls.Load())
	}
}
