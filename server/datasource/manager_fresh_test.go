package datasource

import (
	"context"
	"errors"
	"testing"
	"time"
)

// alwaysFresh 谓词：DataTime 在 now 的 10 分钟内。
func acceptWithin(now time.Time, d time.Duration) QuoteAccept {
	return func(q *Quote) bool { return !q.DataTime.IsZero() && now.Sub(q.DataTime) <= d }
}

func TestGetQuoteFreshPrefersFresh(t *testing.T) {
	now := time.Now()
	stale := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) {
		return &Quote{Symbol: "600000", DataTime: now.Add(-2 * time.Hour), Source: "eastmoney"}, nil
	}}
	fresh := &fakeAdapter{name: "sina", quote: func() (*Quote, error) {
		return &Quote{Symbol: "600000", DataTime: now.Add(-1 * time.Minute), Source: "sina"}, nil
	}}
	m := NewManagerWithAdapters(stale, fresh)
	q, ok, err := m.GetQuoteFresh(context.Background(), "cn", "600000", acceptWithin(now, 10*time.Minute))
	if err != nil || !ok {
		t.Fatalf("应返回 fresh，got ok=%v err=%v", ok, err)
	}
	if q.Source != "sina" {
		t.Fatalf("应命中 sina fresh，got %s", q.Source)
	}
	if stale.calls != 1 || fresh.calls != 1 {
		t.Fatalf("两源都应被尝试，got stale=%d fresh=%d", stale.calls, fresh.calls)
	}
}

func TestGetQuoteFreshAllStale(t *testing.T) {
	now := time.Now()
	older := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) {
		return &Quote{DataTime: now.Add(-3 * time.Hour), Source: "eastmoney"}, nil
	}}
	newer := &fakeAdapter{name: "sina", quote: func() (*Quote, error) {
		return &Quote{DataTime: now.Add(-1 * time.Hour), Source: "sina"}, nil
	}}
	m := NewManagerWithAdapters(older, newer)
	q, ok, err := m.GetQuoteFresh(context.Background(), "cn", "600000", acceptWithin(now, 10*time.Minute))
	if err != nil {
		t.Fatalf("全过期不应返回 err，got %v", err)
	}
	if ok {
		t.Fatalf("全过期 fresh 应为 false")
	}
	if q == nil || q.Source != "sina" {
		t.Fatalf("应返回 DataTime 最新候选 sina，got %+v", q)
	}
}

func TestGetQuoteFreshErrThenFresh(t *testing.T) {
	now := time.Now()
	bad := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) { return nil, errors.New("boom") }}
	good := &fakeAdapter{name: "sina", quote: func() (*Quote, error) {
		return &Quote{DataTime: now, Source: "sina"}, nil
	}}
	m := NewManagerWithAdapters(bad, good)
	q, ok, err := m.GetQuoteFresh(context.Background(), "cn", "600000", acceptWithin(now, 10*time.Minute))
	if err != nil || !ok || q.Source != "sina" {
		t.Fatalf("源1错误应回退源2 fresh，got ok=%v err=%v q=%+v", ok, err, q)
	}
}

func TestGetQuoteFreshSymbolInvalid(t *testing.T) {
	inv := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) { return nil, ErrSymbolInvalid }}
	second := &fakeAdapter{name: "sina", quote: func() (*Quote, error) { return &Quote{}, nil }}
	m := NewManagerWithAdapters(inv, second)
	_, _, err := m.GetQuoteFresh(context.Background(), "cn", "x", func(*Quote) bool { return true })
	if !errors.Is(err, ErrSymbolInvalid) {
		t.Fatalf("ErrSymbolInvalid 应立即终止，got %v", err)
	}
	if second.calls != 0 {
		t.Fatalf("非法代码不应换源，second.calls=%d", second.calls)
	}
}

func TestGetQuoteFreshAllErr(t *testing.T) {
	a := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) { return nil, errors.New("e1") }}
	b := &fakeAdapter{name: "sina", quote: func() (*Quote, error) { return nil, errors.New("e2") }}
	m := NewManagerWithAdapters(a, b)
	_, ok, err := m.GetQuoteFresh(context.Background(), "cn", "600000", func(*Quote) bool { return true })
	if err == nil || ok {
		t.Fatalf("全部失败应返回 err，got ok=%v err=%v", ok, err)
	}
}

// TestGetQuoteFreshProbesCooledOnStale 健康源仅有 stale 候选时，冷却源也做有界探测
// ——冷却源可能恰是唯一有新数据的（主源限流冷却场景）；跳过它却声称「已尝试全部
// 数据源」是撒谎（P1）。
func TestGetQuoteFreshProbesCooledOnStale(t *testing.T) {
	now := time.Now()
	staleSrc := &fakeAdapter{name: "sina", quote: func() (*Quote, error) {
		return &Quote{DataTime: now.Add(-2 * time.Hour), Source: "sina"}, nil
	}}
	cooledFresh := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) {
		return &Quote{DataTime: now.Add(-1 * time.Minute), Source: "eastmoney"}, nil
	}}
	m := &Manager{adapters: []Adapter{cooledFresh, staleSrc}, health: NewHealthTracker()}
	m.health.now = func() time.Time { return now }
	// 人为把 eastmoney 打进冷却。
	for i := 0; i < healthMinSamples+1; i++ {
		m.health.Record("eastmoney", "quote", outcomeError, 1)
	}
	if m.health.Available("eastmoney", "quote") {
		t.Fatal("eastmoney 应处于冷却")
	}
	q, ok, err := m.GetQuoteFresh(context.Background(), "cn", "600000", acceptWithin(now, 10*time.Minute))
	if err != nil || !ok {
		t.Fatalf("冷却源有新数据时应探测并返回 fresh, got ok=%v err=%v", ok, err)
	}
	if q.Source != "eastmoney" {
		t.Fatalf("应命中冷却源的 fresh 行情, got %s", q.Source)
	}
	if cooledFresh.calls == 0 {
		t.Fatal("stale 候选存在时冷却源也应被探测（每源一次有界）")
	}
}
