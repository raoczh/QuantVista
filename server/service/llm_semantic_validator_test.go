package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/model"
	"quantvista/setting"
)

// P0-3 字段路径证据链（ev4）+ P0-4 跨模块 semantic validator 的单测与反例。
// flag 切换测试必须 Cleanup 恢复默认 true（options 表写库，内存库测试间共享）。

func setEvidenceRefsFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMEvidenceRefs(v); err != nil {
		t.Fatalf("切换 llm_evidence_refs 失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMEvidenceRefs(true) })
}

func setSemanticFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMSemanticValidator(v); err != nil {
		t.Fatalf("切换 llm_semantic_validator 失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMSemanticValidator(true) })
}

// ---------- P0-3：证据链 ID / source / unknowns / 关键结论段 ----------

// TestEvidenceRefsIDAndSource ev4：命中项按顺序分配 ev-001…，Source 随值域透传；
// 未命中项无 evidence_id。
func TestEvidenceRefsIDAndSource(t *testing.T) {
	vals := []labeledValue{
		{Path: "quote.price", Value: 9.88, AsOf: "2026-07-21 10:00:00", Source: "eastmoney"},
		{Path: "technicals.ma20", Value: 9.5, Source: "daily_bars"},
	}
	check := verifyEvidenceLabeled([]evidenceSection{
		{Module: "总结", Text: "现价 9.88 站上 MA20=9.5，另有凭空的 77.77"},
	}, vals)
	if check.Version != "ev4" {
		t.Fatalf("版本应为 ev4: %s", check.Version)
	}
	ids := map[string]bool{}
	for _, it := range check.Items {
		if it.Matched {
			if it.EvidenceID == "" {
				t.Fatalf("命中项缺 evidence_id: %+v", it)
			}
			if ids[it.EvidenceID] {
				t.Fatalf("evidence_id 重复: %s", it.EvidenceID)
			}
			ids[it.EvidenceID] = true
			if it.Source == "" {
				t.Fatalf("命中项应带 source（值域已提供）: %+v", it)
			}
		} else if it.EvidenceID != "" {
			t.Fatalf("未命中项不应有 evidence_id: %+v", it)
		}
	}
	if len(ids) != 2 || !ids["ev-001"] || !ids["ev-002"] {
		t.Fatalf("应分配 ev-001/ev-002: %v", ids)
	}
}

// TestStockFieldHintsSource ev4：source 提示从快照元数据推导（quote_source/valuation.source
// 逐次读取；technicals/finance/org_view 为结构性事实），旧快照缺元数据时对应维为空。
func TestStockFieldHintsSource(t *testing.T) {
	snap := map[string]any{
		"quote_as_of":  "2026-07-21 10:00:00",
		"quote_source": "tencent",
		"bars_as_of":   "2026-07-18",
		"valuation":    map[string]any{"pe_ttm": 12.5, "source": "tencent", "source_data_time": "2026-07-21 10:00"},
		"technicals":   map[string]any{"ma20": 9.5},
		"finance":      map[string]any{"roe": 15.2},
	}
	h := stockFieldHints(snap)
	if h == nil {
		t.Fatal("hints 不应为空")
	}
	vals := snapshotLabeledValues(snap, h, "recent_bars")
	bySrc := map[string]string{}
	for _, v := range vals {
		bySrc[v.Path] = v.Source
	}
	if bySrc["valuation.pe_ttm"] != "tencent" {
		t.Fatalf("valuation source 未命中: %q", bySrc["valuation.pe_ttm"])
	}
	if bySrc["technicals.ma20"] != "daily_bars" {
		t.Fatalf("technicals source 应为 daily_bars: %q", bySrc["technicals.ma20"])
	}
	if bySrc["finance.roe"] != "eastmoney_f10" {
		t.Fatalf("finance source 应为 eastmoney_f10: %q", bySrc["finance.roe"])
	}
	// 旧快照（无任何元数据）：hints 为 nil，不报错。
	if h := stockFieldHints(map[string]any{"quote": map[string]any{"price": 1.0}}); h != nil {
		t.Fatalf("无元数据旧快照应返回 nil hints: %+v", h)
	}
}

// TestAppendStockSnapshotUnknowns 快照 builder 结构化缺口：估值/日线/财务/机构观点缺失
// 逐项进 unknowns；flag 关闭不注入（回退 ev3 行为）；ETF 不把财务/机构观点算缺口。
func TestAppendStockSnapshotUnknowns(t *testing.T) {
	mk := func() map[string]any {
		return map[string]any{"quote": map[string]any{"price": 10.0}}
	}
	snap := mk()
	appendStockSnapshotUnknowns(snap, "cn", "600000", true)
	unk := snapshotUnknownItems(snap)
	paths := map[string]bool{}
	for _, u := range unk {
		if u.Reason == "" {
			t.Fatalf("unknown 缺 reason: %+v", u)
		}
		paths[u.FieldPath] = true
	}
	for _, want := range []string{"valuation", "technicals", "finance", "org_view"} {
		if !paths[want] {
			t.Fatalf("缺口应含 %s: %+v", want, unk)
		}
	}
	if _, ok := snap["unknowns_note"]; !ok {
		t.Fatal("应带 unknowns_note 说明语义")
	}

	// ETF：财务/机构观点不适用，不算缺口。
	etf := mk()
	appendStockSnapshotUnknowns(etf, "cn", "510300", false)
	for _, u := range snapshotUnknownItems(etf) {
		if u.FieldPath == "finance" || u.FieldPath == "org_view" {
			t.Fatalf("ETF 不应把 %s 算缺口", u.FieldPath)
		}
	}

	// 数据齐全：无 unknowns 键。
	full := map[string]any{
		"quote":      map[string]any{"price": 10.0},
		"technicals": map[string]any{"ma20": 9.5},
		"finance":    map[string]any{"roe": 15.0},
		"org_view":   map[string]any{"report_count_90d": 3.0},
	}
	appendStockSnapshotUnknowns(full, "cn", "600000", false)
	if _, ok := full["unknowns"]; ok {
		t.Fatal("数据齐全不应注入 unknowns")
	}

	// flag 关闭：回退 ev3 行为（不注入）。
	setEvidenceRefsFlag(t, false)
	off := mk()
	appendStockSnapshotUnknowns(off, "cn", "600000", true)
	if _, ok := off["unknowns"]; ok {
		t.Fatal("flag 关闭不应注入 unknowns")
	}
}

// TestSnapshotUnknownItemsJSONRoundTrip 问答复用落库快照：unknowns 经 JSON 序列化/
// 反序列化（[]any 形态）后仍可提取（QA 链路的真实形态）。
func TestSnapshotUnknownItemsJSONRoundTrip(t *testing.T) {
	snap := map[string]any{"quote": map[string]any{"price": 10.0}}
	appendStockSnapshotUnknowns(snap, "cn", "600000", true)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	unk := snapshotUnknownItems(back)
	if len(unk) == 0 {
		t.Fatal("JSON 回灌后应仍可提取 unknowns")
	}
	for _, u := range unk {
		if u.FieldPath == "" || u.Reason == "" {
			t.Fatalf("回灌 unknown 字段缺失: %+v", u)
		}
	}
	// 旧快照无 unknowns 键：返回 nil（前端 v-if 兜底的后端前提）。
	if got := snapshotUnknownItems(map[string]any{"quote": map[string]any{}}); got != nil {
		t.Fatalf("旧快照应返回 nil: %+v", got)
	}
}

// TestMarkKeySection 关键结论段佐证计数：只统计指定模块段；plan/user/context 复述
// 不算快照佐证；段内无数字时 Total=0。
func TestMarkKeySection(t *testing.T) {
	vals := []labeledValue{
		{Path: "quote.price", Value: 9.88},
		{Path: "交易计划", Value: 10.5, Origin: "plan"},
	}
	check := verifyEvidenceLabeled([]evidenceSection{
		{Module: "总结", Text: "现价 9.88，目标 10.5，另有凭空的 3.14"},
		{Module: "风险", Text: "跌破 9.88 观察"},
	}, vals)
	markKeySection(check, "总结")
	ks := check.KeySection
	if ks == nil || ks.Module != "总结" {
		t.Fatalf("key_section 缺失: %+v", ks)
	}
	if ks.Total != 3 || ks.SnapshotMatched != 1 {
		t.Fatalf("总结段应 3 个数字、1 个快照佐证（plan 复述与未命中不算），got %d/%d", ks.SnapshotMatched, ks.Total)
	}

	// 全复述反例：关键结论只有计划价复述 → SnapshotMatched=0，置信依据点名。
	check2 := verifyEvidenceLabeled([]evidenceSection{
		{Module: "总结", Text: "目标 10.5"},
	}, vals)
	markKeySection(check2, "总结")
	if check2.KeySection.SnapshotMatched != 0 || check2.KeySection.Total != 1 {
		t.Fatalf("全复述关键结论应 0/1: %+v", check2.KeySection)
	}
	_, why := analysisSystemConfidence(check2, map[string]any{})
	if !strings.Contains(why, "关键结论") {
		t.Fatalf("置信依据应点名关键结论无快照佐证: %q", why)
	}
}

// ---------- P0-4：跨字段语义校验 ----------

func blockSnap() map[string]any {
	return map[string]any{
		"quote":     map[string]any{"price": 10.0},
		"risk_gate": map[string]any{"flags": []riskFlag{{Level: "block", Code: "st"}}},
	}
}

// TestValidateAnalysisSemantics block+bullish 拒绝；neutral/无 block 放行；flag 关回退。
func TestValidateAnalysisSemantics(t *testing.T) {
	r := &AnalysisResult{Rating: model.AnalysisRatingBullish}
	if err := validateAnalysisSemantics(r, blockSnap()); err == nil {
		t.Fatal("block+bullish 应拒绝")
	} else if !strings.Contains(err.Error(), "bullish") {
		t.Fatalf("错误文案应指引修复: %v", err)
	}
	if err := validateAnalysisSemantics(&AnalysisResult{Rating: model.AnalysisRatingNeutral}, blockSnap()); err != nil {
		t.Fatalf("block+neutral 应放行: %v", err)
	}
	if err := validateAnalysisSemantics(r, map[string]any{"quote": map[string]any{"price": 10.0}}); err != nil {
		t.Fatalf("无 block 的 bullish 应放行: %v", err)
	}
	// JSON 回灌形态的 risk_gate（问答复用落库快照）同样识别。
	b, _ := json.Marshal(blockSnap())
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	if err := validateAnalysisSemantics(r, back); err == nil {
		t.Fatal("JSON 回灌 block 快照应同样拒绝")
	}

	setSemanticFlag(t, false)
	if err := validateAnalysisSemantics(r, blockSnap()); err != nil {
		t.Fatalf("flag 关闭应回退放行: %v", err)
	}
}

// TestValidatePanelSemantics 多数 bullish + block 拒绝；多数中性放行。
func TestValidatePanelSemantics(t *testing.T) {
	mk := func(ratings ...string) *PanelResult {
		p := &PanelResult{}
		roles := []string{"technical", "momentum", "risk", "contrarian"}
		for i, rt := range ratings {
			p.Roles = append(p.Roles, PanelRole{Role: roles[i], Rating: rt, Summary: "x"})
		}
		return p
	}
	if err := validatePanelSemantics(mk("bullish", "bullish", "bullish", "bearish"), blockSnap()); err == nil {
		t.Fatal("多数 bullish + block 应拒绝")
	}
	if err := validatePanelSemantics(mk("bullish", "neutral", "neutral", "bearish"), blockSnap()); err != nil {
		t.Fatalf("多数非 bullish 应放行: %v", err)
	}
}

// TestValidateTradePlanSemantics 统一收口：既有四价关系恒开；block/偏空上下文反证 flag 控。
func TestValidateTradePlanSemantics(t *testing.T) {
	plan := &tradePlan{BuyLow: 9.0, BuyHigh: 9.5, TargetPrice: 12.0, StopPrice: 8.5, HorizonDays: 10, Checklist: []string{"x"}}
	fresh := map[string]any{"quote": map[string]any{"price": 10.0}}
	if err := validateTradePlanSemantics(10, plan, model.AnalysisRatingBullish, fresh); err != nil {
		t.Fatalf("合法计划应通过: %v", err)
	}
	if err := validateTradePlanSemantics(10, plan, model.AnalysisRatingBullish, blockSnap()); err == nil {
		t.Fatal("block 上下文应拒绝计划")
	}
	if err := validateTradePlanSemantics(10, plan, model.AnalysisRatingBearish, fresh); err == nil {
		t.Fatal("偏空评级应拒绝买入计划")
	}
	// 既有专属校验不受 flag 控制：flag 关，四价违纪仍拒绝。
	setSemanticFlag(t, false)
	bad := &tradePlan{BuyLow: 9.0, BuyHigh: 9.5, TargetPrice: 12.0, StopPrice: 10.5, HorizonDays: 10, Checklist: []string{"x"}}
	if err := validateTradePlanSemantics(10, bad, model.AnalysisRatingBullish, fresh); err == nil {
		t.Fatal("flag 关闭不得放过既有四价违纪")
	}
	if err := validateTradePlanSemantics(10, plan, model.AnalysisRatingBearish, fresh); err != nil {
		t.Fatalf("flag 关闭应回退跨字段规则: %v", err)
	}
}

// TestApplyRecPickSemantics 短线 buy 盈亏比纪律：<1.5 透明降级 watch+注记（价位保留）；
// ≥1.5 保留 buy；watch/无价位不受影响；flag 关回退。
func TestApplyRecPickSemantics(t *testing.T) {
	mkBuy := func(tp float64) recPick {
		return recPick{Action: model.RecActionBuy, BuyZoneLow: 9.8, BuyZoneHigh: 10.2, TakeProfit: tp, StopLoss: 9.0}
	}
	// entry=10, risk=1.0：TP=11.4 → RR 1.4 < 1.5 降级。
	p := applyRecPickSemantics(mkBuy(11.4))
	if p.Action != model.RecActionWatch {
		t.Fatalf("RR 1.4 应降 watch: %s", p.Action)
	}
	if len(p.Risks) == 0 || !strings.Contains(p.Risks[0], "盈亏比") {
		t.Fatalf("降级应带注记: %v", p.Risks)
	}
	if p.TakeProfit != 11.4 || p.StopLoss != 9.0 {
		t.Fatalf("价位关系合法应保留价位: %+v", p)
	}
	// TP=11.5 → RR 1.5 恰达线保留。
	if p := applyRecPickSemantics(mkBuy(11.5)); p.Action != model.RecActionBuy {
		t.Fatalf("RR 1.5 应保留 buy: %s", p.Action)
	}
	// watch 不动。
	w := recPick{Action: model.RecActionWatch, BuyZoneLow: 9.8, BuyZoneHigh: 10.2, TakeProfit: 11.0, StopLoss: 9.0}
	if p := applyRecPickSemantics(w); p.Action != model.RecActionWatch || len(p.Risks) != 0 {
		t.Fatalf("watch 条目不应被改写: %+v", p)
	}
	// 长线无价位：不动。
	long := recPick{Action: model.RecActionBuy}
	if p := applyRecPickSemantics(long); p.Action != model.RecActionBuy {
		t.Fatalf("无价位 buy 不受 RR 纪律影响: %s", p.Action)
	}
	// flag 关：回退。
	setSemanticFlag(t, false)
	if p := applyRecPickSemantics(mkBuy(11.4)); p.Action != model.RecActionBuy {
		t.Fatalf("flag 关闭应回退: %s", p.Action)
	}
}

// TestNormalizePickRRDiscipline normalizePick 端到端：模型输出 buy + RR 不足 →
// 归一化后即为 watch（进入复核/仓位阶段前已定型，watch 不吃仓位）。
func TestNormalizePickRRDiscipline(t *testing.T) {
	p := normalizePick(recPick{
		Action: "buy", Confidence: 70,
		BuyZoneLow: 9.8, BuyZoneHigh: 10.2, TakeProfit: 11.2, StopLoss: 9.0, ValidDays: 5,
	}, "600000", candidate{Symbol: "600000", Price: 10})
	if p.Action != model.RecActionWatch {
		t.Fatalf("RR=(11.2-10)/1=1.2 <1.5 应降 watch: %s", p.Action)
	}
	found := false
	for _, r := range p.Risks {
		if strings.Contains(r, "盈亏比") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应带盈亏比注记: %v", p.Risks)
	}
}

// TestAnalysisSemanticRepairFlow 假 LLM 端到端（callWithRepair 层）：block 快照下
// 首轮 bullish 触发语义 repair（反馈含修复指引），第二轮 neutral 通过；恒 bullish 时
// 打满 1+maxRepairAttempts 轮后 result 仍为 nil（走既有 degraded 语义，不落成功）。
func TestAnalysisSemanticRepairFlow(t *testing.T) {
	mkSrv := func(fixAfter int, calls *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*calls++
			rating := "bullish"
			if fixAfter > 0 && *calls > fixAfter {
				rating = "neutral"
			}
			if *calls > 1 {
				// repair 轮的用户反馈必须含语义修复指引。
				b, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(b), "语义校验失败") {
					t.Errorf("repair 反馈应含语义校验错误文案")
				}
			}
			content := `{\"rating\":\"` + rating + `\",\"confidence\":60,\"summary\":\"ok\",\"disclaimer\":\"x\"}`
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
		}))
	}
	snap := blockSnap()
	runParse := func(srvURL string) (*AnalysisResult, error) {
		s := &AnalysisService{}
		cfg := &model.LLMConfig{BaseURL: srvURL, Model: "m"}
		var result *AnalysisResult
		parse := func(content string) error {
			r, perr := parseAnalysisResult(content)
			if perr != nil {
				return perr
			}
			if serr := validateAnalysisSemantics(r, snap); serr != nil {
				return serr
			}
			result = r
			return nil
		}
		run := newLLMRun("", "", "analysis", "analysis.v1", "test")
		_, _, _, err := s.callWithRepair(context.Background(), 0, run, cfg, "k", true, []chatMessage{{Role: "user", Content: "x"}}, parse, analysisRepairHint)
		return result, err
	}

	// 首轮违纪 → repair 修好。
	calls := 0
	srv := mkSrv(1, &calls)
	result, err := runParse(srv.URL)
	srv.Close()
	if err != nil {
		t.Fatalf("调用不应失败: %v", err)
	}
	if calls != 2 || result == nil || result.Rating != model.AnalysisRatingNeutral {
		t.Fatalf("应 2 轮修复为 neutral: calls=%d result=%+v", calls, result)
	}

	// 恒违纪 → 打满轮次后无结果（degraded，不落成功）。
	calls = 0
	srv2 := mkSrv(0, &calls)
	result2, err2 := runParse(srv2.URL)
	srv2.Close()
	if err2 != nil {
		t.Fatalf("恒违纪走 degraded 而非调用错误: %v", err2)
	}
	if result2 != nil {
		t.Fatal("语义恒不过不得产出成功结构化结果")
	}
	if calls != 1+maxRepairAttempts {
		t.Fatalf("总轮次应 1+maxRepairAttempts=%d, got %d", 1+maxRepairAttempts, calls)
	}
}
