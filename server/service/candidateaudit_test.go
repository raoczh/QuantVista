package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

const (
	auditSignalDate  = "2044-03-04"
	auditOutcomeDate = "2044-03-07"
)

type auditSeedSymbol struct {
	symbol    string
	name      string
	close     float64
	amount    float64
	isST      bool
	suspended bool
}

func cleanCandidateAuditFixture(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	tables := []string{
		"candidate_audit_items", "candidate_audit_runs", "candidate_discovery_items",
		"candidate_discovery_runs", "recommendation_selection_outcomes", "recommendation_labels",
		"recommendations", "recommendation_candidate_events", "recommendation_batches",
		"factor_snapshot_dailies", "stock_universe_dailies", "daily_bars", "trading_calendars",
	}
	for _, table := range tables {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理 %s: %v", table, err)
		}
	}
	t.Cleanup(func() {
		for _, table := range tables {
			common.DB.Exec("DELETE FROM " + table)
		}
	})
}

func seedCandidateAuditFixture(t *testing.T) (shortBatch, longBatch model.RecommendationBatch) {
	t.Helper()
	cleanCandidateAuditFixture(t)
	calendars := []model.TradingCalendar{
		{Market: "cn", TradeDate: auditSignalDate, IsOpen: true},
		{Market: "cn", TradeDate: "2044-03-05", IsOpen: false},
		{Market: "cn", TradeDate: "2044-03-06", IsOpen: false},
		{Market: "cn", TradeDate: auditOutcomeDate, IsOpen: true},
		{Market: "cn", TradeDate: "2044-03-08", IsOpen: true},
	}
	if err := common.DB.Create(&calendars).Error; err != nil {
		t.Fatalf("交易日历: %v", err)
	}

	now := time.Date(2044, 3, 7, 18, 0, 0, 0, time.Local)
	finished := now
	discoveryRuns := []model.CandidateDiscoveryRun{
		{OwnerType: model.JobOwnerSystem, Market: "cn", TradeDate: auditSignalDate, AsOf: now.Add(-72 * time.Hour),
			DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion,
			ParameterHash: discoveryParameterHash(), Status: DiscoveryRunStatusOK, StartedAt: now.Add(-72 * time.Hour), FinishedAt: &finished},
		{OwnerType: model.JobOwnerSystem, Market: "cn", TradeDate: auditOutcomeDate, AsOf: now,
			DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion,
			ParameterHash: discoveryParameterHash(), Status: DiscoveryRunStatusOK, StartedAt: now, FinishedAt: &finished},
	}
	if err := common.DB.Create(&discoveryRuns).Error; err != nil {
		t.Fatalf("发现运行: %v", err)
	}

	symbols := []auditSeedSymbol{
		{symbol: "600101", name: "未发现龙头", close: 10.6, amount: 8e7},
		{symbol: "600102", name: "未进用户池", close: 10.6, amount: 8e7},
		{symbol: "600103", name: "筛选龙头", close: 10.6, amount: 8e7},
		{symbol: "600104", name: "池满龙头", close: 10.6, amount: 8e7},
		{symbol: "600105", name: "评分龙头", close: 10.6, amount: 8e7},
		{symbol: "600106", name: "名单龙头", close: 10.6, amount: 8e7},
		{symbol: "600107", name: "命中龙头", close: 10.6, amount: 8e7},
		{symbol: "600108", name: "次日不利", close: 9.4, amount: 8e7},
		{symbol: "600109", name: "ST排除", close: 10.8, amount: 8e7, isST: true},
		{symbol: "600110", name: "停牌排除", close: 10.8, amount: 8e7, suspended: true},
		{symbol: "600111", name: "低流动性", close: 10.8, amount: 1e7},
		{symbol: "600112", name: "一字涨停", close: 11, amount: 8e7},
	}
	var universes []model.StockUniverseDaily
	var factors []model.FactorSnapshotDaily
	var bars []model.DailyBar
	for _, symbol := range symbols {
		for _, date := range []string{auditSignalDate, auditOutcomeDate} {
			universes = append(universes, model.StockUniverseDaily{TradeDate: date, Symbol: symbol.symbol,
				Market: "cn", Name: symbol.name, IsST: symbol.isST, Suspended: symbol.suspended,
				Close: 10, PrevClose: 10, Amount: symbol.amount, TurnoverRate: 3})
		}
		factors = append(factors, model.FactorSnapshotDaily{TradeDate: auditSignalDate, Symbol: symbol.symbol,
			Market: "cn", Name: symbol.name, LastBarDate: auditSignalDate, FactorsJSON: `{}`,
			FactorVersion: factorSnapshotVersion})
		bars = append(bars, model.DailyBar{Symbol: symbol.symbol, Market: "cn", TradeDate: auditSignalDate,
			Open: 10, High: 10.1, Low: 9.9, Close: 10, Volume: 1e6, Amount: symbol.amount})
		outOpen, outHigh, outLow := 10.0, symbol.close+0.1, symbol.close-0.1
		if symbol.symbol == "600112" {
			outOpen, outHigh, outLow = 11, 11, 11
		}
		bars = append(bars, model.DailyBar{Symbol: symbol.symbol, Market: "cn", TradeDate: auditOutcomeDate,
			Open: outOpen, High: outHigh, Low: outLow, Close: symbol.close, Volume: 1e6, Amount: symbol.amount})
	}
	if err := common.DB.CreateInBatches(universes, 100).Error; err != nil {
		t.Fatalf("宇宙快照: %v", err)
	}
	if err := common.DB.CreateInBatches(factors, 100).Error; err != nil {
		t.Fatalf("因子快照: %v", err)
	}
	if err := common.DB.CreateInBatches(bars, 100).Error; err != nil {
		t.Fatalf("日线: %v", err)
	}

	var discoveryItems []model.CandidateDiscoveryItem
	for i, symbol := range symbols[1:] {
		discoveryItems = append(discoveryItems, model.CandidateDiscoveryItem{RunID: discoveryRuns[0].ID,
			TradeDate: auditSignalDate, Market: "cn", DiscoveryVersion: DiscoveryVersion,
			Channel: "momentum_breakout", Symbol: symbol.symbol, Name: symbol.name, Rank: i + 1,
			Score: float64(100 - i), DataStatus: "ready", AsOf: discoveryRuns[0].AsOf})
	}
	if err := common.DB.Create(&discoveryItems).Error; err != nil {
		t.Fatalf("发现明细: %v", err)
	}

	created := time.Date(2044, 3, 4, 17, 0, 0, 0, time.Local)
	shortBatch = model.RecommendationBatch{UserID: 71, Type: model.RecTypeShortTerm, Market: "cn",
		Status: model.RecStatusSuccess, FactsRecorded: true, Regime: "offense", CreatedAt: created}
	longBatch = model.RecommendationBatch{UserID: 72, Type: model.RecTypeLongTerm, Market: "cn",
		Status: model.RecStatusSuccess, FactsRecorded: true, Regime: "neutral", CreatedAt: created.Add(time.Minute)}
	if err := common.DB.Create(&shortBatch).Error; err != nil {
		t.Fatalf("短线批次: %v", err)
	}
	if err := common.DB.Create(&longBatch).Error; err != nil {
		t.Fatalf("长线批次: %v", err)
	}
	events := []model.RecommendationCandidateEvent{
		{BatchID: shortBatch.ID, UserID: 71, Symbol: "600103", Name: "筛选龙头", Market: "cn",
			CandidateStage: model.CandStageFiltered, RejectionReason: "超出用户股价筛选", Source: "gainer", SourceSet: "gainer", RankingVersion: "rv1"},
		{BatchID: shortBatch.ID, UserID: 71, Symbol: "600104", Name: "池满龙头", Market: "cn",
			CandidateStage: model.CandStagePoolFull, Source: "gainer", SourceSet: "gainer", RankingVersion: "rv1"},
		{BatchID: shortBatch.ID, UserID: 71, Symbol: "600105", Name: "评分龙头", Market: "cn",
			CandidateStage: model.CandStageScored, ScoreRank: 41, Source: "discovery", SourceSet: "discovery", RankingVersion: "rv1"},
		{BatchID: shortBatch.ID, UserID: 71, Symbol: "600106", Name: "名单龙头", Market: "cn",
			CandidateStage: model.CandStageLLMList, ScoreRank: 8, LLMInputOrder: 8, RejectionReason: "量价背离",
			Source: "discovery", SourceSet: "discovery,gainer", RankingVersion: "rv1"},
		{BatchID: shortBatch.ID, UserID: 71, Symbol: "600107", Name: "命中龙头", Market: "cn",
			CandidateStage: model.CandStagePicked, ScoreRank: 2, LLMInputOrder: 2, RawAction: model.RecActionBuy,
			Source: "discovery", SourceSet: "discovery", RankingVersion: "rv1"},
		{BatchID: shortBatch.ID, UserID: 71, Symbol: "600108", Name: "次日不利", Market: "cn",
			CandidateStage: model.CandStagePicked, ScoreRank: 3, LLMInputOrder: 3, RawAction: model.RecActionBuy,
			Source: "watchlist", SourceSet: "watchlist", RankingVersion: "rv1"},
		{BatchID: longBatch.ID, UserID: 72, Symbol: "600108", Name: "次日不利", Market: "cn",
			CandidateStage: model.CandStagePicked, ScoreRank: 3, LLMInputOrder: 3, RawAction: model.RecActionBuy,
			Source: "watchlist", SourceSet: "watchlist", RankingVersion: "rv2"},
	}
	if err := common.DB.Create(&events).Error; err != nil {
		t.Fatalf("候选事件: %v", err)
	}
	return shortBatch, longBatch
}

