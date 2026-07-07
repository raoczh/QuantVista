package service

import (
	"math"
	"testing"

	"quantvista/datasource"
)

// 表驱动对拍：小参数 + 固定输入序列，预算值全部手工按口径递推得出。
// 口径参见 indicator.go 头注释（EMA seed=首值、Wilder α=1/n、MACD 柱 2 倍、BOLL 样本标准差）。

func almostEq(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestEmaSeries(t *testing.T) {
	// EMA(3) α=0.5，seed=1：手算 [1, 1.5, 2.25, 3.125, 4.0625]
	got := emaSeries([]float64{1, 2, 3, 4, 5}, 3)
	want := []float64{1, 1.5, 2.25, 3.125, 4.0625}
	for i := range want {
		if !almostEq(got[i], want[i], 1e-9) {
			t.Fatalf("ema[%d]=%v 期望 %v", i, got[i], want[i])
		}
	}
}

func TestWilderSeries(t *testing.T) {
	// Wilder(3)（通达信 SMA(X,3,1)），seed=1：y=(x+2y')/3
	got := wilderSeries([]float64{1, 2, 3, 4}, 3)
	want := []float64{1, 1.333333, 1.888889, 2.592593}
	for i := range want {
		if !almostEq(got[i], want[i], 1e-6) {
			t.Fatalf("wilder[%d]=%v 期望 %v", i, got[i], want[i])
		}
	}
}

func TestMacdSeriesN(t *testing.T) {
	// MACD(2,4,2) over [1..5]：快慢 EMA 与 DEA 逐步手工递推，柱=2×(DIF−DEA)。
	dif, dea, hist := macdSeriesN([]float64{1, 2, 3, 4, 5}, 2, 4, 2)
	wantDif := []float64{0, 0.266667, 0.515556, 0.694519, 0.811773}
	wantDea := []float64{0, 0.177778, 0.402963, 0.597333, 0.740293}
	wantHist := []float64{0, 0.177778, 0.225185, 0.194371, 0.142960}
	for i := range wantDif {
		if !almostEq(dif[i], wantDif[i], 1e-5) {
			t.Fatalf("dif[%d]=%v 期望 %v", i, dif[i], wantDif[i])
		}
		if !almostEq(dea[i], wantDea[i], 1e-5) {
			t.Fatalf("dea[%d]=%v 期望 %v", i, dea[i], wantDea[i])
		}
		if !almostEq(hist[i], wantHist[i], 1e-5) {
			t.Fatalf("hist[%d]=%v 期望 %v", i, hist[i], wantHist[i])
		}
	}
}

func TestBollSeries(t *testing.T) {
	// BOLL(3,2) over [1..5]：窗口 [1,2,3]/[2,3,4]/[3,4,5] 样本标准差均为 1。
	up, mid, low := bollSeries([]float64{1, 2, 3, 4, 5}, 3, 2)
	if !math.IsNaN(up[0]) || !math.IsNaN(mid[1]) || !math.IsNaN(low[1]) {
		t.Fatalf("前 n-1 位应为 NaN")
	}
	wantMid := []float64{2, 3, 4}
	for i, w := range wantMid {
		j := i + 2
		if !almostEq(mid[j], w, 1e-9) || !almostEq(up[j], w+2, 1e-9) || !almostEq(low[j], w-2, 1e-9) {
			t.Fatalf("boll[%d]=(%v,%v,%v) 期望 (%v,%v,%v)", j, up[j], mid[j], low[j], w+2, w, w-2)
		}
	}
}

func TestRsiSeries(t *testing.T) {
	// RSI(3) over [10,11,10.5,11.5,12]：涨跌幅 Wilder 平滑后手工对拍。
	got := rsiSeries([]float64{10, 11, 10.5, 11.5, 12}, 3)
	if !math.IsNaN(got[0]) {
		t.Fatalf("rsi[0] 应为 NaN（无前收）")
	}
	want := []float64{100, 80, 87.5, 90.2439}
	for i, w := range want {
		if !almostEq(got[i+1], w, 1e-3) {
			t.Fatalf("rsi[%d]=%v 期望 %v", i+1, got[i+1], w)
		}
	}
	// 连续平盘：up+down=0 → 中性 50。
	flat := rsiSeries([]float64{5, 5, 5}, 3)
	if flat[1] != 50 || flat[2] != 50 {
		t.Fatalf("平盘 RSI 应为 50，得到 %v", flat)
	}
}

func TestAtrSeries(t *testing.T) {
	// TR=[2,2,4,2]（首位取高-低，其余含前收跳空），Wilder(3) 递推。
	bars := []datasource.Bar{
		{High: 11, Low: 9, Close: 10},
		{High: 12, Low: 10, Close: 11},
		{High: 15, Low: 13, Close: 14},
		{High: 14, Low: 12, Close: 13},
	}
	got := atrSeries(bars, 3)
	want := []float64{2, 2, 2.666667, 2.444444}
	for i, w := range want {
		if !almostEq(got[i], w, 1e-6) {
			t.Fatalf("atr[%d]=%v 期望 %v", i, got[i], w)
		}
	}
}

// TestIndicatorSnapshot 快照可用性门槛与派生字段（性质断言）。
func TestIndicatorSnapshot(t *testing.T) {
	if computeIndicatorSnapshot(10, genBars(10, 10, 0.1)) != nil {
		t.Fatalf("10 根不足 RSI 门槛应返回 nil")
	}
	// 40 根先跌后涨：末端动量向上，MACD 应为多头（DIF>DEA），RSI 落在强区。
	bars := genBars(20, 20, -0.3)
	c := bars[len(bars)-1].Close
	for i := 0; i < 20; i++ {
		c += 0.35
		bars = append(bars, datasource.Bar{Close: c, High: c * 1.01, Low: c * 0.99, Open: c, Volume: 1000})
	}
	price := c
	snap := computeIndicatorSnapshot(price, bars)
	if snap == nil {
		t.Fatalf("40 根应产出快照")
	}
	if snap.RSI14 <= 50 || snap.RSI14 > 100 {
		t.Fatalf("持续反弹 RSI 应 >50，得到 %v", snap.RSI14)
	}
	if !snap.MACDGold {
		t.Fatalf("V 形反转末端 DIF 应在 DEA 上方: %+v", snap)
	}
	if snap.ATR14 <= 0 || snap.ATRPct <= 0 {
		t.Fatalf("ATR 应为正: %+v", snap)
	}
	if snap.BollUp <= snap.BollMid || snap.BollMid <= snap.BollLow {
		t.Fatalf("布林带应上>中>下: %+v", snap)
	}
	if snap.BollPos <= 50 {
		t.Fatalf("强势反弹末端应在布林带上半区，得到 %v", snap.BollPos)
	}
}

// TestMacdCrossUp 金叉检测：跌势中 DIF<DEA，反转拉升后近 3 根内应捕捉到上穿。
func TestMacdCrossUp(t *testing.T) {
	// 长阴跌让 DIF 深潜 DEA 下方，末尾 3 根急拉制造上穿。
	bars := genBars(40, 30, -0.25)
	c := bars[len(bars)-1].Close
	for i := 0; i < 3; i++ {
		c += 2.5
		bars = append(bars, datasource.Bar{Close: c, High: c * 1.01, Low: c * 0.99, Open: c, Volume: 1000})
	}
	snap := computeIndicatorSnapshot(c, bars)
	if snap == nil || !snap.MACDXUp {
		t.Fatalf("急拉后应检测到近 3 根金叉: %+v", snap)
	}
}

func TestNanToNil(t *testing.T) {
	xs := []float64{math.NaN(), 1.234, 5.678, math.Inf(1)}
	out := nanToNil(xs, 3, round2)
	if len(out) != 3 {
		t.Fatalf("应截尾 3 个，得到 %d", len(out))
	}
	if out[0] == nil || *out[0] != 1.23 {
		t.Fatalf("out[0] 期望 1.23，得到 %v", out[0])
	}
	if out[2] != nil {
		t.Fatalf("Inf 应转 nil")
	}
}
