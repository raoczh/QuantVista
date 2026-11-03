package datasource

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider 健康滑窗（S1）：每 (源,能力,市场) 一个 50 次环形窗口，记录
// success/empty/error 与延迟。能力是否存在只认 capabilityRegistry；滑窗中出现过调用
// 不能反向证明能力受支持，注册但尚未调用的能力必须显示 unknown。
const (
	healthWindowSize = 50
	healthMinSamples = 20 // 样本不足不判定（冷启动期不误杀）
	healthCooldown   = 300 * time.Second
	healthEmptyMax   = 0.5
	healthErrorMax   = 0.3
)

// CapabilitySpec 是代码内 provider x capability x market 注册表的一行。
// QPSLimit=0 表示上游没有可承诺的硬额度，实际纪律见 QPSPolicy，不能解读为无限 QPS。
type CapabilitySpec struct {
	Source               string  `json:"source"`
	Capability           string  `json:"capability"`
	Market               string  `json:"market"`
	Frequency            string  `json:"frequency"`
	ExpectedFreshnessSec int     `json:"expected_freshness_sec"`
	ExpectedFreshness    string  `json:"expected_freshness"`
	TimeoutMs            int     `json:"timeout_ms"`
	QPSLimit             float64 `json:"qps_limit"`
	QPSPolicy            string  `json:"qps_policy"`
	CacheTTLSeconds      int     `json:"cache_ttl_sec"`
	CacheSemantics       string  `json:"cache_semantics"`
	ProbeDisabled        bool    `json:"probe_disabled,omitempty"`
}

