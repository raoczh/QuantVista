package datasource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerReliabilityServerFailuresTrip(test *testing.T) {
	for _, status := range []int{500, 503, 504} {
		test.Run(fmt.Sprint(status), func(test *testing.T) {
			adapter, hosts := newTestEM(func(string) ([]byte, int, error) { return nil, status, nil })
			for attempt := 0; attempt < emBreakThreshold; attempt++ {
				if _, _, err := adapter.get(context.Background(), "https://push2his.eastmoney.com/api", nil); !errors.Is(err, ErrUpstream) {
					test.Fatalf("HTTP failure must not be treated as success: %v", err)
				}
			}
			_, _, _ = adapter.get(context.Background(), "https://push2his.eastmoney.com/api", nil)
			if adapter.br.allow("push2his") || len(*hosts) != emBreakThreshold {
				test.Fatalf("server failures did not trip the breaker: calls=%d", len(*hosts))
			}
		})
	}
}

func TestBreakerReliabilityBackupMustActuallySucceed(test *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{
		{503, `{}`}, {504, `{}`}, {404, `{}`}, {501, `{}`},
		{200, `<html>challenge</html>`}, {200, `{"rc":1,"data":null}`},
		{200, `{"success":false,"message":"denied"}`},
	} {
		test.Run(fmt.Sprintf("%d/%s", response.status, response.body), func(test *testing.T) {
			adapter, hosts := newTestEM(func(rawURL string) ([]byte, int, error) {
				if strings.Contains(rawURL, emBackupHost) {
					return []byte(response.body), response.status, nil
				}
				return nil, http.StatusServiceUnavailable, nil
			})
			_, status, err := adapter.get(context.Background(), "https://push2.eastmoney.com/api", nil)
			if !errors.Is(err, ErrUpstream) || status != response.status || len(*hosts) != 2 {
				test.Fatalf("backup failure was hidden: status=%d calls=%d err=%v", status, len(*hosts), err)
			}
			if adapter.br.isDegraded("push2") || adapter.br.streak["push2"] != 1 {
				test.Fatal("failed backup must not mark recovery or reset the failure streak")
			}
		})
	}
}

