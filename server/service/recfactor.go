package service

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"quantvista/datasource"
	"quantvista/model"
)

// 推荐流水线阶段③：本地量化因子与评分（零 LLM 成本）。
// 因子全部由 90 根前复权日线 + 估值快照派生（Qlib Alpha158 思路的轻量子集），
// 复合分 = 五维技术评分（computeScore，与个股详情/对比同口径）+ 策略加分项（逐条可解释）。
// LLM 在此之后只对排名靠前的候选做「研究员式」解读与否决，不再海选。

// candFactors 单只候选的技术因子快照（全部落库、喂给 LLM、前端可展开）。
type candFactors struct {
	MA5          float64 `json:"ma5,omitempty"`
	MA10         float64 `json:"ma10,omitempty"`
	MA20         float64 `json:"ma20,omitempty"`
	MA60         float64 `json:"ma60,omitempty"`
	Chg5d        float64 `json:"chg_5d"`                  // 近 5 日涨跌 %
	Chg20d       float64 `json:"chg_20d"`                 // 近 20 日涨跌 %
	High20d      bool    `json:"high_20d,omitempty"`      // 收盘创 20 日新高
	BullAlign    bool    `json:"bull_align,omitempty"`    // MA5>MA10>MA20 多头排列
	AboveMA20    bool    `json:"above_ma20,omitempty"`    // 现价站上 MA20
	VolBoost     float64 `json:"vol_boost,omitempty"`     // 今日量 / 前 5 日均量（日线量比近似）
	Vol5v20      float64 `json:"vol_5v20,omitempty"`      // 5 日均量 / 20 日均量
	Volatility20 float64 `json:"volatility_20,omitempty"` // 近 20 日日收益率标准差 %
	Drawdown20   float64 `json:"drawdown_20,omitempty"`   // 近 20 日最大回撤 %（正数）
	Bias20       float64 `json:"bias_20,omitempty"`       // (现价/MA20-1)% 乖离率
	Pos60        float64 `json:"pos_60"`                  // 60 日区间位置 0-100
	BarCount     int     `json:"bar_count"`
}

// scoreDims 五维评分明细（透传 computeScore 结果，前端展示雷达/分项）。
type scoreDims struct {
	Trend    float64 `json:"trend"`
	Momentum float64 `json:"momentum"`
	Position float64 `json:"position"`
	Volume   float64 `json:"volume"`
	Risk     float64 `json:"risk"`
}

// computeCandFactors 由现价与升序日线派生技术因子。bars 为空返回 nil。
func computeCandFactors(price float64, bars []datasource.Bar) *candFactors {
	n := len(bars)
	if n == 0 || price <= 0 {
		return nil
	}
	closes := make([]float64, n)
	vols := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
		vols[i] = float64(b.Volume)
	}
	f := &candFactors{BarCount: n}
	if v, ok := movingAverage(closes, 5); ok {
		f.MA5 = round2(v)
	}
	if v, ok := movingAverage(closes, 10); ok {
		f.MA10 = round2(v)
	}
	if v, ok := movingAverage(closes, 20); ok {
		f.MA20 = round2(v)
	}
	if v, ok := movingAverage(closes, 60); ok {
		f.MA60 = round2(v)
	}
	f.Chg5d = changeOverN(closes, 5)
	f.Chg20d = changeOverN(closes, 20)
	f.BullAlign = f.MA5 > 0 && f.MA10 > 0 && f.MA20 > 0 && f.MA5 > f.MA10 && f.MA10 > f.MA20
	f.AboveMA20 = f.MA20 > 0 && price >= f.MA20
	if f.MA20 > 0 {
		f.Bias20 = round2((price/f.MA20 - 1) * 100)
	}

	// 收盘创 20 日新高：末根收盘 ≥ 前 20 根收盘最大值（收盘口径假突破更少）。
	if n >= 21 {
		maxPrev := closes[n-21]
		for _, c := range closes[n-21 : n-1] {
			if c > maxPrev {
				maxPrev = c
			}
		}
		f.High20d = closes[n-1] >= maxPrev
	}

	// 量能：今日量 / 前 5 日均量（剔除今日，日线口径的量比近似）；5 日均量 / 20 日均量。
	if n >= 6 {
		var prev5 float64
		for _, v := range vols[n-6 : n-1] {
			prev5 += v
		}
		prev5 /= 5
		if prev5 > 0 {
			f.VolBoost = round2(vols[n-1] / prev5)
		}
	}
	avgVol := func(w int) float64 {
		if w > n {
			w = n
		}
		var s float64
		for _, v := range vols[n-w:] {
			s += v
		}
		return s / float64(w)
	}
	if a20 := avgVol(20); a20 > 0 {
		f.Vol5v20 = round2(avgVol(5) / a20)
	}

	// 近 20 日：日收益率标准差（波动率）与最大回撤。
	w := 20
	if w > n {
		w = n
	}
	win := bars[n-w:]
	if len(win) >= 2 {
		var rets []float64
		for i := 1; i < len(win); i++ {
			if win[i-1].Close > 0 {
				rets = append(rets, (win[i].Close-win[i-1].Close)/win[i-1].Close*100)
			}
		}
		f.Volatility20 = round2(stddev(rets))
	}
	peak := win[0].High
	worst := 0.0
	for _, b := range win {
		if b.High > peak {
			peak = b.High
		}
		if peak > 0 {
			if dd := (b.Low - peak) / peak; dd < worst {
				worst = dd
			}
		}
	}
	f.Drawdown20 = round2(-worst * 100)

	// 60 日区间位置。
	hi, lo := bars[0].High, bars[0].Low
	for _, b := range bars {
		if b.High > hi {
			hi = b.High
		}
		if b.Low < lo && b.Low > 0 {
			lo = b.Low
		}
	}
	if hi > lo {
		f.Pos60 = round2((price - lo) / (hi - lo) * 100)
	} else {
		f.Pos60 = 50
	}
	return f
}

