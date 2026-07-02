package datasource

import (
	"math"
	"testing"
)

// 真实响应结构的 fixture（字段值为构造值，位置与 qt.gtimg.cn 实际字段表一致：
// 38=换手率 39=PE-TTM 43=振幅 44=流通市值(亿) 45=总市值(亿) 46=PB 47=涨停 48=跌停 49=量比 52=PE动 53=PE静）。
const tencentFixture = `v_sh600000="1~浦发银行~600000~13.10~13.06~13.07~163223~81264~81959~13.09~1338~13.08~1487~13.07~1550~13.06~1738~13.05~2085~13.10~842~13.11~1656~13.12~2412~13.13~1571~13.14~1522~~20260630150403~0.04~0.31~13.19~13.05~13.10/163223/214028260~163223~21403~0.55~8.87~~13.19~13.05~1.07~3846.83~3874.16~1.05~14.37~11.75~0.90~~~13.11~-3.33~";`

func TestParseTencentFields(t *testing.T) {
	f, err := parseTencentFields(tencentFixture)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f[1] != "浦发银行" || f[2] != "600000" {
		t.Fatalf("名称/代码错位: %q %q", f[1], f[2])
	}
	if got := tencentAtof(f, 3); got != 13.10 {
		t.Fatalf("现价错位: %v", got)
	}
	if _, err := parseTencentFields(`v_sz000000=""`); err == nil {
		t.Fatal("空字段应报 ErrNoData")
	}
}

func TestParseTencentValuation(t *testing.T) {
	f, err := parseTencentFields(tencentFixture)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	v, err := parseTencentValuation(f, "cn", "600000", "tencent")
	if err != nil {
		t.Fatalf("估值解析失败: %v", err)
	}
	assertF := func(label string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-6 {
			t.Fatalf("%s: got %v want %v", label, got, want)
		}
	}
	assertF("换手率", v.TurnoverRate, 0.55)
	assertF("PE-TTM", v.PETTM, 8.87)
	assertF("振幅", v.Amplitude, 1.07)
	assertF("流通市值", v.FloatCap, 3846.83*1e8)
	assertF("总市值", v.TotalCap, 3874.16*1e8)
	assertF("PB", v.PB, 1.05)
	assertF("涨停价", v.LimitUp, 14.37)
	assertF("跌停价", v.LimitDown, 11.75)
	assertF("量比", v.VolumeRatio, 0.90)
	if v.IsST {
		t.Fatal("非 ST 股不应标记 IsST")
	}
	// 涨跌停价与昨收自洽（主板 ±10%）：13.06*1.1≈14.37、13.06*0.9≈11.75。
	if math.Abs(v.LimitUp-13.06*1.1) > 0.01 || math.Abs(v.LimitDown-13.06*0.9) > 0.01 {
		t.Fatalf("涨跌停与昨收不自洽: %v %v", v.LimitUp, v.LimitDown)
	}
}

func TestParseTencentValuationST(t *testing.T) {
	fixture := `v_sz000000="51~*ST某某~000000~2.10~2.08~2.09~1000~500~500~~~~~~~~~~~~~~~~~~~~~~20260630150403~0.02~0.96~2.15~2.05~2.10/1000/210000~1000~21~0.10~-5.2~~2.15~2.05~4.81~10.5~12.3~0.85~2.18~1.98~1.10~~~2.09~-1.1~";`
	f, err := parseTencentFields(fixture)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	v, err := parseTencentValuation(f, "cn", "000000", "tencent")
	if err != nil {
		t.Fatalf("估值解析失败: %v", err)
	}
	if !v.IsST {
		t.Fatal("*ST 股应标记 IsST")
	}
	if v.PETTM >= 0 {
		t.Fatalf("亏损股 PE 应为负: %v", v.PETTM)
	}
}
