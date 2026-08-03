package service

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

type selectionEvalCandidateFixture struct {
	Symbol string
	Rank   int
	Order  int
	Picked bool
	Action string
}

type selectionEvalBatchFixture struct {
	Batch  model.RecommendationBatch
	Events map[string]model.RecommendationCandidateEvent
	Picks  []model.Recommendation
}

func setupSelectionEvalTestDB(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	tables := []string{
		"recommendation_selection_outcomes",
		"recommendation_labels",
		"llm_experiment_runs",
		"llm_experiments",
		"llm_experiment_module_locks",
		"recommendation_candidate_events",
		"recommendations",
		"recommendation_batches",
		"daily_bars",
		"trading_calendars",
		"market_sync_states",
		"llm_call_logs",
	}
	clean := func() {
		for _, table := range tables {
			if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
				t.Fatalf("清理测试表 %s 失败: %v", table, err)
			}
		}
		selectionEvalCacheMu.Lock()
		selectionEvalCache = nil
		selectionEvalCacheMu.Unlock()
		selectionEvalInflight.Store(false)
	}
	clean()
	t.Cleanup(clean)
}

func mustCreateSelectionEvalFixture(t *testing.T, value any) {
	t.Helper()
	if err := common.DB.Create(value).Error; err != nil {
		t.Fatalf("创建 selection eval fixture 失败: %v", err)
	}
}

func seedSelectionEvalBatch(t *testing.T, userID int64, recType, status string, facts bool,
	created time.Time, candidates []selectionEvalCandidateFixture) selectionEvalBatchFixture {
	t.Helper()
	batch := model.RecommendationBatch{
		UserID: userID, Type: recType, Market: "cn", Strategy: "momentum",
		Status: status, FactsRecorded: facts, Provider: "test", Model: "test-model",
		PromptVersion: "test-prompt", CreatedAt: created,
	}
	mustCreateSelectionEvalFixture(t, &batch)
	fixture := selectionEvalBatchFixture{
		Batch: batch, Events: make(map[string]model.RecommendationCandidateEvent, len(candidates)),
	}
	pickOrder := 0
	for _, candidate := range candidates {
		action := candidate.Action
		if action == "" {
			action = model.RecActionBuy
		}
		stage := model.CandStageLLMList
		if candidate.Picked {
			stage = model.CandStagePicked
		}
		event := model.RecommendationCandidateEvent{
			BatchID: batch.ID, UserID: userID, Symbol: candidate.Symbol, Market: "cn",
			Name: candidate.Symbol, CandidateStage: stage, RawScore: float64(100 - candidate.Rank),
			ScoreRank: candidate.Rank, LLMInputOrder: candidate.Order,
			RankingVersion: candidateRankingVersion, SentToLLM: true,
		}
		if candidate.Picked {
			event.RawAction, event.PostGateAction = action, action
		}
		mustCreateSelectionEvalFixture(t, &event)
		fixture.Events[candidate.Symbol] = event
		if candidate.Picked {
			pick := model.Recommendation{
				BatchID: batch.ID, UserID: userID, Symbol: candidate.Symbol, Market: "cn",
				Name: candidate.Symbol, Action: action, Confidence: 70, SortOrder: pickOrder,
			}
			mustCreateSelectionEvalFixture(t, &pick)
			fixture.Picks = append(fixture.Picks, pick)
			pickOrder++
		}
	}
	return fixture
}

