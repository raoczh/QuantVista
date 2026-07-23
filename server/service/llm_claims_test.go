package service

import (
	"strings"
	"testing"

	"quantvista/model"
)

// P1-2 Claim/evidence/invalidator schema 的单测与反例（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md
// §5.2/§7.2 P1-2）。核心不变式：claim 与 evidence_id 的关联程序推导（非模型自报）、
// status 三态判定（contradictory > resolved > unresolved）、invalidators 归一收集。

// TestDeriveClaimsStatus 三态判定 known-answer：同一核验结果按段推导——
// 「总结」段两数字快照命中 → resolved 且 evidence_ids 精确对应；
// 「风险」段方向冲突 → contradictory；「观点」段无可核验数字 → unresolved。
func TestDeriveClaimsStatus(t *testing.T) {
	vals := []labeledValue{
		{Path: "quote.current_price", Value: 12.34},
		{Path: "quote.change_pct", Value: 2.5},
		{Path: "fund.main_net_yi", Value: -3.2}, // 负值：用于方向冲突构造
	}
	sections := []evidenceSection{
		{Module: "总结", Text: "现价 12.34 元，今日上涨 2.5%，趋势健康"},
		{Module: "风险", Text: "主力净流入 3.2 亿支撑股价"}, // 快照是 -3.2（净流出），方向相反
		{Module: "观点", Text: "基本面长期看好，护城河稳固"},   // 无数字
	}
	check := verifyEvidenceLabeled(sections, vals)
	claims := deriveClaims(check, []claimSpec{
		{Section: "总结", Text: sections[0].Text, Invalidators: []string{"跌破 MA20"}},
		{Section: "风险", Text: sections[1].Text},
		{Section: "观点", Text: sections[2].Text},
	})
	if len(claims) != 3 {
		t.Fatalf("应推导 3 条 claim: %d", len(claims))
	}
	// claim 1：resolved，evidence_ids 为「总结」段命中项的 ID（与 items 明细一一对应）。
	c1 := claims[0]
	if c1.ClaimID != "cl-01" || c1.Status != claimResolved {
		t.Fatalf("总结 claim 应 resolved: %+v", c1)
	}
	if len(c1.EvidenceIDs) != 2 {
		t.Fatalf("总结 claim 应关联 2 个 evidence_id: %v", c1.EvidenceIDs)
	}
	for _, id := range c1.EvidenceIDs {
		found := false
		for _, it := range check.Items {
			if it.EvidenceID == id && it.Module == "总结" && it.Matched && it.Origin == "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("evidence_id %s 无法回指「总结」段命中明细", id)
		}
	}
	if len(c1.Invalidators) != 1 || c1.Invalidators[0] != "跌破 MA20" {
		t.Fatalf("失效条件应透传: %v", c1.Invalidators)
	}
	// claim 2：direction_mismatch → contradictory（最强告警，即便同段还有其他命中）。
	if claims[1].Status != claimContradictory {
		t.Fatalf("方向冲突段应 contradictory: %+v", claims[1])
	}
	// claim 3：无数字 → unresolved（不是错误，如实声明无佐证）。
	if claims[2].Status != claimUnresolved || len(claims[2].EvidenceIDs) != 0 {
		t.Fatalf("无数字段应 unresolved 且无 evidence_ids: %+v", claims[2])
	}
}

// TestDeriveClaimsRestatedNotResolved 反例：段内数字全部命中「模型自身计划价」（origin=plan）
// ——合法复述不算快照佐证，claim 必须 unresolved（与 markKeySection/升档口径一致，
// 防「复述自己的结论冒充被数据证明」）。
func TestDeriveClaimsRestatedNotResolved(t *testing.T) {
	vals := markValueOrigin([]labeledValue{{Path: "交易计划", Value: 15.8}}, "plan")
	check := verifyEvidenceLabeled([]evidenceSection{{Module: "交易计划", Text: "目标价 15.8 元"}}, vals)
	if check.Matched != 1 || check.PlanMatched != 1 {
		t.Fatalf("前提：数字应命中 plan 来源: %+v", check)
	}
	claims := deriveClaims(check, []claimSpec{{Section: "交易计划", Text: "目标价 15.8 元"}})
	if len(claims) != 1 || claims[0].Status != claimUnresolved || len(claims[0].EvidenceIDs) != 0 {
		t.Fatalf("全复述命中应 unresolved（复述不算佐证）: %+v", claims)
	}
}

// TestDeriveClaimsEdges 边界：nil check / 空 specs / 空 text spec 跳过 / claim 文本截断。
func TestDeriveClaimsEdges(t *testing.T) {
	if deriveClaims(nil, []claimSpec{{Section: "总结", Text: "x"}}) != nil {
		t.Fatal("nil check 应返回 nil")
	}
	check := verifyEvidenceLabeled([]evidenceSection{{Module: "总结", Text: "无数字"}}, nil)
	if deriveClaims(check, nil) != nil {
		t.Fatal("空 specs 应返回 nil")
	}
	claims := deriveClaims(check, []claimSpec{
		{Section: "总结", Text: "  "}, // 空文本跳过，不建 claim
		{Section: "总结", Text: strings.Repeat("长", 300)},
	})
	if len(claims) != 1 {
		t.Fatalf("空文本 spec 应跳过: %d", len(claims))
	}
	// 跳过后 claim_id 仍连续（cl-01 起）。
	if claims[0].ClaimID != "cl-01" {
		t.Fatalf("claim_id 应从 cl-01 连续分配: %s", claims[0].ClaimID)
	}
	if got := len([]rune(claims[0].Text)); got > claimTextMax {
		t.Fatalf("claim 文本应截断至 %d rune: %d", claimTextMax, got)
	}
}

