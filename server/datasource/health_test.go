package datasource

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeAdapter 仅实现基础 Adapter 接口的假源（路由测试用）。
type fakeAdapter struct {
	name  string
	calls int
	quote func() (*Quote, error)
}

func healthStatFor(t *testing.T, rows []HealthStat, source, capability, market string) HealthStat {
	t.Helper()
	for _, row := range rows {
		if row.Source == source && row.Capability == capability && row.Market == market {
			return row
		}
	}
	t.Fatalf("未找到健康行 %s/%s/%s: %+v", source, capability, market, rows)
	return HealthStat{}
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) GetQuote(context.Context, string, string) (*Quote, error) {
	f.calls++
	return f.quote()
}
func (f *fakeAdapter) GetDailyBars(context.Context, string, string, int) ([]Bar, error) {
	return nil, ErrNotSupported
}

// TestHealthCooldownOnEmpty empty 占比 >50% 触发 300s 冷却；冷却结束自动恢复且窗口清零。
func TestHealthCooldownOnEmpty(t *testing.T) {
	tr := NewHealthTracker()
	now := time.Now()
	tr.now = func() time.Time { return now }

	// 11 empty + 9 success（20 样本、55% empty）→ 冷却。
	for i := 0; i < 9; i++ {
		tr.Record("sina", "quote", outcomeSuccess, 100)
	}
	for i := 0; i < 10; i++ {
		tr.Record("sina", "quote", outcomeEmpty, 100)
		if !tr.Available("sina", "quote") {
			t.Fatalf("第 %d 个 empty 时不应提前冷却（未过半）", i+1)
		}
	}
	tr.Record("sina", "quote", outcomeEmpty, 100)
	if tr.Available("sina", "quote") {
		t.Fatal("empty 占比 11/20 > 50% 应进入冷却")
	}
	// 其他 (源,能力) 不受影响。
	if !tr.Available("sina", "daily_bars") || !tr.Available("eastmoney", "quote") {
		t.Fatal("冷却只作用于对应 (源,能力)")
	}
	// 冷却结束恢复，窗口清零重新观察。
	now = now.Add(healthCooldown + time.Second)
	if !tr.Available("sina", "quote") {
		t.Fatal("冷却期结束应恢复")
	}
	st := healthStatFor(t, tr.Snapshot(), "sina", "quote", "cn")
	if st.Samples != 0 || st.CooldownHits != 1 || !st.Observed || st.Observation != "empty" {
		t.Fatalf("冷却触发时窗口应清零、保留 empty 观测并记一次 cooldown_hits: %+v", st)
	}
}

// TestHealthCooldownOnError error 占比 >30% 触发冷却；恰在阈值内不触发。
func TestHealthCooldownOnError(t *testing.T) {
	tr := NewHealthTracker()
	// 6/20 = 30%：不触发（阈值是严格大于）。
	for i := 0; i < 14; i++ {
		tr.Record("eastmoney", "daily_bars", outcomeSuccess, 50)
	}
	for i := 0; i < 6; i++ {
		tr.Record("eastmoney", "daily_bars", outcomeError, 50)
	}
	if !tr.Available("eastmoney", "daily_bars") {
		t.Fatal("error 占比恰为 30% 不应冷却")
	}
	tr.Record("eastmoney", "daily_bars", outcomeError, 50)
	if tr.Available("eastmoney", "daily_bars") {
		t.Fatal("error 占比 7/21 > 30% 应冷却")
	}
}

// TestHealthMinSamples 样本不足 healthMinSamples 时即使全错也不冷却（冷启动不误杀）。
func TestHealthMinSamples(t *testing.T) {
	tr := NewHealthTracker()
	for i := 0; i < healthMinSamples-1; i++ {
		tr.Record("tencent", "valuation", outcomeError, 10)
	}
	if !tr.Available("tencent", "valuation") {
		t.Fatal("样本不足不应冷却")
	}
}

// TestRouteCapSkipsCooledSource 冷却中的源被跳过、由后备源接管；全部冷却时补跑轮兜底。
func TestRouteCapSkipsCooledSource(t *testing.T) {
	bad := &fakeAdapter{name: "bad", quote: func() (*Quote, error) { return nil, errors.New("boom") }}
	good := &fakeAdapter{name: "good", quote: func() (*Quote, error) { return &Quote{Symbol: "600000"}, nil }}
	m := &Manager{adapters: []Adapter{bad, good}, health: NewHealthTracker()}
	now := time.Now()
	m.health.now = func() time.Time { return now }

	// 人为把 bad 打进冷却。
	for i := 0; i < healthMinSamples+1; i++ {
		m.health.Record("bad", "quote", outcomeError, 1)
	}
	if m.health.Available("bad", "quote") {
		t.Fatal("bad 应处于冷却")
	}
	q, err := m.GetQuote(context.Background(), "cn", "600000")
	if err != nil || q.Symbol != "600000" {
		t.Fatalf("应由 good 接管: %v", err)
	}
	if bad.calls != 0 {
		t.Fatal("冷却中的源不应被调用")
	}

	// 全部冷却：补跑轮仍会尝试（系统不无脑报错）。
	for i := 0; i < healthMinSamples+1; i++ {
		m.health.Record("good", "quote", outcomeError, 1)
	}
	q, err = m.GetQuote(context.Background(), "cn", "600000")
	if err != nil || q == nil {
		t.Fatalf("全冷却时补跑轮应兜底成功: %v", err)
	}
	if bad.calls == 0 {
		t.Fatal("补跑轮应尝试过冷却中的源")
	}
}

