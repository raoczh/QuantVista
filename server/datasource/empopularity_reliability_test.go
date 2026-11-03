package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const popularityReliabilityPayload = `{"status":0,"data":[{"sc":"SZ000725","rk":1,"hisRc":-3}]}`

func TestPopularityReliabilityReplaysUnchangedJSON(test *testing.T) {
	var calls atomic.Int32
	bodies := make(chan string, 8)
	withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/stockrank/getAllCurrentList" ||
			request.Header.Get("Content-Type") != "application/json" || request.Header.Get("User-Agent") != browserUA {
			test.Errorf("popularity request changed: method=%s url=%s headers=%v", request.Method, request.URL, request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			test.Errorf("read query body: %v", err)
		}
		bodies <- string(body)
		if calls.Add(1) < 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(writer, popularityReliabilityPayload)
	})
	rows, err := GetPopularityTop(test.Context())
	if err != nil || calls.Load() != 3 || len(rows) != 1 || rows[0].Symbol != "000725" || rows[0].PrevRank != -3 {
		test.Fatalf("read-only JSON retry changed result or budget: rows=%+v calls=%d err=%v", rows, calls.Load(), err)
	}
	expected := map[string]any{
		"appId": "appId01", "globalId": "786e4c21-70dc-435a-93bb-38", "marketType": "",
		"pageNo": float64(1), "pageSize": float64(100),
	}
	firstBody := <-bodies
	var decoded map[string]any
	if err := json.Unmarshal([]byte(firstBody), &decoded); err != nil || !reflect.DeepEqual(decoded, expected) {
		test.Fatalf("business query fields changed: payload=%s err=%v", firstBody, err)
	}
	for attempt := 1; attempt < 3; attempt++ {
		if body := <-bodies; body != firstBody {
			test.Fatalf("JSON was not identically replayed: first=%s retry=%s", firstBody, body)
		}
	}
}

func TestPopularityReliabilityRejectsFailuresWithoutRetryStorm(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
		body   string
		calls  int32
	}{
		{"unavailable", http.StatusServiceUnavailable, "", 3},
		{"not_found", http.StatusNotFound, "", 1},
		{"redirect", http.StatusFound, "", 1},
		{"challenge", http.StatusOK, "<html>verification</html>", 1},
		{"business_error", http.StatusOK, `{"success":false,"status":0,"data":[{"sc":"SZ000725","rk":1}]}`, 1},
		{"malformed_json", http.StatusOK, `{"data":`, 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var calls atomic.Int32
			withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if scenario.status == http.StatusFound {
					writer.Header().Set("Location", "/challenge")
				}
				writer.WriteHeader(scenario.status)
				fmt.Fprint(writer, scenario.body)
			})
			rows, err := GetPopularityTop(test.Context())
			if !errors.Is(err, ErrUpstream) || errors.Is(err, ErrNoData) || rows != nil || calls.Load() != scenario.calls {
				test.Fatalf("upstream failure was hidden or over-retried: rows=%+v calls=%d err=%v", rows, calls.Load(), err)
			}
		})
	}
}

func TestPopularityReliabilityResponseLimitIsNotSilentTruncation(test *testing.T) {
	for _, size := range []int{maxHTTPResponseBytes, maxHTTPResponseBytes + 1} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			payload := popularityReliabilityPayload + strings.Repeat(" ", size-len(popularityReliabilityPayload))
			var calls atomic.Int32
			withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.(http.Flusher).Flush()
				fmt.Fprint(writer, payload)
			})
			rows, err := GetPopularityTop(test.Context())
			if calls.Load() != 1 {
				test.Fatalf("oversized response must not be retried: calls=%d", calls.Load())
			}
			if size == maxHTTPResponseBytes {
				if err != nil || len(rows) != 1 {
					test.Fatalf("exact response limit rejected: rows=%+v err=%v", rows, err)
				}
			} else if !errors.Is(err, ErrUpstream) || rows != nil {
				test.Fatalf("valid JSON prefix must not hide response truncation: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestPopularityReliabilityRetryAfterAndCancellation(test *testing.T) {
	test.Run("retry_after", func(test *testing.T) {
		var calls atomic.Int32
		withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			writer.Header().Set("Retry-After", "60")
			writer.WriteHeader(http.StatusTooManyRequests)
		})
		ctx, cancel := context.WithTimeout(test.Context(), 2*time.Second)
		defer cancel()
		_, err := GetPopularityTop(ctx)
		var statusFailure *httpStatusError
		if !errors.As(err, &statusFailure) || statusFailure.statusCode != http.StatusTooManyRequests || calls.Load() != 1 || ctx.Err() != nil {
			test.Fatalf("popularity retry bypassed Retry-After: calls=%d err=%v", calls.Load(), err)
		}
	})
	test.Run("cancellation", func(test *testing.T) {
		ctx, cancel := context.WithCancel(test.Context())
		defer cancel()
		var calls atomic.Int32
		withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			cancel()
			writer.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := GetPopularityTop(ctx)
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			test.Fatalf("popularity cancellation was wrapped away: calls=%d err=%v", calls.Load(), err)
		}
	})
}