func TestCandidateAuditAdjacentTradingDaysAndExecutionExclusions(t *testing.T) {
	seedCandidateAuditFixture(t)
	previous, err := candidateAuditAdjacentSignalDate("cn", auditOutcomeDate)
	if err != nil || previous != auditSignalDate {
		t.Fatalf("周末后周一必须相邻到周五: previous=%s err=%v", previous, err)
	}
	if _, err := ExecuteCandidateAudit(context.Background(), nil, 0, candidateAuditJobRequest{
		Market: "cn", SignalDate: auditSignalDate, OutcomeDate: "2044-03-08",
		ParameterHash: candidateAuditParameterHash(),
	}); err == nil || !strings.Contains(err.Error(), "previous=2044-03-07") {
		t.Fatalf("停机日不得跳过周一回退周五: %v", err)
	}

	coverage, gaps := map[string]int{}, map[string]int{}
	observations, err := loadAuditObservations(context.Background(), nil, auditSignalDate,
		auditOutcomeDate, factorSnapshotVersion, coverage, gaps)
	if err != nil {
		t.Fatal(err)
	}
	if observations["600109"].Status != "excluded_st" || observations["600110"].Status != "excluded_suspended" ||
		observations["600111"].Status != "excluded_liquidity" || observations["600112"].Status != btSkipLimitUp {
		t.Fatalf("执行排除不符: ST=%s suspended=%s liquidity=%s limit=%s", observations["600109"].Status,
			observations["600110"].Status, observations["600111"].Status, observations["600112"].Status)
	}
	if observations["600101"].Status != btObserved || coverage["executable"] == 0 {
		t.Fatalf("正常标的应可执行: %+v coverage=%v", observations["600101"], coverage)
	}
}