// stddev 样本标准差（n-1）；样本不足返回 0。
func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// strategyAdjust 策略加分/扣分（逐条给中文说明，随候选落库展示——评分可解释是信任的根基）。
// 返回分数增量与说明列表。参数阈值来自社区实战共识（量比 1.5~5 温和/有效放量、
// 换手 3~15% 活跃、BIAS20>12% 超买、近 20 日涨幅 ≥15% 为前期强势 等）。
func strategyAdjust(recType, stratKey string, c candidate, f *candFactors) (float64, []string) {
	if f == nil {
		return 0, nil
	}
	var delta float64
	var notes []string
	add := func(d float64, note string) {
		delta += d
		sign := "+"
		if d < 0 {
			sign = ""
		}
		notes = append(notes, fmt.Sprintf("%s（%s%.0f）", note, sign, d))
	}

	if recType == model.RecTypeShortTerm {
		// 短线共用风险扣分。
		if f.Bias20 > 12 {
			add(-6, fmt.Sprintf("MA20乖离 %.1f%% 超买", f.Bias20))
		}
		if f.VolBoost > 5 {
			add(-4, fmt.Sprintf("量比近似 %.1f 爆量警惕", f.VolBoost))
		}
		switch stratKey {
		case "momentum":
			if f.High20d {
				add(6, "收盘创20日新高")
			}
			if f.BullAlign {
				add(5, "MA5>MA10>MA20 多头排列")
			}
			if f.VolBoost >= 1.5 && f.VolBoost <= 5 {
				add(4, fmt.Sprintf("放量健康（%.1f 倍）", f.VolBoost))
			}
		case "pullback":
			if f.Chg20d >= 15 {
				add(5, fmt.Sprintf("近20日涨 %.1f%% 前期强势", f.Chg20d))
			}
			if f.AboveMA20 && f.Chg5d <= 0 && f.Chg5d >= -6 {
				add(5, fmt.Sprintf("近5日回调 %.1f%% 未破MA20", f.Chg5d))
			}
			if f.Vol5v20 > 0 && f.Vol5v20 < 0.9 {
				add(3, "回调缩量（供给衰竭）")
			}
		case "active":
			if c.TurnoverRate >= 5 && c.TurnoverRate <= 15 {
				add(5, fmt.Sprintf("换手 %.1f%% 活跃区间", c.TurnoverRate))
			}
			if f.VolBoost >= 2 {
				add(4, fmt.Sprintf("显著放量（%.1f 倍）", f.VolBoost))
			}
			if c.VolumeRatio >= 1.5 && c.VolumeRatio <= 5 {
				add(3, fmt.Sprintf("实时量比 %.1f 温和放量", c.VolumeRatio))
			}
		}
		return delta, notes
	}

	// 长线：估值与稳定性导向（估值缺失只是不加分，不虚构）。
	if c.PETTM <= 0 && c.PB > 0 {
		add(-5, "PE 为负（亏损）")
	}
	switch stratKey {
	case "value":
		switch {
		case c.PETTM > 0 && c.PETTM <= 15:
			add(8, fmt.Sprintf("PE-TTM %.1f 低估区", c.PETTM))
		case c.PETTM > 15 && c.PETTM <= 25:
			add(5, fmt.Sprintf("PE-TTM %.1f 合理区", c.PETTM))
		}
		switch {
		case c.PB > 0 && c.PB <= 1.5:
			add(5, fmt.Sprintf("PB %.2f 破净/低位", c.PB))
		case c.PB > 1.5 && c.PB <= 3:
			add(3, fmt.Sprintf("PB %.2f 不高", c.PB))
		}
		if f.Pos60 <= 50 {
			add(4, fmt.Sprintf("60日区间位置 %.0f%% 未追高", f.Pos60))
		}
		if f.Volatility20 > 0 && f.Volatility20 <= 2 {
			add(3, fmt.Sprintf("波动率 %.1f%% 偏低", f.Volatility20))
		}
	case "growth":
		if f.MA60 > 0 && c.Price > f.MA60 {
			add(5, "站上 MA60 中期趋势向上")
		}
		if f.BullAlign {
			add(4, "均线多头排列")
		}
		if f.Chg20d > 0 && f.Chg20d <= 25 {
			add(4, fmt.Sprintf("近20日涨 %.1f%% 趋势健康", f.Chg20d))
		}
		if f.Bias20 > 20 {
			add(-6, fmt.Sprintf("MA20乖离 %.1f%% 追高风险", f.Bias20))
		}
	case "leader":
		if c.TotalCap >= 500e8 {
			add(5, fmt.Sprintf("总市值 %.0f 亿龙头体量", c.TotalCap/1e8))
		}
		if f.MA60 > 0 && c.Price > f.MA60 {
			add(4, "站上 MA60")
		}
		if f.Volatility20 > 0 && f.Volatility20 <= 2.5 {
			add(3, "波动稳定")
		}
		if c.TurnoverRate > 0 && c.TurnoverRate <= 8 {
			add(2, fmt.Sprintf("换手 %.1f%% 不过热", c.TurnoverRate))
		}
	}
	return delta, notes
}

