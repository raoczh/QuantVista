package service

import (
	"encoding/json"
	"testing"

	"quantvista/datasource"
)

func flagCodes(flags []riskFlag) map[string]string {
	m := map[string]string{}
	for _, f := range flags {
		m[f.Code] = f.Level
	}
	return m
}

// TestComputeRiskGate 各规则触发与不触发的边界。
func TestComputeRiskGate(t *testing.T) {
	// ST：block。
	q := &datasource.Quote{Name: "ST某某", Price: 5, ChangePct: 2, High: 5.1, Low: 4.9, PrevClose: 4.9, Amount: 5e7}
	v := &datasource.Valuation{IsST: true, TotalCap: 50e8}
	m := flagCodes(computeRiskGate(q, v))
	if m["st"] != "block" {
		t.Fatalf("ST 应为 block: %v", m)
	}

	// 名称含「退」且无估值：block（delist）。
	q2 := &datasource.Quote{Name: "某某退", Price: 1, ChangePct: -3, High: 1.02, Low: 0.98, PrevClose: 1.03, Amount: 5e7}
	m = flagCodes(computeRiskGate(q2, nil))
	if m["delist"] != "block" {
		t.Fatalf("退市整理应为 block: %v", m)
	}

	// 一字涨停：涨幅 10.02、振幅 0 → warn；正常涨停（有振幅）不触发。
	q3 := &datasource.Quote{Name: "正常股", Price: 11.0, ChangePct: 10.02, High: 11.0, Low: 11.0, PrevClose: 10.0, Amount: 2e8}
	m = flagCodes(computeRiskGate(q3, nil))
	if m["limit_board"] != "warn" {
		t.Fatalf("一字板应为 warn: %v", m)
	}
	q4 := &datasource.Quote{Name: "正常股", Price: 11.0, ChangePct: 10.02, High: 11.0, Low: 10.2, PrevClose: 10.0, Amount: 2e8}
	if _, ok := flagCodes(computeRiskGate(q4, nil))["limit_board"]; ok {
		t.Fatal("振幅 8% 的涨停不是一字板")
	}
	// 一字跌停同样触发（用估值振幅口径）。
	q5 := &datasource.Quote{Name: "正常股", Price: 9.0, ChangePct: -9.98, Amount: 2e8}
	v5 := &datasource.Valuation{Amplitude: 0.3}
	if flagCodes(computeRiskGate(q5, v5))["limit_board"] != "warn" {
		t.Fatal("一字跌停应为 warn")
	}

	// 流动性：成交额 2000 万 → warn；3000 万整不触发（严格小于）。
	q6 := &datasource.Quote{Name: "正常股", Price: 5, ChangePct: 1, High: 5.1, Low: 4.9, PrevClose: 4.95, Amount: 2e7}
	if flagCodes(computeRiskGate(q6, nil))["low_liquidity"] != "warn" {
		t.Fatal("成交额 2000 万应为流动性 warn")
	}
	q6.Amount = 3e7
	if _, ok := flagCodes(computeRiskGate(q6, nil))["low_liquidity"]; ok {
		t.Fatal("成交额恰为 3000 万不应触发")
	}

	// 小市值：25 亿 → info。
	v7 := &datasource.Valuation{TotalCap: 25e8}
	q7 := &datasource.Quote{Name: "正常股", Price: 5, ChangePct: 1, High: 5.1, Low: 4.9, PrevClose: 4.95, Amount: 2e8}
	if flagCodes(computeRiskGate(q7, v7))["small_cap"] != "info" {
		t.Fatal("市值 25 亿应为 info")
	}

	// 干净标的：无 flags。
	v8 := &datasource.Valuation{TotalCap: 100e8}
	if fl := computeRiskGate(q7, v8); len(fl) != 0 {
		t.Fatalf("干净标的不应有 flags: %+v", fl)
	}
}

// TestRiskGateTexts 原生与 JSON 反序列化两种快照形态都能提取提示文本（值域输入）。
func TestRiskGateTexts(t *testing.T) {
	flags := []riskFlag{{Level: "warn", Code: "limit_board", Text: "涨幅 10.02% 且振幅 0.00%"}}
	snap := map[string]any{"risk_gate": riskGateBlock(flags)}
	if texts := riskGateTexts(snap); len(texts) != 1 || texts[0] != flags[0].Text {
		t.Fatalf("原生形态提取失败: %v", texts)
	}
	// 经 JSON 序列化/反序列化（问答复用落库快照的路径）。
	b, _ := json.Marshal(snap)
	var snap2 map[string]any
	_ = json.Unmarshal(b, &snap2)
	if texts := riskGateTexts(snap2); len(texts) != 1 || texts[0] != flags[0].Text {
		t.Fatalf("反序列化形态提取失败: %v", texts)
	}
}

// TestParseRiskFlagsFromSnapshot 落库快照 JSON → 前端 risk_flags。
func TestParseRiskFlagsFromSnapshot(t *testing.T) {
	snap := map[string]any{"risk_gate": riskGateBlock([]riskFlag{{Level: "block", Code: "st", Text: "x"}})}
	b, _ := json.Marshal(snap)
	flags := parseRiskFlagsFromSnapshot(string(b))
	if len(flags) != 1 || flags[0].Code != "st" || flags[0].Level != "block" {
		t.Fatalf("解析不符: %+v", flags)
	}
	if parseRiskFlagsFromSnapshot("") != nil {
		t.Fatal("空快照应返回 nil")
	}
	if parseRiskFlagsFromSnapshot(`{"quote":{}}`) != nil {
		t.Fatal("无 risk_gate 块应返回 nil")
	}
}
