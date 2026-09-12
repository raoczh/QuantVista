package service

import (
	"encoding/json"
	"testing"

	"quantvista/model"
)

func reviewScoreBlindProtocolStatus(t *testing.T, mode string, threshold float64) *SelectionScoreBlindProtocolStatus {
	t.Helper()
	protocol := &ScoreBlindEvaluationProtocol{MinEffectiveBatches: 1, MaxCoverageDropPct: 100, MaxSevereLossRatePct: 100}
	if mode == "coverage" {
		protocol.MaxCoverageDropPct = threshold
	} else {
		protocol.MaxSevereLossRatePct = threshold
	}
	batches := map[int64]selectionBatchFacts{}
	outcomes := map[string]model.RecommendationSelectionOutcome{}
	var runs []selectionChallengerRun
	count := 3
	if mode == "missing" {
		count = 1
	}
	for i := 1; i <= count; i++ {
		id := int64(i)
		batches[id] = selectionBatchFacts{Batch: model.RecommendationBatch{ID: id, Type: model.RecTypeShortTerm, Status: model.RecStatusSuccess},
			Picks:       []model.Recommendation{{Symbol: "600000", Action: model.RecActionBuy}},
			Opportunity: []model.RecommendationCandidateEvent{{Symbol: "600000", ScoreRank: 1}}}
		outcome := model.RecommendationSelectionOutcome{BatchID: id, Symbol: "600000", HorizonDays: 5, MaturityStatus: model.LabelMatured}
		if mode == "severe" && i == 1 {
			outcome.NetReturnPct = -10
		}
		outcomes[selectionOutcomeKey(id, "600000", 5)] = outcome
		run := selectionChallengerRun{Run: model.LLMExperimentRun{ExperimentID: 1, BatchID: id, RunStatus: model.LLMExperimentRunSuccess},
			ExperimentType: model.LLMExperimentTypeScoreBlind, Protocol: protocol, ProtocolAttempt: true,
			Challenger: []selectionChallengerFact{{Symbol: "600000", Order: 1, Action: model.RecActionBuy}}}
		if mode == "coverage" && i == count {
			run.Issue, run.Run.RunStatus = "call_failed", model.LLMExperimentRunFailed
		}
		if mode == "missing" {
			run.Challenger = append(run.Challenger, selectionChallengerFact{Symbol: "000001", Order: 2, Action: model.RecActionBuy})
		}
		runs = append(runs, run)
	}
	views := buildSelectionChallengerEvals(model.RecTypeShortTerm, 5, batches, outcomes, runs)
	if len(views) != 1 || views[0].ProtocolStatus == nil || !views[0].ProtocolStatus.Ready {
		t.Fatalf("应已有可用于 matched-K 的成熟批次：%+v", views)
	}
	return views[0].ProtocolStatus
}

func TestSelectionReviewProtocolUsesUnroundedGuardrails(t *testing.T) {
	for _, mode := range []string{"coverage", "severe"} {
		t.Run(mode, func(t *testing.T) {
			status := reviewScoreBlindProtocolStatus(t, mode, 33.33)
			if status.GuardrailsPassed {
				t.Fatalf("实际 1/3=33.3333%% 超过 33.33%%，不能舍入后放行：%+v", status)
			}
			if status := reviewScoreBlindProtocolStatus(t, mode, 100.0/3); !status.GuardrailsPassed {
				t.Fatalf("数学上相等的阈值仍应通过：%+v", status)
			}
		})
	}
}

func TestSelectionReviewMissingNativeLossesAreUnknown(t *testing.T) {
	status := reviewScoreBlindProtocolStatus(t, "missing", 100)
	if status.GuardrailsPassed {
		t.Error("matched-K 已成熟但原生 K 尚无可比亏损观测，不得宣布全部护栏通过")
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value["severe_loss_rate_pct"] != nil {
		t.Fatalf("未知严重亏损率不能返回 0%%：%s", raw)
	}
}