func TestCandidateAuditEndToEndIdempotentPITIsolationAndConclusions(t *testing.T) {
	shortBatch, longBatch := seedCandidateAuditFixture(t)
	req := candidateAuditJobRequest{Version: 1, Market: "cn", SignalDate: auditSignalDate,
		OutcomeDate: auditOutcomeDate, ParameterHash: candidateAuditParameterHash()}

	var wg sync.WaitGroup
	runs := make([]*model.CandidateAuditRun, 2)
	errs := make([]error, 2)
	for i := range runs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			runs[index], errs[index] = ExecuteCandidateAudit(context.Background(), nil, 0, req)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("并发审计: %v", err)
		}
	}
	if runs[0].ID != runs[1].ID || runs[0].Status != model.CandidateAuditStatusPartial {
		t.Fatalf("并发应复用同一 partial 运行（无基准）: %+v %+v", runs[0], runs[1])
	}
	var runCount int64
	common.DB.Model(&model.CandidateAuditRun{}).Count(&runCount)
	if runCount != 1 {
		t.Fatalf("应只有一条审计运行，得到 %d", runCount)
	}

	var shortItems []model.CandidateAuditItem
	if err := common.DB.Where("run_id = ? AND user_id = ?", runs[0].ID, shortBatch.UserID).Find(&shortItems).Error; err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, item := range shortItems {
		if item.AuditType == model.CandidateAuditTypeMissedLeader {
			reasons[item.Symbol] = item.PrimaryReasonCode
		}
	}
	wantReasons := map[string]string{"600101": "not_discovered_marketwide", "600102": "discovered_not_in_user_pool",
		"600103": "user_price_filter", "600104": "pool_capacity", "600105": "quant_rank_below_cutoff",
		"600106": "llm_rejected_recorded"}
	for symbol, want := range wantReasons {
		if reasons[symbol] != want {
			t.Fatalf("%s 主原因 want=%s got=%s all=%v", symbol, want, reasons[symbol], reasons)
		}
	}
	var shortBad, longBad model.CandidateAuditItem
	if err := common.DB.Where("run_id = ? AND batch_id = ? AND symbol = ? AND audit_type = ?", runs[0].ID,
		shortBatch.ID, "600108", model.CandidateAuditTypeFalsePositive).First(&shortBad).Error; err != nil {
		t.Fatalf("短线误选缺失: %v", err)
	}
	if err := common.DB.Where("run_id = ? AND batch_id = ? AND symbol = ? AND audit_type = ?", runs[0].ID,
		longBatch.ID, "600108", model.CandidateAuditTypeFalsePositive).First(&longBad).Error; err != nil {
		t.Fatalf("长线早期不利缺失: %v", err)
	}
	if shortBad.ConclusionCode != "false_positive_next_day" || longBad.ConclusionCode != "early_adverse_observation" {
		t.Fatalf("short/long 结论不得混用: short=%s long=%s", shortBad.ConclusionCode, longBad.ConclusionCode)
	}

	beforeHash := runs[0].ContentHash
	beforeInput := shortBad.InputRefsJSON
	if err := common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", "600108", auditOutcomeDate).
		Updates(map[string]any{"close": 20, "high": 20}).Error; err != nil {
		t.Fatal(err)
	}
	second, err := ExecuteCandidateAudit(context.Background(), nil, 0, req)
	if err != nil || second.ID != runs[0].ID || second.ContentHash != beforeHash {
		t.Fatalf("修改结果日数据后幂等重试不得改封存事实: run=%+v err=%v", second, err)
	}
	var after model.CandidateAuditItem
	if err := common.DB.First(&after, shortBad.ID).Error; err != nil || after.InputRefsJSON != beforeInput || after.NetReturnPct != shortBad.NetReturnPct {
		t.Fatalf("PIT 输入或结果被重算: before=%+v after=%+v err=%v", shortBad, after, err)
	}
}

