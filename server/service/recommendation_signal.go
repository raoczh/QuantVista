package service

import (
	"fmt"
	"math"

	"quantvista/datasource"
)

const recommendationSignalVersion = "sq1"

// 所有形态只取已完成日线；只有 Distance 字段引用当前报价，明确保持两种时点。
// 指针表示可用性，零值可以是一个真实观测，不能用 omitempty 将它隐藏成未知。
type recSignalQuality struct {
	Version               string   `json:"version"`
	AsOf                  string   `json:"as_of"`
	Bars                  int      `json:"bars"`
	ATR                   *float64 `json:"atr,omitempty"` // 信号日之前的 ATR，突破大阳线不扩大自身参照尺度
	BreakoutLevel         *float64 `json:"breakout_level,omitempty"`
	BreakoutConfirmed     *bool    `json:"breakout_confirmed,omitempty"`
	BreakoutRun           int      `json:"breakout_run"` // 连续创 20 日新高数，最多回看 10 日
	BreakoutDistanceATR   *float64 `json:"breakout_distance_atr,omitempty"`
	MA20DistanceATR       *float64 `json:"ma20_distance_atr,omitempty"`
	Compression           *float64 `json:"compression,omitempty"`        // 信号前 5 日/20 日平均真实振幅
	VolumeContraction     *float64 `json:"volume_contraction,omitempty"` // 信号前 5 日/20 日均量
	CloseLocation         *float64 `json:"close_location,omitempty"`     // 最后完整 K 线收盘位置 0..1
	UpperWick             *float64 `json:"upper_wick,omitempty"`
	RangeShock            *float64 `json:"range_shock,omitempty"`
	Efficiency20          *float64 `json:"efficiency_20,omitempty"`    // 净位移/总路径长度 -1..1
	DemandBalance5        *float64 `json:"demand_balance_5,omitempty"` // 上涨/下跌日成交量的有符号占比
	PullbackDepthATR      *float64 `json:"pullback_depth_atr,omitempty"`
	Stabilized            *bool    `json:"stabilized,omitempty"`
	HigherLow             *bool    `json:"higher_low,omitempty"`
	Support               *float64 `json:"support,omitempty"`
	SupportDistanceATR    *float64 `json:"support_distance_atr,omitempty"`
	Resistance            *float64 `json:"resistance,omitempty"`
	ResistanceDistanceATR *float64 `json:"resistance_distance_atr,omitempty"`
	Missing               []string `json:"missing,omitempty"`
}

func recNumber(v float64) *float64 {
	if !finiteRecNumber(v) {
		return nil
	}
	n := math.Round(v*1e6) / 1e6
	return &n
}

func signalQualityLabeledValues(q *recSignalQuality) []labeledValue {
	if q == nil {
		return nil
	}
	var out []labeledValue
	for _, item := range []struct {
		key   string
		value *float64
	}{
		{"atr", q.ATR}, {"breakout_level", q.BreakoutLevel}, {"breakout_distance_atr", q.BreakoutDistanceATR},
		{"ma20_distance_atr", q.MA20DistanceATR}, {"compression", q.Compression}, {"volume_contraction", q.VolumeContraction},
		{"close_location", q.CloseLocation}, {"upper_wick", q.UpperWick}, {"range_shock", q.RangeShock},
		{"efficiency_20", q.Efficiency20}, {"demand_balance_5", q.DemandBalance5}, {"pullback_depth_atr", q.PullbackDepthATR},
		{"support", q.Support}, {"support_distance_atr", q.SupportDistanceATR}, {"resistance", q.Resistance}, {"resistance_distance_atr", q.ResistanceDistanceATR},
	} {
		if item.value != nil {
			out = append(out, labeledVals("signal_quality."+item.key, *item.value)...)
		}
	}
	return out
}

