package service

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	marketCacheMaxEntries    = 1024
	marketCacheMaxBytes      = 32 << 20
	marketCacheMaxEntryBytes = 2 << 20
	marketCacheFetchTimeout  = 20 * time.Second
)

type marketResponseEntry struct {
	key       string
	value     []byte
	expiresAt time.Time
}

type marketResponseFlight struct {
	context  context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  int
	finished bool
	value    []byte
	err      error
}

// marketResponseCache stores public market responses only, not permissions or
// user-specific data. Redis is deliberately outside this cache: a Redis hit must
// not acquire a fresh local TTL, and abandoned work must not republish old data.
// A key must identify one response type, freshness policy, and TTL across callers.
type marketResponseCache struct {
	mutex     sync.Mutex
	entries   map[string]*list.Element
	recency   list.List
	byteCount int
	flights   map[string]*marketResponseFlight

	// These limits are immutable after the cache starts serving requests.
	maxEntries    int
	maxBytes      int
	maxEntryBytes int
	fetchTimeout  time.Duration
}

func newMarketResponseCache() *marketResponseCache {
	return &marketResponseCache{
		entries:       make(map[string]*list.Element),
		flights:       make(map[string]*marketResponseFlight),
		maxEntries:    marketCacheMaxEntries,
		maxBytes:      marketCacheMaxBytes,
		maxEntryBytes: marketCacheMaxEntryBytes,
		fetchTimeout:  marketCacheFetchTimeout,
	}
}

func (service *MarketService) responseCache() *marketResponseCache {
	service.cacheOnce.Do(func() {
		service.cache = newMarketResponseCache()
	})
	return service.cache
}

// Load merges concurrent misses for one key. The fetch context has its own
// bounded lifetime, without retaining the first caller's values or deadline.
// Every returned slice is owned by its caller. A nonpositive TTL merges requests
// without retaining their response. Fetch must honor its context to release any
// upstream resources; Go cannot forcibly stop an uncooperative callback.
func (cache *marketResponseCache) Load(ctx context.Context, key string, ttl time.Duration,
	fetch func(context.Context) ([]byte, error)) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cache.mutex.Lock()
	if element := cache.entries[key]; element != nil {
		entry := element.Value.(*marketResponseEntry)
		if time.Now().Before(entry.expiresAt) {
			cache.recency.MoveToFront(element)
			value := entry.value
			cache.mutex.Unlock()
			result := bytes.Clone(value)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return result, nil
		}
		cache.removeEntryLocked(element)
	}

	flight := cache.flights[key]
	if flight != nil && flight.context.Err() != nil {
		// A timed-out worker may still be unwinding. Do not attach new callers.
		cache.finishFlightLocked(key, flight, nil, flight.context.Err(), 0)
		flight = nil
	}
	created := flight == nil
	if created {
		fetchContext, cancel := context.WithTimeout(context.Background(), cache.fetchTimeout)
		flight = &marketResponseFlight{
			context: fetchContext,
			cancel:  cancel,
			done:    make(chan struct{}),
		}
		cache.flights[key] = flight
	}
	flight.waiters++
	cache.mutex.Unlock()

	if created {
		go cache.fetchFlight(key, flight, ttl, fetch)
	}
	return cache.waitForFlight(ctx, key, flight)
}

func (cache *marketResponseCache) waitForFlight(ctx context.Context, key string,
	flight *marketResponseFlight) ([]byte, error) {
	defer cache.releaseWaiter(key, flight)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if flight.err != nil {
			return nil, flight.err
		}
		value := bytes.Clone(flight.value)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return value, nil
	}
}

func (cache *marketResponseCache) releaseWaiter(key string, flight *marketResponseFlight) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	flight.waiters--
	if cache.flights[key] == flight && flight.waiters == 0 {
		flight.cancel()
		cache.finishFlightLocked(key, flight, nil, context.Canceled, 0)
	}
}

func (cache *marketResponseCache) fetchFlight(key string, flight *marketResponseFlight,
	ttl time.Duration, fetch func(context.Context) ([]byte, error)) {
	// Even a callback ignoring cancellation cannot make waiters outlive the
	// shared budget. Its late result is rejected by finished/identity checks.
	stopTimeout := context.AfterFunc(flight.context, func() {
		cache.mutex.Lock()
		defer cache.mutex.Unlock()
		cache.finishFlightLocked(key, flight, nil, flight.context.Err(), 0)
	})
	defer func() {
		stopTimeout()
		flight.cancel()
	}()

	var value []byte
	var err error
	if err = flight.context.Err(); err == nil {
		value, err = fetch(flight.context)
	}
	if err == nil && len(value) > 0 && !json.Valid(value) {
		err = errors.New("market response is not valid JSON")
	}
	if err == nil {
		value = bytes.Clone(value)
	}

	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.finishFlightLocked(key, flight, value, err, ttl)
}

