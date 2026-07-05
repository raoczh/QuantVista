package service

import (
	"testing"

	"quantvista/datasource"
	"quantvista/model"
)

// mkBars 造升序日线：从 base 价起按 dailyPct 逐日复利，volume 恒定 vol（最后 lastVolMul 倍）。
func mkBars(n int, base, dailyPct float64, vol int64, lastVolMul float64) []datasource.Bar {
	bars := make([]datasource.Bar, 0, n)
	price := base
	for i := 0; i < n; i++ {
		price *= 1 + dailyPct/100
		v := vol
		if i == n-1 {
			v = int64(float64(vol) * lastVolMul)
		}
		bars = append(bars, datasource.Bar{
			TradeDate: "2026-01-02", Open: price * 0.99, High: price * 1.01, Low: price * 0.985,
			Close: price, Volume: v,
		})
	}
	return bars
}

// TestComputeCandFactors_Uptrend 稳步上升 + 末日放量：多头排列、创新高、放量因子。
func TestComputeCandFactors_Uptrend(t *testing.T) {
	bars := mkBars(90, 10, 0.8, 10000, 3.0) // 90 日日涨 0.8%，末日 3 倍量
	price := bars[len(bars)-1].Close
	f := computeCandFactors(price, bars)
	if f == nil {
		t.Fatalf("有数据不应返回 nil")
	}
	if !f.BullAlign {
		t.Fatalf("持续上升应为多头排列: MA5=%v MA10=%v MA20=%v", f.MA5, f.MA10, f.MA20)
	}
	if !f.High20d {
		t.Fatalf("持续上升末根应创 20 日新高")
	}
	if !f.AboveMA20 || f.Bias20 <= 0 {
		t.Fatalf("现价应站上 MA20 且乖离为正: above=%v bias=%v", f.AboveMA20, f.Bias20)
	}
	if f.VolBoost < 2.5 || f.VolBoost > 3.5 {
		t.Fatalf("末日 3 倍量的 VolBoost 应≈3，得到 %v", f.VolBoost)
	}
	if f.Chg5d <= 0 || f.Chg20d <= 0 {
		t.Fatalf("上升趋势近 5/20 日涨幅应为正: %v %v", f.Chg5d, f.Chg20d)
	}
	if f.Pos60 < 90 {
		t.Fatalf("持续新高的 60 日位置应接近 100，得到 %v", f.Pos60)
	}
	if f.MA60 <= 0 {
		t.Fatalf("90 根日线应能算出 MA60")
	}
}

// TestComputeCandFactors_Downtrend 阴跌：空头结构、不创新高。
func TestComputeCandFactors_Downtrend(t *testing.T) {
	bars := mkBars(90, 50, -0.7, 10000, 1)
	price := bars[len(bars)-1].Close
	f := computeCandFactors(price, bars)
	if f.BullAlign || f.High20d || f.AboveMA20 {
		t.Fatalf("阴跌不应有多头信号: %+v", f)
	}
	if f.Chg20d >= 0 {
		t.Fatalf("阴跌近 20 日涨幅应为负，得到 %v", f.Chg20d)
	}
	if f.Pos60 > 15 {
		t.Fatalf("阴跌 60 日位置应接近 0，得到 %v", f.Pos60)
	}
}

// TestComputeCandFactors_Empty 无数据返回 nil，不臆造。
func TestComputeCandFactors_Empty(t *testing.T) {
	if computeCandFactors(10, nil) != nil {
		t.Fatalf("空日线应返回 nil")
	}
	if computeCandFactors(0, mkBars(30, 10, 0.5, 100, 1)) != nil {
		t.Fatalf("无现价应返回 nil")
	}
}

