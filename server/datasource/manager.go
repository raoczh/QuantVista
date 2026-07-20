package datasource

import (
	"context"
	"errors"
	"strings"
	"time"

	"quantvista/common"
)

// Manager 按优先级编排多个 Adapter：主源失败时回退到下一个，
// 上层只依赖内部标准结构，单源挂掉可整体切换（见 docs/DATA_SOURCES.md）。
// S1 起各能力路由入口统一走 routeCap：健康滑窗踢源 + 两层超时预算 + 错误归一日志。
type Manager struct {
	adapters []Adapter // 按优先级排列，[0] 为主源
	health   *HealthTracker
}

// 两层超时预算：总预算兜底（调用方自带 deadline 时尊重调用方），单源短预算超时即换源。
// 单源预算须小于 doGet 的「2 次尝试 × 8s」最坏耗时，否则一个挂死的源会吃光总预算。
const (
	mgrTotalBudget  = 15 * time.Second
	mgrSourceBudget = 6 * time.Second
)

// DefaultManager 默认编排：东财为主（数据最全），腾讯次之（稳定），新浪兜底（含日线/指数/榜单）。
func DefaultManager() *Manager {
	return &Manager{
		adapters: []Adapter{
			NewEastMoneyAdapter(),
			NewTencentAdapter(),
			NewSinaAdapter(),
		},
		health: NewHealthTracker(),
	}
}

// NewManagerWithAdapters 用指定源顺序构造 Manager（供测试注入假源）。
func NewManagerWithAdapters(adapters ...Adapter) *Manager {
	return &Manager{adapters: adapters, health: NewHealthTracker()}
}

// HealthSnapshot 各 (源,能力) 健康快照（GET /api/admin/datasources）。
func (m *Manager) HealthSnapshot() []HealthStat { return m.health.Snapshot() }

// classifyErr 错误归一：{code, outcome}。code 进日志，outcome 进健康滑窗。
func classifyErr(err error) (string, callOutcome) {
	switch {
	case errors.Is(err, ErrNoData):
		return "EMPTY", outcomeEmpty
	case errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout"):
		return "UPSTREAM_TIMEOUT", outcomeError
	case strings.Contains(err.Error(), "解析失败"):
		return "PARSE_ERROR", outcomeError
	default:
		return "UPSTREAM_ERROR", outcomeError
	}
}

// routeCap 能力路由通用骨架：
//  1. 无 deadline 的调用补总预算，单源再包一层短预算（超预算即换下一源）；
//  2. 冷却中的源先跳过；若一轮下来全军覆没，再对被跳过的源补跑一轮（宁可试坏源也不无脑报错）；
//  3. ErrNotSupported 静默跳过（能力缺失非健康问题）；ErrSymbolInvalid 立即终止（换源无意义）；
//  4. 其余错误归一为 {code, source, latency} 记 DEBUG 日志，并作为滑窗输入。
//
// 单源失败只记 DEBUG（有备源兜底不必刷屏）；全部失败由各入口记一条 WARN。
func routeCap[T any](m *Manager, ctx context.Context, capability string, call func(ctx context.Context, a Adapter) (T, error)) (T, error) {
	var zero T
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mgrTotalBudget)
		defer cancel()
	}

	var lastErr error
	trySource := func(a Adapter) (T, bool, error) {
		return attemptSource(m, ctx, capability, a, call)
	}

	var cooled []Adapter
	for _, a := range m.adapters {
		if ctx.Err() != nil {
			break
		}
		if !m.health.Available(a.Name(), capability) {
			cooled = append(cooled, a)
			continue
		}
		r, ok, err := trySource(a)
		if ok {
			return r, nil
		}
		if errors.Is(err, ErrSymbolInvalid) {
			return zero, err
		}
		if !errors.Is(err, ErrNotSupported) {
			lastErr = err
		}
	}
	// 补跑轮：健康源全部失败/缺席时，冷却中的源作为最后手段仍然一试。
	for _, a := range cooled {
		if ctx.Err() != nil {
			break
		}
		r, ok, err := trySource(a)
		if ok {
			return r, nil
		}
		if errors.Is(err, ErrSymbolInvalid) {
			return zero, err
		}
		if !errors.Is(err, ErrNotSupported) {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoData
	}
	return zero, lastErr
}

