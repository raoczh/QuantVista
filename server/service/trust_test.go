package service

import (
	"strconv"
	"strings"
	"testing"
)

// labeledHas 判断带路径值域中是否含某数值（测试辅助）。
func labeledHas(vals []labeledValue, x float64) bool {
	for _, v := range vals {
		if v.Value == x {
			return true
		}
	}
	return false
}

// secs 便捷构造单模块 sections。
func secs(module string, texts ...string) []evidenceSection {
	out := make([]evidenceSection, 0, len(texts))
	for _, t := range texts {
		out = append(out, evidenceSection{Module: module, Text: t})
	}
	return out
}

func TestVerifyEvidenceLabeled(t *testing.T) {
	vals := []labeledValue{
		{Path: "quote.price", Value: 12.34},
		{Path: "quant_score.total", Value: 78.5},
		{Path: "float_cap(亿)", Value: 156, Unit: "亿", Derived: true},
	}
	sections := secs("回答",
		"现价 12.34 站上 MA20，quant_score=78.5 池内第 3",
		"流通市值 156 亿，2026 年以来强势",
		"目标价 99.99 来自记忆", // 未吻合
	)
	ev := verifyEvidenceLabeled(sections, vals)
	if ev.Total != 4 {
		t.Fatalf("Total=%d, want 4（12.34/78.5/156/99.99；「第 3」「2026」跳过）", ev.Total)
	}
	if ev.Matched != 3 {
		t.Fatalf("Matched=%d, want 3", ev.Matched)
	}
	if ev.SkippedCount != 3 {
		t.Fatalf("SkippedCount=%d, want 3（MA20 的 20、第 3、2026）", ev.SkippedCount)
	}
	if ev.UnmatchedTotal != 1 {
		t.Fatalf("UnmatchedTotal=%d, want 1", ev.UnmatchedTotal)
	}
	if len(ev.Unmatched) != 1 || ev.Unmatched[0] != "99.99" {
		t.Fatalf("Unmatched=%v, want [99.99]", ev.Unmatched)
	}
	if len(ev.Items) != 4 {
		t.Fatalf("Items=%d, want 4", len(ev.Items))
	}
}

func TestVerifyEvidenceDirection(t *testing.T) {
	vals := []labeledValue{{Path: "quote.change_pct", Value: -4.21}}
	// 「下跌 4.2%」方向 down 与负值一致 → 命中。
	if ev := verifyEvidenceLabeled(secs("回答", "今日下跌 4.2%"), vals); ev.Matched != 1 {
		t.Fatalf("下跌 4.2%% 应命中 -4.21，matched=%d", ev.Matched)
	}
	// 「上涨 4.2%」方向 up 与负值相反 → direction_mismatch。
	ev := verifyEvidenceLabeled(secs("回答", "今日上涨 4.2%"), vals)
	if ev.Matched != 0 || len(ev.Items) != 1 || ev.Items[0].Reason != "direction_mismatch" {
		t.Fatalf("上涨 4.2%% 应 direction_mismatch，got matched=%d items=%+v", ev.Matched, ev.Items)
	}
	// 裸「4.2%」无方向词 → 取消无条件绝对值匹配，不命中。
	if ev := verifyEvidenceLabeled(secs("回答", "振幅约 4.2%"), vals); ev.Matched != 0 {
		t.Fatalf("裸 4.2%% 不应命中负值，matched=%d", ev.Matched)
	}
}

func TestVerifyEvidenceUnits(t *testing.T) {
	vals := []labeledValue{
		{Path: "main_net", Value: 3e7},         // 3000 万
		{Path: "cap", Value: 1.56e8},           // 1.56 亿
		{Path: "big", Value: 156e8},            // 156 亿
		{Path: "turnover", Value: 6.3, Unit: "%"},
	}
	ev := verifyEvidenceLabeled(secs("回答",
		"主力净流入 3000 万，市值 1.56 亿，总市值 156 亿，换手 6.3%"), vals)
	if ev.Matched != 4 {
		t.Fatalf("单位规范化后应全命中，matched=%d items=%+v", ev.Matched, ev.Items)
	}
}

