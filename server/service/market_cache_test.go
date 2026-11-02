package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"quantvista/common"
)

type marketCacheTestResult struct {
	value []byte
	err   error
}

func TestMarketResponseCacheWithoutRedisAndByteIsolation(test *testing.T) {
	previousRedis := common.RDB
	common.RDB = nil
	defer func() { common.RDB = previousRedis }()

	cache := newMarketResponseCache()
	original := []byte(`{"price":12}`)
	var calls atomic.Int32
	fetch := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return original, nil
	}
	first, err := cache.Load(context.Background(), "quote:cn:600001", time.Minute, fetch)
	if err != nil {
		test.Fatal(err)
	}
	second, err := cache.Load(context.Background(), "quote:cn:600001", time.Minute, fetch)
	if err != nil {
		test.Fatal(err)
	}
	first[0] = '!'
	original[0] = '?'
	if string(second) != `{"price":12}` {
		test.Fatalf("a cache hit shares mutable bytes: %s", second)
	}
	second[0] = '#'
	third, err := cache.Load(nil, "quote:cn:600001", time.Minute, fetch)
	if err != nil || string(third) != `{"price":12}` || calls.Load() != 1 {
		test.Fatalf("memory cache was mutated or unavailable without Redis: %s, %v, calls=%d", third, err, calls.Load())
	}
}

func TestMarketResponseCacheConcurrentMissAndResultIsolation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		cache := newMarketResponseCache()
		const workers = 24
		release := make(chan struct{})
		results := make(chan marketCacheTestResult, workers)
		var calls atomic.Int32
		fetch := func(context.Context) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte(`{"price":12}`), nil
		}
		for range workers {
			go func() {
				value, err := cache.Load(context.Background(), "shared", time.Minute, fetch)
				results <- marketCacheTestResult{value: value, err: err}
			}()
		}
		synctest.Wait()
		cache.mutex.Lock()
		waiters := cache.flights["shared"].waiters
		cache.mutex.Unlock()
		close(release)
		if waiters != workers || calls.Load() != 1 {
			test.Fatalf("misses were not coalesced: waiters=%d calls=%d", waiters, calls.Load())
		}
		for range workers {
			result := <-results
			if result.err != nil || string(result.value) != `{"price":12}` {
				test.Fatalf("waiters did not receive independent results: %s, %v", result.value, result.err)
			}
			result.value[0] = '!'
		}
	})
}

func TestMarketResponseCacheLeaderCancellationKeepsOtherWaiters(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		cache := newMarketResponseCache()
		leaderContext, cancelLeader := context.WithTimeout(context.Background(), time.Second)
		defer cancelLeader()
		release := make(chan struct{})
		leaderResult := make(chan error, 1)
		waiterResults := make(chan marketCacheTestResult, 2)
		var calls atomic.Int32
		var upstreamContext context.Context
		fetch := func(fetchContext context.Context) ([]byte, error) {
			calls.Add(1)
			upstreamContext = fetchContext
			select {
			case <-release:
				return []byte(`{"price":12}`), nil
			case <-fetchContext.Done():
				return nil, fetchContext.Err()
			}
		}
		go func() {
			_, err := cache.Load(leaderContext, "shared", time.Minute, fetch)
			leaderResult <- err
		}()
		synctest.Wait()
		go func() {
			value, err := cache.Load(context.Background(), "shared", time.Minute, fetch)
			waiterResults <- marketCacheTestResult{value: value, err: err}
		}()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		if err := <-leaderResult; !errors.Is(err, context.DeadlineExceeded) {
			test.Fatalf("leader should use its own deadline: %v", err)
		}
		if upstreamContext.Err() != nil {
			test.Fatalf("leader cancellation reached shared upstream: %v", upstreamContext.Err())
		}
		go func() {
			value, err := cache.Load(context.Background(), "shared", time.Minute, fetch)
			waiterResults <- marketCacheTestResult{value: value, err: err}
		}()
		synctest.Wait()
		close(release)
		for range 2 {
			result := <-waiterResults
			if result.err != nil || string(result.value) != `{"price":12}` {
				test.Fatalf("live waiter failed after leader cancellation: %+v", result)
			}
		}
		if calls.Load() != 1 {
			test.Fatalf("new waiter did not join the existing flight: calls=%d", calls.Load())
		}
	})
}