// capabilityRegistry 是支持面的唯一事实来源。这里只登记 Manager 实际可路由的能力；
// An endpoint that cannot meet the consumer's price-basis contract is not advertised.
var capabilityRegistry = []CapabilitySpec{
	{Source: "eastmoney", Capability: "quote", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 10, ExpectedFreshness: "盘中约 10 秒", TimeoutMs: 6000, QPSPolicy: "共享限速和 15 秒总预算", CacheTTLSeconds: 10, CacheSemantics: "进程内缓存并合并请求；命中仍核验源时间"},
	{Source: "eastmoney", Capability: "daily_bars", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "交易日收盘后", TimeoutMs: 6000, QPSPolicy: "批任务逐标的至少间隔 300ms", CacheTTLSeconds: 600, CacheSemantics: "按市场+标的+根数缓存非空序列"},
	{Source: "eastmoney", Capability: "sector", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "盘中约 1 分钟", TimeoutMs: 6000, QPSPolicy: "无硬额度；由市场总览缓存合并请求", CacheTTLSeconds: 15, CacheSemantics: "市场总览聚合缓存"},
	{Source: "sina", Capability: "sector", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "新浪行业涨幅榜；源未提供统一时间", TimeoutMs: 6000, QPSPolicy: "单个行业汇总请求，不逐股扫描", CacheTTLSeconds: 15, CacheSemantics: "市场总览缓存；分类不与东财混接"},
	{Source: "eastmoney", Capability: "board_industry", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "行业热力图；源未提供统一时间", TimeoutMs: 6000, QPSPolicy: "共享东财限速；探测仅一行", CacheTTLSeconds: 60, CacheSemantics: "按板块类型合并请求，失败不缓存"},
	{Source: "eastmoney", Capability: "board_concept", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "概念热力图；源未提供统一时间", TimeoutMs: 6000, QPSPolicy: "共享东财限速；探测仅一行", CacheTTLSeconds: 60, CacheSemantics: "按板块类型合并请求，失败不缓存"},
	{Source: "sina", Capability: "board_industry", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "新浪行业分类；无统一源时间及涨跌家数", TimeoutMs: 6000, QPSPolicy: "一次有界汇总查询，探测取一行，不逐股扫描", CacheTTLSeconds: 60, CacheSemantics: "来源与 sina: 分类代码保留，不冒充 BK 板块"},
	{Source: "sina", Capability: "board_concept", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "新浪概念分类；无统一源时间及涨跌家数", TimeoutMs: 6000, QPSPolicy: "一次有界汇总查询，探测取一行，不逐股扫描", CacheTTLSeconds: 60, CacheSemantics: "来源与 sina: 分类代码保留，不冒充 BK 板块"},
	{Source: "eastmoney", Capability: "board_stocks", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "东财 BK 板块成员行情", TimeoutMs: 6000, QPSPolicy: "单板块 Top50；探测仅一只", CacheTTLSeconds: 60, CacheSemantics: "仅使用所属分类来源，不按同名板块猜映射"},
	{Source: "sina", Capability: "board_stocks", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "新浪分类成员与带日期的真实报价", TimeoutMs: 6000, QPSPolicy: "成员列表与批量报价各一次；探测仅一只", CacheTTLSeconds: 60, CacheSemantics: "保留真实报价日期；不把列表时分秒当今天"},
	{Source: "eastmoney", Capability: "board_bars", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "东财板块指数日线；仍为单源", TimeoutMs: 6000, QPSPolicy: "按需请求；探测一根", CacheTTLSeconds: 600, CacheSemantics: "不同分类的指数不作为替代品"},
	{Source: "eastmoney", Capability: "board_fundflow", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "东财板块资金流；仍为单源", TimeoutMs: 6000, QPSPolicy: "按需请求；探测一根", CacheTTLSeconds: 300, CacheSemantics: "按板块代码缓存真实资金流历史"},
	{Source: "eastmoney", Capability: "breadth", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "盘中约 1 分钟", TimeoutMs: 6000, QPSPolicy: "无硬额度；由市场总览缓存合并请求", CacheTTLSeconds: 15, CacheSemantics: "市场总览聚合缓存"},
	{Source: "eastmoney", Capability: "fundflow", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "交易日盘后", TimeoutMs: 6000, QPSPolicy: "无硬额度；由市场总览缓存合并请求", CacheTTLSeconds: 15, CacheSemantics: "市场总览聚合缓存"},
	{Source: "tencent", Capability: "quote", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 10, ExpectedFreshness: "盘中约 10 秒", TimeoutMs: 6000, QPSPolicy: "共享限速和 15 秒总预算", CacheTTLSeconds: 10, CacheSemantics: "进程内缓存并合并请求；命中仍核验源时间"},
	{Source: "tencent", Capability: "valuation", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "盘中约 1 分钟", TimeoutMs: 6000, QPSPolicy: "无硬额度；按需读取", CacheTTLSeconds: 60, CacheSemantics: "按市场+标的覆盖缓存"},
	{Source: "sina", Capability: "quote", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 10, ExpectedFreshness: "盘中约 10 秒", TimeoutMs: 6000, QPSPolicy: "共享限速和 15 秒总预算", CacheTTLSeconds: 10, CacheSemantics: "进程内缓存并合并请求；命中仍核验源时间"},
	{Source: "sina", Capability: "indices", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 15, ExpectedFreshness: "盘中约 15 秒", TimeoutMs: 6000, QPSPolicy: "无硬额度；批量指数单请求", CacheTTLSeconds: 15, CacheSemantics: "市场总览聚合缓存"},
	{Source: "sina", Capability: "ranking", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "盘中约 1 分钟", TimeoutMs: 6000, QPSPolicy: "无硬额度；按排序条件合并缓存", CacheTTLSeconds: 60, CacheSemantics: "按市场+排序+方向+数量缓存非空榜单"},
	{Source: "sina", Capability: "trading_days", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "最近收盘交易日", TimeoutMs: 6000, QPSPolicy: "仅日历回填按需调用", CacheSemantics: "不上 Redis；结果直接落本地交易日历"},
	{Source: "sina", Capability: "benchmark", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "交易日收盘后", TimeoutMs: 6000, QPSPolicy: "跟踪/评估按需调用", CacheSemantics: "不上独立 Redis；消费方使用本地日线结果"},
	{Source: "tencent", Capability: "daily_bars", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "交易日收盘后；仅前复权 qfqday", TimeoutMs: 6000, QPSPolicy: "共享行情限速；与在线历史共用缓存", CacheTTLSeconds: 600, CacheSemantics: "250 日公共窗口；成交额和换手率缺失不估算"},
	{Source: "tencent", Capability: "indices", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 15, ExpectedFreshness: "盘中约 15 秒", TimeoutMs: 6000, QPSPolicy: "主要指数一次批量请求", CacheTTLSeconds: 15, CacheSemantics: "市场总览进程内缓存；不要求 Redis"},
	{Source: "tencent", Capability: "trading_days", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "已发生交易日；不推测未来休市", TimeoutMs: 6000, QPSPolicy: "仅日历回填按需读取", CacheSemantics: "实际指数日期落库，历史窗口有上限"},
	{Source: "tencent", Capability: "benchmark", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "交易日收盘后", TimeoutMs: 6000, QPSPolicy: "跟踪/评估按需读取", CacheSemantics: "上证指数真实历史；不与深市同号股票混用"},
	{Source: "eastmoney", Capability: "ranking", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "盘中约 1 分钟", TimeoutMs: 6000, QPSPolicy: "与东财快照共享限速", CacheTTLSeconds: 60, CacheSemantics: "排序、方向、数量隔离缓存"},
	{Source: "eastmoney", Capability: "valuation", Market: "cn", Frequency: "realtime", ExpectedFreshnessSec: 60, ExpectedFreshness: "盘中约 1 分钟", TimeoutMs: 6000, QPSPolicy: "与东财行情共享限速", CacheTTLSeconds: 60, CacheSemantics: "仅提供已核实字段；缺失保留"},
	{Source: "tencent", Capability: "min1", Market: "cn", Frequency: "intraday", ExpectedFreshnessSec: 60, ExpectedFreshness: "真实 1 分钟线", TimeoutMs: 6000, QPSPolicy: "共享限速；同标的请求合并", CacheTTLSeconds: 60, CacheSemantics: "分时图有界进程缓存；均价仍为估算"},
	{Source: "tencent", Capability: "min5", Market: "cn", Frequency: "intraday", ExpectedFreshnessSec: 300, ExpectedFreshness: "真实 5 分钟线", TimeoutMs: 6000, QPSPolicy: "盘后任务共享限速", CacheSemantics: "完整交易日聚合后落库；不合成缺根"},
	{Source: "sina", Capability: "min5", Market: "cn", Frequency: "intraday", ExpectedFreshnessSec: 300, ExpectedFreshness: "真实 5 分钟线备用", TimeoutMs: 6000, QPSPolicy: "新浪请求共享限速", CacheSemantics: "股转换为手；不能冒充 1 分钟数据"},
	{Source: "eastmoney", Capability: "announcements", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 3600, ExpectedFreshness: "按需或盘后更新", TimeoutMs: 6000, QPSPolicy: "关注标的采集与按需刷新合并", CacheTTLSeconds: 3600, CacheSemantics: "公告数据库缓存；保留原发布日期和来源"},
	{Source: "cninfo", Capability: "announcements", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 3600, ExpectedFreshness: "巨潮公开披露公告", TimeoutMs: 6000, QPSPolicy: "按需查询；免 token；不下载全文", CacheTTLSeconds: 3600, CacheSemantics: "核对证券代码与机构标识；保留官方 PDF 链接"},
	{Source: "eastmoney", Capability: "spot_snapshot", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "盘后完整沪深 A 股快照", TimeoutMs: 240000, QPSPolicy: "分页串行；共享东财限速", CacheSemantics: "完整性验证后落库；请使用全市场同步任务，不做全量按钮探测", ProbeDisabled: true},
	{Source: "sina", Capability: "spot_snapshot", Market: "cn", Frequency: "daily", ExpectedFreshnessSec: 86400, ExpectedFreshness: "盘后完整沪深 A 股备用快照", TimeoutMs: 240000, QPSPolicy: "分页与百股报价串行；共享新浪限速", CacheSemantics: "核对首尾计数、唯一代码及逐股时间；请使用全市场同步任务", ProbeDisabled: true},
}

