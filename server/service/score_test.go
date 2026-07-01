package service

import (
	"testing"

	"quantvista/datasource"
)

// genBars 造升序日线：从 start 起按 step 线性变动，high/low 围绕 close，量固定。
func genBars(n int, start, step float64) []datasource.Bar {
	bars := make([]datasource.Bar, n)
	c := start
	for i := 0; i < n; i++ {
		bars[i] = datasource.Bar{
			Close: c, High: c * 1.01, Low: c * 0.99, Open: c, Volume: 1000,
		}
		c += step
	}
	return bars
}

// TestComputeScore_Strong 单边上涨（多头排列 + 正动量 + 高位）应得高分。
func TestComputeScore_Strong(t *testing.T) {
	bars := genBars(30, 10, 0.2) // 10 → 15.8 稳步上行
	price := bars[len(bars)-1].Close * 1.005
	r := computeScore(price, bars)
	if r.DataLimited {
		t.Fatalf("30 根不应标记数据受限")
	}
	if r.Total < 70 {
		t.Fatalf("强势单边上涨综合分应偏高(>=70)，得到 %v (%+v)", r.Total, r)
	}
	if r.Trend < 90 {
		t.Fatalf("多头排列趋势分应接近满分，得到 %v", r.Trend)
	}
	if r.Label != "强" && r.Label != "偏强" {
		t.Fatalf("标签应为强/偏强，得到 %s", r.Label)
	}
}

// TestComputeScore_Weak 单边下跌（空头排列 + 负动量 + 低位）应得低分。
func TestComputeScore_Weak(t *testing.T) {
	bars := genBars(30, 20, -0.3) // 20 → 11.3 单边下行
	price := bars[len(bars)-1].Close * 0.995
	r := computeScore(price, bars)
	if r.Total > 40 {
		t.Fatalf("弱势单边下跌综合分应偏低(<=40)，得到 %v (%+v)", r.Total, r)
	}
	if r.Trend > 20 {
		t.Fatalf("空头排列趋势分应很低，得到 %v", r.Trend)
	}
	if r.Label != "弱" && r.Label != "偏弱" {
		t.Fatalf("标签应为弱/偏弱，得到 %s", r.Label)
	}
}

// TestComputeScore_NoData 无日线/无价 → 全维中性 50。
func TestComputeScore_NoData(t *testing.T) {
	r := computeScore(0, nil)
	if r.Total != 50 || !r.DataLimited {
		t.Fatalf("无数据应全中性且标记受限: %+v", r)
	}
	r2 := computeScore(10, nil)
	if r2.Total != 50 {
		t.Fatalf("有价无日线应中性 50，得到 %v", r2.Total)
	}
}

// TestComputeScore_DataLimited 日线不足 20 根应标记受限但仍出分。
func TestComputeScore_DataLimited(t *testing.T) {
	bars := genBars(8, 10, 0.1)
	r := computeScore(bars[len(bars)-1].Close, bars)
	if !r.DataLimited {
		t.Fatalf("8 根应标记数据受限")
	}
	if r.Total < 0 || r.Total > 100 {
		t.Fatalf("分数应在 0-100，得到 %v", r.Total)
	}
}

// TestScoreLabel 阈值边界。
func TestScoreLabel(t *testing.T) {
	cases := map[float64]string{80: "强", 60: "偏强", 45: "中性", 30: "偏弱", 10: "弱"}
	for v, want := range cases {
		if got := scoreLabel(v); got != want {
			t.Fatalf("scoreLabel(%v)=%s，期望 %s", v, got, want)
		}
	}
}
