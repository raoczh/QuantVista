package service

import (
	"strings"
	"testing"

	"quantvista/datasource"
)

// chipFlatBars 造 n 根一字板日线（o=h=l=c=px），换手/量按参数。
func chipFlatBars(n int, px, turnover float64, volume int64) []datasource.Bar {
	bars := make([]datasource.Bar, n)
	for i := range bars {
		bars[i] = datasource.Bar{
			TradeDate: "2026-01-01", Open: px, High: px, Low: px, Close: px,
			Volume: volume, TurnoverRate: turnover,
		}
	}
	return bars
}

func TestChipTurnovers(t *testing.T) {
	// 自带换手优先；缺失根按中位数反推的股本换算；钳制上限。
	bars := []datasource.Bar{
		{Volume: 1000, TurnoverRate: 10},  // 股本=1000*100/0.1=1e6 股
		{Volume: 500, TurnoverRate: 0},    // 推断：500*100/1e6=5%
		{Volume: 2000, TurnoverRate: 250}, // 极端换手钳到 0.99
		{Volume: 0, TurnoverRate: 0},      // 停牌：0
	}
	ts := chipTurnovers(bars, 0)
	if ts == nil {
		t.Fatalf("有自带换手应可推断")
	}
	if !almostEq(ts[0], 0.10, 1e-9) || !almostEq(ts[1], 0.05, 1e-9) || ts[2] != chipMaxTurnover || ts[3] != 0 {
		t.Fatalf("换手归一不符: %v", ts)
	}
	// 全缺失 + 无外部股本 → nil；给外部股本则可算。
	noTr := []datasource.Bar{{Volume: 1000}, {Volume: 500}}
	if chipTurnovers(noTr, 0) != nil {
		t.Fatalf("无换手无股本应返回 nil")
	}
	ts2 := chipTurnovers(noTr, 1e6)
	if ts2 == nil || !almostEq(ts2[0], 0.10, 1e-9) || !almostEq(ts2[1], 0.05, 1e-9) {
		t.Fatalf("外部股本兜底不符: %v", ts2)
	}
}

func TestTriangleDist(t *testing.T) {
	// 5 档 [1..5]，level=1，[low=1.5,high=4.5] 峰=3：对称三角，合计=1。
	prices := []float64{1, 2, 3, 4, 5}
	dist := triangleDist(prices, 1, 1.5, 4.5, 3)
	var sum float64
	for _, w := range dist {
		sum += w
	}
	if !almostEq(sum, 1, 1e-9) {
		t.Fatalf("分布应归一，合计 %v", sum)
	}
	if dist[2] <= dist[1] || dist[2] <= dist[3] {
		t.Fatalf("峰档应最重: %v", dist)
	}
	if !almostEq(dist[1], dist[3], 1e-9) {
		t.Fatalf("对称峰两侧应等权: %v", dist)
	}
	// 一字板：单档全落。
	one := triangleDist(prices, 1, 3, 3, 3)
	if !almostEq(one[2], 1, 1e-9) {
		t.Fatalf("一字板应全落峰档: %v", one)
	}
}