func TestMarketResponseCacheAllWaitersCancelUpstream(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		cache := newMarketResponseCache()
		firstContext, cancelFirst := context.WithCancel(context.Background())
		secondContext, cancelSecond := context.WithCancel(context.Background())
		defer cancelFirst()
		defer cancelSecond()
		upstreamCanceled := make(chan struct{})
		results := make(chan error, 2)
		fetch := func(fetchContext context.Context) ([]byte, error) {
			<-fetchContext.Done()
			close(upstreamCanceled)
			return []byte(`{"late":true}`), nil
		}
		for _, callerContext := range []context.Context{firstContext, secondContext} {
			go func() {
				_, err := cache.Load(callerContext, "shared", time.Minute, fetch)
				results <- err
			}()
		}
		synctest.Wait()
		cancelFirst()
		cancelSecond()
		synctest.Wait()
		for range 2 {
			if err := <-results; !errors.Is(err, context.Canceled) {
				test.Fatalf("canceled waiter returned %v", err)
			}
		}
		select {
		case <-upstreamCanceled:
		default:
			test.Fatal("last departing waiter did not cancel upstream")
		}
		cache.mutex.Lock()
		entryCount, flightCount := len(cache.entries), len(cache.flights)
		cache.mutex.Unlock()
		if entryCount != 0 || flightCount != 0 {
			test.Fatalf("canceled work was retained: entries=%d flights=%d", entryCount, flightCount)
		}
	})
}

func TestMarketResponseCacheAbandonedFlightCannotReplaceNewWork(test *testing.T) {
	for _, finishOldFirst := range []bool{true, false} {
		test.Run(fmt.Sprintf("old_finishes_first=%v", finishOldFirst), func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				cache := newMarketResponseCache()
				firstContext, cancelFirst := context.WithCancel(context.Background())
				defer cancelFirst()
				releaseOld := make(chan struct{})
				releaseNew := make(chan struct{})
				firstResult := make(chan error, 1)
				newResult := make(chan marketCacheTestResult, 1)
				go func() {
					_, err := cache.Load(firstContext, "shared", time.Minute, func(context.Context) ([]byte, error) {
						<-releaseOld // Simulate a source that returns success after cancellation.
						return []byte(`{"version":"old"}`), nil
					})
					firstResult <- err
				}()
				synctest.Wait()
				cancelFirst()
				if err := <-firstResult; !errors.Is(err, context.Canceled) {
					test.Fatalf("first caller was not canceled: %v", err)
				}
				go func() {
					value, err := cache.Load(context.Background(), "shared", time.Minute, func(context.Context) ([]byte, error) {
						<-releaseNew
						return []byte(`{"version":"new"}`), nil
					})
					newResult <- marketCacheTestResult{value: value, err: err}
				}()
				synctest.Wait()
				cache.mutex.Lock()
				newFlight := cache.flights["shared"]
				cache.mutex.Unlock()
				if finishOldFirst {
					close(releaseOld)
					synctest.Wait()
					cache.mutex.Lock()
					current := cache.flights["shared"]
					cache.mutex.Unlock()
					if current != newFlight {
						test.Fatal("old completion removed or replaced the new flight")
					}
				}
				close(releaseNew)
				result := <-newResult
				if result.err != nil || string(result.value) != `{"version":"new"}` {
					test.Fatalf("replacement failed: %+v", result)
				}
				if !finishOldFirst {
					close(releaseOld)
				}
				synctest.Wait()
				value, err := cache.Load(context.Background(), "shared", time.Minute, func(context.Context) ([]byte, error) {
					return nil, errors.New("replacement cache was lost")
				})
				if err != nil || string(value) != `{"version":"new"}` {
					test.Fatalf("late result overwrote the replacement cache: %s, %v", value, err)
				}
			})
		})
	}
}

func TestMarketResponseCacheSharedBudgetStopsWaitingForUncooperativeFetch(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		cache := newMarketResponseCache()
		release := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			_, err := cache.Load(context.Background(), "slow", time.Minute, func(context.Context) ([]byte, error) {
				<-release
				return []byte(`{"version":"expired"}`), nil
			})
			result <- err
		}()
		synctest.Wait()
		time.Sleep(marketCacheFetchTimeout)
		if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
			test.Fatalf("shared fetch budget did not release waiters: %v", err)
		}
		value, err := cache.Load(context.Background(), "slow", time.Minute, func(context.Context) ([]byte, error) {
			return []byte(`{"version":"current"}`), nil
		})
		close(release)
		synctest.Wait()
		if err != nil || string(value) != `{"version":"current"}` {
			test.Fatalf("expired flight prevented a new request: %s, %v", value, err)
		}
	})
}

