package datasource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPReliabilityTransportDefaults(t *testing.T) {
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil ||
		reflect.ValueOf(transport.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("shared transport must honor the environment proxy policy")
	}
	if transport.TLSHandshakeTimeout <= 0 || transport.ResponseHeaderTimeout <= 0 || httpClient.Timeout <= 0 {
		t.Fatal("TLS, response headers and whole attempts must have timeouts")
	}
	if transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 {
		t.Fatal("public sources must continue to use HTTP/1.1")
	}
}

func TestHTTPReliabilityRetryAfter(t *testing.T) {
	var calls atomic.Int32
	attemptTimes := make(chan time.Time, httpMaxAttempts)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attemptTimes <- time.Now()
		if calls.Add(1) == 1 {
			writer.Header().Set("Retry-After", "1")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(writer, `{"ok":true}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, status, err := doGet(ctx, server.URL, nil)
	if err != nil || status != http.StatusOK || string(body) != `{"ok":true}` || calls.Load() != 2 {
		t.Fatalf("expected a successful, bounded 429 retry: body=%s status=%d calls=%d err=%v", body, status, calls.Load(), err)
	}
	first, second := <-attemptTimes, <-attemptTimes
	if gap := second.Sub(first); gap < time.Second {
		t.Fatalf("Retry-After was shortened: %v", gap)
	}
}

func TestHTTPReliabilityRetryAfterExceedsBudget(t *testing.T) {
	for _, retryAfter := range []string{"60", "999999999999999999999999999999", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(retryAfter, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Retry-After", retryAfter)
				writer.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, status, err := doGet(ctx, server.URL, nil)
			if status != http.StatusTooManyRequests || !errors.Is(err, ErrUpstream) || calls.Load() != 1 {
				t.Fatalf("long Retry-After must stop without another request: status=%d calls=%d err=%v", status, calls.Load(), err)
			}
			if ctx.Err() != nil {
				t.Fatal("a wait outside the budget should fail immediately, not consume the caller deadline")
			}
		})
	}
}

func TestHTTPReliabilityRetryAfterDate(t *testing.T) {
	now := time.Date(2026, time.November, 2, 8, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		value string
		delay time.Duration
		valid bool
	}{
		{"3", 3 * time.Second, true},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"-1", 0, false},
		{"invalid", 0, false},
	} {
		delay, valid := parseHTTPRetryAfter(testCase.value, now)
		if delay != testCase.delay || valid != testCase.valid {
			t.Errorf("Retry-After %q: got (%v,%v), want (%v,%v)", testCase.value, delay, valid, testCase.delay, testCase.valid)
		}
	}
}

func TestHTTPReliabilityStatusRetryBounds(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504, 400, 403, 404, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.WriteHeader(status)
			}))
			defer server.Close()
			_, actualStatus, err := doGetWithPolicy(context.Background(), server.URL, nil,
				httpRequestPolicy{retryBackoff: time.Millisecond})
			wantCalls := int32(1)
			if status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504 {
				wantCalls = 3
			}
			if actualStatus != status || !errors.Is(err, ErrUpstream) || calls.Load() != wantCalls {
				t.Fatalf("status=%d calls=%d err=%v; want status=%d calls=%d and failure", actualStatus, calls.Load(), err, status, wantCalls)
			}
		})
	}
}

func TestHTTPReliabilityCancellation(t *testing.T) {
	for _, phase := range []string{"before", "retry", "body"} {
		t.Run(phase, func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{}, 3)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if phase == "body" {
					writer.WriteHeader(http.StatusOK)
					writer.(http.Flusher).Flush()
					started <- struct{}{}
					<-request.Context().Done()
					return
				}
				writer.WriteHeader(http.StatusServiceUnavailable)
				started <- struct{}{}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if phase == "before" {
				cancel()
			}
			finished := make(chan error, 1)
			go func() { _, _, err := doGet(ctx, server.URL, nil); finished <- err }()
			if phase != "before" {
				select {
				case <-started:
					cancel()
				case <-ctx.Done():
					t.Fatal("local request did not start")
				}
			}
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("caller cancellation was lost: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not interrupt the request")
			}
			wantCalls := int32(1)
			if phase == "before" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("canceled request made %d calls, want %d", calls.Load(), wantCalls)
			}
		})
	}
}

func TestHTTPReliabilityResponseLimit(t *testing.T) {
	for _, size := range []int{maxHTTPResponseBytes, maxHTTPResponseBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), size)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.(http.Flusher).Flush() // Chunked body: no Content-Length shortcut.
				_, _ = writer.Write(payload)
			}))
			defer server.Close()
			body, status, err := doGet(context.Background(), server.URL, nil)
			if status != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("unexpected status or retry: status=%d calls=%d", status, calls.Load())
			}
			if size == maxHTTPResponseBytes {
				if err != nil || len(body) != size {
					t.Fatalf("exactly 4 MiB must remain valid: len=%d err=%v", len(body), err)
				}
			} else if !errors.Is(err, ErrUpstream) || body != nil {
				t.Fatalf("oversized bodies must not be silently truncated: len=%d err=%v", len(body), err)
			}
		})
	}
}

func TestHTTPReliabilityRejectsFalseSuccess(t *testing.T) {
	for _, payload := range []string{
		" \xef\xbb\xbf <!DOCTYPE html><html>verification required</html>",
		`{"success":false,"code":500,"data":[{"value":1}]}`,
		`{"rc":1,"data":{"price":10}}`,
		`{"errno":2,"data":{}}`,
		`{"data":`,
	} {
		t.Run(payload, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				fmt.Fprint(writer, payload)
			}))
			defer server.Close()
			_, status, err := doGet(context.Background(), server.URL, nil)
			if status != http.StatusOK || !errors.Is(err, ErrUpstream) || calls.Load() != 1 {
				t.Fatalf("HTTP 200 is not a successful API response: status=%d calls=%d err=%v", status, calls.Load(), err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(writer, `{"data":[]}`)
	}))
	defer server.Close()
	if _, _, err := doGet(context.Background(), server.URL, nil); err != nil {
		t.Fatalf("JSON mislabeled text/html must still work: %v", err)
	}
}

func TestHTTPReliabilityDoesNotFollowChallengeRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		http.Redirect(writer, request, "/challenge", http.StatusFound)
	}))
	defer server.Close()
	_, status, err := doGet(context.Background(), server.URL, nil)
	if status != http.StatusFound || !errors.Is(err, ErrUpstream) || calls.Load() != 1 {
		t.Fatalf("redirects must not bypass governance: status=%d calls=%d err=%v", status, calls.Load(), err)
	}
}

func TestHTTPReliabilityReadOnlyPostReplaysForm(t *testing.T) {
	values := url.Values{"searchkey": {"a+b & c"}, "column": {"first", "second"}}
	headers := map[string]string{"Referer": "https://www.cninfo.com.cn/", "X-Query-Tag": "readonly"}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
			request.Header.Get("User-Agent") != browserUA || request.Header.Get("X-Query-Tag") != "readonly" {
			t.Errorf("POST method or default/custom headers changed: %s %v", request.Method, request.Header)
		}
		if err := request.ParseForm(); err != nil || !reflect.DeepEqual(request.PostForm, values) {
			t.Errorf("form was not replayed: values=%v err=%v", request.PostForm, err)
		}
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(writer, `{"announcements":[]}`)
	}))
	defer server.Close()
	body, status, err := doPostForm(context.Background(), server.URL, values, headers)
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), "announcements") || calls.Load() != 2 {
		t.Fatalf("read-only POST retry failed: status=%d calls=%d err=%v", status, calls.Load(), err)
	}
	if len(headers) != 2 || headers["Content-Type"] != "" || values.Get("searchkey") != "a+b & c" {
		t.Fatal("request helper mutated caller-owned headers or values")
	}
}

func TestHTTPReliabilitySourceFamilies(t *testing.T) {
	for _, host := range []string{"1.push2.eastmoney.com", "99.push2his.eastmoney.com", emBackupHost} {
		if publicSourceLimiter(host) != emQuoteLimiter {
			t.Errorf("EastMoney nodes must share one limit: %s", host)
		}
	}
	if publicSourceLimiter("qt.gtimg.cn") != tencentLimiter || publicSourceLimiter("ifzq.gtimg.cn") != tencentLimiter ||
		publicSourceLimiter("hq.sinajs.cn") != sinaLimiter || publicSourceLimiter("money.finance.sina.com.cn") != sinaLimiter {
		t.Fatal("Tencent/Sina public host families are not sharing limits")
	}
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "push2.eastmoney.com.invalid", "evilgtimg.cn", "datacenter.eastmoney.com"} {
		if publicSourceLimiter(host) != nil {
			t.Errorf("non-quote/local host was unexpectedly throttled: %s", host)
		}
	}
}
