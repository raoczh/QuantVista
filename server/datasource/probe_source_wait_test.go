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

func shortenSourceProbeBudget(test *testing.T, source string, timeout time.Duration) {
	test.Helper()
	previousRegistry := capabilityRegistry
	capabilityRegistry = append([]CapabilitySpec(nil), previousRegistry...)
	for index := range capabilityRegistry {
		spec := &capabilityRegistry[index]
		if spec.Source == source && spec.Capability == "quote" && spec.Market == "cn" {
			spec.TimeoutMs = int(timeout.Milliseconds())
		}
	}
	test.Cleanup(func() { capabilityRegistry = previousRegistry })
	ResetProbeLimiterForTest()
	test.Cleanup(ResetProbeLimiterForTest)
}

func TestProbeLocalSourceWaitDoesNotObserveUpstream(test *testing.T) {
	for _, source := range []string{"eastmoney", "tencent", "sina"} {
		test.Run(source, func(test *testing.T) {
			shortenSourceProbeBudget(test, source, 50*time.Millisecond)
			adapter, limiter := sourceWaitTestAdapter(test, source)
			limiter.deferUntil(time.Now().Add(time.Hour))
			var calls atomic.Int32
			withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.WriteHeader(http.StatusServiceUnavailable)
			})
			manager := NewManagerWithAdapters(adapter)
			manager.health.RecordForMarket(source, "quote", "cn", outcomeError, 1)
			result, err := manager.Probe(test.Context(), source, "quote", "cn")
			if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "LOCAL_SOURCE_WAIT" || result.Code != "LOCAL_SOURCE_WAIT" ||
				result.Outcome != ProbeError || result.SampleCount != 0 || calls.Load() != 0 {
				test.Fatalf("local waiting must be an unsampled, sanitized probe failure: result=%+v calls=%d err=%v", result, calls.Load(), err)
			}
			row := probeHealthRow(test, manager, source, "quote", "cn")
			if row.Samples != 1 || row.Errors != 1 || row.Success != 0 || row.Empty != 0 {
				test.Fatalf("local probe admission changed source health: %+v", row)
			}
		})
	}
}

func TestProbeRealAttemptThenWaitStillRecordsFailure(test *testing.T) {
	for _, phase := range []string{"retry", "next_page", "response_body"} {
		test.Run(phase, func(test *testing.T) {
			shortenSourceProbeBudget(test, "tencent", 150*time.Millisecond)
			replaceSourceLimiter(test, &tencentLimiter, time.Hour)
			var calls atomic.Int32
			withLocalSourceHTTP(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if phase == "retry" {
					writer.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if phase == "response_body" {
					writer.(http.Flusher).Flush()
					<-request.Context().Done()
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
			result, err := manager.Probe(test.Context(), "tencent", "quote", "cn")
			var localWait *localSourceWaitError
			row := probeHealthRow(test, manager, "tencent", "quote", "cn")
			if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &localWait) || result.Code != "UPSTREAM_TIMEOUT" ||
				result.SampleCount != 1 || calls.Load() != 1 || row.Errors != 1 || row.Samples != 1 {
				test.Fatalf("real attempted probe failure was erased: result=%+v calls=%d health=%+v err=%v", result, calls.Load(), row, err)
			}
		})
	}
}

func TestProbeUnmarkedAdapterTimeoutIsNotAssumedLocal(test *testing.T) {
	shortenSourceProbeBudget(test, "tencent", 30*time.Millisecond)
	adapter := &probeFakeAdapter{name: "tencent", quote: func(ctx context.Context) (*Quote, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	manager := NewManagerWithAdapters(adapter)
	result, err := manager.Probe(test.Context(), "tencent", "quote", "cn")
	row := probeHealthRow(test, manager, "tencent", "quote", "cn")
	if !errors.Is(err, context.DeadlineExceeded) || result.Code != "UPSTREAM_TIMEOUT" || result.SampleCount != 1 || row.Errors != 1 {
		test.Fatalf("only positively identified local waiting may skip health: result=%+v health=%+v err=%v", result, row, err)
	}
}
