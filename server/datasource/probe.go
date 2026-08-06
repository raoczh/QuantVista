package datasource

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// ProbeOutcome 是主动探测的三态结果。结果只包含计数和归一化代码，绝不携带
// 上游响应、请求正文或凭据。
type ProbeOutcome string

const (
	ProbeSuccess ProbeOutcome = "success"
	ProbeEmpty   ProbeOutcome = "empty"
	ProbeError   ProbeOutcome = "error"
)

// ProbeResult 是管理员主动探测的安全摘要。
type ProbeResult struct {
	Source      string       `json:"source"`
	Capability  string       `json:"capability"`
	Market      string       `json:"market"`
	Outcome     ProbeOutcome `json:"outcome"`
	Code        string       `json:"code"`
	LatencyMs   int64        `json:"latency_ms"`
	SampleCount int          `json:"sample_count"`
}

// ProbeFailure 对上游错误只暴露固定代码；cause 仅用于 errors.Is，不能通过 Error
// 或 JSON 序列化泄露原始错误内容。
type ProbeFailure struct {
	Code  string
	cause error
}

func (e *ProbeFailure) Error() string { return e.Code }
func (e *ProbeFailure) Unwrap() error { return e.cause }

const (
	probeGlobalWindow      = time.Minute
	probeGlobalMax         = 12
	probeTupleInterval     = 15 * time.Second
	probeGlobalConcurrency = 2
	probeDefaultTimeout    = 5 * time.Second
	probeMaxTimeout        = 8 * time.Second
	probeSampleSymbol      = "600000"
)

var (
	globalProbeMu     sync.Mutex
	globalProbeStarts []time.Time
	globalProbeSem    = make(chan struct{}, probeGlobalConcurrency)
)

// ResetProbeLimiterForTest 清空进程级探测限流状态，供 datasource 包测试隔离使用。
// 业务代码不应调用此函数。
func ResetProbeLimiterForTest() {
	globalProbeMu.Lock()
	globalProbeStarts = nil
	globalProbeMu.Unlock()
	for {
		select {
		case <-globalProbeSem:
		default:
			return
		}
	}
}

func registeredCapability(source, capability, market string) (CapabilitySpec, bool) {
	source = strings.ToLower(strings.TrimSpace(source))
	capability = strings.ToLower(strings.TrimSpace(capability))
	market = strings.ToLower(strings.TrimSpace(market))
	for _, spec := range capabilityRegistry {
		if spec.Source == source && spec.Capability == capability && spec.Market == market {
			return spec, true
		}
	}
	return CapabilitySpec{}, false
}

// LookupCapability 按代码注册表精确查找三元组并返回规范值。管理员运维入口在
// 写审计前先调用它，避免把未经白名单确认的请求字段写入日志。
func LookupCapability(source, capability, market string) (CapabilitySpec, bool) {
	return registeredCapability(source, capability, market)
}

func (m *Manager) probeAdapter(source string) Adapter {
	for _, a := range m.adapters {
		if a != nil && strings.EqualFold(a.Name(), source) {
			return a
		}
	}
	return nil
}

func (m *Manager) beginProbe(ctx context.Context, key string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.probeMu.Lock()
	if m.probeBusy == nil {
		m.probeBusy = make(map[string]struct{})
	}
	if m.probeLast == nil {
		m.probeLast = make(map[string]time.Time)
	}
	now := time.Now()
	if _, ok := m.probeBusy[key]; ok {
		m.probeMu.Unlock()
		return nil, ErrProbeBusy
	}
	if last := m.probeLast[key]; !last.IsZero() && now.Sub(last) < probeTupleInterval {
		m.probeMu.Unlock()
		return nil, ErrProbeRateLimited
	}
	m.probeBusy[key] = struct{}{}
	m.probeMu.Unlock()

	// 全局窗口与并发闸门均在实际调用前占用；失败时释放三元组占位。
	globalProbeMu.Lock()
	cutoff := now.Add(-probeGlobalWindow)
	kept := globalProbeStarts[:0]
	for _, started := range globalProbeStarts {
		if started.After(cutoff) {
			kept = append(kept, started)
		}
	}
	globalProbeStarts = kept
	if len(globalProbeStarts) >= probeGlobalMax {
		globalProbeMu.Unlock()
		m.finishProbe(key, false)
		return nil, ErrProbeRateLimited
	}
	globalProbeStarts = append(globalProbeStarts, now)
	globalProbeMu.Unlock()

	select {
	case globalProbeSem <- struct{}{}:
		return func() {
			<-globalProbeSem
			m.finishProbe(key, true)
		}, nil
	case <-ctx.Done():
		m.finishProbe(key, false)
		return nil, ctx.Err()
	}
}

