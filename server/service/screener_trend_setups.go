package service

import (
	"math"

	"quantvista/datasource"
)

// Wilder DMI：首个均值取完整 n 个方向变动/TR，ADX 再取完整 n 个 DX 作种子。
// 未满窗是 NaN；同幅向上/向下扩张都不计 DM，ADX 只代表强度，不代表方向。
func directionalMovementSeries(bars []datasource.Bar, period int) (plus, minus, adx []float64) {
	n := len(bars)
	plus, minus, adx = make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range bars {
		plus[i], minus[i], adx[i] = math.NaN(), math.NaN(), math.NaN()
	}
	if period < 2 || n <= period {
		return
	}
	for _, b := range bars {
		if b.Close <= 0 || b.Low <= 0 || b.High < b.Low || b.Close < b.Low || b.Close > b.High || !finiteRecNumber(b.High+b.Low+b.Close) {
			return
		}
	}
	var trSum, upSum, downSum, dxSum, lastADX float64
	for i := 1; i < n; i++ {
		up, down := bars[i].High-bars[i-1].High, bars[i-1].Low-bars[i].Low
		upDM, downDM := 0.0, 0.0
		if up > 0 && up > down {
			upDM = up
		}
		if down > 0 && down > up {
			downDM = down
		}
		tr := math.Max(bars[i].High-bars[i].Low, math.Max(math.Abs(bars[i].High-bars[i-1].Close), math.Abs(bars[i].Low-bars[i-1].Close)))
		if i <= period {
			trSum, upSum, downSum = trSum+tr, upSum+upDM, downSum+downDM
		} else {
			trSum = trSum - trSum/float64(period) + tr
			upSum = upSum - upSum/float64(period) + upDM
			downSum = downSum - downSum/float64(period) + downDM
		}
		if i < period {
			continue
		}
		plus[i], minus[i] = 0, 0
		if trSum > 0 {
			plus[i], minus[i] = 100*upSum/trSum, 100*downSum/trSum
		}
		dx := 0.0
		if sum := plus[i] + minus[i]; sum > 0 {
			dx = 100 * math.Abs(plus[i]-minus[i]) / sum
		}
		if i < 2*period {
			dxSum += dx
			if i == 2*period-1 {
				lastADX = dxSum / float64(period)
				adx[i] = lastADX
			}
		} else {
			lastADX = (lastADX*float64(period-1) + dx) / float64(period)
			adx[i] = lastADX
		}
	}
	return
}

func extendedSetupFactors(bars []datasource.Bar) map[string]float64 {
	out := map[string]float64{}
	n := len(bars)
	if n == 0 || validateAdjustedBars("cn", bars) != nil {
		return out
	}
	closes := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
	}
	flag := func(key string, hit bool) {
		out[key] = 0
		if hit {
			out[key] = 1
		}
	}
	last := bars[n-1]
	for _, w := range []struct {
		key string
		n   int
	}{{"ma50", 50}, {"ma150", 150}, {"ma200", 200}} {
		if n >= w.n {
			out[w.key], _ = movingAverage(closes, w.n)
		}
	}
	if n >= 220 {
		previous, _ := movingAverage(closes[:n-20], 200)
		flag("ma200_rising", out["ma200"] > previous)
	}
	if n >= 30 {
		rsi := rsiSeries(closes, 2)
		out["rsi_2"], out["rsi2_previous"] = rsi[n-1], rsi[n-2]
		flag("rsi2_reclaim", rsi[n-2] < 10 && rsi[n-1] >= 10 && last.Close > bars[n-2].Close && last.Low >= bars[n-2].Low)
	}
	if n >= 150 {
		// 额外预热抑制双重平滑的初始值影响，不能拿刚满 28 根的 ADX 排全市场。
		plus, minus, adx := directionalMovementSeries(bars, 14)
		out["dmi_pdi14"], out["dmi_mdi14"], out["adx_14"] = plus[n-1], minus[n-1], adx[n-1]
		flag("adx_rising", adx[n-1] > adx[n-2])
		cross := false
		for i := n - 3; i < n; i++ {
			cross = cross || plus[i-1] <= minus[i-1] && plus[i] > minus[i]
		}
		flag("dmi_bull_cross", cross && plus[n-1] > minus[n-1])
	}
	if n >= 8 {
		prior := bars[n-2]
		width := prior.High - prior.Low
		narrow := width > 0 && prior.Volume > 0
		for _, b := range bars[n-8 : n-2] {
			narrow = narrow && b.Volume > 0 && width < b.High-b.Low
		}
		flag("nr7_break", narrow && last.Volume > 0 && last.Close > prior.High)
		if narrow {
			out["nr7_level"] = prior.High
		}
	}
	if n >= 250 {
		hi, lo := last.High, last.Low
		for _, b := range bars[n-250:] {
			hi, lo = math.Max(hi, b.High), math.Min(lo, b.Low)
		}
		flag("long_trend_template", last.Close > out["ma50"] && out["ma50"] > out["ma150"] && out["ma150"] > out["ma200"] &&
			out["ma200_rising"] == 1 && last.Close >= lo*1.3 && last.Close >= hi*.75)
	}
	return out
}