// CapabilityRegistry 返回注册表副本，避免调用方修改进程内支持声明。
func CapabilityRegistry() []CapabilitySpec {
	out := make([]CapabilitySpec, len(capabilityRegistry))
	copy(out, capabilityRegistry)
	return out
}

// callOutcome 单次调用结局（滑窗输入）。
type callOutcome int

const (
	outcomeSuccess callOutcome = iota
	outcomeEmpty               // 上游正常但无数据（ErrNoData）
	outcomeError               // 超时/网络/解析等错误
)

func outcomeName(v callOutcome) string {
	switch v {
	case outcomeSuccess:
		return "success"
	case outcomeEmpty:
		return "empty"
	default:
		return "error"
	}
}

type healthRec struct {
	outcome   callOutcome
	latencyMs int64
}

// healthWindow 单 (源,能力,市场) 的环形窗口与跨窗口观测状态。
type healthWindow struct {
	ring           [healthWindowSize]healthRec
	idx            int
	filled         int
	cooldownUntil  time.Time
	cooldownHits   int
	hasObserved    bool
	lastOutcome    callOutcome
	lastObservedAt time.Time
	lastSuccessAt  time.Time
}

// HealthTracker 全部 (源,能力,市场) 的健康状态。并发安全。
type HealthTracker struct {
	mu      sync.Mutex
	windows map[string]*healthWindow
	now     func() time.Time // 可注入时钟（单测）
}