// attemptSource 对单个源发起一次能力调用（含单源短预算、错误归一、健康滑窗记录、DEBUG 日志）。
// 从 routeCap 内联闭包提取，供 routeCap 与 GetQuoteFresh 复用；行为与原闭包完全一致。
func attemptSource[T any](m *Manager, ctx context.Context, capability string, a Adapter, call func(ctx context.Context, a Adapter) (T, error)) (T, bool, error) {
	var zero T
	sctx, cancel := context.WithTimeout(ctx, mgrSourceBudget)
	defer cancel()
	start := time.Now()
	r, err := call(sctx, a)
	latency := time.Since(start).Milliseconds()
	if err == nil {
		m.health.Record(a.Name(), capability, outcomeSuccess, latency)
		return r, true, nil
	}
	if errors.Is(err, ErrNotSupported) || errors.Is(err, ErrSymbolInvalid) {
		return zero, false, err // 不记滑窗：非源健康问题
	}
	code, outcome := classifyErr(err)
	m.health.Record(a.Name(), capability, outcome, latency)
	common.SysDebug("[datasource] code=%s source=%s cap=%s latency=%dms err=%v", code, a.Name(), capability, latency, err)
	return zero, false, err
}

// QuoteAccept 判定一个行情是否「足够新鲜」可直接采用。由 service 层按交易日历/时段闭包注入
// （datasource 层不依赖 DB/日历）。
type QuoteAccept func(*Quote) bool

// GetQuoteFresh 取「新鲜行情」：遍历各源，成功但未通过 accept（数据过期）时不当成功，
// 保留 DataTime 最新的候选继续尝试下一源；任一源通过 accept 立即返回 fresh=true。
// 全部源都不新鲜时返回最新候选（fresh=false，err=nil，由调用方按候选自行降级并诚实标注）；
// 全部源取数失败才返回 err。
//
// 与 routeCap 的差异：成功恒记 outcomeSuccess（数据旧不是源健康问题）。冷却源在健康源
// 之后**有界探测**（每源至多一次、共享 ctx 总预算）：健康源全 stale 时冷却源可能恰是
// 唯一有新数据的（如主源限流被冷却、其余源数据停滞），跳过它却对外声称「已尝试全部
// 数据源」是撒谎；停牌/休市场景确实白试一轮，由 service 层 freshMissTTL 短标记兜住
// 重复扫源成本。
func (m *Manager) GetQuoteFresh(ctx context.Context, market, symbol string, accept QuoteAccept) (*Quote, bool, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mgrTotalBudget)
		defer cancel()
	}
	call := func(ctx context.Context, a Adapter) (*Quote, error) {
		return a.GetQuote(ctx, market, symbol)
	}

	var best *Quote
	var lastErr error
	consider := func(q *Quote) bool { // 返回 true 表示已找到新鲜行情，可终止
		if accept != nil && accept(q) {
			best = q
			return true
		}
		if best == nil || (q != nil && q.DataTime.After(best.DataTime)) {
			best = q
		}
		return false
	}

	var cooled []Adapter
	for _, a := range m.adapters {
		if ctx.Err() != nil {
			break
		}
		if !m.health.Available(a.Name(), "quote") {
			cooled = append(cooled, a)
			continue
		}
		q, ok, err := attemptSource(m, ctx, "quote", a, call)
		if ok {
			if consider(q) {
				return best, true, nil
			}
			continue
		}
		if errors.Is(err, ErrSymbolInvalid) {
			return nil, false, err
		}
		if !errors.Is(err, ErrNotSupported) {
			lastErr = err
		}
	}
	// 冷却源有界探测：健康源没找到 fresh（候选缺席或仅有 stale）时，冷却中的源也各试
	// 一次——健康源全 stale 不代表冷却源没有新数据（主源限流冷却场景），且只有真正试过
	// 才配得上「已尝试全部可用数据源」的对外声明。预算由 ctx 总超时约束。
	for _, a := range cooled {
		if ctx.Err() != nil {
			break
		}
		q, ok, err := attemptSource(m, ctx, "quote", a, call)
		if ok {
			if consider(q) {
				return best, true, nil
			}
			continue
		}
		if errors.Is(err, ErrSymbolInvalid) {
			return nil, false, err
		}
		if !errors.Is(err, ErrNotSupported) {
			lastErr = err
		}
	}
	if best != nil {
		return best, false, nil
	}
	if lastErr == nil {
		lastErr = ErrNoData
	}
	return nil, false, lastErr
}

// GetQuote 依次尝试各源，返回首个成功结果（含实际命中的 Source）。
func (m *Manager) GetQuote(ctx context.Context, market, symbol string) (*Quote, error) {
	q, err := routeCap(m, ctx, "quote", func(ctx context.Context, a Adapter) (*Quote, error) {
		return a.GetQuote(ctx, market, symbol)
	})
	if err != nil && !errors.Is(err, ErrSymbolInvalid) {
		common.SysWarn("所有数据源取行情失败 symbol=%s: %v", symbol, err)
	}
	return q, err
}