func seedSelectionEvalOutcome(t *testing.T, fixture selectionEvalBatchFixture, symbol string,
	horizon int, netPct float64, forced bool) model.RecommendationSelectionOutcome {
	t.Helper()
	event, ok := fixture.Events[symbol]
	if !ok {
		t.Fatalf("fixture batch=%d 缺少标的 %s", fixture.Batch.ID, symbol)
	}
	gross := round2(netPct + 0.2)
	row := model.RecommendationSelectionOutcome{
		BatchID: fixture.Batch.ID, CandidateEventID: event.ID, UserID: fixture.Batch.UserID,
		Symbol: symbol, Market: "cn", Name: symbol, Type: fixture.Batch.Type,
		HorizonDays: horizon, OutcomeVersion: model.SelectionOutcomeVersion,
		RankingVersion: candidateRankingVersion, SchemaVersion: model.SelectionOutcomeSchemaVersion,
		EntryMode: model.EntryModeNextOpen, SignalDate: batchSignalDate(fixture.Batch),
		SignalAsOf: fixture.Batch.CreatedAt, EntryDate: "2025-01-03", EntryPrice: 10,
		ExitDate: "2025-01-10", ExitPrice: round2(10 * (1 + gross/100)),
		GrossReturnPct: gross, NetReturnPct: netPct,
		BenchReturnPct: 1, AlphaPct: round2(netPct - 1), HasBench: true,
		MfePct: round2(math.Max(2, netPct+2)), MaePct: -2,
		MaturityStatus: model.LabelMatured, Forced: forced,
	}
	if forced {
		row.ForcedReason = selectionForcedExecution
	}
	mustCreateSelectionEvalFixture(t, &row)
	return row
}

func seedSelectionEvalPlanLabel(t *testing.T, fixture selectionEvalBatchFixture,
	pick model.Recommendation, horizon int, netPct float64) {
	t.Helper()
	label := model.RecommendationLabel{
		RecommendationID: pick.ID, HorizonDays: horizon, EntryMode: model.EntryModeNextOpen,
		BatchID: fixture.Batch.ID, UserID: fixture.Batch.UserID, Symbol: pick.Symbol,
		Market: "cn", Type: fixture.Batch.Type, Action: pick.Action,
		SignalDate: batchSignalDate(fixture.Batch), SignalAsOf: fixture.Batch.CreatedAt,
		EntryDate: "2025-01-03", EntryPrice: 10, ExitDate: "2025-01-08", ExitPrice: 10.8,
		GrossReturnPct: round2(netPct + 0.2), NetReturnPct: netPct,
		BenchReturnPct: 1, AlphaPct: round2(netPct - 1), HasBench: true,
		MfePct: 9, MaePct: -2, MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
	}
	mustCreateSelectionEvalFixture(t, &label)
}

func selectionEvalSectionFor(t *testing.T, report *SelectionEvalReport,
	recType string, horizon int) SelectionEvalSection {
	t.Helper()
	for _, section := range report.Sections {
		if section.RecType == recType && section.HorizonDays == horizon {
			return section
		}
	}
	t.Fatalf("缺少 section %s/%d", recType, horizon)
	return SelectionEvalSection{}
}

func selectionEvalPairFor(t *testing.T, pairs []SelectionPairedRow, key string) SelectionPairedRow {
	t.Helper()
	for _, pair := range pairs {
		if pair.Pair == key {
			return pair
		}
	}
	t.Fatalf("缺少 paired row %s", key)
	return SelectionPairedRow{}
}

func selectionEvalMetricFor(t *testing.T, metrics []SelectionMetric, group string) SelectionMetric {
	t.Helper()
	for _, metric := range metrics {
		if metric.Group == group {
			return metric
		}
	}
	t.Fatalf("缺少 metric group=%s", group)
	return SelectionMetric{}
}