func NewHealthTracker() *HealthTracker {
	return &HealthTracker{windows: map[string]*healthWindow{}, now: time.Now}
}

func healthKey(source, capability, market string) string {
	return source + "|" + capability + "|" + market
}

// Record 保留旧测试/包内调用的默认 cn 入口。
func (t *HealthTracker) Record(source, capability string, outcome callOutcome, latencyMs int64) {
	t.RecordForMarket(source, capability, "cn", outcome, latencyMs)
}

// RecordForMarket 记一次调用结局；样本足够且劣化超阈值时进入冷却并清空滑窗。
// last outcome / last success 跨清窗保留，避免冷却后被误显示为“从未观测”。
func (t *HealthTracker) RecordForMarket(source, capability, market string, outcome callOutcome, latencyMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	key := healthKey(source, capability, market)
	w := t.windows[key]
	if w == nil {
		w = &healthWindow{}
		t.windows[key] = w
	}
	w.hasObserved = true
	w.lastOutcome = outcome
	w.lastObservedAt = now
	if outcome == outcomeSuccess {
		w.lastSuccessAt = now
	}
	w.ring[w.idx] = healthRec{outcome: outcome, latencyMs: latencyMs}
	w.idx = (w.idx + 1) % healthWindowSize
	if w.filled < healthWindowSize {
		w.filled++
	}
	if w.filled < healthMinSamples {
		return
	}
	var empty, errs int
	for i := 0; i < w.filled; i++ {
		switch w.ring[i].outcome {
		case outcomeEmpty:
			empty++
		case outcomeError:
			errs++
		}
	}
	n := float64(w.filled)
	if float64(empty)/n > healthEmptyMax || float64(errs)/n > healthErrorMax {
		w.cooldownUntil = now.Add(healthCooldown)
		w.cooldownHits++
		w.idx, w.filled = 0, 0
	}
}

// Available 保留默认 cn 入口。
func (t *HealthTracker) Available(source, capability string) bool {
	return t.AvailableForMarket(source, capability, "cn")
}

// AvailableForMarket 返回该组合当前是否可参与轮询。
func (t *HealthTracker) AvailableForMarket(source, capability, market string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.windows[healthKey(source, capability, market)]
	return w == nil || !t.now().Before(w.cooldownUntil)
}

// ClearCooldownForMarket 只清除指定三元组的 cooldownUntil，保留环形窗口、
// last outcome 和 cooldown 次数，便于管理员解冷后继续观察同一历史。
// 返回解冷前剩余秒数与是否实际清除了冷却。
func (t *HealthTracker) ClearCooldownForMarket(source, capability, market string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.windows[healthKey(source, capability, market)]
	if w == nil {
		return 0, false
	}
	now := t.now()
	left := w.cooldownUntil.Sub(now)
	if left <= 0 {
		return 0, false
	}
	seconds := int(math.Ceil(left.Seconds()))
	w.cooldownUntil = time.Time{}
	return seconds, true
}

