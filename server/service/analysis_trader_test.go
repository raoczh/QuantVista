package service

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/datasource"
	"quantvista/model"
)

func almostEq6(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// TestComputePositionAdvice 仓位公式手工验算：
// 21 根收盘价交替 ×1.05/×0.95 → 20 个日收益率恰为 +5%/-5% 各 10 个，
// 样本标准差 = sqrt(20×25/19) = 5.129892…，volCoef = 2.5/5.1299 = 0.4873 → round2 0.49；
// 涨家占比 3000/5000 = 0.6 → 择时 = 0.6+(0.6-0.5)×1.2 = 0.72；仓位 = 100×0.49×0.72 = 35.28。
func TestComputePositionAdvice(t *testing.T) {
	closes := []float64{100}
	for i := 0; i < 20; i++ {
		f := 1.05
		if i%2 == 1 {
			f = 0.95
		}
		closes = append(closes, closes[len(closes)-1]*f)
	}
	br := &datasource.Breadth{Advances: 3000, Declines: 1000, Unchanged: 1000}
	pos := computePositionAdvice(closes, br)
	if !almostEq6(pos.Vol20, 5.13) {
		t.Fatalf("vol20 = %v, want 5.13", pos.Vol20)
	}
	if !almostEq6(pos.VolCoef, 0.49) {
		t.Fatalf("volCoef = %v, want 0.49", pos.VolCoef)
	}
	if !almostEq6(pos.TimingCoef, 0.72) || !almostEq6(pos.AdvanceRatio, 0.6) {
		t.Fatalf("timing = %v adv = %v, want 0.72 / 0.6", pos.TimingCoef, pos.AdvanceRatio)
	}
	if !almostEq6(pos.PositionPct, 35.28) {
		t.Fatalf("positionPct = %v, want 35.28", pos.PositionPct)
	}
	if pos.Note != "" {
		t.Fatalf("数据齐全不应有缺失声明，got %q", pos.Note)
	}
}

// TestComputePositionAdvice_ClipAndMissing 边界：低波动贴上限、缺涨跌家数按中性 0.6、样本不足缺席。
func TestComputePositionAdvice_ClipAndMissing(t *testing.T) {
	// 低波动（每日 +0.1%）：2.5/0.1 = 25 → clip 1.0；极端多头 breadth → timing 夹 1.2 上限；仓位夹 100。
	closes := []float64{100}
	for i := 0; i < 20; i++ {
		closes = append(closes, closes[len(closes)-1]*1.001)
	}
	pos := computePositionAdvice(closes, &datasource.Breadth{Advances: 5000, Declines: 0, Unchanged: 0})
	if pos.VolCoef != 1.0 || pos.TimingCoef != 1.2 {
		t.Fatalf("clip 失败: volCoef=%v timing=%v", pos.VolCoef, pos.TimingCoef)
	}
	if pos.PositionPct != 100 {
		t.Fatalf("仓位应夹 100, got %v", pos.PositionPct)
	}
	// 极端空头：timing 夹 0.3 下限。
	pos = computePositionAdvice(closes, &datasource.Breadth{Advances: 0, Declines: 5000})
	if pos.TimingCoef != 0.3 {
		t.Fatalf("timing 下限应为 0.3, got %v", pos.TimingCoef)
	}
	// 缺 breadth：择时按中性 0.6 且声明。
	pos = computePositionAdvice(closes, nil)
	if pos.TimingCoef != 0.6 || !strings.Contains(pos.Note, "涨跌家数不可得") {
		t.Fatalf("缺 breadth 应按 0.6 并声明, got timing=%v note=%q", pos.TimingCoef, pos.Note)
	}
	// 样本不足：不给仓位数字。
	pos = computePositionAdvice(closes[:15], nil)
	if pos.PositionPct != 0 || !strings.Contains(pos.Note, "20 日波动率不可得") {
		t.Fatalf("样本不足应缺席, got %+v", pos)
	}
}

// TestValidateTradePlan 自洽纪律逐条：止损<现价是硬纪律。
func TestValidateTradePlan(t *testing.T) {
	px := 10.0
	valid := func() *tradePlan {
		return &tradePlan{BuyLow: 9.0, BuyHigh: 9.5, TargetPrice: 12.0, StopPrice: 8.5,
			HorizonDays: 10, Checklist: []string{"竞价高开超 5% 放弃"}}
	}
	if err := validateTradePlan(px, valid()); err != nil {
		t.Fatalf("合法计划不应报错: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*tradePlan)
		want string
	}{
		{"止损高于现价", func(p *tradePlan) { p.StopPrice = 10.5; p.BuyLow = 11; p.BuyHigh = 11.5; p.TargetPrice = 13 }, "低于现价"},
		{"止损等于现价", func(p *tradePlan) { p.StopPrice = 10.0 }, "低于现价"},
		{"止损不低于区间下沿", func(p *tradePlan) { p.StopPrice = 9.2 }, "买入区间下沿"},
		{"目标不高于区间上沿", func(p *tradePlan) { p.TargetPrice = 9.4 }, "目标价"},
		{"区间倒挂", func(p *tradePlan) { p.BuyLow = 9.6 }, "buy_low"},
		{"周期越界", func(p *tradePlan) { p.HorizonDays = 0 }, "horizon_days"},
		{"清单为空", func(p *tradePlan) { p.Checklist = nil }, "checklist"},
		{"价位非正", func(p *tradePlan) { p.StopPrice = 0 }, "正数"},
	}
	for _, c := range cases {
		p := valid()
		c.mut(p)
		err := validateTradePlan(px, p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 期望含 %q 的错误, got %v", c.name, c.want, err)
		}
	}
}

// TestApplyPlanDiscipline 盈亏比手工验算与 <2:1 降仓。
func TestApplyPlanDiscipline(t *testing.T) {
	// entry = 9.25, risk = 0.75, reward = 2.75 → rr = 3.67 不降仓。
	p := &tradePlan{BuyLow: 9.0, BuyHigh: 9.5, TargetPrice: 12.0, StopPrice: 8.5}
	pos := &positionAdvice{PositionPct: 40}
	applyPlanDiscipline(p, pos)
	if !almostEq6(p.RRRatio, 3.67) || pos.PositionPct != 40 || len(p.Discipline) != 0 {
		t.Fatalf("rr=3.67 不应降仓, got rr=%v pos=%v disc=%v", p.RRRatio, pos.PositionPct, p.Discipline)
	}
	// reward = 1.25 → rr = 1.6667 < 2 → 仓位减半 + 纪律说明。
	p = &tradePlan{BuyLow: 9.0, BuyHigh: 9.5, TargetPrice: 10.5, StopPrice: 8.5}
	pos = &positionAdvice{PositionPct: 40}
	applyPlanDiscipline(p, pos)
	if !almostEq6(p.RRRatio, 1.67) || !almostEq6(pos.PositionPct, 20) {
		t.Fatalf("rr<2 应减半, got rr=%v pos=%v", p.RRRatio, pos.PositionPct)
	}
	if len(p.Discipline) != 1 || !strings.Contains(p.Discipline[0], "减半") {
		t.Fatalf("缺纪律说明: %v", p.Discipline)
	}
}

// TestParseTradePlan_StripServerFields 模型伪造的服务端字段（rr_ratio/position/discipline_notes）必须剥除。
func TestParseTradePlan_StripServerFields(t *testing.T) {
	raw := `{"buy_low":9,"buy_high":9.5,"target_price":12,"stop_price":8.5,"horizon_days":10,
		"plan_note":"回踩买入","checklist":["a"],"rr_ratio":99,"position":{"position_pct":100},"discipline_notes":["伪造通过"]}`
	p, err := parseTradePlan(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.RRRatio != 0 || p.Position != nil || p.Discipline != nil {
		t.Fatalf("服务端字段未剥除: %+v", p)
	}
	// position 输出成字符串等错误类型也不该炸（剥除在反序列化前）。
	if _, err := parseTradePlan(`{"no_plan":true,"position":"pass"}`); err != nil {
		t.Fatalf("错误类型的服务端字段应被剥除后正常解析: %v", err)
	}
}

// TestTradePlanSnapshotHelpers 快照提取兼容 Go 原生 map 与 JSON 回灌两形态。
func TestTradePlanSnapshotHelpers(t *testing.T) {
	snap := map[string]any{
		"quote":       map[string]any{"price": 12.34},
		"recent_bars": []map[string]any{{"c": 10.0}, {"c": 10.5}},
		"risk_gate":   map[string]any{"flags": []riskFlag{{Level: "warn"}}},
	}
	if quotePriceFromSnapshot(snap) != 12.34 {
		t.Fatal("quote price 提取失败")
	}
	if cs := closesFromSnapshot(snap); len(cs) != 2 || cs[1] != 10.5 {
		t.Fatalf("closes 提取失败: %v", cs)
	}
	if hasBlockRiskFlag(snap) {
		t.Fatal("warn 不应判 block")
	}
	// JSON 回灌形态（[]any + map[string]any）。
	b, _ := json.Marshal(snap)
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	if cs := closesFromSnapshot(back); len(cs) != 2 {
		t.Fatalf("JSON 形态 closes 提取失败: %v", cs)
	}
	back["risk_gate"].(map[string]any)["flags"].([]any)[0].(map[string]any)["level"] = "block"
	if !hasBlockRiskFlag(back) {
		t.Fatal("JSON 形态 block 判定失败")
	}
}

// TestAttachTradePlan_DeterministicSkips 偏空/风险闸门/现价缺失三种确定性拒绝不发起 LLM 调用。
func TestAttachTradePlan_DeterministicSkips(t *testing.T) {
	s := &AnalysisService{}
	req := AnalyzeRequest{Module: model.AnalysisModuleStock, Mode: model.AnalysisModeStandard}
	// 偏空。
	r := &AnalysisResult{Rating: model.AnalysisRatingBearish}
	u, _ := s.attachTradePlan(context.Background(), 0, nil, "", true, req,
		map[string]any{"quote": map[string]any{"price": 10.0}}, r, "", "")
	if u.TotalTokens != 0 || r.TradePlan == nil || !r.TradePlan.NoPlan {
		t.Fatalf("偏空应零成本 NoPlan: %+v", r.TradePlan)
	}
	// 风险闸门 block。
	r = &AnalysisResult{Rating: model.AnalysisRatingBullish}
	snap := map[string]any{
		"quote":     map[string]any{"price": 10.0},
		"risk_gate": map[string]any{"flags": []riskFlag{{Level: "block"}}},
	}
	s.attachTradePlan(context.Background(), 0, nil, "", true, req, snap, r, "", "")
	if r.TradePlan == nil || !r.TradePlan.NoPlan || !strings.Contains(r.TradePlan.NoPlanReason, "风险闸门") {
		t.Fatalf("block 应 NoPlan: %+v", r.TradePlan)
	}
	// 现价缺失。
	r = &AnalysisResult{Rating: model.AnalysisRatingNeutral}
	s.attachTradePlan(context.Background(), 0, nil, "", true, req, map[string]any{"freshness_status": "fresh"}, r, "", "")
	if r.TradePlan == nil || !r.TradePlan.NoPlan {
		t.Fatal("现价缺失应 NoPlan")
	}
	// 行情过期（stale）/时效未知（缺 freshness_status）：不生成精确交易计划。
	r = &AnalysisResult{Rating: model.AnalysisRatingBullish}
	s.attachTradePlan(context.Background(), 0, nil, "", true, req,
		map[string]any{"quote": map[string]any{"price": 10.0}, "freshness_status": "stale"}, r, "", "")
	if r.TradePlan == nil || !r.TradePlan.NoPlan || !strings.Contains(r.TradePlan.NoPlanReason, "行情") {
		t.Fatalf("stale 行情应 NoPlan: %+v", r.TradePlan)
	}
	r = &AnalysisResult{Rating: model.AnalysisRatingBullish}
	s.attachTradePlan(context.Background(), 0, nil, "", true, req,
		map[string]any{"quote": map[string]any{"price": 10.0}}, r, "", "")
	if r.TradePlan == nil || !r.TradePlan.NoPlan {
		t.Fatal("缺 freshness_status（时效未知）应 NoPlan")
	}
	// 有 freshness_note（全源过期仍带旧价返回）同样拒绝。
	r = &AnalysisResult{Rating: model.AnalysisRatingBullish}
	s.attachTradePlan(context.Background(), 0, nil, "", true, req,
		map[string]any{"quote": map[string]any{"price": 10.0}, "freshness_status": "fresh", "freshness_note": "行情仅更新至…"}, r, "", "")
	if r.TradePlan == nil || !r.TradePlan.NoPlan {
		t.Fatal("带 freshness_note 应 NoPlan")
	}
	// 非个股/回溯模式不追加。
	r = &AnalysisResult{Rating: model.AnalysisRatingBullish}
	s.attachTradePlan(context.Background(), 0, nil, "", true,
		AnalyzeRequest{Module: model.AnalysisModuleStock, Mode: model.AnalysisModeStandard, AsOf: "2026-06-01"},
		map[string]any{"quote": map[string]any{"price": 10.0}}, r, "", "")
	if r.TradePlan != nil {
		t.Fatal("回溯模式不应生成交易计划")
	}
}

// TestAttachTradePlan_EndToEnd 假 LLM 服务器端到端：首次输出止损违纪触发 repair，
// 第二次合格 → 计划落位、盈亏比/仓位回填、token 累计两次调用。
func TestAttachTradePlan_EndToEnd(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		content := `{\"buy_low\":9.0,\"buy_high\":9.5,\"target_price\":12.0,\"stop_price\":10.5,\"horizon_days\":10,\"plan_note\":\"回踩 MA20=9.2 附近买入\",\"checklist\":[\"竞价高开超 5% 放弃\",\"跌破 8.5 止损\"]}`
		if calls >= 2 {
			content = `{\"buy_low\":9.0,\"buy_high\":9.5,\"target_price\":12.0,\"stop_price\":8.5,\"horizon_days\":10,\"plan_note\":\"回踩 MA20=9.2 附近买入\",\"checklist\":[\"竞价高开超 5% 放弃\",\"跌破 8.5 止损\"]}`
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}))
	defer srv.Close()

	bars := []map[string]any{}
	c := 100.0
	for i := 0; i < 25; i++ {
		c *= 1.01
		bars = append(bars, map[string]any{"c": c})
	}
	snap := map[string]any{
		"quote":            map[string]any{"price": 10.0},
		"recent_bars":      bars,
		"freshness_status": "fresh",
	}
	s := &AnalysisService{} // market=nil → breadth 缺失走中性 0.6
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m"}
	r := &AnalysisResult{Rating: model.AnalysisRatingBullish}
	usage, _ := s.attachTradePlan(context.Background(), 0, cfg, "k", true,
		AnalyzeRequest{Module: model.AnalysisModuleStock, Mode: model.AnalysisModeStandard}, snap, r, "", "")

	if calls != 2 {
		t.Fatalf("违纪应触发 repair，期望 2 次调用, got %d", calls)
	}
	if usage.TotalTokens != 300 {
		t.Fatalf("token 应累计两次 = 300, got %d", usage.TotalTokens)
	}
	tp := r.TradePlan
	if tp == nil || tp.NoPlan {
		t.Fatalf("应产出计划: %+v", tp)
	}
	if tp.StopPrice != 8.5 || tp.TargetPrice != 12.0 {
		t.Fatalf("计划价位错误: %+v", tp)
	}
	if !almostEq6(tp.RRRatio, 3.67) {
		t.Fatalf("rr = %v, want 3.67", tp.RRRatio)
	}
	if tp.Position == nil || tp.Position.PositionPct <= 0 || tp.Position.TimingCoef != 0.6 {
		t.Fatalf("仓位建议缺失或择时非中性: %+v", tp.Position)
	}
	if len(tp.Checklist) != 2 {
		t.Fatalf("checklist 丢失: %v", tp.Checklist)
	}
}

// TestParseAnalysisResult_StripTradePlan 主分析解析剥除模型伪造的 trade_plan 字段。
func TestParseAnalysisResult_StripTradePlan(t *testing.T) {
	raw := `{"rating":"bullish","confidence":60,"summary":"ok","trade_plan":{"buy_low":1}}`
	r, err := parseAnalysisResult(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.TradePlan != nil {
		t.Fatal("模型自附的 trade_plan 未被剥除")
	}
}
