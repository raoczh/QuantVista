package service

import "testing"

func TestVerifyEvidenceValues(t *testing.T) {
	vals := []float64{12.34, 78.5, 156e8, 156} // 价格 / 量化分 / 市值（元与亿双口径）
	texts := []string{
		"现价 12.34 站上 MA20，quant_score=78.5 池内第 3",
		"流通市值 156 亿，2026 年以来强势",
		"目标价 99.99 来自记忆", // 未吻合
	}
	ev := verifyEvidenceValues(texts, vals)
	if ev.Total != 4 {
		t.Fatalf("Total=%d, want 4（12.34/78.5/156/99.99；「第 3」「2026」应跳过）", ev.Total)
	}
	if ev.Matched != 3 {
		t.Fatalf("Matched=%d, want 3", ev.Matched)
	}
	if len(ev.Unmatched) != 1 || ev.Unmatched[0] != "99.99" {
		t.Fatalf("Unmatched=%v, want [99.99]", ev.Unmatched)
	}
}

func TestVerifyEvidenceValuesTolerance(t *testing.T) {
	// 2% 容差与绝对值匹配（负涨幅 vs 正引用）。
	ev := verifyEvidenceValues([]string{"今日下跌 4.2%，接近 4.25 的引用"}, []float64{-4.21})
	if ev.Total != 2 || ev.Matched != 2 {
		t.Fatalf("got total=%d matched=%d, want 2/2", ev.Total, ev.Matched)
	}
}

func TestSnapshotValueSet(t *testing.T) {
	snap := map[string]any{
		"quote": map[string]any{"price": 12.34, "change_pct": -1.5, "amount": 2.5e9},
		"technicals": map[string]any{"ma20": 11.98},
		"recent_bars": []map[string]any{{"close": 777.77}}, // 应整棵排除
		"note":        "文本不参与",
		"zero":        0.0,
	}
	vals := snapshotValueSet(snap, "recent_bars")
	has := func(x float64) bool {
		for _, v := range vals {
			if v == x {
				return true
			}
		}
		return false
	}
	for _, want := range []float64{12.34, -1.5, 11.98, 2.5e9, 25} { // 25 = 2.5e9/1e8 亿元换算
		if !has(want) {
			t.Fatalf("值域缺少 %v，got %v", want, vals)
		}
	}
	if has(777.77) {
		t.Fatalf("recent_bars 未被排除：%v", vals)
	}
	if has(0) {
		t.Fatalf("零值不应进入值域：%v", vals)
	}
}

func TestSnapshotValueSetStruct(t *testing.T) {
	// 结构体经 JSON 归一化后同样可收集（日报快照是 struct）。
	type pos struct {
		ChangePctToday float64 `json:"change_pct_today"`
	}
	type snap struct {
		Positions []pos   `json:"positions"`
		MainNetYi float64 `json:"main_net_yi"`
	}
	vals := snapshotValueSet(snap{Positions: []pos{{ChangePctToday: -4.2}}, MainNetYi: 35.6})
	if len(vals) != 2 {
		t.Fatalf("got %v, want 2 个值", vals)
	}
}

// TestDecimalNumbersIn 文本型合法数据源（提醒文案/用户提问）的小数提取：
// 只取带小数点的、剔零；整数交给 verifyEvidenceValues 的跳过规则处理。
func TestDecimalNumbersIn(t *testing.T) {
	vals := decimalNumbersIn([]string{"现价 12.34 站上 MA20（12.10）", "如果回调到 11.5 加仓", "近5日 涨停 2026"})
	want := []float64{12.34, 12.10, 11.5}
	if len(vals) != len(want) {
		t.Fatalf("got %v, want %v", vals, want)
	}
	for i, v := range want {
		if vals[i] != v {
			t.Fatalf("got %v, want %v", vals, want)
		}
	}
}
