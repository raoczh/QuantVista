package datasource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// Keep the public hostname visible to governance, but route every socket locally.
func withLocalSourceHTTP(test *testing.T, handler http.HandlerFunc) *httptest.Server {
	test.Helper()
	server := httptest.NewServer(handler)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		test.Fatal(err)
	}
	transport := &http.Transport{}
	previousClient := httpClient
	client := *previousClient
	client.Transport = reviewTransport(func(request *http.Request) (*http.Response, error) {
		localRequest := request.Clone(request.Context())
		localRequest.URL.Scheme = endpoint.Scheme
		localRequest.URL.Host = endpoint.Host
		return transport.RoundTrip(localRequest)
	})
	httpClient = &client
	test.Cleanup(func() {
		httpClient = previousClient
		transport.CloseIdleConnections()
		server.Close()
	})
	return server
}

func replaceSourceLimiter(test *testing.T, target **httpSourceLimiter, interval time.Duration) *httpSourceLimiter {
	test.Helper()
	previous := *target
	limiter := newHTTPSourceLimiter(1, interval)
	*target = limiter
	test.Cleanup(func() { *target = previous })
	return limiter
}

func TestHTTPSourceWaitKeepsCancellationAndReleasesSlots(test *testing.T) {
	for _, phase := range []string{"slot", "interval", "cooldown"} {
		for _, canceled := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/canceled=%v", phase, canceled), func(test *testing.T) {
				limiter := replaceSourceLimiter(test, &tencentLimiter, 0)
				if phase == "slot" {
					release, err := limiter.acquire(test.Context())
					if err != nil {
						test.Fatal(err)
					}
					defer release()
				} else if phase == "interval" {
					limiter.interval = time.Hour
					release, err := limiter.acquire(test.Context())
					if err != nil {
						test.Fatal(err)
					}
					release()
				} else {
					limiter.deferUntil(time.Now().Add(time.Hour))
				}
				var calls atomic.Int32
				withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
					calls.Add(1)
					fmt.Fprint(writer, `{}`)
				})
				ctx, cancel := context.WithTimeout(test.Context(), 80*time.Millisecond)
				defer cancel()
				wantCause := context.DeadlineExceeded
				if canceled {
					wantCause = context.Canceled
					timer := time.AfterFunc(20*time.Millisecond, cancel)
					defer timer.Stop()
				}
				body, status, err := doGet(ctx, "https://qt.gtimg.cn/quote", nil)
				var localWait *localSourceWaitError
				if !errors.Is(err, wantCause) || !errors.As(err, &localWait) || status != 0 || body != nil || calls.Load() != 0 {
					test.Fatalf("local admission must not become an HTTP failure: status=%d calls=%d err=%v", status, calls.Load(), err)
				}
				wantSlots := 0
				if phase == "slot" {
					wantSlots = 1 // Only the permit deliberately held by this test remains.
				}
				if len(limiter.slots) != wantSlots {
					test.Fatalf("canceled admission leaked permits: got=%d want=%d", len(limiter.slots), wantSlots)
				}
			})
		}
	}
}

func TestHTTPRealAttemptThenThrottleTimeoutIsNotLocalWait(test *testing.T) {
	replaceSourceLimiter(test, &tencentLimiter, time.Hour)
	var calls atomic.Int32
	withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithTimeout(test.Context(), 150*time.Millisecond)
	defer cancel()
	_, status, err := doGetWithPolicy(ctx, "https://qt.gtimg.cn/quote", nil, httpRequestPolicy{retryBackoff: time.Millisecond})
	var localWait *localSourceWaitError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &localWait) || status != http.StatusServiceUnavailable || calls.Load() != 1 {
		test.Fatalf("retry admission must preserve the real attempt: status=%d calls=%d err=%v", status, calls.Load(), err)
	}
}

func TestHTTPImplicitRetryWaitIsNotLocalAdmission(test *testing.T) {
	replaceSourceLimiter(test, &tencentLimiter, time.Hour)
	var calls atomic.Int32
	server := withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/warm" {
			fmt.Fprint(writer, `{}`)
			return
		}
		calls.Add(1)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			test.Errorf("hijack local connection: %v", err)
			return
		}
		_ = connection.Close()
	})
	if _, _, err := doGet(test.Context(), server.URL+"/warm", nil); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(test.Context(), 150*time.Millisecond)
	defer cancel()
	_, _, err := doGetWithPolicy(ctx, "https://qt.gtimg.cn/quote", nil, httpRequestPolicy{retryBackoff: time.Millisecond})
	var localWait *localSourceWaitError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &localWait) || calls.Load() != 1 {
		test.Fatalf("implicit replay waiting must not erase its first request: calls=%d err=%v", calls.Load(), err)
	}
}

func TestHTTPGovernanceDoesNotRequireDataField(test *testing.T) {
	for _, payload := range []string{`{}`, `{"$type":"1"}`, `{"announcements":[]}`, `{"result":{"data":[]}}`, `var hq_str_sh600000="";`} {
		test.Run(payload, func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/html")
				fmt.Fprint(writer, payload)
			}))
			defer server.Close()
			body, status, err := doGet(test.Context(), server.URL, nil)
			if err != nil || status != http.StatusOK || string(body) != payload {
				test.Fatalf("API-specific empty shapes must reach their parser: status=%d body=%s err=%v", status, body, err)
			}
		})
	}
}