func TestSelectionEvalSyntheticPairingCoverageAndBootstrap(t *testing.T) {
	setupSelectionEvalTestDB(t)
	ctx := context.Background()
	base := time.Date(2025, 1, 2, 15, 30, 0, 0, time.Local)

	aiWin := seedSelectionEvalBatch(t, 101, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		base, []selectionEvalCandidateFixture{
			{Symbol: "AW_QUANT", Rank: 1, Order: 1},
			{Symbol: "AW_AI", Rank: 2, Order: 2, Picked: true},
		})
	seedSelectionEvalOutcome(t, aiWin, "AW_QUANT", 5, 1, false)
	seedSelectionEvalOutcome(t, aiWin, "AW_AI", 5, 5, false)
	seedSelectionEvalPlanLabel(t, aiWin, aiWin.Picks[0], 5, 6)

	quantWin := seedSelectionEvalBatch(t, 102, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		base.Add(time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "QW_QUANT", Rank: 1, Order: 1},
			{Symbol: "QW_AI", Rank: 2, Order: 2, Picked: true},
		})
	seedSelectionEvalOutcome(t, quantWin, "QW_QUANT", 5, 5, false)
	seedSelectionEvalOutcome(t, quantWin, "QW_AI", 5, 1, false)
	seedSelectionEvalPlanLabel(t, quantWin, quantWin.Picks[0], 5, 2)

	tie := seedSelectionEvalBatch(t, 103, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		base.Add(2*time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "TIE_QUANT", Rank: 1, Order: 1},
			{Symbol: "TIE_AI", Rank: 2, Order: 2, Picked: true},
		})
	seedSelectionEvalOutcome(t, tie, "TIE_QUANT", 5, 3, false)
	seedSelectionEvalOutcome(t, tie, "TIE_AI", 5, 3, false)
	seedSelectionEvalPlanLabel(t, tie, tie.Picks[0], 5, 4)

	forced := seedSelectionEvalBatch(t, 104, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		base.Add(3*time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "FORCED_QUANT", Rank: 1, Order: 1},
			{Symbol: "FORCED_AI", Rank: 2, Order: 2, Picked: true},
		})
	seedSelectionEvalOutcome(t, forced, "FORCED_QUANT", 5, 2, false)
	seedSelectionEvalOutcome(t, forced, "FORCED_AI", 5, -8, true)

	zero := seedSelectionEvalBatch(t, 105, model.RecTypeShortTerm, model.RecStatusDegraded, true,
		base.Add(4*time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "ZERO_A", Rank: 1, Order: 1},
			{Symbol: "ZERO_B", Rank: 2, Order: 2},
		})

	missingPlan := seedSelectionEvalBatch(t, 106, model.RecTypeLongTerm, model.RecStatusSuccess, true,
		base.Add(5*time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "NO_PLAN", Rank: 1, Order: 1, Picked: true},
		})
	seedSelectionEvalOutcome(t, missingPlan, "NO_PLAN", 20, 4, false)

	factsFalse := seedSelectionEvalBatch(t, 107, model.RecTypeShortTerm, model.RecStatusSuccess, false,
		base.Add(6*time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "NO_FACTS", Rank: 1, Order: 1, Picked: true},
		})
	oldVersion := seedSelectionEvalBatch(t, 108, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		base.Add(7*time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "OLD_VERSION", Rank: 1, Order: 1, Picked: true},
		})
	if err := common.DB.Model(&model.RecommendationCandidateEvent{}).
		Where("id = ?", oldVersion.Events["OLD_VERSION"].ID).Update("ranking_version", "").Error; err != nil {
		t.Fatalf("制造旧 ranking fixture 失败: %v", err)
	}
	oldOrder := seedSelectionEvalBatch(t, 109, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		base.Add(8*time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "OLD_ORDER", Rank: 1, Order: 1, Picked: true},
		})
	if err := common.DB.Model(&model.RecommendationCandidateEvent{}).
		Where("id = ?", oldOrder.Events["OLD_ORDER"].ID).Update("llm_input_order", 0).Error; err != nil {
		t.Fatalf("制造旧 order fixture 失败: %v", err)
	}

	first, err := RunSelectionEval(ctx, nil)
	if err != nil {
		t.Fatalf("首次 selection eval 失败: %v", err)
	}
	if first.Bootstrap.Seed != selectionBootstrapSeed || first.Bootstrap.Iterations != selectionBootstrapIterations {
		t.Fatalf("bootstrap 规格未冻结: %+v", first.Bootstrap)
	}
	if first.Coverage.Batches != 9 || first.Coverage.FactsReadyBatches != 6 ||
		first.Coverage.DegradedExcluded != 1 || first.Coverage.FactsMissingExcluded != 1 ||
		first.Coverage.RankingExcluded != 2 {
		t.Fatalf("全局事实覆盖统计不符: %+v", first.Coverage)
	}
	if first.Coverage.ZeroPickBatches != 1 || first.Coverage.ZeroPickRatePct != 16.67 {
		t.Fatalf("零 picks 应只计拒选覆盖率: %+v", first.Coverage)
	}

	short5 := selectionEvalSectionFor(t, first, model.RecTypeShortTerm, 5)
	if short5.Coverage.CandidateBatches != 4 || short5.Coverage.ComparableBatches != 3 ||
		short5.Coverage.ForcedExcluded != 1 {
		t.Fatalf("短线 5 日 comparable/forced 覆盖不符: %+v", short5.Coverage)
	}
	for _, view := range short5.Batches {
		if view.BatchID == zero.Batch.ID {
			t.Fatalf("零 picks degraded 批次不得进入 selection 收益样本: %+v", view)
		}
	}
	pair := selectionEvalPairFor(t, short5.Pairs, "ai_minus_quant")
	if pair.Batches != 3 || pair.LeftWins != 1 || pair.RightWins != 1 || pair.Ties != 1 {
		t.Fatalf("AI 胜/Quant 胜/平局计数不符: %+v", pair)
	}
	diffByBatch := map[int64]float64{}
	for _, diff := range pair.BatchDiffs {
		diffByBatch[diff.BatchID] = diff.AvgNetDiffPct
	}
	if diffByBatch[aiWin.Batch.ID] != 4 || diffByBatch[quantWin.Batch.ID] != -4 ||
		diffByBatch[tie.Batch.ID] != 0 || pair.AvgNetPct.Estimate != 0 {
		t.Fatalf("逐批配对差不符: diffs=%v ci=%+v", diffByBatch, pair.AvgNetPct)
	}
	aiMetric := selectionEvalMetricFor(t, short5.Groups, "ai")
	quantMetric := selectionEvalMetricFor(t, short5.Groups, "quant")
	if aiMetric.SampleSymbols != 3 || quantMetric.SampleSymbols != 3 {
		t.Fatalf("forced 批次不得进入主指标: ai=%+v quant=%+v", aiMetric, quantMetric)
	}

	long20 := selectionEvalSectionFor(t, first, model.RecTypeLongTerm, 20)
	if long20.Coverage.ComparableBatches != 1 {
		t.Fatalf("缺 plan 标签不得污染 selection 主层: %+v", long20.Coverage)
	}
	if long20.Plan.Coverage.CandidateBatches != 1 || long20.Plan.Coverage.ComparableBatches != 0 ||
		long20.Plan.Coverage.MissingExcluded != 1 || long20.Plan.PlanL2.Evaluated {
		t.Fatalf("缺 l2 plan 标签应只在 plan 面板剔除: %+v", long20.Plan)
	}

	for _, invalidBatchID := range []int64{factsFalse.Batch.ID, oldVersion.Batch.ID, oldOrder.Batch.ID} {
		var count int64
		if err := common.DB.Model(&model.RecommendationSelectionOutcome{}).
			Where("batch_id = ?", invalidBatchID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("事实不完整批次 %d 不得生成 so1 outcome，实得 %d 行", invalidBatchID, count)
		}
	}

	second, err := RunSelectionEval(ctx, nil)
	if err != nil {
		t.Fatalf("第二次 selection eval 失败: %v", err)
	}
	secondPair := selectionEvalPairFor(t,
		selectionEvalSectionFor(t, second, model.RecTypeShortTerm, 5).Pairs, "ai_minus_quant")
	if !reflect.DeepEqual(pair, secondPair) {
		t.Fatalf("固定 seed 重跑结果漂移:\nfirst=%+v\nsecond=%+v", pair, secondPair)
	}
}