// TestStrategyAdjust 各策略加分方向正确且给出可解释说明。
func TestStrategyAdjust(t *testing.T) {
	upBars := mkBars(90, 10, 0.8, 10000, 2.0)
	upPrice := upBars[len(upBars)-1].Close
	upF := computeCandFactors(upPrice, upBars)

	// momentum：新高+多头+温和放量应显著加分。
	delta, notes := strategyAdjust(model.RecTypeShortTerm, "momentum", candidate{Price: upPrice}, upF)
	if delta <= 0 || len(notes) == 0 {
		t.Fatalf("动量策略对强势股应加分并给说明: delta=%v notes=%v", delta, notes)
	}

	// value：低 PE/PB 加分。
	f2 := computeCandFactors(upPrice, upBars)
	f2.Pos60 = 30
	delta2, notes2 := strategyAdjust(model.RecTypeLongTerm, "value", candidate{Price: upPrice, PETTM: 12, PB: 1.2}, f2)
	if delta2 <= 0 {
		t.Fatalf("低估值应加分: %v %v", delta2, notes2)
	}

	// 亏损股扣分。
	delta3, _ := strategyAdjust(model.RecTypeLongTerm, "value", candidate{Price: 10, PETTM: -5, PB: 3.5}, f2)
	if delta3 >= delta2 {
		t.Fatalf("亏损股得分应低于低估值股: %v vs %v", delta3, delta2)
	}

	// nil 因子不加分不崩。
	if d, n := strategyAdjust(model.RecTypeShortTerm, "momentum", candidate{}, nil); d != 0 || n != nil {
		t.Fatalf("无因子应返回零: %v %v", d, n)
	}
}

// TestVerifyEvidence 证据数字核验：吻合/不吻合/窗口参数与小整数噪声跳过/extra 值域并入。
func TestVerifyEvidence(t *testing.T) {
	c := candidate{
		Price: 12.34, ChangePct: 3.21, TurnoverRate: 6.3, VolumeRatio: 2.1,
		Amount: 23.5e8, FloatCap: 156e8, PETTM: 18.6, Score: 78.5,
		Factors: &candFactors{MA20: 11.98, Bias20: 4.2, Chg5d: 5.6},
	}
	ev := verifyEvidence([]string{
		"现价 12.34 站上 MA20=11.98",       // 两个数字都吻合
		"换手率 6.3%、量比 2.1 温和放量",         // 吻合
		"score=78.5 池内第 11",            // 78.5 吻合；11 为 rank 小整数（≤99）跳过，不误报
		"量化分池内第 2/38",                 // rank/池大小小整数全部跳过（prompt 示范的引用格式）
		"近5日涨 5.6%（MA20 乖离 +4.2%）",    // 5/20 窗口噪声跳过；5.6/4.2 吻合
		"成交额 23.5 亿、流通市值 156 亿",       // 23.5 吻合；156 为 3 位整数（>99）参与核验且吻合
		"止盈 13.5 元，跌破 11.2 止损",        // 模型自身计划价经 extra 并入值域，应吻合
		"600000 为浦发银行代码，000001 亦不核验",  // 六位代码（含前导零解析为 1）全部跳过
		"主力资金净流入 8.88 亿（编造：快照里没有该数据）", // 8.88 不吻合
	}, c, 13.5, 11.2)
	if ev.Total == 0 || ev.Matched == 0 {
		t.Fatalf("应提取并匹配到数字: %+v", ev)
	}
	if ev.Matched != ev.Total-1 {
		t.Fatalf("应恰有 1 个数字不吻合（8.88），得到 matched=%d total=%d unmatched=%v", ev.Matched, ev.Total, ev.Unmatched)
	}
	if len(ev.Unmatched) != 1 || ev.Unmatched[0] != "8.88" {
		t.Fatalf("不吻合清单应为 [8.88]: %v", ev.Unmatched)
	}
}

// TestSystemConfidence 程序合成置信度：排名靠前+核验吻合=高；数据缺失=降档。
func TestSystemConfidence(t *testing.T) {
	strong := candidate{Rank: 2, PETTM: 15, Factors: &candFactors{BarCount: 90}}
	lvl, why := systemConfidence(strong, &evidenceCheck{Total: 6, Matched: 6}, 30)
	if lvl != "high" {
		t.Fatalf("前 25%% 且证据全吻合应为 high，得到 %s (%s)", lvl, why)
	}

	weak := candidate{Rank: 25, Factors: &candFactors{BarCount: 10}}
	lvl, _ = systemConfidence(weak, &evidenceCheck{Total: 5, Matched: 1}, 30)
	if lvl != "low" {
		t.Fatalf("排名靠后+核验差+数据不足应为 low，得到 %s", lvl)
	}

	mid := candidate{Rank: 12, PETTM: 20, Factors: &candFactors{BarCount: 60}}
	lvl, _ = systemConfidence(mid, &evidenceCheck{Total: 4, Matched: 2}, 30)
	if lvl != "medium" {
		t.Fatalf("中间情形应为 medium，得到 %s", lvl)
	}
}