func computeRecSignalQuality(price float64, bars []datasource.Bar) *recSignalQuality {
	q := &recSignalQuality{Version: recommendationSignalVersion, Bars: len(bars)}
	n := len(bars)
	if n == 0 || price <= 0 || !finiteRecNumber(price) {
		q.Missing = []string{"完整日线或当前价格"}
		return q
	}
	last := bars[n-1]
	q.AsOf = last.TradeDate
	for _, b := range bars {
		if b.Close <= 0 || b.Low <= 0 || b.High < b.Low || b.Open <= 0 || !finiteRecNumber(b.Close+b.High+b.Low+b.Open) {
			q.Missing = []string{"有效 OHLC"}
			return q
		}
	}
	closes := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
	}
	if width := last.High - last.Low; width > 0 {
		q.CloseLocation = recNumber(bounded((last.Close-last.Low)/width, 0, 1))
		q.UpperWick = recNumber(bounded((last.High-math.Max(last.Open, last.Close))/width, 0, 1))
	} else {
		q.Missing = append(q.Missing, "收盘位置（一字 K 线）")
	}
	if n >= 2 {
		q.Stabilized = boolPtr(last.Low >= bars[n-2].Low && last.Close >= bars[n-2].Close)
	}
	if n < 22 {
		q.Missing = append(q.Missing, "信号前完整 20 日窗口")
		return q
	}
	atr := atrSeries(bars[:n-1], 14)[n-2]
	if !finiteRecNumber(atr) || atr <= 0 {
		q.Missing = append(q.Missing, "有效 ATR")
		return q
	}
	q.ATR = recNumber(atr)
	ma, _ := movingAverage(closes, 20)
	q.MA20DistanceATR = recNumber((price - ma) / atr)
	priorMax := func(idx int) float64 {
		hi := bars[idx-20].Close
		for _, b := range bars[idx-20 : idx] {
			hi = math.Max(hi, b.Close)
		}
		return hi
	}
	level := priorMax(n - 1)
	q.BreakoutConfirmed = boolPtr(last.Close > level)
	// 连续创新高时锚定这段突破的起点，避免每天重设到昨天高点掩盖延伸。
	for i := n - 1; i >= 20 && n-i <= 10; i-- {
		if bars[i].Close <= priorMax(i) {
			break
		}
		q.BreakoutRun++
		level = priorMax(i)
	}
	q.BreakoutLevel = recNumber(level)
	q.BreakoutDistanceATR = recNumber((price - level) / atr)
	var tr5, tr20, v5, v20 float64
	for i := n - 21; i < n-1; i++ {
		b := bars[i]
		tr := math.Max(b.High-b.Low, math.Max(math.Abs(b.High-bars[i-1].Close), math.Abs(b.Low-bars[i-1].Close)))
		tr20 += tr
		v20 += float64(b.Volume)
		if i >= n-6 {
			tr5 += tr
			v5 += float64(b.Volume)
		}
	}
	if tr20 > 0 {
		q.Compression = recNumber((tr5 / 5) / (tr20 / 20))
	} else {
		q.Missing = append(q.Missing, "历史振幅")
	}
	if v20 > 0 && v5 > 0 {
		q.VolumeContraction = recNumber((v5 / 5) / (v20 / 20))
	} else {
		q.Missing = append(q.Missing, "历史成交量")
	}
	q.RangeShock = recNumber(math.Max(last.High-last.Low, math.Max(math.Abs(last.High-bars[n-2].Close), math.Abs(last.Low-bars[n-2].Close))) / atr)
	var path, demand, totalVol float64
	for i := n - 20; i < n; i++ {
		path += math.Abs(bars[i].Close - bars[i-1].Close)
	}
	if path > 0 {
		q.Efficiency20 = recNumber((last.Close - bars[n-21].Close) / path)
	} else {
		q.Efficiency20 = recNumber(0)
	}
	for i := n - 5; i < n; i++ {
		v := float64(bars[i].Volume)
		if v < 0 {
			continue
		}
		totalVol += v
		if bars[i].Close > bars[i-1].Close {
			demand += v
		} else if bars[i].Close < bars[i-1].Close {
			demand -= v
		}
	}
	if totalVol > 0 {
		q.DemandBalance5 = recNumber(demand / totalVol)
	} else {
		q.Missing = append(q.Missing, "近 5 日成交量")
	}
	low5, low3, priorLow3 := last.Low, last.Low, bars[n-4].Low
	high20 := bars[n-21].High
	for _, b := range bars[n-21 : n-1] {
		high20 = math.Max(high20, b.High)
	}
	for _, b := range bars[n-5:] {
		low5 = math.Min(low5, b.Low)
	}
	for _, b := range bars[n-3:] {
		low3 = math.Min(low3, b.Low)
	}
	for _, b := range bars[n-6 : n-3] {
		priorLow3 = math.Min(priorLow3, b.Low)
	}
	q.HigherLow = boolPtr(low3 >= priorLow3)
	q.PullbackDepthATR = recNumber(math.Max(0, high20-low5) / atr)
	// 结构价是研究参照，不自动等同于止损或预期目标价。
	support := low5
	for _, v := range []float64{ma, level} {
		if v <= price && v > support {
			support = v
		}
	}
	if support > 0 && support <= price {
		q.Support = recNumber(support)
		q.SupportDistanceATR = recNumber((price - support) / atr)
	} else {
		q.Missing = append(q.Missing, "下方结构支撑")
	}
	if high20 > price {
		q.Resistance = recNumber(high20)
		q.ResistanceDistanceATR = recNumber((high20 - price) / atr)
	}
	return q
}