func TestBreakerReliabilityDegradedRouteRecoversHTTP(test *testing.T) {
	var primaryHealthy atomic.Bool
	var primaryCalls, backupCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("upstream") == emBackupHost {
			backupCalls.Add(1)
			fmt.Fprint(writer, `{"data":{"source":"backup"}}`)
			return
		}
		primaryCalls.Add(1)
		if !primaryHealthy.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(writer, `{"data":{"source":"primary"}}`)
	}))
	defer server.Close()
	adapter := NewEastMoneyAdapter()
	adapter.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
		upstream, err := url.Parse(rawURL)
		if err != nil {
			return nil, 0, err
		}
		return doGetWithPolicy(ctx, server.URL+"?upstream="+url.QueryEscape(upstream.Hostname()), headers,
			httpRequestPolicy{retryBackoff: time.Millisecond})
	}
	now := time.Now()
	adapter.br.now = func() time.Time { return now }
	const primaryURL = "https://33.push2.eastmoney.com/api"
	if _, _, err := adapter.get(context.Background(), primaryURL, nil); err != nil || !adapter.br.isDegraded("push2") {
		test.Fatalf("initial degradation failed: %v", err)
	}
	now = now.Add(emBreakCooldown / 2)
	if _, _, err := adapter.get(context.Background(), primaryURL, nil); err != nil {
		test.Fatal(err)
	}
	primaryHealthy.Store(true)
	now = now.Add(emBreakCooldown / 2)
	for request := 0; request < 2; request++ {
		body, _, err := adapter.get(context.Background(), primaryURL, nil)
		if err != nil || !strings.Contains(string(body), "primary") || adapter.br.isDegraded("push2") {
			test.Fatalf("cooldown must recover the primary at its exact deadline: body=%s err=%v", body, err)
		}
	}
	if primaryCalls.Load() != httpMaxAttempts+2 || backupCalls.Load() != 2 {
		test.Fatalf("backup successes extended cooldown or primary recovery failed: primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestBreakerReliabilityOnlyOneRecoveryProbe(test *testing.T) {
	for _, degraded := range []bool{true, false} {
		test.Run(fmt.Sprint(degraded), func(test *testing.T) {
			breaker := newEmBreaker()
			now := time.Now()
			breaker.now = func() time.Time { return now }
			if degraded {
				breaker.degrade("push2")
			} else {
				for failure := 0; failure < emBreakThreshold; failure++ {
					breaker.fail("push2")
				}
			}
			now = now.Add(emBreakCooldown)
			const concurrentRequests = 12
			results := make(chan emRequestRoute, concurrentRequests)
			for request := 0; request < concurrentRequests; request++ {
				go func() {
					route, allowed := breaker.begin("push2", true)
					if !allowed {
						results <- emRequestRoute{}
						return
					}
					results <- route
				}()
			}
			probes, backups := 0, 0
			for request := 0; request < concurrentRequests; request++ {
				route := <-results
				if route.probe {
					probes++
				}
				if route.backup {
					backups++
				}
			}
			if probes != 1 || (degraded && backups != concurrentRequests-1) || (!degraded && backups != 0) {
				test.Fatalf("recovery stampede: probes=%d backups=%d", probes, backups)
			}
			breaker.complete("push2", emRequestRoute{probe: true}, emRequestCanceled)
			if route, allowed := breaker.begin("push2", true); !allowed || !route.probe {
				test.Fatal("canceled recovery probe left the breaker permanently stuck")
			}
		})
	}
}

func TestBreakerReliabilityCanceledRequestsPreserveStreak(test *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		test.Run(cause.Error(), func(test *testing.T) {
			adapter, hosts := newTestEM(func(string) ([]byte, int, error) {
				return nil, 0, fmt.Errorf("wrapped cancellation: %w", cause)
			})
			adapter.br.streak["push2"] = emBreakThreshold - 1
			_, _, err := adapter.get(context.Background(), "https://push2.eastmoney.com/api", nil)
			if !errors.Is(err, cause) || len(*hosts) != 1 || adapter.br.streak["push2"] != emBreakThreshold-1 || !adapter.br.allow("push2") {
				test.Fatalf("cancel changed breaker health or triggered fallback: calls=%d err=%v", len(*hosts), err)
			}
		})
	}
	clientTimeout := &url.Error{Op: "Get", URL: "https://push2his.eastmoney.com/api", Err: context.DeadlineExceeded}
	if !emRateLimited(context.Background(), clientTimeout, 0) {
		test.Fatal("client timeout with a live caller must remain an upstream failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	adapter, _ := newTestEM(func(rawURL string) ([]byte, int, error) {
		if strings.Contains(rawURL, emBackupHost) {
			cancel()
			return nil, 0, context.Canceled
		}
		return nil, 0, io.EOF
	})
	defer cancel()
	adapter.br.streak["push2"] = emBreakThreshold - 1
	_, _, err := adapter.get(ctx, "https://push2.eastmoney.com/api", nil)
	if !errors.Is(err, context.Canceled) || adapter.br.streak["push2"] != emBreakThreshold-1 {
		test.Fatal("cancellation during backup must not count the partially completed call")
	}
}

func TestBreakerReliabilityRetryAfterDoesNotSwitchHosts(test *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	adapter := NewEastMoneyAdapter()
	adapter.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
		return doGet(ctx, server.URL, headers)
	}
	_, status, err := adapter.get(context.Background(), "https://push2.eastmoney.com/api", nil)
	if status != http.StatusTooManyRequests || !errors.Is(err, ErrUpstream) || calls.Load() != 1 || adapter.br.isDegraded("push2") {
		test.Fatalf("Retry-After must not be bypassed using the backup host: status=%d calls=%d err=%v", status, calls.Load(), err)
	}
}

func TestBreakerReliabilityEmptyPoolDoesNotMeanRecovery(test *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		fmt.Fprint(writer, `{"rc":102,"data":null}`)
	}))
	defer server.Close()
	adapter := NewEastMoneyAdapter()
	adapter.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
		return doGet(ctx, server.URL, headers)
	}
	adapter.br.streak["push2ex"] = emBreakThreshold - 1
	_, err := adapter.GetZBPool(context.Background(), "20261102")
	if !errors.Is(err, ErrNoData) || calls.Load() != 1 || adapter.br.streak["push2ex"] != emBreakThreshold-1 {
		test.Fatalf("empty pool must preserve no-data semantics without resetting health: calls=%d err=%v", calls.Load(), err)
	}
}