// --- 程序化证据核验（防幻觉最便宜的一招）---

// evidenceCheck 单条推荐的证据数字核验结果：LLM evidence 里引用的数字逐一与
// 该标的的数据快照（行情/估值/因子/评分）容差比对，不吻合的列出来。
type evidenceCheck struct {
	Total     int      `json:"total"`               // 提取到的数字个数
	Matched   int      `json:"matched"`             // 与快照吻合的个数
	Unmatched []string `json:"unmatched,omitempty"` // 未能吻合的数字（原样，供人工核对）
}

var evidenceNumRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

// 惯用窗口/参数整数中 >99 的部分（MA120、250 日年线、百分位 100）；
// ≤99 的整数已由 verifyEvidence 的小整数规则整体跳过。
var evidenceNoiseInts = map[float64]bool{
	100: true, 120: true, 250: true,
}

// candidateValueSet 汇总一只候选可被引用的全部数值（含亿元换算），供证据核验比对。
func candidateValueSet(c candidate) []float64 {
	vals := []float64{
		c.Price, c.ChangePct, c.Amount, c.Amount / 1e8,
		c.PETTM, c.PB, c.TotalCap, c.TotalCap / 1e8, c.FloatCap, c.FloatCap / 1e8,
		c.TurnoverRate, c.VolumeRatio, c.Amplitude, c.Score,
	}
	if c.Factors != nil {
		f := c.Factors
		vals = append(vals, f.MA5, f.MA10, f.MA20, f.MA60, f.Chg5d, f.Chg20d,
			f.VolBoost, f.Vol5v20, f.Volatility20, f.Drawdown20, f.Bias20, f.Pos60)
	}
	if c.ScoreDims != nil {
		vals = append(vals, c.ScoreDims.Trend, c.ScoreDims.Momentum, c.ScoreDims.Position,
			c.ScoreDims.Volume, c.ScoreDims.Risk)
	}
	out := vals[:0]
	for _, v := range vals {
		if v != 0 {
			out = append(out, v)
		}
	}
	return out
}