func TestCandidateAuditReasonStagesUnknownAndSampleDiscipline(t *testing.T) {
	tests := []struct {
		name       string
		event      *model.RecommendationCandidateEvent
		discovered bool
		facts      bool
		ambiguous  bool
		stage      string
		reason     string
	}{
		{"absent market", nil, false, true, false, "absent", "not_discovered_marketwide"},
		{"absent user", nil, true, true, false, "absent", "discovered_not_in_user_pool"},
		{"filtered", &model.RecommendationCandidateEvent{CandidateStage: model.CandStageFiltered, RejectionReason: "停牌"}, true, true, false, "filtered", "suspended_filter"},
		{"pool full", &model.RecommendationCandidateEvent{CandidateStage: model.CandStagePoolFull}, true, true, false, "pool_full", "pool_capacity"},
		{"scored", &model.RecommendationCandidateEvent{CandidateStage: model.CandStageScored, ScoreRank: 99}, true, true, false, "scored", "quant_rank_below_cutoff"},
		{"llm", &model.RecommendationCandidateEvent{CandidateStage: model.CandStageLLMList}, true, true, false, "llm_list", "llm_not_selected"},
		{"picked", &model.RecommendationCandidateEvent{CandidateStage: model.CandStagePicked}, true, true, false, "picked", "picked_observed"},
		{"missing", nil, true, false, false, "unknown", "candidate_facts_missing"},
		{"ambiguous", nil, true, true, true, "unknown", "candidate_facts_ambiguous"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stage, reason := candidateAuditReason("", test.event, test.discovered, test.facts, test.ambiguous)
			if stage != test.stage || reason != test.reason {
				t.Fatalf("want %s/%s got %s/%s", test.stage, test.reason, stage, reason)
			}
		})
	}

	makeItems := func(days int, count int) []model.CandidateAuditItem {
		items := make([]model.CandidateAuditItem, count)
		for i := range items {
			items[i] = model.CandidateAuditItem{OutcomeStatus: btObserved,
				OutcomeDate: time.Date(2044, 1, 1+i%days, 0, 0, 0, 0, time.Local).Format("2006-01-02"),
				RecType:     model.RecTypeShortTerm, Regime: "neutral", DiscoveryVersion: "dv1",
				RankingVersion: "rv1", HoldingPeriodDays: 5, NetReturnPct: 1}
		}
		return items
	}
	if metric := candidateAuditMetric(makeItems(9, 30)); metric.Evaluated {
		t.Fatalf("9 个结果日不得宣称已评估: %+v", metric)
	}
	items := makeItems(10, 30)
	if metric := candidateAuditMetric(items); !metric.Evaluated {
		t.Fatalf("10 日 30 条应达到集中配置的评估门槛: %+v", metric)
	}
	items[0].RankingVersion = "rv2"
	slices := candidateAuditSlices(items)
	foundRV1, foundRV2 := false, false
	for _, row := range slices {
		if row.Dimension == "ranking_version" && row.Key == "rv1" {
			foundRV1 = true
		}
		if row.Dimension == "ranking_version" && row.Key == "rv2" && !row.Evaluated {
			foundRV2 = true
		}
	}
	if !foundRV1 || !foundRV2 {
		t.Fatalf("排名版本必须独立分层且小样本未评估: %+v", slices)
	}
}

