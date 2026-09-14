package service

import (
	"math"
	"sort"

	"quantvista/datasource"
)

// 时间序列形态必须验证前后关系。只读取传入的完整日线；缺少整个所需窗口时
// 不返回该因子，宽表保留 NaN，不能以 false 或中性数值代替未知。
func commonSetupFactors(bars []datasource.Bar) map[string]float64 {
	out := extendedSetupFactors(bars)
	n := len(bars)
	if n == 0 {
		return out
	}
	closes := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
	}
	flag := func(key string, value bool) {
		out[key] = 0
		if value {
			out[key] = 1
		}
	}
	last := bars[n-1]
	if last.Open > 0 {
		out["body_pct"] = (last.Close/last.Open - 1) * 100
	}
	if n >= 20 {
		rsi := rsiSeries(closes, 14)
		minimum := rsi[n-6]
		for _, v := range rsi[n-6 : n-1] {
			minimum = math.Min(minimum, v)
		}
		if finiteRecNumber(minimum) && finiteRecNumber(rsi[n-1]) {
			out["rsi_prior5_min"] = minimum
			flag("rsi_rising", rsi[n-1] > rsi[n-2])
		}
	}
	if n >= 21 {
		_, _, lower := bollSeries(closes, 20, 2)
		level := lower[n-2]
		flag("boll_lower_reclaim", level > 0 && (last.Low <= level || bars[n-2].Close <= level) && last.Close > level && last.Close > bars[n-2].Close)
	}
	if n >= 63 {
		cross := false
		for i := n - 3; i < n; i++ {
			prev20, _ := movingAverage(closes[:i], 20)
			prev60, _ := movingAverage(closes[:i], 60)
			ma20, _ := movingAverage(closes[:i+1], 20)
			ma60, _ := movingAverage(closes[:i+1], 60)
			cross = cross || prev20 <= prev60 && ma20 > ma60
		}
		ma20, _ := movingAverage(closes, 20)
		ma60, _ := movingAverage(closes, 60)
		flag("ma20_cross_ma60", cross && ma20 > ma60)
	}
	if n >= 56 {
		hi := bars[n-56].High
		for _, b := range bars[n-56 : n-1] {
			hi = math.Max(hi, b.High)
		}
		flag("donchian55_break", last.Close > hi)
	}
	if n >= 80 {
		up, mid, low := bollSeries(closes, 20, 2)
		widths := make([]float64, 0, 60)
		for i := n - 61; i < n-1; i++ {
			if mid[i] > 0 {
				widths = append(widths, (up[i]-low[i])/mid[i])
			}
		}
		if len(widths) == 60 {
			sort.Float64s(widths)
			priorWidth := (up[n-2] - low[n-2]) / mid[n-2]
			flag("boll_squeeze_break", priorWidth <= widths[17] && last.Close > up[n-2] && bars[n-2].Close <= up[n-2])
		}
	}
	if n >= 31 {
		flag("breakout_retest", false)
		// 先出现有量的收盘突破，间隔 2～10 根后再回踩原平台；当前低点和
		// 收盘都要确认，且中间不能已经明显跌破平台。不能从两根 K 线猜先后。
		for i := n - 3; i >= n-11; i-- {
			hi, vol := bars[i-20].High, 0.0
			for _, b := range bars[i-20 : i] {
				hi = math.Max(hi, b.High)
			}
			for _, b := range bars[i-5 : i] {
				vol += float64(b.Volume)
			}
			if bars[i].Close <= hi || vol <= 0 || float64(bars[i].Volume) < 1.3*vol/5 {
				continue
			}
			atr := atrSeries(bars[:i], 14)[i-1]
			if !finiteRecNumber(atr) || atr <= 0 {
				break
			}
			held := true
			for _, b := range bars[i+1 : n-1] {
				held = held && b.Close >= hi-0.5*atr
			}
			out["breakout_retest_level"] = hi
			flag("breakout_retest", held && last.Low >= hi-0.5*atr && last.Low <= hi+0.5*atr && last.Close >= hi &&
				last.Close > last.Open && last.Close >= bars[n-2].Close && last.Low >= bars[n-2].Low)
			break
		}
	}
	if n >= 30 {
		// A 股常用 KDJ(9,3,3)：RSV9，K/D 用 1/3 递推，初始 50；
		// J=3K-2D 可超出 0..100，不截断。至少 30 根减轻初始化影响。
		k, d, previousK, previousD := 50.0, 50.0, 50.0, 50.0
		for i := 8; i < n; i++ {
			hi, lo := bars[i-8].High, bars[i-8].Low
			for _, b := range bars[i-8 : i+1] {
				hi, lo = math.Max(hi, b.High), math.Min(lo, b.Low)
			}
			rsv := 50.0
			if hi > lo {
				rsv = (bars[i].Close - lo) / (hi - lo) * 100
			}
			previousK, previousD = k, d
			k = (2*k + rsv) / 3
			d = (2*d + k) / 3
		}
		out["kdj_k"], out["kdj_d"], out["kdj_j"] = k, d, 3*k-2*d
		flag("kdj_low_cross", previousK <= previousD && k > d && previousK < 30 && previousD < 30)
	}
	return out
}
