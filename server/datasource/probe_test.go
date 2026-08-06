package datasource

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type probeFakeAdapter struct {
	name  string
	quote func(context.Context) (*Quote, error)
	daily func(context.Context) ([]Bar, error)
}

func (f *probeFakeAdapter) Name() string { return f.name }
func (f *probeFakeAdapter) GetQuote(ctx context.Context, _, _ string) (*Quote, error) {
	if f.quote == nil {
		return nil, ErrNotSupported
	}
	return f.quote(ctx)
}
func (f *probeFakeAdapter) GetDailyBars(ctx context.Context, _, _ string, _ int) ([]Bar, error) {
	if f.daily == nil {
		return nil, ErrNotSupported
	}
	return f.daily(ctx)
}

func probeHealthRow(t *testing.T, m *Manager, source, capability, market string) HealthStat {
	t.Helper()
	for _, row := range m.HealthSnapshot() {
		if row.Source == source && row.Capability == capability && row.Market == market {
			return row
		}
	}
	t.Fatalf("health row not found: %s/%s/%s", source, capability, market)
	return HealthStat{}
}

func TestProbeRecordsSuccessEmptyAndError(t *testing.T) {
	ResetProbeLimiterForTest()
	good := NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) {
		return &Quote{Symbol: probeSampleSymbol}, nil
	}})
	result, err := good.Probe(context.Background(), "eastmoney", "quote", "cn")
	if err != nil || result.Outcome != ProbeSuccess || result.Code != "OK" || result.SampleCount != 1 {
		t.Fatalf("success probe mismatch: %+v err=%v", result, err)
	}
	row := probeHealthRow(t, good, "eastmoney", "quote", "cn")
	if row.Observation != "success" || row.Samples != 1 || row.Success != 1 {
		t.Fatalf("success health mismatch: %+v", row)
	}

	ResetProbeLimiterForTest()
	empty := NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) {
		return nil, ErrNoData
	}})
	result, err = empty.Probe(context.Background(), "eastmoney", "quote", "cn")
	if err != nil || result.Outcome != ProbeEmpty || result.Code != "EMPTY" {
		t.Fatalf("empty probe mismatch: %+v err=%v", result, err)
	}
	row = probeHealthRow(t, empty, "eastmoney", "quote", "cn")
	if row.Observation != "empty" || row.Empty != 1 {
		t.Fatalf("empty health mismatch: %+v", row)
	}

	ResetProbeLimiterForTest()
	failing := NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) {
		return nil, errors.New("upstream body must not escape")
	}})
	result, err = failing.Probe(context.Background(), "eastmoney", "quote", "cn")
	if err == nil || result.Outcome != ProbeError || result.Code != "UPSTREAM_ERROR" {
		t.Fatalf("error probe mismatch: %+v err=%v", result, err)
	}
	if err.Error() != "UPSTREAM_ERROR" {
		t.Fatalf("probe error must be normalized, got %q", err.Error())
	}
	row = probeHealthRow(t, failing, "eastmoney", "quote", "cn")
	if row.Observation != "error" || row.Errors != 1 {
		t.Fatalf("error health mismatch: %+v", row)
	}
}

