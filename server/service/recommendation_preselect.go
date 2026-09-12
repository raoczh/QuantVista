package service

import (
	"math"
	"sort"
	"strings"
	"time"
)

const recommendationPreselectionVersion = "ps1"

// 预选只读取本地收盘因子，负责决定有限的富化预算；不能冒充最终推荐分。
type recPreselection struct {
	Version   string             `json:"version"`
	Profile   string             `json:"profile"`
	TradeDate string             `json:"trade_date"`
	Status    string             `json:"status"` // ready / partial / illiquid
	Score     *float64           `json:"score,omitempty"`
	Rank      int                `json:"rank,omitempty"`
	Matched   int                `json:"matched,omitempty"`
	Retained  int                `json:"retained,omitempty"`
	Truncated int                `json:"truncated,omitempty"`
	Values    map[string]float64 `json:"values,omitempty"`
	Missing   []string           `json:"missing,omitempty"`
}

func bounded(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func recommendationWidePreselection(t *FactorTable, i int, profile string) *recPreselection {
	out := &recPreselection{Version: recommendationPreselectionVersion, Profile: profile, TradeDate: t.LastDates[i], Status: "ready", Values: map[string]float64{}}
	get := func(key string) (float64, bool) {
		col := t.Col(key)
		if i >= len(col) || !finiteRecNumber(col[i]) {
			out.Missing = append(out.Missing, key)
			return 0, false
		}
		out.Values[key] = col[i]
		return col[i], true
	}
	price, priceOK := get("close")
	amount, amountOK := get("amount_yi")
	if !priceOK || price <= 0 || !t.Fresh(i) {
		out.Status = "partial"
		return out
	}
	// 已知低流动性放到预选队尾；最终准入仍以当前有效行情与用户条件复核。
	if amountOK && amount < 0.3 {
		out.Status = "illiquid"
	}
	trend, momentum, volume, risk, entry := 50.0, 50.0, 50.0, 50.0, 50.0
	if v, ok := get("above_ma20"); ok {
		trend += 20 * (2*v - 1)
	}
	if v, ok := get("above_ma60"); ok {
		trend += 15 * (2*v - 1)
	}
	if v, ok := get("bull_align"); ok {
		trend += 15 * v
	}
	if v, ok := get("chg_5d"); ok {
		momentum += bounded(v*2, -20, 20)
	}
	if v, ok := get("chg_20d"); ok {
		momentum += bounded(v, -20, 20)
	}
	if v, ok := get("vol_boost"); ok {
		volume += 25 * math.Exp(-math.Pow((v-1.8)/1.5, 2))
		if v > 5 {
			volume -= bounded((v-5)*6, 0, 25)
		}
	}
	if v, ok := get("volatility_20"); ok {
		risk = bounded(100-v*12, 0, 100)
	}
	if v, ok := get("drawdown_20"); ok {
		risk -= bounded(v*1.5, 0, 40)
	}
	atr, atrOK := get("atr_pct")
	bias, biasOK := get("bias_20")
	if atrOK && atr > 0 && biasOK {
		distance := bias / atr
		// ATR 归一化使不同波动率股票可比较；不统一压低所有涨幅。
		entry = bounded(100-math.Abs(distance)*22, 0, 100)
		if profile == "momentum" || profile == "growth" {
			entry = bounded(100-math.Max(0, distance-1)*25, 0, 100)
		}
		if distance < -1.5 && profile != "value" {
			entry -= 20
		}
	}
	if profile == "pullback" || profile == "leader" {
		if v, ok := get("vol_5v20"); ok && v > 0 && v < 1 {
			volume = 70
		}
	}
	// 预选没有 PIT 财务面，value/leader 只按流动性、风险和支撑距离分配研究预算。
	wt, wm, we, wv, wr := strategyDimWeights("", profile)
	if profile == "momentum" || profile == "growth" {
		wm -= 0.10
		we += 0.10
	}
	score := wt*bounded(trend, 0, 100) + wm*bounded(momentum, 0, 100) + we*bounded(entry, 0, 100) + wv*bounded(volume, 0, 100) + wr*bounded(risk, 0, 100)
	// 缺失维度不伪装完整，折扣只用于预算优先级，缺口随事实保留。
	if len(out.Missing) > 0 {
		score -= math.Min(float64(len(out.Missing))*2, 12)
		if out.Status == "ready" {
			out.Status = "partial"
		}
	}
	score, _ = normalizeRankingScore(score)
	out.Score = &score
	return out
}

func preselectionBefore(a, b *recPreselection) (bool, bool) {
	if a == nil || a.Score == nil {
		if b != nil && b.Score != nil {
			return false, true
		}
		return false, false
	}
	if b == nil || b.Score == nil {
		return true, true
	}
	if (a.Status == "illiquid") != (b.Status == "illiquid") {
		return a.Status != "illiquid", true
	}
	if *a.Score != *b.Score {
		return *a.Score > *b.Score, true
	}
	return false, false
}

func rankRecommendationScan(t *FactorTable, idxs []int, profile string, limit int) map[int]*recPreselection {
	facts := make(map[int]*recPreselection, len(idxs))
	for _, i := range idxs {
		facts[i] = recommendationWidePreselection(t, i, profile)
	}
	sort.SliceStable(idxs, func(a, b int) bool {
		if before, decided := preselectionBefore(facts[idxs[a]], facts[idxs[b]]); decided {
			return before
		}
		return t.Symbols[idxs[a]] < t.Symbols[idxs[b]]
	})
	retained := len(idxs)
	if retained > limit {
		retained = limit
	}
	for rank, i := range idxs {
		facts[i].Rank = rank + 1
		facts[i].Matched = len(idxs)
		facts[i].Retained = retained
		facts[i].Truncated = len(idxs) - retained
	}
	return facts
}

// 使用已经发布的不可变宽表，不在推荐请求里额外触发一次全市场重建。
func preselectRecommendationPool(pool []candidate, profile string) {
	factorTableMu.RLock()
	t := factorTableCur
	factorTableMu.RUnlock()
	if t == nil {
		return
	}
	if t.LagOpenDays < 0 || t.TradeDate < wideExpectedDate(time.Now()) {
		return
	}
	bySymbol := make(map[string]int, t.Len())
	for i, symbol := range t.Symbols {
		bySymbol[symbol] = i
	}
	for i := range pool {
		if pool[i].Excluded != "" || pool[i].Preselection != nil {
			continue
		}
		row, ok := bySymbol[pool[i].Symbol]
		if !ok {
			continue
		}
		date := t.LastDates[row]
		asOf := pool[i].QuoteAsOf
		if asOf == "" {
			asOf = time.Now().In(time.Local).Format("2006-01-02 15:04")
		}
		if len(asOf) < 16 || date > asOf[:10] || (date == asOf[:10] && asOf[11:] < "15:00") {
			continue
		}
		pool[i].Preselection = recommendationWidePreselection(t, row, profile)
	}
}

type recScanBudget struct {
	Version       string `json:"version"`
	Source        string `json:"source"`
	Order         int    `json:"order"`
	Limit         int    `json:"limit"`
	OmittedSource int    `json:"omitted_source,omitempty"` // 极大自选在观察阶段超出上限，单列计数
}

type recSourceCoverage struct {
	Source   string `json:"source"`
	Observed int    `json:"observed"`
	Intake   int    `json:"intake"`
	Scored   int    `json:"scored"`
	Ranked   int    `json:"ranked"`
	Sent     int    `json:"sent"`
	Omitted  int    `json:"omitted"`
}

func recommendationSourceCoverage(pool []candidate) []recSourceCoverage {
	bySource := map[string]*recSourceCoverage{}
	for _, c := range pool {
		for _, src := range c.Sources {
			row := bySource[src]
			if row == nil {
				row = &recSourceCoverage{Source: src}
				bySource[src] = row
			}
			row.Observed++
			if c.IntakeBudget != nil && c.IntakeBudget.Order <= c.IntakeBudget.Limit {
				row.Intake++
			}
			if c.ScoreDims != nil {
				row.Scored++
			}
			if c.Rank > 0 {
				row.Ranked++
			}
			if c.SentToLLM {
				row.Sent++
			}
			if c.IntakeBudget != nil && c.IntakeBudget.OmittedSource > row.Omitted && src == "watchlist" {
				row.Omitted = c.IntakeBudget.OmittedSource
			}
		}
	}
	out := make([]recSourceCoverage, 0, len(bySource))
	for _, row := range bySource {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func sortRecommendationRefill(pool []candidate) []int {
	idxs := make([]int, 0)
	for i := range pool {
		if strings.HasPrefix(pool[i].Excluded, poolFullPrefix) {
			idxs = append(idxs, i)
		}
	}
	sort.SliceStable(idxs, func(a, b int) bool {
		left, right := pool[idxs[a]].ScanBudget, pool[idxs[b]].ScanBudget
		if left != nil && right != nil && left.Order != right.Order {
			return left.Order < right.Order
		}
		return false
	})
	return idxs
}