// TestChipTwoStates 可控两态手算：前 119 根一字 @10 零成交（不衰减），
// 末根一字 @20 换手 50% → 新旧筹码各半，收盘 20 全获利、平均成本 15。
func TestChipTwoStates(t *testing.T) {
	bars := chipFlatBars(119, 10, 0, 0)
	bars = append(bars, datasource.Bar{
		TradeDate: "2026-07-06", Open: 20, High: 20, Low: 20, Close: 20,
		Volume: 5000, TurnoverRate: 50,
	})
	res, err := computeChipDistribution(bars, 0)
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if res.Profit != 100 {
		t.Fatalf("收盘 20 应全获利，得到 %v", res.Profit)
	}
	if !almostEq(res.AvgCost, 15, 0.01) {
		t.Fatalf("平均成本应为 15，得到 %v", res.AvgCost)
	}
	// 双峰各半：90% 区间跨两峰（5%/95% 分位落峰内），70% 区间同理。
	if !almostEq(res.C90Low, 10.01, 0.05) || !almostEq(res.C90High, 19.99, 0.05) {
		t.Fatalf("90%% 成本区间不符: [%v, %v]", res.C90Low, res.C90High)
	}
	if !almostEq(res.Conc90, 33.27, 0.2) {
		t.Fatalf("90%% 集中度应≈33.3，得到 %v", res.Conc90)
	}
	if res.BarCount != 120 || !res.DataLimited {
		t.Fatalf("120 根应标 data_limited: count=%d limited=%v", res.BarCount, res.DataLimited)
	}
	// 末根一字 @20 之后再涨：获利盘应随收盘价单调不减。
	bars2 := append(append([]datasource.Bar(nil), bars...), datasource.Bar{
		TradeDate: "2026-07-07", Open: 25, High: 25, Low: 25, Close: 25,
		Volume: 1000, TurnoverRate: 10,
	})
	res2, err := computeChipDistribution(bars2, 0)
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if res2.Profit < res.Profit-1e-9 {
		t.Fatalf("更高收盘获利盘不应下降: %v -> %v", res.Profit, res2.Profit)
	}
}

// TestChipRealistic 210 根真实感序列的结构性质断言。
func TestChipRealistic(t *testing.T) {
	bars := make([]datasource.Bar, 0, chipBarLimit)
	px := 10.0
	for i := 0; i < chipBarLimit; i++ {
		step := 0.08
		if i%3 == 2 { // 两涨一跌的爬升节奏
			step = -0.05
		}
		px += step
		bars = append(bars, datasource.Bar{
			TradeDate: "2026-01-02", Open: px - step/2, High: px * 1.015, Low: px * 0.985,
			Close: px, Volume: 8000, TurnoverRate: 2.5,
		})
	}
	res, err := computeChipDistribution(bars, 0)
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if res.DataLimited || res.BarCount != chipBarLimit {
		t.Fatalf("210 根不应标受限: %+v", res.chipDay)
	}
	if len(res.Days) != chipTrendDays {
		t.Fatalf("趋势序列应 %d 日，得到 %d", chipTrendDays, len(res.Days))
	}
	if res.Profit < 0 || res.Profit > 100 {
		t.Fatalf("获利比例应在 0~100: %v", res.Profit)
	}
	if !(res.C90Low <= res.C70Low && res.C70Low <= res.C70High && res.C70High <= res.C90High) {
		t.Fatalf("成本区间嵌套关系错误: %+v", res.chipDay)
	}
	if res.AvgCost < res.C90Low || res.AvgCost > res.C90High {
		t.Fatalf("平均成本应落在 90%% 区间内: %+v", res.chipDay)
	}
	var sum float64
	for _, c := range res.Chips {
		sum += c
	}
	if !almostEq(sum, 1, 0.01) {
		t.Fatalf("末日分布合计应≈1，得到 %v", sum)
	}
	// 持续爬升 + 持续换手衰减后，早期低价老筹码大多被换走，末端获利盘应偏高。
	if res.Profit < 60 {
		t.Fatalf("爬升序列末端获利盘应偏高，得到 %v", res.Profit)
	}
}

func TestChipInsufficientBars(t *testing.T) {
	if _, err := computeChipDistribution(chipFlatBars(119, 10, 5, 1000), 0); err == nil {
		t.Fatalf("119 根应拒算")
	}
	_, err := computeChipDistribution(chipFlatBars(150, 10, 0, 1000), 0)
	if err == nil || !strings.Contains(err.Error(), "换手率") {
		t.Fatalf("换手全缺且无股本应报换手率缺失，得到 %v", err)
	}
	if _, err := computeChipDistribution(chipFlatBars(150, 10, 0, 1000), 1e7); err != nil {
		t.Fatalf("外部股本兜底应可算: %v", err)
	}
}
