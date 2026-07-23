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

// ---------- P1-3 条件式独立 bull/bear/judge 辩论 ----------

func setDebateFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMConditionalDebate(v); err != nil {
		t.Fatalf("切换条件式辩论开关失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMConditionalDebate(true) })
}

// snapWithFlags 构造带风险闸门标志的快照。
func snapWithFlags(levels ...string) map[string]any {
	flags := make([]riskFlag, 0, len(levels))
	for _, lv := range levels {
		flags = append(flags, riskFlag{Level: lv, Code: "x", Text: "t"})
	}
	return map[string]any{"risk_gate": map[string]any{"flags": flags}}
}

// TestDebateTriggerReasons 触发判定表驱动：低置信/矛盾 claim/warn 风险各自触发；
// 高置信+无冲突不触发（默认单路零成本）；block 不触发（语义校验已硬约束，辩论无从
// 改变结论）；flag 关不触发。
func TestDebateTriggerReasons(t *testing.T) {
	setDebateFlag(t, true)

	highOK := &AnalysisResult{SysConfidence: "high"}
	if got := debateTriggerReasons(highOK, map[string]any{}); got != nil {
		t.Fatalf("高置信+无冲突不应触发: %v", got)
	}
	low := &AnalysisResult{SysConfidence: "low"}
	if got := debateTriggerReasons(low, map[string]any{}); len(got) != 1 || got[0] != debateTriggerLowConfidence {
		t.Fatalf("低置信应触发 low_confidence: %v", got)
	}
	contra := &AnalysisResult{
		SysConfidence: "medium",
		EvidenceCheck: &evidenceCheck{Claims: []llmClaim{{ClaimID: "cl-01", Status: claimContradictory}}},
	}
	if got := debateTriggerReasons(contra, map[string]any{}); len(got) != 1 || got[0] != debateTriggerContradictory {
		t.Fatalf("矛盾 claim 应触发 contradictory_claims: %v", got)
	}
	warn := &AnalysisResult{SysConfidence: "medium"}
	if got := debateTriggerReasons(warn, snapWithFlags("warn")); len(got) != 1 || got[0] != debateTriggerRiskBorder {
		t.Fatalf("warn 风险应触发 risk_gate_borderline: %v", got)
	}
	// 多条件叠加：低置信 + warn。
	multi := &AnalysisResult{SysConfidence: "low"}
	if got := debateTriggerReasons(multi, snapWithFlags("warn")); len(got) != 2 {
		t.Fatalf("多条件应并列: %v", got)
	}
	// block 级：即使低置信也不触发（辩论无从改变 block 硬约束下的结论）。
	if got := debateTriggerReasons(low, snapWithFlags("block", "warn")); got != nil {
		t.Fatalf("block 不应触发辩论: %v", got)
	}
	// JSON 回灌形态的 warn flag（问答/历史快照复用路径）。
	jsonSnap := map[string]any{"risk_gate": map[string]any{
		"flags": []any{map[string]any{"level": "warn", "code": "x", "text": "t"}},
	}}
	if got := debateTriggerReasons(warn, jsonSnap); len(got) != 1 {
		t.Fatalf("JSON 形态 warn 应触发: %v", got)
	}

	// flag 关：任何条件都不触发。
	if err := setting.SetLLMConditionalDebate(false); err != nil {
		t.Fatalf("关闭开关失败: %v", err)
	}
	if got := debateTriggerReasons(low, snapWithFlags("warn")); got != nil {
		t.Fatalf("flag 关不应触发: %v", got)
	}
}

// TestNormalizeDebateClaims 归一纪律：程序重编号、证据白名单外剥除、条数上限、
// 全部无 invalidator 报错（触发 repair）、空 claims 报错。
func TestNormalizeDebateClaims(t *testing.T) {
	allow := map[string]bool{"ev-001": true, "ev-002": true}
	in := []debateClaim{
		{ID: "模型自编-99", Text: "论点A", EvidenceIDs: []string{"ev-001", "ev-999"}, Invalidator: "条件A"},
		{Text: "  "}, // 空文本剔除
		{Text: "论点B", EvidenceIDs: []string{"ev-002"}},
		{Text: "论点C"},
		{Text: "论点D"},
		{Text: "论点E（超上限丢弃）", Invalidator: "x"},
	}
	out, err := normalizeDebateClaims(in, "bu", allow)
	if err != nil {
		t.Fatalf("归一失败: %v", err)
	}
	if len(out) != debateMaxClaims {
		t.Fatalf("应钳制在 %d 条: %d", debateMaxClaims, len(out))
	}
	if out[0].ID != "bu-01" || out[1].ID != "bu-02" {
		t.Fatalf("id 应程序重编号（不信模型自报）: %+v", out)
	}
	if len(out[0].EvidenceIDs) != 1 || out[0].EvidenceIDs[0] != "ev-001" {
		t.Fatalf("白名单外 evidence_id 应剥除: %+v", out[0].EvidenceIDs)
	}

	if _, err := normalizeDebateClaims([]debateClaim{{Text: "无失效条件"}}, "bu", allow); err == nil {
		t.Fatalf("全部无 invalidator 应报错触发 repair")
	}
	if _, err := normalizeDebateClaims(nil, "bu", allow); err == nil {
		t.Fatalf("空 claims 应报错")
	}
}

// TestHasSharedEvidence rebuttal 触发判定：双方引用同一 evidence_id 才触发。
func TestHasSharedEvidence(t *testing.T) {
	bull := []debateClaim{{ID: "bu-01", EvidenceIDs: []string{"ev-001", "ev-002"}}}
	if hasSharedEvidence(bull, []debateClaim{{ID: "be-01", EvidenceIDs: []string{"ev-003"}}}) {
		t.Fatalf("证据集不相交不应触发反驳轮")
	}
	if !hasSharedEvidence(bull, []debateClaim{{ID: "be-01", EvidenceIDs: []string{"ev-002"}}}) {
		t.Fatalf("同一证据对立解读应触发反驳轮")
	}
}

// debateTestResult 构造一份触发辩论的主分析结果（低置信 + 证据白名单两项）。
func debateTestResult() *AnalysisResult {
	return &AnalysisResult{
		Rating: model.AnalysisRatingNeutral, Summary: "主分析总结",
		SysConfidence: "low", SysConfidenceWhy: "核验吻合率低",
		EvidenceCheck: &evidenceCheck{
			Version: "ev5",
			Items: []evidenceItem{
				{Matched: true, EvidenceID: "ev-001", Path: "现价", SnapValue: 10.5},
				{Matched: true, EvidenceID: "ev-002", Path: "涨跌幅%", SnapValue: -3.2},
			},
			Claims: []llmClaim{{ClaimID: "cl-01", Section: "总结", Text: "主分析总结", Status: claimUnresolved}},
		},
	}
}

// debateFakeServer 按请求体中的角色 system 关键词分流响应的假 LLM。
func debateFakeServer(t *testing.T, calls *[]string, bullContent, bearContent, rebuttalContent, judgeContent func() string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		var content string
		switch {
		case strings.Contains(body, "本轮你只写反驳"):
			*calls = append(*calls, "rebuttal")
			content = rebuttalContent()
		case strings.Contains(body, "只建立当前数据快照下最强的看多论证"):
			*calls = append(*calls, "bull")
			content = bullContent()
		case strings.Contains(body, "只寻找会让看多论点失败的风险"):
			*calls = append(*calls, "bear")
			content = bearContent()
		case strings.Contains(body, "你是辩论裁判"):
			*calls = append(*calls, "judge")
			content = judgeContent()
		default:
			t.Errorf("未识别的辩论请求: %s", body[:min(200, len(body))])
			content = "{}"
		}
		esc, _ := json.Marshal(content)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(esc) + `}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
}

// TestDebateEndToEnd 假 LLM 端到端（无共享证据 → 无 rebuttal，3 次调用）：
// claim id 程序重编号、越界 evidence_id/claim_id 剥除、judge 落位、主结果零改写、
// manifest 声明（bull run 携带触发条件与预算）。
func TestDebateEndToEnd(t *testing.T) {
	setDebateFlag(t, true)

	var calls []string
	srv := debateFakeServer(t, &calls,
		func() string {
			return `{"claims":[{"id":"乱写","text":"量能改善","evidence_ids":["ev-001","ev-偽造"],"invalidator":"跌破支撑"}]}`
		},
		func() string {
			return `{"claims":[{"text":"趋势走弱","evidence_ids":["ev-002"],"confirmed":true,"invalidator":"放量收复"}],` +
				`"challenges":[{"claim_id":"bu-01","text":"量能改善持续性存疑"},{"claim_id":"bu-99","text":"越界引用"}]}`
		},
		func() string { return `{"rebuttals":[]}` },
		func() string {
			return `{"verdict":"bearish","decisive_claim_ids":["be-01","bu-88"],"rejected_claim_ids":["bu-01"],` +
				`"unresolved_claim_ids":[],"confidence_reason":"看空证据更直接","invalidators":["放量反包"],"conflict_note":""}`
		})
	defer srv.Close()

	svc := &AnalysisService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 8000}
	result := debateTestResult()
	ratingBefore, summaryBefore := result.Rating, result.Summary

	usage, runs := svc.attachDebate(context.Background(), 7, cfg, "sk", true,
		map[string]any{}, result, "t1", "r-main")

	if got := strings.Join(calls, ","); got != "bull,bear,judge" {
		t.Fatalf("无共享证据应恰 3 次调用（无 rebuttal）: %s", got)
	}
	if usage.TotalTokens != 45 {
		t.Fatalf("usage 应累计 3 次调用: %+v", usage)
	}
	deb := result.Debate
	if deb == nil || !deb.Triggered || deb.DegradedReason != "" {
		t.Fatalf("辩论应完整成功: %+v", deb)
	}
	if len(deb.TriggerReasons) != 1 || deb.TriggerReasons[0] != debateTriggerLowConfidence {
		t.Fatalf("触发原因应为 low_confidence: %v", deb.TriggerReasons)
	}
	if deb.Rounds != 1 || deb.Version != debateVersion {
		t.Fatalf("无共享证据应 1 轮: %+v", deb)
	}
	// claim id 重编号 + 越界证据剥除。
	if len(deb.Bull) != 1 || deb.Bull[0].ID != "bu-01" ||
		len(deb.Bull[0].EvidenceIDs) != 1 || deb.Bull[0].EvidenceIDs[0] != "ev-001" {
		t.Fatalf("bull claims 归一不符: %+v", deb.Bull)
	}
	if len(deb.Bear) != 1 || deb.Bear[0].ID != "be-01" || deb.Bear[0].Confirmed == nil || !*deb.Bear[0].Confirmed {
		t.Fatalf("bear claims 归一不符: %+v", deb.Bear)
	}
	// challenge 越界引用剥除。
	if len(deb.Challenges) != 1 || deb.Challenges[0].ClaimID != "bu-01" {
		t.Fatalf("越界 challenge 应剥除: %+v", deb.Challenges)
	}
	// judge：非法 claim id（bu-88）剥除，合法引用保留。
	j := deb.Judge
	if j == nil || j.Verdict != model.AnalysisRatingBearish {
		t.Fatalf("judge 应落位: %+v", j)
	}
	if len(j.DecisiveClaimIDs) != 1 || j.DecisiveClaimIDs[0] != "be-01" {
		t.Fatalf("judge 非法 claim id 应剥除: %+v", j.DecisiveClaimIDs)
	}
	if len(j.RejectedClaimIDs) != 1 || j.RejectedClaimIDs[0] != "bu-01" {
		t.Fatalf("rejected 引用应保留: %+v", j.RejectedClaimIDs)
	}
	// 主结果零改写（rating=neutral 与 bearish 不构成相反，置信度已是 low 不再变化）。
	if result.Rating != ratingBefore || result.Summary != summaryBefore {
		t.Fatalf("辩论不得改写主结果: rating=%s summary=%s", result.Rating, result.Summary)
	}
	// manifest：3 个 run，bull run 携带 DebateInfo 声明。
	if len(runs) != 3 {
		t.Fatalf("应 3 个 run: %d", len(runs))
	}
	if runs[0].Module != "debate_bull" || runs[0].DebateInfo == nil {
		t.Fatalf("bull run 应携带辩论声明: %+v", runs[0])
	}
	di := runs[0].DebateInfo
	if di.Rounds != 1 || di.MaxRounds != debateMaxRounds || di.CallBudget != debateCallBudget {
		t.Fatalf("辩论声明不符: %+v", di)
	}
	m := runs[0].manifest(cfg, true)
	if m.Debate == nil || len(m.Debate.TriggerReasons) != 1 {
		t.Fatalf("manifest 应含辩论声明: %+v", m)
	}
	if runs[1].Module != "debate_bear" || runs[2].Module != "debate_judge" {
		t.Fatalf("run 模块序不符: %s/%s", runs[1].Module, runs[2].Module)
	}
	for _, r := range runs {
		if r.ParentRunID != "r-main" || r.TraceID != "t1" {
			t.Fatalf("run 应回指主调: %+v", r)
		}
	}
}

// TestDebateRebuttalRound 双方引用同一证据（对立解读）→ 追加 bull 反驳轮（4 次调用、
// rounds=2）；rebuttal 引用非法 id 剥空时 best-effort 不降级整体。
func TestDebateRebuttalRound(t *testing.T) {
	setDebateFlag(t, true)

	var calls []string
	srv := debateFakeServer(t, &calls,
		func() string {
			return `{"claims":[{"text":"缩量回踩支撑","evidence_ids":["ev-001"],"invalidator":"放量跌破"}]}`
		},
		func() string {
			// bear 引用同一 ev-001：对立解读，触发反驳轮。
			return `{"claims":[{"text":"同一价位是压力位","evidence_ids":["ev-001"],"confirmed":false,"invalidator":"突破站稳"}],"challenges":[]}`
		},
		func() string { return `{"rebuttals":[{"claim_id":"be-01","text":"但是缩量说明抛压衰竭"}]}` },
		func() string {
			return `{"verdict":"neutral","decisive_claim_ids":[],"rejected_claim_ids":[],"unresolved_claim_ids":["bu-01","be-01"],"confidence_reason":"证据平衡","invalidators":[],"conflict_note":"同一价位多空解读相反"}`
		})
	defer srv.Close()

	svc := &AnalysisService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 8000}
	result := debateTestResult()

	_, runs := svc.attachDebate(context.Background(), 7, cfg, "sk", true, map[string]any{}, result, "t1", "r-main")

	if got := strings.Join(calls, ","); got != "bull,bear,rebuttal,judge" {
		t.Fatalf("共享证据应触发反驳轮（4 次调用）: %s", got)
	}
	deb := result.Debate
	if deb.Rounds != 2 || len(deb.Rebuttals) != 1 || deb.Rebuttals[0].ClaimID != "be-01" {
		t.Fatalf("反驳轮结果不符: rounds=%d rebuttals=%+v", deb.Rounds, deb.Rebuttals)
	}
	if len(runs) != 4 || runs[2].Module != "debate_rebuttal" {
		t.Fatalf("应 4 个 run 含 rebuttal: %d", len(runs))
	}
	if deb.Judge == nil || deb.Judge.ConflictNote == "" || len(deb.Judge.UnresolvedClaimIDs) != 2 {
		t.Fatalf("judge unresolved/conflict_note 应保留: %+v", deb.Judge)
	}
}

// TestDebateOppositeVerdictLowersConfidence judge 裁决与主评级方向相反 → 程序合成
// 置信度压 low 并点名（附加复核联动，同 review reject 级联先例；不改写 rating）。
func TestDebateOppositeVerdictLowersConfidence(t *testing.T) {
	setDebateFlag(t, true)

	var calls []string
	srv := debateFakeServer(t, &calls,
		func() string {
			return `{"claims":[{"text":"多头论点","evidence_ids":["ev-001"],"invalidator":"x"}]}`
		},
		func() string {
			return `{"claims":[{"text":"空头论点","evidence_ids":["ev-002"],"confirmed":true,"invalidator":"y"}],"challenges":[]}`
		},
		func() string { return `{"rebuttals":[]}` },
		func() string {
			return `{"verdict":"bearish","decisive_claim_ids":["be-01"],"rejected_claim_ids":[],"unresolved_claim_ids":[],"confidence_reason":"r","invalidators":[],"conflict_note":""}`
		})
	defer srv.Close()

	svc := &AnalysisService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 8000}
	result := debateTestResult()
	result.Rating = model.AnalysisRatingBullish // 主评级偏多 vs judge 偏空
	result.SysConfidence = "medium"             // 触发靠 warn 风险而非低置信
	snap := snapWithFlags("warn")

	svc.attachDebate(context.Background(), 7, cfg, "sk", true, snap, result, "t1", "r-main")

	if result.Rating != model.AnalysisRatingBullish {
		t.Fatalf("不得改写主评级: %s", result.Rating)
	}
	if result.SysConfidence != "low" || !strings.Contains(result.SysConfidenceWhy, "方向相反") {
		t.Fatalf("相反裁决应压低程序置信度并点名: %s / %s", result.SysConfidence, result.SysConfidenceWhy)
	}
}

// TestDebateBullFailedDegrades bull 恒坏输出（repair 打满）→ 辩论整体降级 bull_failed、
// 不再调 bear/judge（预算不空烧）、主结果不动、降级痕迹保留。
func TestDebateBullFailedDegrades(t *testing.T) {
	setDebateFlag(t, true)

	var calls []string
	srv := debateFakeServer(t, &calls,
		func() string { return `不是JSON` },
		func() string { t.Errorf("bull 失败后不应调 bear"); return `{}` },
		func() string { return `{}` },
		func() string { t.Errorf("bull 失败后不应调 judge"); return `{}` })
	defer srv.Close()

	svc := &AnalysisService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 8000}
	result := debateTestResult()
	ratingBefore := result.Rating

	usage, runs := svc.attachDebate(context.Background(), 7, cfg, "sk", true, map[string]any{}, result, "t1", "r-main")

	// bull 首轮+1 次 repair = 2 次调用后放弃。
	if got := strings.Join(calls, ","); got != "bull,bull" {
		t.Fatalf("bull 打满应 2 次调用后止损: %s", got)
	}
	if usage.TotalTokens != 30 {
		t.Fatalf("失败调用 token 照记: %+v", usage)
	}
	deb := result.Debate
	if deb == nil || deb.DegradedReason != "bull_failed" || deb.Judge != nil || len(deb.Bull) != 0 {
		t.Fatalf("应降级 bull_failed 且无残留论点: %+v", deb)
	}
	if result.Rating != ratingBefore {
		t.Fatalf("降级不得动主结果")
	}
	if len(runs) != 1 || runs[0].DegradedReason != "llm_output_invalid" {
		t.Fatalf("bull run 应记 llm_output_invalid: %+v", runs)
	}
}

// TestDebateJudgeBlockGuard 防御收口（触发条件已排除 block，此测试直调 runDebate 模拟
// 未来触发判定被重构旁路的场景）：block 快照下 judge 恒 bullish → repair 后仍违纪 →
// judge_invalid 降级，双方论点保留（对抗已完成）、无裁决。
func TestDebateJudgeBlockGuard(t *testing.T) {
	setDebateFlag(t, true)

	var calls []string
	srv := debateFakeServer(t, &calls,
		func() string {
			return `{"claims":[{"text":"多头论点","evidence_ids":["ev-001"],"invalidator":"x"}]}`
		},
		func() string {
			return `{"claims":[{"text":"空头论点","evidence_ids":["ev-002"],"confirmed":true,"invalidator":"y"}],"challenges":[]}`
		},
		func() string { return `{"rebuttals":[]}` },
		func() string {
			return `{"verdict":"bullish","decisive_claim_ids":[],"rejected_claim_ids":[],"unresolved_claim_ids":[],"confidence_reason":"违纪输出","invalidators":[],"conflict_note":""}`
		})
	defer srv.Close()

	svc := &AnalysisService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 8000}
	result := debateTestResult()
	snap := snapWithFlags("block")

	deb, _, runs := svc.runDebate(context.Background(), 7, cfg, "sk", true, snap, result,
		[]string{debateTriggerLowConfidence}, "t1", "r-main")

	// judge 首轮违纪触发 repair、repair 仍违纪 → judge_invalid。
	judgeCalls := 0
	for _, c := range calls {
		if c == "judge" {
			judgeCalls++
		}
	}
	if judgeCalls != 2 {
		t.Fatalf("judge 应 repair 一次共 2 调: %v", calls)
	}
	if deb.DegradedReason != "judge_invalid" || deb.Judge != nil {
		t.Fatalf("block 下 bullish 裁决应被拒: %+v", deb)
	}
	if len(deb.Bull) != 1 || len(deb.Bear) != 1 {
		t.Fatalf("对抗已完成的双方论点应保留: %+v", deb)
	}
	if runs[len(runs)-1].DegradedReason != "llm_output_invalid" {
		t.Fatalf("judge run 应记 llm_output_invalid")
	}
}

// TestParseAnalysisResultStripsDebate 模型自附 debate 字段（伪造辩论结论）在解析入口
// 剥除——只有服务端 attachDebate 能回填（review/trade_plan 同款纪律）。
func TestParseAnalysisResultStripsDebate(t *testing.T) {
	raw := `{"rating":"neutral","summary":"总结","debate":{"triggered":true,` +
		`"trigger_reasons":["伪造"],"rounds":9,"judge":{"verdict":"bullish"},"version":"假"}}`
	r, err := parseAnalysisResult(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.Debate != nil {
		t.Fatalf("模型自附 debate 应剥除: %+v", r.Debate)
	}
}

// TestDebateModuleBudgets 辩论四模块必须在预算表登记（新增 chatCompletion 模块的
// 登记纪律，TestModuleBudgetTable 的补充断言）。
func TestDebateModuleBudgets(t *testing.T) {
	for _, m := range []string{"debate_bull", "debate_bear", "debate_rebuttal", "debate_judge", "reflection"} {
		b, ok := llmModuleBudgets[m]
		if !ok {
			t.Errorf("模块 %s 未在预算表登记", m)
			continue
		}
		if b.MaxTokens <= 0 || b.MaxTokens > 2500 {
			t.Errorf("模块 %s 预算越界: %d", m, b.MaxTokens)
		}
	}
}