type recEntryQuality struct {
	Version string   `json:"version"`
	Status  string   `json:"status"` // aligned / extended / waiting_confirmation / insufficient
	Reasons []string `json:"reasons,omitempty"`
}

func entryQualityFor(profile, intent string, c candidate, q *recSignalQuality) *recEntryQuality {
	out := &recEntryQuality{Version: "eq1", Status: "aligned"}
	if q == nil || q.ATR == nil || q.MA20DistanceATR == nil {
		out.Status = "insufficient"
		out.Reasons = []string{"缺少足够日线或波动尺度，无法核对入场距离"}
		return out
	}
	extension := *q.MA20DistanceATR
	if q.BreakoutDistanceATR != nil && q.BreakoutRun > 1 {
		extension = math.Max(extension, *q.BreakoutDistanceATR)
	}
	threshold := 3.0
	if profile == "pullback" || profile == "value" || profile == "leader" {
		threshold = 2.0
	}
	if extension > threshold {
		out.Status = "extended"
		out.Reasons = append(out.Reasons, fmt.Sprintf("现价相对均线或本段突破起点已延伸 %.1f 个 ATR，等待价格与支撑重新靠近", extension))
	}
	if q.SupportDistanceATR != nil && *q.SupportDistanceATR < 0 {
		if out.Status == "aligned" {
			out.Status = "waiting_confirmation"
		}
		out.Reasons = append(out.Reasons, "现价已跌破评分快照中的结构支撑，需重新确认")
	}
	if (profile == "pullback" || intent == "reversal" || profile == "value") && (q.Stabilized == nil || !*q.Stabilized) {
		if out.Status == "aligned" {
			out.Status = "waiting_confirmation"
		}
		out.Reasons = append(out.Reasons, "最新完整日线尚未同时确认低点抬高与收盘企稳")
	}
	if q.CloseLocation != nil && q.RangeShock != nil && *q.CloseLocation < 0.35 && *q.RangeShock > 2 {
		if out.Status == "aligned" {
			out.Status = "waiting_confirmation"
		}
		out.Reasons = append(out.Reasons, "最近完整日线振幅扩大但收盘靠近低位，量价确认不足")
	}
	if profileUsesFinance(profile) && c.Fin == nil {
		if out.Status == "aligned" {
			out.Status = "insufficient"
		}
		out.Reasons = append(out.Reasons, "所选评分需要财务证据，目前未取得有效财报摘要")
	}
	return out
}

func entryQualityAtPrice(strat *strategyTemplate, c candidate, price float64) *recEntryQuality {
	if c.SignalQuality == nil || c.SignalQuality.ATR == nil || *c.SignalQuality.ATR <= 0 {
		return c.EntryQuality
	}
	q := *c.SignalQuality
	shift := (price - c.Price) / *q.ATR
	for _, p := range []**float64{&q.MA20DistanceATR, &q.BreakoutDistanceATR, &q.SupportDistanceATR} {
		if *p != nil {
			*p = recNumber(**p + shift)
		}
	}
	if q.ResistanceDistanceATR != nil {
		q.ResistanceDistanceATR = recNumber(*q.ResistanceDistanceATR - shift)
	}
	return entryQualityFor(strat.baseKey, strat.Intent, c, &q)
}
