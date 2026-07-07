package datasource

import (
	"sort"
	"sync"
	"time"
)

// Provider 健康滑窗（S1）：每 (源,数据类型) 一个 50 次环形窗口，记录 success/empty/error
// 与延迟。empty 占比 >50% 或 error 占比 >30%（样本 ≥healthMinSamples）时冷却 300s——
// 冷却期内该源在 Manager 能力路由中被跳过（除非所有源都被冷却，见 routeCap 的补跑轮），
// 避免持续把请求打向一个明显坏掉/被限流的源、白白消耗上层超时预算。
const (
	healthWindowSize = 50
	healthMinSamples = 20 // 样本不足不判定（冷启动期不误杀）
	healthCooldown   = 300 * time.Second
	healthEmptyMax   = 0.5
	healthErrorMax   = 0.3
)

// callOutcome 单次调用结局（滑窗输入）。
type callOutcome int

const (
	outcomeSuccess callOutcome = iota
	outcomeEmpty                // 上游正常但无数据（ErrNoData）
	outcomeError                // 超时/网络/解析等错误
)

type healthRec struct {
	outcome   callOutcome
	latencyMs int64
}

// healthWindow 单 (源,能力) 的环形窗口与冷却状态。
type healthWindow struct {
	ring          [healthWindowSize]healthRec
	idx           int
	filled        int
	cooldownUntil time.Time
	cooldownHits  int // 累计被踢出轮询次数（观测用）
}

// HealthTracker 全部 (源,能力) 的健康状态。并发安全。
type HealthTracker struct {
	mu      sync.Mutex
	windows map[string]*healthWindow
	now     func() time.Time // 可注入时钟（单测）
}

func NewHealthTracker() *HealthTracker {
	return &HealthTracker{windows: map[string]*healthWindow{}, now: time.Now}
}

func healthKey(source, capability string) string { return source + "|" + capability }

// Record 记一次调用结局；样本足够且劣化超阈值时进入冷却并清空窗口
// （冷却结束后从零观察，避免旧的坏样本让源刚恢复就再次被踢）。
func (t *HealthTracker) Record(source, capability string, outcome callOutcome, latencyMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.windows[healthKey(source, capability)]
	if w == nil {
		w = &healthWindow{}
		t.windows[healthKey(source, capability)] = w
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
		w.cooldownUntil = t.now().Add(healthCooldown)
		w.cooldownHits++
		w.idx, w.filled = 0, 0
	}
}

// Available 该 (源,能力) 当前是否可参与轮询（冷却期内为 false）。
func (t *HealthTracker) Available(source, capability string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.windows[healthKey(source, capability)]
	if w == nil {
		return true
	}
	return t.now().After(w.cooldownUntil)
}

// HealthStat 健康端点（GET /api/admin/datasources）的单行快照。
type HealthStat struct {
	Source       string `json:"source"`
	Capability   string `json:"capability"`
	Samples      int    `json:"samples"`
	Success      int    `json:"success"`
	Empty        int    `json:"empty"`
	Errors       int    `json:"errors"`
	AvgLatencyMs int64  `json:"avg_latency_ms"`
	Available    bool   `json:"available"`
	CooldownLeft int    `json:"cooldown_left_sec"` // >0 表示冷却剩余秒数
	CooldownHits int    `json:"cooldown_hits"`
}

// Snapshot 全部 (源,能力) 的健康快照（按 source+capability 排序，稳定输出）。
func (t *HealthTracker) Snapshot() []HealthStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]HealthStat, 0, len(t.windows))
	for key, w := range t.windows {
		var st HealthStat
		for i := 0; i < len(key); i++ {
			if key[i] == '|' {
				st.Source, st.Capability = key[:i], key[i+1:]
				break
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
		st.Available = t.now().After(w.cooldownUntil)
		if left := w.cooldownUntil.Sub(t.now()); left > 0 {
			st.CooldownLeft = int(left.Seconds())
		}
		st.CooldownHits = w.cooldownHits
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Capability < out[j].Capability
	})
	return out
}