// HealthStat 健康端点（GET /api/admin/datasources）的单行能力矩阵。
type HealthStat struct {
	CapabilitySpec
	Registered     bool   `json:"registered"`
	Supported      bool   `json:"supported"`
	Observation    string `json:"observation"` // unknown / success / empty / error
	Observed       bool   `json:"observed"`
	Samples        int    `json:"samples"`
	Success        int    `json:"success"`
	Empty          int    `json:"empty"`
	Errors         int    `json:"errors"`
	AvgLatencyMs   int64  `json:"avg_latency_ms"`
	LastObservedAt string `json:"last_observed_at,omitempty"`
	LastSuccessAt  string `json:"last_success_at,omitempty"`
	Available      bool   `json:"available"`
	CooldownLeft   int    `json:"cooldown_left_sec"`
	CooldownHits   int    `json:"cooldown_hits"`
	RecoveryAdvice string `json:"recovery_advice"`
}

func healthStatOf(spec CapabilitySpec, registered bool, w *healthWindow, now time.Time) HealthStat {
	st := HealthStat{
		CapabilitySpec: spec,
		Registered:     registered,
		Supported:      registered,
		Observation:    "unknown",
		Available:      true,
	}
	if w != nil {
		st.Observed = w.hasObserved
		if w.hasObserved {
			st.Observation = outcomeName(w.lastOutcome)
			st.LastObservedAt = w.lastObservedAt.Format(time.RFC3339)
			if !w.lastSuccessAt.IsZero() {
				st.LastSuccessAt = w.lastSuccessAt.Format(time.RFC3339)
			}
		}
		st.Samples = w.filled
		var totalLatency int64
		for i := 0; i < w.filled; i++ {
			switch w.ring[i].outcome {
			case outcomeSuccess:
				st.Success++
			case outcomeEmpty:
				st.Empty++
			case outcomeError:
				st.Errors++
			}
			totalLatency += w.ring[i].latencyMs
		}
		if w.filled > 0 {
			st.AvgLatencyMs = totalLatency / int64(w.filled)
		}
		st.Available = !now.Before(w.cooldownUntil)
		if left := w.cooldownUntil.Sub(now); left > 0 {
			st.CooldownLeft = int(math.Ceil(left.Seconds()))
		}
		st.CooldownHits = w.cooldownHits
	}
	st.RecoveryAdvice = recoveryAdvice(st)
	return st
}

func recoveryAdvice(st HealthStat) string {
	if !st.Registered {
		return "注册表未声明该组合；历史观测不等于能力受支持，请先补注册与契约测试"
	}
	if !st.Observed {
		if st.ProbeDisabled {
			return "等待盘后全市场同步任务形成完整性观测；普通探测不会发起全市场扫描"
		}
		return "等待正常业务调用形成观测，或由管理员执行一次受控单能力探测"
	}
	if st.CooldownLeft > 0 {
		return fmt.Sprintf("等待冷却 %d 秒后自动恢复；确认上游恢复后可由管理员填写原因并解除本能力冷却", st.CooldownLeft)
	}
	switch st.Observation {
	case "empty":
		return "核对交易时段、市场和标的范围；空响应与上游错误分开处理"
	case "error":
		return "检查上游连通、限流和解析日志；可用既有补采动作在 dry-run 后重试"
	default:
		return "当前无需恢复操作"
	}
}

// Snapshot 合并代码注册表与健康滑窗。注册但无窗口的能力保留为 unknown；窗口中出现但
// 未注册的组合也会作为 unsupported 诊断行返回，明确提示“观测不等于支持”。
func (t *HealthTracker) Snapshot() []HealthStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	out := make([]HealthStat, 0, len(capabilityRegistry)+len(t.windows))
	registered := make(map[string]struct{}, len(capabilityRegistry))
	for _, spec := range capabilityRegistry {
		key := healthKey(spec.Source, spec.Capability, spec.Market)
		registered[key] = struct{}{}
		out = append(out, healthStatOf(spec, true, t.windows[key], now))
	}
	for key, w := range t.windows {
		if _, ok := registered[key]; ok {
			continue
		}
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 {
			continue
		}
		spec := CapabilitySpec{Source: parts[0], Capability: parts[1], Market: parts[2]}
		out = append(out, healthStatOf(spec, false, w, now))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Capability != out[j].Capability {
			return out[i].Capability < out[j].Capability
		}
		return out[i].Market < out[j].Market
	})
	return out
}