// TestNormalizeInvalidators 失效条件归一：去空白、单条截断、条数上限、nil 安全。
func TestNormalizeInvalidators(t *testing.T) {
	if normalizeInvalidators(nil) != nil {
		t.Fatal("nil 入参应返回 nil")
	}
	in := []string{" ", "跌破 MA20", strings.Repeat("超", 200), "a", "b", "c", "d"}
	out := normalizeInvalidators(in)
	if len(out) != claimInvalidatorMax {
		t.Fatalf("应限 %d 条: %d", claimInvalidatorMax, len(out))
	}
	if out[0] != "跌破 MA20" {
		t.Fatalf("空白应剔除: %v", out)
	}
	if got := len([]rune(out[1])); got > claimInvalidatorLen {
		t.Fatalf("单条应截断至 %d rune: %d", claimInvalidatorLen, got)
	}
}

// TestAnalysisClaimSpecs 分析 spec 组装：总结恒在（失效条件=kill_switches）；
// 有计划时追加交易计划 spec（失效条件=计划 invalidators）；no_plan 不建计划 spec。
func TestAnalysisClaimSpecs(t *testing.T) {
	r := &AnalysisResult{Summary: "总结", KillSwitches: []string{"信号A"}}
	specs := analysisClaimSpecs(r)
	if len(specs) != 1 || specs[0].Section != "总结" || specs[0].Invalidators[0] != "信号A" {
		t.Fatalf("无计划应只有总结 spec: %+v", specs)
	}
	r.TradePlan = &tradePlan{NoPlan: true}
	if got := analysisClaimSpecs(r); len(got) != 1 {
		t.Fatalf("no_plan 不建计划 spec: %+v", got)
	}
	r.TradePlan = &tradePlan{PlanNote: "回踩低吸", Invalidators: []string{"放量跌破 MA20"}}
	specs = analysisClaimSpecs(r)
	if len(specs) != 2 || specs[1].Section != "交易计划" || specs[1].Invalidators[0] != "放量跌破 MA20" {
		t.Fatalf("有计划应含交易计划 spec: %+v", specs)
	}
}

// TestRecPickClaimSpec 推荐 spec：长线 thesis 优先、短线首条理由；invalidation 进失效
// 条件；理由与逻辑全空时兜底动作陈述（pick 恒有 claim）。
func TestRecPickClaimSpec(t *testing.T) {
	long := recPick{Symbol: "600519", Action: model.RecActionBuy,
		Thesis: "估值低位+盈利稳健", Reason: []string{"理由1"}, Invalidation: "净利连续两期转负"}
	sp := recPickClaimSpec(long)
	if sp.Section != recClaimSection || sp.Text != "估值低位+盈利稳健" || sp.Invalidators[0] != "净利连续两期转负" {
		t.Fatalf("长线 spec 错误: %+v", sp)
	}
	short := recPick{Symbol: "000001", Action: model.RecActionWatch, Reason: []string{"量价配合"}}
	if sp := recPickClaimSpec(short); sp.Text != "量价配合" || sp.Invalidators != nil {
		t.Fatalf("短线 spec 应取首条理由、无 invalidation 如实为空: %+v", sp)
	}
	bare := recPick{Symbol: "000002", Action: model.RecActionBuy}
	if sp := recPickClaimSpec(bare); !strings.Contains(sp.Text, "000002") || !strings.Contains(sp.Text, "买入") {
		t.Fatalf("空理由应兜底动作陈述: %+v", sp)
	}
}

// TestDailyClaimSpecs 日报 spec：总结+明日计划两段；无失效条件字段如实为空。
func TestDailyClaimSpecs(t *testing.T) {
	rv := &dailyReview{Summary: "今日总结", TomorrowPlan: "明日关注"}
	specs := dailyClaimSpecs(rv)
	if len(specs) != 2 || specs[0].Section != "总结" || specs[1].Section != "明日计划" {
		t.Fatalf("日报 spec 错误: %+v", specs)
	}
	for _, sp := range specs {
		if sp.Invalidators != nil {
			t.Fatalf("日报无失效条件字段，不得硬造: %+v", sp)
		}
	}
}

// TestDailyReviewEvidenceClaims 日报端到端（纯函数层）：复盘文本引用快照数字 →
// claims 挂 evidenceCheck 且总结段 resolved。
func TestDailyReviewEvidenceClaims(t *testing.T) {
	snap := &reportSnapshot{TradeDate: "2026-07-22", Market: &reportMarket{
		Breadth: map[string]any{"advances": 3120, "declines": 1890, "trade_date": "2026-07-22"},
	}}
	rv := &dailyReview{
		Summary:      "今日上涨 3120 家、下跌 1890 家，市场偏强",
		TomorrowPlan: "关注量能持续性",
	}
	check := dailyReviewEvidence(rv, snap)
	if len(check.Claims) != 2 {
		t.Fatalf("日报应推导 2 条 claim: %+v", check.Claims)
	}
	if check.Claims[0].Status != claimResolved || len(check.Claims[0].EvidenceIDs) == 0 {
		t.Fatalf("总结引用真实涨跌家数应 resolved: %+v", check.Claims[0])
	}
	if check.Claims[1].Status != claimUnresolved {
		t.Fatalf("明日计划无数字应 unresolved: %+v", check.Claims[1])
	}
}