func (m *Manager) finishProbe(key string, completed bool) {
	m.probeMu.Lock()
	delete(m.probeBusy, key)
	if completed {
		if m.probeLast == nil {
			m.probeLast = make(map[string]time.Time)
		}
		m.probeLast[key] = time.Now()
	}
	m.probeMu.Unlock()
}

func probeTimeout(spec CapabilitySpec) time.Duration {
	d := probeDefaultTimeout
	if spec.TimeoutMs > 0 {
		d = time.Duration(spec.TimeoutMs) * time.Millisecond
	}
	if d > probeMaxTimeout {
		d = probeMaxTimeout
	}
	if d <= 0 {
		d = probeDefaultTimeout
	}
	return d
}

// probeCall 只调用一个适配器、一个固定样本；所有列表能力的 limit 都固定为 1。
func probeCall(ctx context.Context, a Adapter, spec CapabilitySpec) error {
	market := spec.Market
	switch spec.Capability {
	case "quote":
		v, err := a.GetQuote(ctx, market, probeSampleSymbol)
		if err == nil && v == nil {
			err = ErrNoData
		}
		return err
	case "daily_bars":
		v, err := a.GetDailyBars(ctx, market, probeSampleSymbol, 1)
		if err == nil && len(v) == 0 {
			err = ErrNoData
		}
		return err
	case "sector":
		p, ok := a.(SectorProvider)
		if !ok {
			return ErrNotSupported
		}
		v, err := p.GetSectorRanking(ctx, market, 1)
		if err == nil && len(v) == 0 {
			err = ErrNoData
		}
		return err
	case "breadth":
		p, ok := a.(BreadthProvider)
		if !ok {
			return ErrNotSupported
		}
		v, err := p.GetBreadth(ctx, market)
		if err == nil && v == nil {
			err = ErrNoData
		}
		return err
	case "fundflow":
		p, ok := a.(FundFlowProvider)
		if !ok {
			return ErrNotSupported
		}
		v, err := p.GetMarketFundFlow(ctx, market)
		if err == nil && v == nil {
			err = ErrNoData
		}
		return err
	case "valuation":
		p, ok := a.(ValuationProvider)
		if !ok {
			return ErrNotSupported
		}
		v, err := p.GetValuation(ctx, market, probeSampleSymbol)
		if err == nil && v == nil {
			err = ErrNoData
		}
		return err
	case "indices":
		p, ok := a.(IndexProvider)
		if !ok {
			return ErrNotSupported
		}
		v, err := p.GetIndices(ctx, market)
		if err == nil && len(v) == 0 {
			err = ErrNoData
		}
		return err
	case "ranking":
		p, ok := a.(RankingProvider)
		if !ok {
			return ErrNotSupported
		}
		v, err := p.GetStockRanking(ctx, market, "amount", false, 1)
		if err == nil && len(v) == 0 {
			err = ErrNoData
		}
		return err
	case "trading_days":
		p, ok := a.(TradingDaysProvider)
		if !ok {
			return ErrNotSupported
		}
		v, err := p.GetTradingDays(ctx, market, 1)
		if err == nil && len(v) == 0 {
			err = ErrNoData
		}
		return err
	case "benchmark":
		p, ok := a.(BenchmarkBarsProvider)
		if !ok {
			return ErrNotSupported
		}
		_, v, err := p.GetBenchmarkBars(ctx, market, 1)
		if err == nil && len(v) == 0 {
			err = ErrNoData
		}
		return err
	default:
		return ErrProbeNotAllowed
	}
}