// verifyEvidence 核验一条推荐的 evidence 数字与快照的吻合度。
// 数字提取排除窗口参数噪声（无小数点整数：常用参数集合、年份、六位代码、≤99 小整数——
// 「池内第 11」「第 2/38」类 rank/池大小引用是 prompt 自己示范的格式，核验它属信任层自伤）；
// 容差 = max(0.02, 2%)。extra 为快照之外的合法引用值域（模型自身计划价、用户筛选阈值），
// 由调用方按上下文传入。这是启发式核对：matched 高说明模型确在引用真实数据；
// unmatched 交给用户肉眼复核。
func verifyEvidence(evidence []string, c candidate, extra ...float64) *evidenceCheck {
	vals := candidateValueSet(c)
	for _, v := range extra {
		if v != 0 {
			vals = append(vals, v)
		}
	}
	check := &evidenceCheck{}
	for _, line := range evidence {
		for _, tok := range evidenceNumRe.FindAllString(line, -1) {
			num, err := strconv.ParseFloat(tok, 64)
			if err != nil {
				continue
			}
			// 无小数点的窗口参数/年份/股票代码/小整数不核验（「MA20」「近5日」「2026」
			// 「600000」「池内第 11」）。阈值 1e5：六位股票代码 ≥100000，而市值（亿）/
			// 成交额（亿）等真实引用值远小于它；≤99 的整数几乎总是 rank/池大小/天数。
			if !strings.Contains(tok, ".") {
				abs := math.Abs(num)
				if abs <= 99 || evidenceNoiseInts[abs] || (abs >= 1900 && abs <= 2100) || abs >= 1e5 {
					continue
				}
			}
			check.Total++
			matched := false
			for _, v := range vals {
				tol := math.Max(0.02, math.Abs(v)*0.02)
				if math.Abs(num-v) <= tol || math.Abs(math.Abs(num)-math.Abs(v)) <= tol {
					matched = true
					break
				}
			}
			if matched {
				check.Matched++
			} else if len(check.Unmatched) < 10 {
				check.Unmatched = append(check.Unmatched, tok)
			}
		}
	}
	return check
}

// systemConfidence 程序合成置信度（高/中/低三档）：LLM 口头置信度系统性过度自信，
// 不能单独作为信任依据；这里由 量化排名分位 × 证据核验 × 数据完备度 合成，
// 与 LLM 的 confidence 并排展示。
func systemConfidence(c candidate, ev *evidenceCheck, poolSize int) (string, string) {
	var reasons []string
	level := 1 // 0=低 1=中 2=高

	if poolSize > 0 && c.Rank > 0 {
		pct := float64(c.Rank) / float64(poolSize)
		switch {
		case pct <= 0.25:
			level++
			reasons = append(reasons, fmt.Sprintf("量化分池内前 25%%（第 %d/%d）", c.Rank, poolSize))
		case pct > 0.6:
			level--
			reasons = append(reasons, fmt.Sprintf("量化分池内靠后（第 %d/%d）", c.Rank, poolSize))
		default:
			reasons = append(reasons, fmt.Sprintf("量化分池内第 %d/%d", c.Rank, poolSize))
		}
	}
	if ev != nil && ev.Total > 0 {
		ratio := float64(ev.Matched) / float64(ev.Total)
		switch {
		case ratio >= 0.7:
			level++
			reasons = append(reasons, fmt.Sprintf("证据核验 %d/%d 吻合", ev.Matched, ev.Total))
		case ratio < 0.4:
			level--
			reasons = append(reasons, fmt.Sprintf("证据核验仅 %d/%d 吻合", ev.Matched, ev.Total))
		default:
			reasons = append(reasons, fmt.Sprintf("证据核验 %d/%d 吻合", ev.Matched, ev.Total))
		}
	}
	if c.Factors == nil || c.Factors.BarCount < 20 {
		level--
		reasons = append(reasons, "日线历史不足，技术因子精度受限")
	}
	if c.PETTM == 0 && c.PB == 0 && c.FloatCap == 0 {
		level--
		reasons = append(reasons, "估值快照缺失")
	}

	if level < 0 {
		level = 0
	}
	if level > 2 {
		level = 2
	}
	labels := [3]string{"low", "medium", "high"}
	return labels[level], strings.Join(reasons, "；")
}