func TestSelectionEvalChallengerNativeAndMatchedK(t *testing.T) {
	setupSelectionEvalTestDB(t)
	created := time.Date(2025, 2, 3, 15, 30, 0, 0, time.Local)
	fixture := seedSelectionEvalBatch(t, 201, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		created, []selectionEvalCandidateFixture{
			{Symbol: "CHAL_QUANT", Rank: 1, Order: 1},
			{Symbol: "CHAL_FIRST", Rank: 2, Order: 2},
			{Symbol: "CHAL_AI", Rank: 3, Order: 3, Picked: true},
		})
	seedSelectionEvalOutcome(t, fixture, "CHAL_QUANT", 5, 1, false)
	seedSelectionEvalOutcome(t, fixture, "CHAL_FIRST", 5, 4, false)
	seedSelectionEvalOutcome(t, fixture, "CHAL_AI", 5, 2, false)

	experiment := model.LLMExperiment{UserID: fixture.Batch.UserID, Module: "recommendation", Name: "K 不同实验"}
	mustCreateSelectionEvalFixture(t, &experiment)
	champion := []recPick{{Symbol: "CHAL_AI", Action: model.RecActionBuy, Confidence: 70}}
	challenger := []recPick{
		{Symbol: "CHAL_FIRST", Action: model.RecActionWatch, Confidence: 60},
		{Symbol: "CHAL_QUANT", Action: model.RecActionBuy, Confidence: 80},
	}
	run := model.LLMExperimentRun{
		ExperimentID: experiment.ID, UserID: fixture.Batch.UserID, BatchID: fixture.Batch.ID,
		Valid: true, PicksCount: len(challenger), ChampionPicks: len(champion),
		PickSchemaVersion:   llmExperimentPickSchemaVersion,
		ChampionPicksJSON:   marshalLLMExperimentPicks(champion),
		ChallengerPicksJSON: marshalLLMExperimentPicks(challenger),
	}
	mustCreateSelectionEvalFixture(t, &run)
	zeroAI := seedSelectionEvalBatch(t, fixture.Batch.UserID, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		created.Add(time.Minute), []selectionEvalCandidateFixture{
			{Symbol: "CHAL_ZERO_AI", Rank: 1, Order: 1},
		})
	seedSelectionEvalOutcome(t, zeroAI, "CHAL_ZERO_AI", 5, 5, false)
	emptyChampion := []recPick{}
	zeroRun := model.LLMExperimentRun{
		ExperimentID: experiment.ID, UserID: zeroAI.Batch.UserID, BatchID: zeroAI.Batch.ID,
		Valid: true, PicksCount: 1, ChampionPicks: 0,
		PickSchemaVersion:   llmExperimentPickSchemaVersion,
		ChampionPicksJSON:   marshalLLMExperimentPicks(emptyChampion),
		ChallengerPicksJSON: marshalLLMExperimentPicks([]recPick{{Symbol: "CHAL_ZERO_AI", Action: model.RecActionBuy, Confidence: 75}}),
	}
	mustCreateSelectionEvalFixture(t, &zeroRun)

	report, err := RunSelectionEval(context.Background(), nil)
	if err != nil {
		t.Fatalf("challenger selection eval 失败: %v", err)
	}
	if report.Coverage.ChallengerRuns != 2 || report.Coverage.ChallengerValidRuns != 2 ||
		report.Coverage.ChallengerInvalidRuns != 0 {
		t.Fatalf("ep1 challenger 有效性统计不符: %+v", report.Coverage)
	}
	section := selectionEvalSectionFor(t, report, model.RecTypeShortTerm, 5)
	if len(section.Challengers) != 1 {
		t.Fatalf("应有一个 challenger 实验结果: %+v", section.Challengers)
	}
	eval := section.Challengers[0]
	if eval.ExperimentID != experiment.ID || eval.Coverage.Runs != 2 ||
		eval.Coverage.NativeKMin != 1 || eval.Coverage.NativeKMax != 2 ||
		eval.Coverage.NativeKAvg != 1.5 || eval.Coverage.NativeEligible != 2 ||
		eval.Coverage.MatchedEligible != 1 || eval.Coverage.ZeroMatched != 1 {
		t.Fatalf("challenger 原生 K/matched-K 覆盖不符: %+v", eval.Coverage)
	}
	native := selectionEvalMetricFor(t, eval.Groups, "challenger_native")
	matched := selectionEvalMetricFor(t, eval.Groups, "challenger_matched")
	aiMatched := selectionEvalMetricFor(t, eval.Groups, "ai_matched")
	quantMatched := selectionEvalMetricFor(t, eval.Groups, "quant_matched")
	if native.SelectedSymbols != 3 || native.SampleSymbols != 3 ||
		matched.SelectedSymbols != 1 || matched.SampleSymbols != 1 ||
		aiMatched.SampleSymbols != 1 || quantMatched.SampleSymbols != 1 {
		t.Fatalf("原生 K 与 matched-K 样本量不符: native=%+v matched=%+v ai=%+v quant=%+v",
			native, matched, aiMatched, quantMatched)
	}
	chalAI := selectionEvalPairFor(t, eval.Pairs, "challenger_minus_ai")
	chalQuant := selectionEvalPairFor(t, eval.Pairs, "challenger_minus_quant")
	if len(chalAI.BatchDiffs) != 1 || chalAI.BatchDiffs[0].AvgNetDiffPct != 2 ||
		len(chalQuant.BatchDiffs) != 1 || chalQuant.BatchDiffs[0].AvgNetDiffPct != 3 {
		t.Fatalf("challenger matched-K 配对差不符: ai=%+v quant=%+v", chalAI, chalQuant)
	}
	if !reflect.DeepEqual(chalAI.BatchDiffs[0].LeftSymbols, []string{"CHAL_FIRST"}) ||
		!reflect.DeepEqual(chalAI.BatchDiffs[0].RightSymbols, []string{"CHAL_AI"}) ||
		!reflect.DeepEqual(chalQuant.BatchDiffs[0].RightSymbols, []string{"CHAL_QUANT"}) {
		t.Fatalf("matched-K 截取顺序不符: ai=%+v quant=%+v", chalAI.BatchDiffs[0], chalQuant.BatchDiffs[0])
	}
}