func TestCandidateAuditUserIsolationAndAdminAggregatePrivacy(t *testing.T) {
	shortBatch, _ := seedCandidateAuditFixture(t)
	run, err := ExecuteCandidateAudit(context.Background(), nil, 0, candidateAuditJobRequest{
		Market: "cn", SignalDate: auditSignalDate, OutcomeDate: auditOutcomeDate,
		ParameterHash: candidateAuditParameterHash()})
	if err != nil {
		t.Fatal(err)
	}
	user, err := LoadCandidateAuditUserReport(shortBatch.UserID, "", 30)
	if err != nil || len(user.Items) == 0 {
		t.Fatalf("用户报表: %+v err=%v", user, err)
	}
	for _, item := range user.Items {
		if item.BatchID != shortBatch.ID {
			t.Fatalf("用户报表泄露其他批次: %+v", item)
		}
	}
	other, err := LoadCandidateAuditUserReport(9999, "", 30)
	if err != nil || len(other.Items) != 0 || len(other.Runs) != 0 {
		t.Fatalf("无关用户不得读到任何明细或运行: %+v err=%v", other, err)
	}
	admin, err := LoadCandidateAuditAdminReport(30)
	if err != nil || admin.Runs != 1 || admin.Users != 2 {
		t.Fatalf("管理员聚合错误: %+v err=%v", admin, err)
	}
	wire, _ := json.Marshal(admin)
	text := string(wire)
	for _, sensitive := range []string{"600101", "600108", `"batch_id"`, `"user_id"`} {
		if strings.Contains(text, sensitive) {
			t.Fatalf("管理员聚合泄露敏感明细 %s: %s", sensitive, text)
		}
	}
	if run.ID == 0 {
		t.Fatal("审计运行未落库")
	}
}