// TestRouteCapRecordsOutcome 路由成功/失败会写入健康滑窗。
func TestRouteCapRecordsOutcome(t *testing.T) {
	empty := &fakeAdapter{name: "e", quote: func() (*Quote, error) { return nil, ErrNoData }}
	ok := &fakeAdapter{name: "o", quote: func() (*Quote, error) { return &Quote{}, nil }}
	m := &Manager{adapters: []Adapter{empty, ok}, health: NewHealthTracker()}
	if _, err := m.GetQuote(context.Background(), "cn", "600000"); err != nil {
		t.Fatal(err)
	}
	snap := m.HealthSnapshot()
	emptyStat := healthStatFor(t, snap, "e", "quote", "cn")
	okStat := healthStatFor(t, snap, "o", "quote", "cn")
	if emptyStat.Empty != 1 || emptyStat.Observation != "empty" || okStat.Success != 1 || okStat.Observation != "success" {
		t.Fatalf("结局记录不符: %+v", snap)
	}
}

// TestCapabilityRegistryUnknown 注册表是支持面的唯一来源：支持但从未调用仍必须返回
// unknown，不能因为滑窗没有 key 就把能力当不存在。
func TestCapabilityRegistryUnknown(t *testing.T) {
	tr := NewHealthTracker()
	rows := tr.Snapshot()
	st := healthStatFor(t, rows, "tencent", "valuation", "cn")
	if !st.Registered || !st.Supported || st.Observed || st.Observation != "unknown" || st.Samples != 0 {
		t.Fatalf("未观测注册能力应为 unknown: %+v", st)
	}
	if st.TimeoutMs != 6000 || st.ExpectedFreshness == "" || st.QPSPolicy == "" || st.CacheSemantics == "" {
		t.Fatalf("能力契约字段不完整: %+v", st)
	}
}

// TestUnregisteredObservationDoesNotBecomeSupported 滑窗偶然出现未注册组合只做诊断，
// supported 仍为 false。
func TestUnregisteredObservationDoesNotBecomeSupported(t *testing.T) {
	tr := NewHealthTracker()
	tr.RecordForMarket("custom", "quote", "hk", outcomeSuccess, 12)
	st := healthStatFor(t, tr.Snapshot(), "custom", "quote", "hk")
	if st.Registered || st.Supported || st.Observation != "success" {
		t.Fatalf("观测不得反推支持面: %+v", st)
	}
}

func TestNilSuccessIsRecordedAsEmpty(t *testing.T) {
	empty := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) { return nil, nil }}
	m := NewManagerWithAdapters(empty)
	if _, err := m.GetQuote(context.Background(), "cn", "600000"); !errors.Is(err, ErrNoData) {
		t.Fatalf("nil,nil 应归一为空响应，got %v", err)
	}
	st := healthStatFor(t, m.HealthSnapshot(), "eastmoney", "quote", "cn")
	if st.Empty != 1 || st.Errors != 0 || st.Success != 0 || st.Observation != "empty" {
		t.Fatalf("空响应不能计 success/error: %+v", st)
	}
}

func TestHealthMarketIsolation(t *testing.T) {
	tr := NewHealthTracker()
	tr.RecordForMarket("custom", "quote", "cn", outcomeError, 20)
	if !tr.AvailableForMarket("custom", "quote", "hk") {
		t.Fatal("cn 观测不得污染 hk 维度")
	}
	cn := healthStatFor(t, tr.Snapshot(), "custom", "quote", "cn")
	if cn.Errors != 1 || cn.Market != "cn" {
		t.Fatalf("市场维度记录错误: %+v", cn)
	}
}

// TestClassifyErr 错误归一口径。
func TestClassifyErr(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{ErrNoData, "EMPTY"},
		{context.DeadlineExceeded, "UPSTREAM_TIMEOUT"},
		{errors.New("上游数据源异常: 解析失败 unexpected token"), "PARSE_ERROR"},
		{errors.New("connection reset"), "UPSTREAM_ERROR"},
	}
	for _, c := range cases {
		code, _ := classifyErr(c.err)
		if code != c.code {
			t.Fatalf("%v: 期望 %s 得到 %s", c.err, c.code, code)
		}
	}
}