// The cache mutex guards publication, cancellation, and replacement together.
// A canceled or invalidated flight can neither delete nor populate a newer one.
func (cache *marketResponseCache) finishFlightLocked(key string, flight *marketResponseFlight,
	value []byte, err error, ttl time.Duration) {
	if flight.finished {
		return
	}
	if contextError := flight.context.Err(); contextError != nil {
		err = contextError
	}
	if cache.flights[key] != flight {
		err = context.Canceled
	} else {
		delete(cache.flights, key)
		if err == nil && ttl > 0 && hasMarketResponseData(value) {
			cache.storeEntryLocked(key, value, ttl)
		}
	}
	if err == nil {
		flight.value = value
	}
	flight.err = err
	flight.finished = true
	close(flight.done)
}

// null and an empty JSON array are successful responses but not reusable data.
func hasMarketResponseData(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	return trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' ||
		len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) > 0
}

func (cache *marketResponseCache) storeEntryLocked(key string, value []byte, ttl time.Duration) {
	entryBytes := len(key) + len(value)
	if cache.maxEntries <= 0 || entryBytes > cache.maxEntryBytes || entryBytes > cache.maxBytes {
		return
	}
	now := time.Now()
	for element := cache.recency.Back(); element != nil; {
		previous := element.Prev()
		if !now.Before(element.Value.(*marketResponseEntry).expiresAt) {
			cache.removeEntryLocked(element)
		}
		element = previous
	}
	if previous := cache.entries[key]; previous != nil {
		cache.removeEntryLocked(previous)
	}
	for len(cache.entries) >= cache.maxEntries || cache.byteCount+entryBytes > cache.maxBytes {
		cache.removeEntryLocked(cache.recency.Back())
	}
	cache.entries[key] = cache.recency.PushFront(&marketResponseEntry{
		key: key, value: value, expiresAt: now.Add(ttl),
	})
	cache.byteCount += entryBytes
}

func (cache *marketResponseCache) removeEntryLocked(element *list.Element) {
	entry := element.Value.(*marketResponseEntry)
	delete(cache.entries, entry.key)
	cache.byteCount -= len(entry.key) + len(entry.value)
	cache.recency.Remove(element)
}

// Forget invalidates one exact key, canceling any current shared fetch. Call it
// after a successful re-anchor commit, for every affected key/limit. This is a
// local publication barrier, not a database lock: already-returned values and
// upstream side effects cannot be revoked. Other processes/Redis are unaffected.
func (cache *marketResponseCache) Forget(key string) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if element := cache.entries[key]; element != nil {
		cache.removeEntryLocked(element)
	}
	if flight := cache.flights[key]; flight != nil {
		flight.cancel()
		cache.finishFlightLocked(key, flight, nil, context.Canceled, 0)
	}
}

// ForgetPrefix invalidates all windows for a stock after an external rebase.
// In-flight readers are canceled so an older result cannot repopulate the cache.
func (cache *marketResponseCache) ForgetPrefix(prefix string) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	for key, element := range cache.entries {
		if strings.HasPrefix(key, prefix) {
			cache.removeEntryLocked(element)
		}
	}
	for key, flight := range cache.flights {
		if strings.HasPrefix(key, prefix) {
			flight.cancel()
			cache.finishFlightLocked(key, flight, nil, context.Canceled, 0)
		}
	}
}

// marketCachedJSON decodes a fresh object for every caller, including the fetch
// leader. fetch must return upstream data, not a Redis hit with an unknown age.
func marketCachedJSON[Response any](ctx context.Context, cache *marketResponseCache,
	key string, ttl time.Duration, fetch func(context.Context) (Response, error)) (Response, error) {
	var response Response
	value, err := cache.Load(ctx, key, ttl, func(fetchContext context.Context) ([]byte, error) {
		fetched, fetchError := fetch(fetchContext)
		if fetchError != nil {
			return nil, fetchError
		}
		return json.Marshal(fetched)
	})
	if err != nil {
		return response, err
	}
	if err := json.Unmarshal(value, &response); err != nil {
		return response, err
	}
	if ctx != nil && ctx.Err() != nil {
		var empty Response
		return empty, ctx.Err()
	}
	return response, nil
}