// GetDailyBars 依次尝试支持日线的源：东财在前（fqt=1 前复权），腾讯不支持日线
// 返回 ErrNotSupported 被跳过，新浪殿后兜底（无复权参数、无成交额）。
func (m *Manager) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	bars, err := routeCap(m, ctx, "daily_bars", func(ctx context.Context, a Adapter) ([]Bar, error) {
		bs, berr := a.GetDailyBars(ctx, market, symbol, limit)
		if berr == nil {
			for i := range bs {
				if bs[i].Source == "" {
					bs[i].Source = a.Name()
				}
			}
		}
		return bs, berr
	})
	if err != nil && !errors.Is(err, ErrSymbolInvalid) {
		common.SysWarn("所有数据源取日线失败 symbol=%s: %v", symbol, err)
	}
	return bars, err
}

// GetIndices 路由到实现 IndexProvider 的源（新浪批量优先）。
func (m *Manager) GetIndices(ctx context.Context, market string) ([]Index, error) {
	return routeCap(m, ctx, "indices", func(ctx context.Context, a Adapter) ([]Index, error) {
		p, ok := a.(IndexProvider)
		if !ok {
			return nil, ErrNotSupported
		}
		return p.GetIndices(ctx, market)
	})
}

// GetStockRanking 路由到实现 RankingProvider 的源（新浪 Market_Center）。
func (m *Manager) GetStockRanking(ctx context.Context, market, sort string, asc bool, limit int) ([]StockRank, error) {
	return routeCap(m, ctx, "ranking", func(ctx context.Context, a Adapter) ([]StockRank, error) {
		p, ok := a.(RankingProvider)
		if !ok {
			return nil, ErrNotSupported
		}
		return p.GetStockRanking(ctx, market, sort, asc, limit)
	})
}

// GetSectorRanking 路由到实现 SectorProvider 的源（东财 clist，best-effort）。
func (m *Manager) GetSectorRanking(ctx context.Context, market string, limit int) ([]SectorRank, error) {
	return routeCap(m, ctx, "sector", func(ctx context.Context, a Adapter) ([]SectorRank, error) {
		p, ok := a.(SectorProvider)
		if !ok {
			return nil, ErrNotSupported
		}
		return p.GetSectorRanking(ctx, market, limit)
	})
}

// GetBreadth 路由到实现 BreadthProvider 的源（东财涨跌分布）。
func (m *Manager) GetBreadth(ctx context.Context, market string) (*Breadth, error) {
	return routeCap(m, ctx, "breadth", func(ctx context.Context, a Adapter) (*Breadth, error) {
		p, ok := a.(BreadthProvider)
		if !ok {
			return nil, ErrNotSupported
		}
		return p.GetBreadth(ctx, market)
	})
}

// GetMarketFundFlow 路由到实现 FundFlowProvider 的源（东财两市资金流）。
func (m *Manager) GetMarketFundFlow(ctx context.Context, market string) (*MarketFundFlow, error) {
	return routeCap(m, ctx, "fundflow", func(ctx context.Context, a Adapter) (*MarketFundFlow, error) {
		p, ok := a.(FundFlowProvider)
		if !ok {
			return nil, ErrNotSupported
		}
		return p.GetMarketFundFlow(ctx, market)
	})
}

// GetTradingDays 路由到实现 TradingDaysProvider 的源（新浪上证指数日线）。
func (m *Manager) GetTradingDays(ctx context.Context, market string, limit int) ([]string, error) {
	return routeCap(m, ctx, "trading_days", func(ctx context.Context, a Adapter) ([]string, error) {
		p, ok := a.(TradingDaysProvider)
		if !ok {
			return nil, ErrNotSupported
		}
		return p.GetTradingDays(ctx, market, limit)
	})
}

// benchmarkResult GetBenchmarkBars 的双返回值打包（routeCap 单泛型参数）。
type benchmarkResult struct {
	name string
	bars []Bar
}

// GetBenchmarkBars 路由到实现 BenchmarkBarsProvider 的源（新浪上证指数日线）。
func (m *Manager) GetBenchmarkBars(ctx context.Context, market string, limit int) (string, []Bar, error) {
	r, err := routeCap(m, ctx, "benchmark", func(ctx context.Context, a Adapter) (benchmarkResult, error) {
		p, ok := a.(BenchmarkBarsProvider)
		if !ok {
			return benchmarkResult{}, ErrNotSupported
		}
		name, bars, berr := p.GetBenchmarkBars(ctx, market, limit)
		return benchmarkResult{name: name, bars: bars}, berr
	})
	return r.name, r.bars, err
}

// GetValuation 路由到实现 ValuationProvider 的源（腾讯行情自带估值字段）。
func (m *Manager) GetValuation(ctx context.Context, market, symbol string) (*Valuation, error) {
	return routeCap(m, ctx, "valuation", func(ctx context.Context, a Adapter) (*Valuation, error) {
		p, ok := a.(ValuationProvider)
		if !ok {
			return nil, ErrNotSupported
		}
		return p.GetValuation(ctx, market, symbol)
	})
}
