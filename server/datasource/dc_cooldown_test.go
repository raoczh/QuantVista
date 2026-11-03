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

func resetDCCooldown(test *testing.T) {
	test.Helper()
	resetDCReliabilityClock(test)
	dcMu.Lock()
	previousCooldown := dcCooldownUntil
	dcCooldownUntil = time.Time{}
	dcMu.Unlock()
	test.Cleanup(func() {
		dcMu.Lock()
		dcCooldownUntil = previousCooldown
		dcMu.Unlock()
	})
}

func TestDCCooldownSharedAcrossFinanceAndQueries(test *testing.T) {
	for _, firstSource := range []string{"datacenter", "finance"} {
		for _, retryAfterAttempt := range []int32{1, 3} {
			test.Run(fmt.Sprintf("%s/header-on-%d", firstSource, retryAfterAttempt), func(test *testing.T) {
				var calls atomic.Int32
				handler := func(writer http.ResponseWriter, request *http.Request) {
					if calls.Add(1) == retryAfterAttempt {
						writer.Header().Set("Retry-After", "60")
						writer.WriteHeader(http.StatusTooManyRequests)
						return
					}
					writer.WriteHeader(http.StatusServiceUnavailable)
				}
				withDcTestServer(test, handler, 0)
				financeServer := httptest.NewServer(http.HandlerFunc(handler))
				defer financeServer.Close()
				resetDCCooldown(test)
				query := func(ctx context.Context) error {
					iterator := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
					_, err := iterator.Next(ctx)
					return err
				}
				finance := func(ctx context.Context) error {
					_, err := finGet(ctx, financeServer.URL)
					return err
				}
				first, second := query, finance
				if firstSource == "finance" {
					first, second = finance, query
				}
				firstContext, cancelFirst := context.WithTimeout(test.Context(), 2*time.Second)
				defer cancelFirst()
				err := first(firstContext)
				var statusFailure *httpStatusError
				if !errors.As(err, &statusFailure) || statusFailure.statusCode != http.StatusTooManyRequests || calls.Load() != retryAfterAttempt || firstContext.Err() != nil {
					test.Fatalf("Retry-After must retain final status and stop within budget: calls=%d err=%v", calls.Load(), err)
				}
				dcMu.Lock()
				cooldown := dcCooldownUntil
				dcMu.Unlock()
				if cooldown.Before(statusFailure.retryAt) {
					test.Fatalf("shared cooldown shortened Retry-After: got=%v want>=%v", cooldown, statusFailure.retryAt)
				}
				// A later, shorter header must not reduce an existing family cooldown.
				dcDeferUntil(time.Now().Add(time.Second))
				secondContext, cancelSecond := context.WithTimeout(test.Context(), 50*time.Millisecond)
				defer cancelSecond()
				err = second(secondContext)
				var localWait *localSourceWaitError
				if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &localWait) || calls.Load() != retryAfterAttempt {
					test.Fatalf("another DC/F10 endpoint bypassed shared cooldown: calls=%d err=%v", calls.Load(), err)
				}
			})
		}
	}
}

func TestDCCooldownDoesNotTurnConcurrentReleaseIntoBurst(test *testing.T) {
	const interval = 40 * time.Millisecond
	const requests = 4
	permits := make(chan time.Time, requests)
	server := withDcTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/warm" {
			fmt.Fprint(writer, `{}`)
			return
		}
		dcMu.Lock()
		permit := dcLast
		dcMu.Unlock()
		permits <- permit
		fmt.Fprint(writer, dcPage(1, `{"VALUE":1}`))
	}, interval)
	resetDCCooldown(test)
	if _, _, err := doGet(test.Context(), server.URL+"/warm", nil); err != nil {
		test.Fatal(err)
	}
	cooldown := time.Now().Add(80 * time.Millisecond)
	dcDeferUntil(cooldown)
	ctx, cancel := context.WithTimeout(test.Context(), 3*time.Second)
	defer cancel()
	results := make(chan error, requests)
	for request := 0; request < requests; request++ {
		go func(request int) {
			if request%2 == 0 {
				_, err := finGet(ctx, server.URL)
				results <- err
				return
			}
			iterator := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
			_, err := iterator.Next(ctx)
			results <- err
		}(request)
	}
	for request := 0; request < requests; request++ {
		if err := <-results; err != nil {
			test.Fatalf("request after cooldown failed: %v", err)
		}
	}
	if len(permits) != requests {
		test.Fatalf("unexpected physical request count: %d", len(permits))
	}
	previous := <-permits
	if previous.Before(cooldown) {
		test.Fatalf("request bypassed cooldown: permit=%v cooldown=%v", previous, cooldown)
	}
	for request := 1; request < requests; request++ {
		current := <-permits
		if gap := current.Sub(previous); gap < interval {
			test.Fatalf("shared cooldown released concurrent requests in a burst: gap=%v", gap)
		}
		previous = current
	}
}

func TestDCCooldownIgnoresNonRetryableStatus(test *testing.T) {
	var calls atomic.Int32
	server := withDcTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Retry-After", "60")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprint(writer, dcPage(1, `{"VALUE":1}`))
	}, 0)
	resetDCCooldown(test)
	iterator := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
	_, err := iterator.Next(test.Context())
	var statusFailure *httpStatusError
	if !errors.As(err, &statusFailure) || statusFailure.statusCode != http.StatusBadRequest || calls.Load() != 1 {
		test.Fatalf("nonretryable response changed: calls=%d err=%v", calls.Load(), err)
	}
	ctx, cancel := context.WithTimeout(test.Context(), time.Second)
	defer cancel()
	if _, err := finGet(ctx, server.URL); err != nil || calls.Load() != 2 {
		test.Fatalf("business rejection must not cool unrelated queries: calls=%d err=%v", calls.Load(), err)
	}
}