func probeCode(err error) (string, ProbeOutcome) {
	if err == nil {
		return "OK", ProbeSuccess
	}
	if errors.Is(err, ErrNoData) {
		return "EMPTY", ProbeEmpty
	}
	if errors.Is(err, ErrNotSupported) {
		return "NOT_SUPPORTED", ProbeError
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "UPSTREAM_TIMEOUT", ProbeError
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELED", ProbeError
	}
	return "UPSTREAM_ERROR", ProbeError
}

// Probe 对注册表中的一个 provider/capability/market 做一次有界样本探测。
// 该方法不走 routeCap，避免探测指定源时悄悄切换到备用源。
func (m *Manager) Probe(ctx context.Context, source, capability, market string) (ProbeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	source = strings.ToLower(strings.TrimSpace(source))
	capability = strings.ToLower(strings.TrimSpace(capability))
	market = strings.ToLower(strings.TrimSpace(market))
	result := ProbeResult{Source: source, Capability: capability, Market: market}
	spec, ok := registeredCapability(source, capability, market)
	if !ok {
		result.Outcome, result.Code = ProbeError, "NOT_REGISTERED"
		return result, ErrProbeNotAllowed
	}
	a := m.probeAdapter(source)
	if a == nil {
		result.Outcome, result.Code = ProbeError, "SOURCE_UNAVAILABLE"
		return result, ErrProbeUnavailable
	}
	release, err := m.beginProbe(ctx, source+"|"+capability+"|"+market)
	if err != nil {
		result.Outcome = ProbeError
		switch {
		case errors.Is(err, ErrProbeRateLimited):
			result.Code = "PROBE_RATE_LIMITED"
		case errors.Is(err, ErrProbeBusy):
			result.Code = "PROBE_BUSY"
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			result.Code = "PROBE_ADMISSION_TIMEOUT"
		default:
			result.Code = "PROBE_UNAVAILABLE"
		}
		return result, err
	}
	defer release()

	pctx, cancel := context.WithTimeout(ctx, probeTimeout(spec))
	defer cancel()
	result.SampleCount = 1
	started := time.Now()
	err = probeCall(pctx, a, spec)
	result.LatencyMs = time.Since(started).Milliseconds()
	result.Code, result.Outcome = probeCode(err)
	if m.health != nil {
		if err == nil {
			m.health.RecordForMarket(source, capability, market, outcomeSuccess, result.LatencyMs)
		} else if errors.Is(err, ErrNoData) {
			m.health.RecordForMarket(source, capability, market, outcomeEmpty, result.LatencyMs)
		} else {
			m.health.RecordForMarket(source, capability, market, outcomeError, result.LatencyMs)
		}
	}
	if err == nil || errors.Is(err, ErrNoData) {
		return result, nil
	}
	// Do not wrap validation/rate errors here; callers can use errors.Is safely.
	return result, &ProbeFailure{Code: result.Code, cause: err}
}

// ClearCooldown 清理且仅清理注册表中指定三元组的 cooldown。
func (m *Manager) ClearCooldown(source, capability, market string) (int, bool, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	capability = strings.ToLower(strings.TrimSpace(capability))
	market = strings.ToLower(strings.TrimSpace(market))
	if _, ok := registeredCapability(source, capability, market); !ok {
		return 0, false, ErrProbeNotAllowed
	}
	if m.health == nil {
		return 0, false, ErrProbeUnavailable
	}
	left, cleared := m.health.ClearCooldownForMarket(source, capability, market)
	return left, cleared, nil
}
