package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"quantvista/model"
)

func TestDebateEvidenceIncludesOmittedRiskAndRejectsCircularSupport(t *testing.T) {
	snapshot := map[string]any{
		"quote": map[string]any{"price": 10.5, "change_pct": -3.2, "source": "test_quote", "data_time": "2026-09-15 10:00"},
		"finance": map[string]any{"version": financeFactorVersion,
			"latest":     map[string]any{"report_date": "2026-06-30", "net_profit": -50000000, "deduct_profit": 0, "net_profit_yoy": 50},
			"comparable": map[string]any{"report_date": "2025-06-30", "net_profit": -100000000},
		},
		"price_plan": map[string]any{"target_price": 999},
		"user":       map[string]any{"target": 888},
	}
	main := &evidenceCheck{Items: []evidenceItem{
		{Matched: true, EvidenceID: "ev-001", Path: "quote.price", SnapValue: 10.5},
		{Matched: true, EvidenceID: "ev-002", Path: "quote.price", SnapValue: 10.5},
		{Matched: true, EvidenceID: "ev-plan", Path: "price_plan.target_price", SnapValue: 999, Origin: "plan"},
		{Matched: true, EvidenceID: "ev-fake", Path: "finance.latest.net_profit", SnapValue: 50000000},
	}}
	refs, allowed := buildDebateEvidenceIndex(snapshot, main)
	paths := map[string]debateEvidenceRef{}
	for _, ref := range refs {
		if _, exists := paths[ref.Path]; exists {
			t.Fatal("重复引用不能挤占证据预算")
		}
		paths[ref.Path] = ref
	}
	if paths["finance.latest.net_profit"].Value != -50000000 || paths["finance.latest.deduct_profit"].EvidenceID == "" {
		t.Fatalf("主分析未提及的亏损和真实零扣非必须仍可引用：%+v", refs)
	}
	if paths["finance.latest.net_profit"].AsOf != "2026-06-30" || paths["finance.latest.net_profit"].Source != "eastmoney_f10" {
		t.Fatal("财报证据应保留来源和报告期")
	}
	if !allowed["ev-001"] || allowed["ev-002"] || allowed["ev-plan"] || allowed["ev-fake"] || paths["user.target"].EvidenceID != "" {
		t.Fatalf("只能复用与快照一致的唯一证据，不能复用模型自证：%+v", allowed)
	}
	withoutMain, _ := buildDebateEvidenceIndex(snapshot, nil)
	if len(withoutMain) != len(refs) {
		t.Fatal("主分析是否引用事实不能改变独立复核的事实集合")
	}
	before, _ := json.Marshal(refs)
	again, _ := buildDebateEvidenceIndex(snapshot, main)
	after, _ := json.Marshal(again)
	if string(before) != string(after) {
		t.Fatal("同一冻结输入的引用编号必须稳定")
	}
}

func TestDebateEvidenceBudgetKeepsCriticalFinancialFacts(t *testing.T) {
	metrics := map[string]any{}
	for i := 0; i < 100; i++ {
		metrics[fmt.Sprintf("indicator_%03d", i)] = i + 1
	}
	snapshot := map[string]any{"technicals": metrics, "finance": map[string]any{
		"version": financeFactorVersion, "latest": map[string]any{"net_profit": -1, "deduct_profit": 0},
	}}
	refs, _ := buildDebateEvidenceIndex(snapshot, nil)
	if len(refs) != debateEvidenceMax || refs[0].Path != "finance.latest.net_profit" || refs[1].Path != "finance.latest.deduct_profit" {
		t.Fatalf("指标很多时仍须保留关键盈利反证：%+v", refs)
	}
}

func TestDebateWithoutSnapshotEvidenceDoesNotCallModel(t *testing.T) {
	result := &AnalysisResult{EvidenceCheck: &evidenceCheck{Items: []evidenceItem{{Matched: true, EvidenceID: "ev-001", Path: "quote.price", SnapValue: 99}}}}
	debate, usage, runs := (&AnalysisService{}).runDebate(context.Background(), 1, &model.LLMConfig{}, "", false,
		map[string]any{"price_plan": map[string]any{"target": 99}}, result, []string{debateTriggerLowConfidence}, "test", "test")
	if debate.DegradedReason != "evidence_unavailable" || usage.TotalTokens != 0 || len(runs) != 0 {
		t.Fatalf("没有独立事实时不应空转调用模型：%+v", debate)
	}
}