func TestProbeWhitelistTimeoutAndTupleRate(t *testing.T) {
	ResetProbeLimiterForTest()
	m := NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) {
		return &Quote{Symbol: probeSampleSymbol}, nil
	}})
	if _, err := m.Probe(context.Background(), "evil", "quote", "cn"); !errors.Is(err, ErrProbeNotAllowed) {
		t.Fatalf("unregistered provider must be rejected: %v", err)
	}
	if _, err := m.Probe(context.Background(), "eastmoney", "quote", "us"); !errors.Is(err, ErrProbeNotAllowed) {
		t.Fatalf("unregistered market must be rejected: %v", err)
	}
	if _, err := m.Probe(context.Background(), "eastmoney", "quote", "cn"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Probe(context.Background(), "eastmoney", "quote", "cn"); !errors.Is(err, ErrProbeRateLimited) {
		t.Fatalf("same tuple must be rate limited: %v", err)
	}

	ResetProbeLimiterForTest()
	timeoutManager := NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(ctx context.Context) (*Quote, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := timeoutManager.Probe(ctx, "eastmoney", "quote", "cn")
	if err == nil || result.Code != "UPSTREAM_TIMEOUT" || result.Outcome != ProbeError {
		t.Fatalf("timeout must be normalized: %+v err=%v", result, err)
	}
}

func TestProbeTupleConcurrencyAndClearKeepsHistory(t *testing.T) {
	ResetProbeLimiterForTest()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m := NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(ctx context.Context) (*Quote, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return &Quote{Symbol: probeSampleSymbol}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	firstDone := make(chan error, 1)
	go func() {
		_, err := m.Probe(context.Background(), "eastmoney", "quote", "cn")
		firstDone <- err
	}()
	<-entered
	if _, err := m.Probe(context.Background(), "eastmoney", "quote", "cn"); !errors.Is(err, ErrProbeBusy) {
		t.Fatalf("same tuple in flight must be busy: %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first probe failed: %v", err)
	}

	// 20 error observations trigger cooldown and clear the ring, while the last
	// observation remains visible after manual unfreeze.
	for i := 0; i < healthMinSamples; i++ {
		m.health.RecordForMarket("eastmoney", "quote", "cn", outcomeError, 1)
	}
	row := probeHealthRow(t, m, "eastmoney", "quote", "cn")
	if row.CooldownLeft <= 0 || !row.Observed || row.Observation != "error" {
		t.Fatalf("expected cooldown before unfreeze: %+v", row)
	}
	left, cleared, err := m.ClearCooldown("eastmoney", "quote", "cn")
	if err != nil || !cleared || left <= 0 {
		t.Fatalf("unfreeze mismatch: left=%d cleared=%v err=%v", left, cleared, err)
	}
	row = probeHealthRow(t, m, "eastmoney", "quote", "cn")
	if row.CooldownLeft != 0 || !row.Observed || row.Observation != "error" || row.Samples > 1 {
		t.Fatalf("unfreeze must preserve history but not refill ring: %+v", row)
	}
}

func TestProbeGlobalRateAndConcurrency(t *testing.T) {
	ResetProbeLimiterForTest()
	newFastManager := func() *Manager {
		return NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) {
			return &Quote{Symbol: probeSampleSymbol}, nil
		}})
	}
	for i := 0; i < probeGlobalMax; i++ {
		if result, err := newFastManager().Probe(context.Background(), "eastmoney", "quote", "cn"); err != nil || result.Outcome != ProbeSuccess {
			t.Fatalf("global rate warmup %d failed: %+v err=%v", i, result, err)
		}
	}
	result, err := newFastManager().Probe(context.Background(), "eastmoney", "quote", "cn")
	if !errors.Is(err, ErrProbeRateLimited) || result.Code != "PROBE_RATE_LIMITED" {
		t.Fatalf("global rate limit mismatch: %+v err=%v", result, err)
	}

	ResetProbeLimiterForTest()
	entered := make(chan struct{}, probeGlobalConcurrency)
	release := make(chan struct{})
	blockingManager := func() *Manager {
		return NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(ctx context.Context) (*Quote, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return &Quote{Symbol: probeSampleSymbol}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}})
	}
	done := make(chan error, probeGlobalConcurrency)
	for i := 0; i < probeGlobalConcurrency; i++ {
		go func(m *Manager) {
			_, probeErr := m.Probe(context.Background(), "eastmoney", "quote", "cn")
			done <- probeErr
		}(blockingManager())
	}
	for i := 0; i < probeGlobalConcurrency; i++ {
		<-entered
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err = blockingManager().Probe(ctx, "eastmoney", "quote", "cn")
	if !errors.Is(err, context.DeadlineExceeded) || result.Code != "PROBE_ADMISSION_TIMEOUT" {
		t.Fatalf("global concurrency guard mismatch: %+v err=%v", result, err)
	}
	close(release)
	for i := 0; i < probeGlobalConcurrency; i++ {
		if err := <-done; err != nil {
			t.Fatalf("admitted probe failed: %v", err)
		}
	}
}