func TestRunSelectionEvalSO1SettlementIdempotentAndNoLLM(t *testing.T) {
	setupSelectionEvalTestDB(t)
	created := time.Date(2025, 1, 2, 15, 30, 0, 0, time.Local)
	fixture := seedSelectionEvalBatch(t, 301, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		created, []selectionEvalCandidateFixture{
			{Symbol: "600901", Rank: 1, Order: 1, Picked: true},
		})

	dates := []string{"2025-01-02", "2025-01-03", "2025-01-06", "2025-01-07", "2025-01-08", "2025-01-09", "2025-01-10"}
	for _, date := range dates {
		mustCreateSelectionEvalFixture(t, &model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true})
	}
	bars := []model.DailyBar{
		{Symbol: "600901", Market: "cn", TradeDate: "2025-01-02", Open: 9.9, High: 10.1, Low: 9.8, Close: 10, Source: "test"},
		{Symbol: "600901", Market: "cn", TradeDate: "2025-01-03", Open: 10, High: 10.5, Low: 9.8, Close: 10.2, Source: "test"},
		{Symbol: "600901", Market: "cn", TradeDate: "2025-01-06", Open: 10.2, High: 10.8, Low: 10, Close: 10.5, Source: "test"},
		{Symbol: "600901", Market: "cn", TradeDate: "2025-01-07", Open: 10.5, High: 11, Low: 10.3, Close: 10.8, Source: "test"},
		{Symbol: "600901", Market: "cn", TradeDate: "2025-01-08", Open: 10.8, High: 11.2, Low: 10.6, Close: 11, Source: "test"},
		{Symbol: "600901", Market: "cn", TradeDate: "2025-01-09", Open: 11, High: 11.4, Low: 10.9, Close: 11.2, Source: "test"},
		{Symbol: "600901", Market: "cn", TradeDate: "2025-01-10", Open: 11.2, High: 11.6, Low: 11.1, Close: 11.5, Source: "test"},
	}
	for i := range bars {
		mustCreateSelectionEvalFixture(t, &bars[i])
	}
	mustCreateSelectionEvalFixture(t, &model.LLMCallLog{
		UserID: fixture.Batch.UserID, Module: "selection-test-sentinel", Status: "success",
	})
	var llmBefore int64
	if err := common.DB.Model(&model.LLMCallLog{}).Count(&llmBefore).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := RunSelectionEval(context.Background(), nil); err != nil {
		t.Fatalf("首次真实 so1 结算失败: %v", err)
	}
	var countFirst int64
	if err := common.DB.Model(&model.RecommendationSelectionOutcome{}).
		Where("batch_id = ? AND outcome_version = ?", fixture.Batch.ID, model.SelectionOutcomeVersion).
		Count(&countFirst).Error; err != nil {
		t.Fatal(err)
	}
	if countFirst != int64(len(model.SelectionOutcomeHorizons)) {
		t.Fatalf("首次应每个固定 horizon 恰一行，得到 %d", countFirst)
	}
	var first model.RecommendationSelectionOutcome
	if err := common.DB.Where("batch_id = ? AND symbol = ? AND horizon_days = ? AND outcome_version = ?",
		fixture.Batch.ID, "600901", 5, model.SelectionOutcomeVersion).First(&first).Error; err != nil {
		t.Fatalf("读取 h5 so1 失败: %v", err)
	}
	if first.MaturityStatus != model.LabelMatured || first.Forced ||
		first.EntryDate != "2025-01-03" || first.EntryPrice != 10 ||
		first.ExitDate != "2025-01-10" || first.ExitPrice != 11.5 {
		t.Fatalf("真实 so1 入出场语义不符: %+v", first)
	}
	if first.GrossReturnPct != 15 || first.NetReturnPct != 14.89 ||
		first.MfePct != 16 || first.MaePct != -2 {
		t.Fatalf("真实 so1 gross/net/MFE/MAE 不符: %+v", first)
	}
	if first.EntryMode != model.EntryModeNextOpen || first.OutcomeVersion != model.SelectionOutcomeVersion ||
		first.SchemaVersion != model.SelectionOutcomeSchemaVersion || first.RankingVersion != candidateRankingVersion {
		t.Fatalf("so1 版本或入场口径不符: %+v", first)
	}

	if _, err := RunSelectionEval(context.Background(), nil); err != nil {
		t.Fatalf("第二次真实 so1 结算失败: %v", err)
	}
	var countSecond int64
	if err := common.DB.Model(&model.RecommendationSelectionOutcome{}).
		Where("batch_id = ? AND outcome_version = ?", fixture.Batch.ID, model.SelectionOutcomeVersion).
		Count(&countSecond).Error; err != nil {
		t.Fatal(err)
	}
	if countSecond != countFirst {
		t.Fatalf("重复重算不得增加复合唯一 outcome：first=%d second=%d", countFirst, countSecond)
	}
	var second model.RecommendationSelectionOutcome
	if err := common.DB.First(&second, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !selectionOutcomeSame(first, second) {
		t.Fatalf("重复重算后 outcome 业务字段漂移:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if !first.UpdatedAt.Equal(second.UpdatedAt) {
		t.Fatalf("终态 outcome 重跑不应产生无效更新: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}
	var llmAfter int64
	if err := common.DB.Model(&model.LLMCallLog{}).Count(&llmAfter).Error; err != nil {
		t.Fatal(err)
	}
	if llmAfter != llmBefore {
		t.Fatalf("selection eval 不得产生 LLM 调用日志：before=%d after=%d", llmBefore, llmAfter)
	}
}