func TestMarketResponseCacheDifferentKeysDoNotSerialize(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		cache := newMarketResponseCache()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			_, _ = cache.Load(ctx, "slow", time.Minute, func(fetchContext context.Context) ([]byte, error) {
				<-fetchContext.Done()
				return nil, fetchContext.Err()
			})
		}()
		synctest.Wait()
		value, err := cache.Load(context.Background(), "fast", time.Minute, func(context.Context) ([]byte, error) {
			return []byte(`{"fast":true}`), nil
		})
		if err != nil || string(value) != `{"fast":true}` {
			test.Fatalf("unrelated key waited for slow upstream: %s, %v", value, err)
		}
	})
}

func TestMarketResponseCacheExpirationDoesNotSlideOnHit(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		cache := newMarketResponseCache()
		var calls atomic.Int32
		fetch := func(context.Context) ([]byte, error) {
			return []byte(fmt.Sprintf(`{"call":%d}`, calls.Add(1))), nil
		}
		for _, elapsed := range []time.Duration{0, 750 * time.Millisecond, 250 * time.Millisecond} {
			time.Sleep(elapsed)
			if _, err := cache.Load(context.Background(), "expiring", time.Second, fetch); err != nil {
				test.Fatal(err)
			}
		}
		if calls.Load() != 2 {
			test.Fatalf("cache hit extended TTL or expired too early: calls=%d", calls.Load())
		}
		time.Sleep(time.Second)
		if _, err := cache.Load(context.Background(), "other", time.Second, fetch); err != nil {
			test.Fatal(err)
		}
		cache.mutex.Lock()
		expiredRetained := cache.entries["expiring"] != nil
		cache.mutex.Unlock()
		if expiredRetained {
			test.Fatal("new insertion did not reclaim expired entries")
		}
	})
}

func TestMarketResponseCacheLRUAndByteLimits(test *testing.T) {
	for _, limitByBytes := range []bool{false, true} {
		test.Run(fmt.Sprintf("byte_limit=%v", limitByBytes), func(test *testing.T) {
			cache := newMarketResponseCache()
			payload := []byte(`{"price":12}`)
			entryBytes := len("first") + len(payload)
			if limitByBytes {
				cache.maxBytes = entryBytes * 2
			} else {
				cache.maxEntries = 2
			}
			fetch := func(context.Context) ([]byte, error) { return payload, nil }
			for _, key := range []string{"first", "other", "first", "third"} {
				if _, err := cache.Load(context.Background(), key, time.Minute, fetch); err != nil {
					test.Fatal(err)
				}
			}
			cache.mutex.Lock()
			entryCount, byteCount := len(cache.entries), cache.byteCount
			keptRecent := cache.entries["first"] != nil && cache.entries["third"] != nil
			removedLeastRecent := cache.entries["other"] == nil
			cache.mutex.Unlock()
			if entryCount != 2 || byteCount != entryBytes*2 || !keptRecent || !removedLeastRecent {
				test.Fatalf("LRU bounds/accounting failed: entries=%d bytes=%d recent=%v removed=%v", entryCount, byteCount, keptRecent, removedLeastRecent)
			}
			cache.Forget("first")
			cache.Forget("third")
			if cache.byteCount != 0 || cache.recency.Len() != 0 || len(cache.entries) != 0 {
				test.Fatal("invalidation did not release cache capacity")
			}
		})
	}
}

func TestMarketResponseCacheOversizedResponseIsNotRetained(test *testing.T) {
	cache := newMarketResponseCache()
	payload := []byte(`"` + strings.Repeat("x", marketCacheMaxEntryBytes) + `"`)
	var calls atomic.Int32
	fetch := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return payload, nil
	}
	for range 2 {
		value, err := cache.Load(context.Background(), "large", time.Minute, fetch)
		if err != nil || !bytes.Equal(value, payload) {
			test.Fatalf("large responses should still be returned: size=%d err=%v", len(value), err)
		}
	}
	if calls.Load() != 2 || cache.byteCount != 0 || len(cache.entries) != 0 {
		test.Fatalf("oversized entry was retained: calls=%d bytes=%d", calls.Load(), cache.byteCount)
	}
}