func TestCandidateAuditJobRunDedupTimeoutAndIdempotentRetry(t *testing.T) {
	resetDurableJobs(t)
	common.DB.Exec("DELETE FROM candidate_audit_items")
	common.DB.Exec("DELETE FROM candidate_audit_runs")
	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	var calls atomic.Int32
	handler := func(ctx context.Context, _ int64, _ bool, _ json.RawMessage) (DurableJobResult, error) {
		call := calls.Add(1)
		resultID, ok := currentJobResultID(ctx)
		if !ok {
			return DurableJobResult{}, context.Canceled
		}
		if call == 1 {
			<-ctx.Done()
			return DurableJobResult{}, ctx.Err()
		}
		now := time.Now()
		if err := common.DB.Model(&model.CandidateAuditRun{}).Where("id = ?", resultID).Updates(map[string]any{
			"status": model.CandidateAuditStatusSuccess, "finished_at": now, "item_count": 0,
		}).Error; err != nil {
			return DurableJobResult{}, err
		}
		return DurableJobResult{Status: model.JobStatusSuccess}, nil
	}
	runtime.registerWithBinding(JobKindCandidateAudit, 40*time.Millisecond, handler, registerCandidateAuditBinding(), false)
	req := candidateAuditJobRequest{Version: 1, Market: "cn", SignalDate: "2044-04-01",
		OutcomeDate: "2044-04-04", ParameterHash: candidateAuditParameterHash()}
	first, created, err := runtime.startSystemWithBindingStatus(nil, JobKindCandidateAudit, req, nil)
	if err != nil || !created {
		t.Fatalf("首次审计作业: run=%+v created=%v err=%v", first, created, err)
	}
	duplicate, duplicateCreated, err := runtime.startSystemWithBindingStatus(nil, JobKindCandidateAudit, req, nil)
	if err != nil || duplicateCreated || duplicate.ID != first.ID {
		t.Fatalf("在途重复启动必须复用: first=%+v duplicate=%+v created=%v err=%v", first, duplicate, duplicateCreated, err)
	}
	waitSystemJob := func(id int64) model.JobRun {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var row model.JobRun
			if err := common.DB.First(&row, id).Error; err == nil && row.Status != model.JobStatusQueued && row.Status != model.JobStatusRunning {
				return row
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("等待 system JobRun %d 超时", id)
		return model.JobRun{}
	}
	if failed := waitSystemJob(first.ID); failed.Status != model.JobStatusFailed || failed.OwnerType != model.JobOwnerSystem || failed.UserID != 0 {
		t.Fatalf("超时必须安全失败且保持 system/global: %+v", failed)
	}
	var failedAudit model.CandidateAuditRun
	if err := common.DB.Where("job_run_id = ?", first.ID).First(&failedAudit).Error; err != nil || failedAudit.Status != model.CandidateAuditStatusFailed {
		t.Fatalf("超时必须同步收敛审计运行: %+v err=%v", failedAudit, err)
	}

	retry, retryCreated, err := runtime.startSystemWithBindingStatus(nil, JobKindCandidateAudit, req, nil)
	if err != nil || !retryCreated || retry.ID == first.ID {
		t.Fatalf("失败后应创建新 JobRun 并复用业务身份: retry=%+v created=%v err=%v", retry, retryCreated, err)
	}
	if done := waitSystemJob(retry.ID); done.Status != model.JobStatusSuccess {
		t.Fatalf("幂等重试应成功: %+v", done)
	}
	var audits []model.CandidateAuditRun
	if err := common.DB.Find(&audits).Error; err != nil || len(audits) != 1 || audits[0].ID != failedAudit.ID || audits[0].Status != model.CandidateAuditStatusSuccess {
		t.Fatalf("业务运行应复用同一唯一事实: %+v err=%v", audits, err)
	}
}

func TestSelectionEvalJSONDoesNotExposeCrossUserBatchDetails(t *testing.T) {
	report := SelectionEvalReport{Sections: []SelectionEvalSection{{
		Batches: []SelectionBatchView{{BatchID: 998877, SignalDate: "2044-05-06",
			AI: []SelectionPickView{{Symbol: "SENSITIVE_AI"}}, Quant: []SelectionPickView{{Symbol: "SENSITIVE_QUANT"}}}},
		Pairs: []SelectionPairedRow{{Pair: "ai_minus_quant", Batches: 1,
			BatchDiffs: []SelectionBatchDiff{{BatchID: 998877, SignalDate: "2044-05-06",
				LeftSymbols: []string{"SENSITIVE_AI"}, RightSymbols: []string{"SENSITIVE_QUANT"}}}}},
	}}}
	wire, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(wire)
	for _, sensitive := range []string{"998877", "2044-05-06", "SENSITIVE_AI", "SENSITIVE_QUANT", "batch_diffs"} {
		if strings.Contains(text, sensitive) {
			t.Fatalf("SelectionEval JSON 泄露跨用户批次明细 %s: %s", sensitive, text)
		}
	}
}