func TestVerifyEvidenceDedup(t *testing.T) {
	vals := []labeledValue{{Path: "quote.price", Value: 12.34}}
	ev := verifyEvidenceLabeled(secs("回答", "现价 12.34；再看 12.34；仍是 12.34"), vals)
	if ev.Total != 1 || ev.Matched != 1 {
		t.Fatalf("去重后 Total/Matched 应为 1/1，got %d/%d", ev.Total, ev.Matched)
	}
	if len(ev.Items) != 1 || ev.Items[0].Count != 3 {
		t.Fatalf("重复计数应为 3，got items=%+v", ev.Items)
	}
}

func TestVerifyEvidenceTruncate(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		// 互异未命中小数（>99 避免跳过）。
		sb.WriteString(strconv.FormatFloat(100.01+float64(i), 'f', 2, 64))
		sb.WriteString(" ")
	}
	ev := verifyEvidenceLabeled(secs("回答", sb.String()), nil)
	if ev.Total != 60 || ev.UnmatchedTotal != 60 {
		t.Fatalf("Total/UnmatchedTotal want 60/60, got %d/%d", ev.Total, ev.UnmatchedTotal)
	}
	if !ev.Truncated || len(ev.Items) != 50 {
		t.Fatalf("应截断至 50，truncated=%v items=%d", ev.Truncated, len(ev.Items))
	}
	if len(ev.Unmatched) != 10 {
		t.Fatalf("legacy Unmatched 应 ≤10，got %d", len(ev.Unmatched))
	}
}

func TestSnapshotLabeledValues(t *testing.T) {
	snap := map[string]any{
		"quote":       map[string]any{"price": 12.34, "change_pct": -1.5, "amount": 2.5e9},
		"technicals":  map[string]any{"ma20": 11.98},
		"recent_bars": []map[string]any{{"close": 777.77}}, // 整棵排除
		"note":        "文本不参与",
		"zero":        0.0,
	}
	vals := snapshotLabeledValues(snap, map[string]string{"quote.": "2026-07-17 15:00:00"}, "recent_bars")
	for _, want := range []float64{12.34, -1.5, 11.98, 2.5e9, 25} { // 25 = 亿元换算
		if !labeledHas(vals, want) {
			t.Fatalf("值域缺少 %v，got %+v", want, vals)
		}
	}
	if labeledHas(vals, 777.77) {
		t.Fatalf("recent_bars 未被排除")
	}
	if labeledHas(vals, 0) {
		t.Fatalf("零值不应进入值域")
	}
	// 路径与 as_of 断言。
	var priceItem *labeledValue
	for i := range vals {
		if vals[i].Path == "quote.price" {
			priceItem = &vals[i]
		}
	}
	if priceItem == nil {
		t.Fatalf("缺 quote.price 路径")
	}
	if priceItem.AsOf != "2026-07-17 15:00:00" {
		t.Fatalf("as_of hint 未命中，got %q", priceItem.AsOf)
	}
}

func TestSnapshotLabeledSymbolPath(t *testing.T) {
	rows := []map[string]any{{"symbol": "600000", "price": 8.88}}
	vals := snapshotLabeledValues(rows, nil)
	found := false
	for _, v := range vals {
		if v.Path == "[600000].price" && v.Value == 8.88 {
			found = true
		}
	}
	if !found {
		t.Fatalf("数组元素应以 symbol 命名路径，got %+v", vals)
	}
}

func TestTextLabeledValues(t *testing.T) {
	vals := textLabeledValues("新闻标题", "context", []string{"现价 12.34（12.10）", "回调到 11.5 加仓", "净流入 3000 万", "近5日 涨停 2026"})
	// 12.34/12.10/11.5 三个小数 + 3000万整数（换算 3e7）；2026 整数无单位不取。
	for _, want := range []float64{12.34, 12.10, 11.5, 3e7} {
		if !labeledHas(vals, want) {
			t.Fatalf("值域缺少 %v，got %+v", want, vals)
		}
	}
	if labeledHas(vals, 2026) {
		t.Fatalf("无单位整数 2026 不应进入值域")
	}
	for _, v := range vals {
		if v.Origin != "context" {
			t.Fatalf("文本值域应带 origin=context，got %+v", v)
		}
	}
}