func TestMarketResponseCacheEmptyErrorAndCanceledResultsAreNotCached(test *testing.T) {
	upstreamError := errors.New("upstream unavailable")
	for _, scenario := range []struct {
		name    string
		value   []byte
		err     error
		wantErr bool
	}{
		{name: "nil"},
		{name: "empty_array", value: []byte("[ \n ]")},
		{name: "null", value: []byte("null")},
		{name: "invalid_json", value: []byte("invalid"), wantErr: true},
		{name: "error", err: upstreamError, wantErr: true},
		{name: "canceled", err: context.Canceled, wantErr: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			cache := newMarketResponseCache()
			var calls atomic.Int32
			for range 2 {
				_, err := cache.Load(context.Background(), "empty", time.Minute, func(context.Context) ([]byte, error) {
					calls.Add(1)
					return scenario.value, scenario.err
				})
				if (err != nil) != scenario.wantErr || scenario.err != nil && !errors.Is(err, scenario.err) {
					test.Fatalf("unexpected error: %v", err)
				}
			}
			if calls.Load() != 2 || len(cache.entries) != 0 {
				test.Fatalf("empty/failed response was cached: calls=%d entries=%d", calls.Load(), len(cache.entries))
			}
		})
	}
}

func TestMarketResponseCacheCanceledCallerCannotReadHitOrStartMiss(test *testing.T) {
	cache := newMarketResponseCache()
	var calls atomic.Int32
	fetch := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"price":12}`), nil
	}
	if _, err := cache.Load(context.Background(), "cached", time.Minute, fetch); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, key := range []string{"cached", "missing"} {
		value, err := cache.Load(ctx, key, time.Minute, fetch)
		if !errors.Is(err, context.Canceled) || value != nil {
			test.Fatalf("canceled request for %s returned %s, %v", key, value, err)
		}
	}
	if calls.Load() != 1 {
		test.Fatalf("canceled caller started upstream: calls=%d", calls.Load())
	}
}

func TestMarketResponseCacheForgetCancelsInflightPublication(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		cache := newMarketResponseCache()
		release := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			_, err := cache.Load(context.Background(), "rebased", time.Minute, func(context.Context) ([]byte, error) {
				<-release
				return []byte(`{"basis":"old"}`), nil
			})
			result <- err
		}()
		synctest.Wait()
		cache.Forget("rebased")
		if err := <-result; !errors.Is(err, context.Canceled) {
			test.Fatalf("invalidation did not release existing waiters: %v", err)
		}
		close(release)
		synctest.Wait()
		value, err := cache.Load(context.Background(), "rebased", time.Minute, func(context.Context) ([]byte, error) {
			return []byte(`{"basis":"new"}`), nil
		})
		if err != nil || string(value) != `{"basis":"new"}` {
			test.Fatalf("invalidated fetch published old basis: %s, %v", value, err)
		}
	})
}

func TestMarketCachedJSONReturnsIndependentObjects(test *testing.T) {
	type quoteResponse struct {
		Prices []float64         `json:"prices"`
		Labels map[string]string `json:"labels"`
	}
	cache := newMarketResponseCache()
	original := &quoteResponse{Prices: []float64{12}, Labels: map[string]string{"source": "fake"}}
	var calls atomic.Int32
	fetch := func(context.Context) (*quoteResponse, error) {
		calls.Add(1)
		return original, nil
	}
	first, err := marketCachedJSON(context.Background(), cache, "typed", time.Minute, fetch)
	if err != nil {
		test.Fatal(err)
	}
	first.Prices[0], first.Labels["source"] = 20, "caller"
	original.Prices[0], original.Labels["source"] = 30, "upstream"
	second, err := marketCachedJSON(context.Background(), cache, "typed", time.Minute, fetch)
	if err != nil || second.Prices[0] != 12 || second.Labels["source"] != "fake" || calls.Load() != 1 {
		test.Fatalf("typed cache shared mutable response objects: %+v, %v, calls=%d", second, err, calls.Load())
	}
}
