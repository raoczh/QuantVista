package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/model"
)

const (
	selectionBootstrapSeed       int64 = 20260731
	selectionBootstrapIterations       = 2000
	selectionSliceMinBatches           = 10
)

type SelectionBootstrapSpec struct {
	Seed       int64 `json:"seed"`
	Iterations int   `json:"iterations"`
}

type SelectionEvalCoverage struct {
	Batches                int     `json:"batches"`
	FactsReadyBatches      int     `json:"facts_ready_batches"`
	SuccessBatches         int     `json:"success_batches"`
	DegradedExcluded       int     `json:"degraded_excluded"`
	FactsMissingExcluded   int     `json:"facts_missing_excluded"`
	EventsMissingExcluded  int     `json:"events_missing_excluded"`
	RankingExcluded        int     `json:"ranking_excluded"`
	DuplicateFactsExcluded int     `json:"duplicate_facts_excluded"`
	PickMismatchExcluded   int     `json:"pick_mismatch_excluded"`
	ZeroPickBatches        int     `json:"zero_pick_batches"`
	ZeroPickRatePct        float64 `json:"zero_pick_rate_pct"`
	OpportunitySymbols     int     `json:"opportunity_symbols"`
	AIPicks                int     `json:"ai_picks"`
	OutcomeRows            int     `json:"outcome_rows"`
	OutcomeMatured         int     `json:"outcome_matured"`
	OutcomePending         int     `json:"outcome_pending"`
	OutcomeSkipped         int     `json:"outcome_skipped"`
	OutcomeNoData          int     `json:"outcome_no_data"`
	OutcomeForced          int     `json:"outcome_forced"`
	ChallengerRuns         int     `json:"challenger_runs"`
	ChallengerValidRuns    int     `json:"challenger_valid_runs"`
	ChallengerInvalidRuns  int     `json:"challenger_invalid_runs"`
	ChallengerZeroPickRuns int     `json:"challenger_zero_pick_runs"`
}

type SelectionMetric struct {
	Group            string  `json:"group"`
	Label            string  `json:"label"`
	Evaluated        bool    `json:"evaluated"`
	Batches          int     `json:"batches"`
	SelectedSymbols  int     `json:"selected_symbols"`
	SampleSymbols    int     `json:"sample_symbols"`
	CoveragePct      float64 `json:"coverage_pct"`
	AvgGrossPct      float64 `json:"avg_gross_pct"`
	AvgNetPct        float64 `json:"avg_net_pct"`
	MedianNetPct     float64 `json:"median_net_pct"`
	P10NetPct        float64 `json:"p10_net_pct"`
	NetPositivePct   float64 `json:"net_positive_pct"`
	SevereLossPct    float64 `json:"severe_loss_pct"`
	AlphaSample      int     `json:"alpha_sample"`
	AvgAlphaPct      float64 `json:"avg_alpha_pct"`
	MedianAlphaPct   float64 `json:"median_alpha_pct"`
	P10AlphaPct      float64 `json:"p10_alpha_pct"`
	AlphaPositivePct float64 `json:"alpha_positive_pct"`
	AvgMfePct        float64 `json:"avg_mfe_pct"`
	MedianMfePct     float64 `json:"median_mfe_pct"`
	AvgMaePct        float64 `json:"avg_mae_pct"`
	MedianMaePct     float64 `json:"median_mae_pct"`
}

type SelectionBootstrapCI struct {
	SampleBatches int     `json:"sample_batches"`
	Estimate      float64 `json:"estimate"`
	Low95         float64 `json:"low_95"`
	High95        float64 `json:"high_95"`
}

type SelectionBatchDiff struct {
	BatchID            int64    `json:"batch_id"`
	SignalDate         string   `json:"signal_date"`
	K                  int      `json:"k"`
	LeftSymbols        []string `json:"left_symbols"`
	RightSymbols       []string `json:"right_symbols"`
	AvgNetDiffPct      float64  `json:"avg_net_diff_pct"`
	MedianNetDiffPct   float64  `json:"median_net_diff_pct"`
	P10NetDiffPct      float64  `json:"p10_net_diff_pct"`
	NetPositiveDiffPct float64  `json:"net_positive_diff_pct"`
	SevereLossDiffPct  float64  `json:"severe_loss_diff_pct"`
	AvgMfeDiffPct      float64  `json:"avg_mfe_diff_pct"`
	AvgMaeDiffPct      float64  `json:"avg_mae_diff_pct"`
	HasAlpha           bool     `json:"has_alpha"`
	AvgAlphaDiffPct    float64  `json:"avg_alpha_diff_pct"`
}

type SelectionPairedRow struct {
	Pair           string               `json:"pair"`
	Label          string               `json:"label"`
	LeftGroup      string               `json:"left_group"`
	RightGroup     string               `json:"right_group"`
	Batches        int                  `json:"batches"`
	LeftWins       int                  `json:"left_wins"`
	Ties           int                  `json:"ties"`
	RightWins      int                  `json:"right_wins"`
	AvgNetPct      SelectionBootstrapCI `json:"avg_net_pct"`
	MedianNetPct   SelectionBootstrapCI `json:"median_net_pct"`
	P10NetPct      SelectionBootstrapCI `json:"p10_net_pct"`
	NetPositivePct SelectionBootstrapCI `json:"net_positive_pct"`
	AvgAlphaPct    SelectionBootstrapCI `json:"avg_alpha_pct"`
	SevereLossPct  SelectionBootstrapCI `json:"severe_loss_pct"`
	AvgMfePct      SelectionBootstrapCI `json:"avg_mfe_pct"`
	AvgMaePct      SelectionBootstrapCI `json:"avg_mae_pct"`
	BatchDiffs     []SelectionBatchDiff `json:"batch_diffs"`
}

type SelectionSectionCoverage struct {
	CandidateBatches  int     `json:"candidate_batches"`
	ComparableBatches int     `json:"comparable_batches"`
	CoveragePct       float64 `json:"coverage_pct"`
	MissingExcluded   int     `json:"missing_excluded"`
	PendingExcluded   int     `json:"pending_excluded"`
	SkippedExcluded   int     `json:"skipped_excluded"`
	NoDataExcluded    int     `json:"no_data_excluded"`
	ForcedExcluded    int     `json:"forced_excluded"`
}

type SelectionPickView struct {
	Symbol    string `json:"symbol"`
	Name      string `json:"name"`
	Action    string `json:"action,omitempty"`
	Order     int    `json:"order"`
	ScoreRank int    `json:"score_rank"`
}

type SelectionBatchView struct {
	BatchID    int64               `json:"batch_id"`
	SignalDate string              `json:"signal_date"`
	N          int                 `json:"n"`
	AI         []SelectionPickView `json:"ai"`
	Quant      []SelectionPickView `json:"quant"`
	Comparable bool                `json:"comparable"`
	Exclusion  string              `json:"exclusion,omitempty"`
}

type SelectionActionRow struct {
	Action string          `json:"action"`
	Label  string          `json:"label"`
	Metric SelectionMetric `json:"metric"`
}

type SelectionActionTransition struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count"`
}

type SelectionPlanPanel struct {
	Coverage  SelectionSectionCoverage `json:"coverage"`
	FixedHold SelectionMetric          `json:"fixed_hold"`
	PlanL2    SelectionMetric          `json:"plan_l2"`
	Pair      SelectionPairedRow       `json:"pair"`
	Notes     []string                 `json:"notes"`
}

type SelectionSliceRow struct {
	Key       string                `json:"key"`
	Batches   int                   `json:"batches"`
	Evaluated bool                  `json:"evaluated"`
	AvgNetPct *SelectionBootstrapCI `json:"avg_net_pct,omitempty"`
	Note      string                `json:"note,omitempty"`
}

type SelectionSliceGroup struct {
	Dim   string              `json:"dim"`
	Label string              `json:"label"`
	Rows  []SelectionSliceRow `json:"rows"`
}

type SelectionChallengerCoverage struct {
	Runs            int     `json:"runs"`
	NativeKMin      int     `json:"native_k_min"`
	NativeKMax      int     `json:"native_k_max"`
	NativeKAvg      float64 `json:"native_k_avg"`
	NativeEligible  int     `json:"native_eligible"`
	MatchedEligible int     `json:"matched_eligible"`
	OutcomeExcluded int     `json:"outcome_excluded"`
	ZeroMatched     int     `json:"zero_matched"`
}

type SelectionChallengerEval struct {
	ExperimentID int64                       `json:"experiment_id"`
	Name         string                      `json:"name"`
	Coverage     SelectionChallengerCoverage `json:"coverage"`
	Groups       []SelectionMetric           `json:"groups"`
	Pairs        []SelectionPairedRow        `json:"pairs"`
	Notes        []string                    `json:"notes"`
}

type SelectionEvalSection struct {
	RecType           string                      `json:"rec_type"`
	HorizonDays       int                         `json:"horizon_days"`
	Coverage          SelectionSectionCoverage    `json:"coverage"`
	Groups            []SelectionMetric           `json:"groups"`
	Pairs             []SelectionPairedRow        `json:"pairs"`
	Batches           []SelectionBatchView        `json:"batches"`
	ActionVeto        []SelectionActionRow        `json:"action_veto"`
	ActionTransitions []SelectionActionTransition `json:"action_transitions"`
	Plan              SelectionPlanPanel          `json:"plan"`
	Challengers       []SelectionChallengerEval   `json:"challengers"`
	Slices            []SelectionSliceGroup       `json:"slices"`
	Notes             []string                    `json:"notes"`
}

type SelectionEvalReport struct {
	GeneratedAt             string                 `json:"generated_at"`
	OutcomeVersion          string                 `json:"outcome_version"`
	SchemaVersion           string                 `json:"schema_version"`
	RankingVersion          string                 `json:"ranking_version"`
	ChallengerSchemaVersion string                 `json:"challenger_schema_version"`
	Bootstrap               SelectionBootstrapSpec `json:"bootstrap"`
	Coverage                SelectionEvalCoverage  `json:"coverage"`
	Sections                []SelectionEvalSection `json:"sections"`
	Notes                   []string               `json:"notes"`
	ElapsedMs               int64                  `json:"elapsed_ms"`
}

var (
	selectionEvalInflight atomic.Bool
	selectionEvalCacheMu  sync.RWMutex
	selectionEvalCache    *SelectionEvalReport
)

func CachedSelectionEvalReport() *SelectionEvalReport {
	selectionEvalCacheMu.RLock()
	defer selectionEvalCacheMu.RUnlock()
	return selectionEvalCache
}

// RunSelectionEval 推进 so1 fixed-hold outcome 并重建配对报表。路径只调用执行模拟、
// 本地数据查询和统计函数，不经过任何 chatCompletion/LLM 服务。
func RunSelectionEval(ctx context.Context, market *MarketService) (*SelectionEvalReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !selectionEvalInflight.CompareAndSwap(false, true) {
		return nil, errors.New("选股配对评估正在计算中，请稍候")
	}
	defer selectionEvalInflight.Store(false)
	start := time.Now()
	batches, err := loadSelectionBatchFacts()
	if err != nil {
		return nil, err
	}
	if _, err := advanceSelectionOutcomes(ctx, market, batches, start); err != nil {
		return nil, err
	}
	rep, err := buildSelectionEvalReport(batches, start)
	if err != nil {
		return nil, err
	}
	rep.ElapsedMs = time.Since(start).Milliseconds()
	selectionEvalCacheMu.Lock()
	selectionEvalCache = rep
	selectionEvalCacheMu.Unlock()
	return rep, nil
}

type selectionObs struct {
	BatchID  int64
	Symbol   string
	Gross    float64
	Net      float64
	Alpha    float64
	HasAlpha bool
	MFE      float64
	MAE      float64
}

type selectionBatchAgg struct {
	AvgNet      float64
	MedianNet   float64
	P10Net      float64
	NetPositive float64
	Severe      float64
	AvgAlpha    float64
	HasAlpha    bool
	AvgMFE      float64
	AvgMAE      float64
}

type selectionPlanLabelKey struct {
	BatchID int64
	Symbol  string
	Horizon int
}

type selectionChallengerFact struct {
	Symbol     string
	Order      int
	Action     string
	Confidence int
}

type selectionChallengerRun struct {
	Run        model.LLMExperimentRun
	Name       string
	Challenger []selectionChallengerFact
	Issue      string
}

type selectionPlanLabelEntry struct {
	Label model.RecommendationLabel
	Count int
}

func buildSelectionEvalReport(batches []selectionBatchFacts, now time.Time) (*SelectionEvalReport, error) {
	rep := &SelectionEvalReport{
		GeneratedAt:             now.In(time.Local).Format("2006-01-02 15:04:05"),
		OutcomeVersion:          model.SelectionOutcomeVersion,
		SchemaVersion:           model.SelectionOutcomeSchemaVersion,
		RankingVersion:          candidateRankingVersion,
		ChallengerSchemaVersion: llmExperimentPickSchemaVersion,
		Bootstrap:               SelectionBootstrapSpec{Seed: selectionBootstrapSeed, Iterations: selectionBootstrapIterations},
	}
	factByBatch := make(map[int64]*selectionBatchFacts, len(batches))
	rankingVersions := map[string]bool{}
	for i := range batches {
		bf := &batches[i]
		factByBatch[bf.Batch.ID] = bf
		rep.Coverage.Batches++
		if bf.Batch.Status == model.RecStatusSuccess {
			rep.Coverage.SuccessBatches++
		} else if bf.Batch.Status == model.RecStatusDegraded {
			rep.Coverage.DegradedExcluded++
		}
		if bf.Issue != "" {
			switch bf.Issue {
			case selectionFactFactsMissing:
				rep.Coverage.FactsMissingExcluded++
			case selectionFactEventsMissing:
				rep.Coverage.EventsMissingExcluded++
			case selectionFactRankingOld:
				rep.Coverage.RankingExcluded++
			case selectionFactDuplicate:
				rep.Coverage.DuplicateFactsExcluded++
			case selectionFactPickMismatch:
				rep.Coverage.PickMismatchExcluded++
			}
			continue
		}
		rep.Coverage.FactsReadyBatches++
		rankingVersions[bf.RankingVersion] = true
		rep.Coverage.OpportunitySymbols += len(bf.Opportunity)
		rep.Coverage.AIPicks += len(bf.Picks)
		if len(bf.Picks) == 0 {
			rep.Coverage.ZeroPickBatches++
		}
	}
	if rep.Coverage.FactsReadyBatches > 0 {
		rep.Coverage.ZeroPickRatePct = round2(float64(rep.Coverage.ZeroPickBatches) /
			float64(rep.Coverage.FactsReadyBatches) * 100)
	}
	if len(rankingVersions) > 0 {
		versions := make([]string, 0, len(rankingVersions))
		for version := range rankingVersions {
			versions = append(versions, version)
		}
		sort.Strings(versions)
		rep.RankingVersion = strings.Join(versions, ",")
	}

	var outcomes []model.RecommendationSelectionOutcome
	if err := common.DB.Where("outcome_version = ?", model.SelectionOutcomeVersion).Find(&outcomes).Error; err != nil {
		return nil, err
	}
	outcomeByKey := make(map[string]model.RecommendationSelectionOutcome, len(outcomes))
	for _, row := range outcomes {
		rep.Coverage.OutcomeRows++
		switch row.MaturityStatus {
		case model.LabelMatured:
			rep.Coverage.OutcomeMatured++
		case model.LabelPending:
			rep.Coverage.OutcomePending++
		case model.LabelSkipped:
			rep.Coverage.OutcomeSkipped++
		case model.LabelNoData:
			rep.Coverage.OutcomeNoData++
		}
		if row.Forced {
			rep.Coverage.OutcomeForced++
		}
		bf, batchOK := factByBatch[row.BatchID]
		if row.SchemaVersion == model.SelectionOutcomeSchemaVersion &&
			selectionRankingVersionSupported(row.RankingVersion) && batchOK && bf.Issue == "" &&
			row.RankingVersion == bf.RankingVersion {
			outcomeByKey[selectionOutcomeKey(row.BatchID, row.Symbol, row.HorizonDays)] = row
		}
	}

	planLabels := map[selectionPlanLabelKey]selectionPlanLabelEntry{}
	var labels []model.RecommendationLabel
	if err := common.DB.Where("entry_mode = ? AND label_version = ? AND horizon_days IN ?",
		model.EntryModeNextOpen, labelVersion, model.SelectionOutcomeHorizons).Find(&labels).Error; err != nil {
		return nil, err
	}
	for _, label := range labels {
		if label.RecommendationID <= 0 {
			continue
		}
		key := selectionPlanLabelKey{BatchID: label.BatchID, Symbol: label.Symbol, Horizon: label.HorizonDays}
		entry := planLabels[key]
		entry.Label, entry.Count = label, entry.Count+1
		planLabels[key] = entry
	}

	challengers, err := loadSelectionChallengerRuns(factByBatch, &rep.Coverage)
	if err != nil {
		return nil, err
	}
	for _, spec := range []struct {
		recType  string
		horizons []int
	}{
		{model.RecTypeShortTerm, []int{5, 10}},
		{model.RecTypeLongTerm, []int{20, 60}},
	} {
		for _, horizon := range spec.horizons {
			rep.Sections = append(rep.Sections, buildSelectionSection(spec.recType, horizon,
				batches, outcomeByKey, planLabels, challengers))
		}
	}
	rep.Notes = []string{
		"so1 是独立 fixed-hold 测量事实：next_open、统一费税、T+1/可成交规则、固定 5/10/20/60 交易日，不读取 TP/SL；l2 计划标签未被改写",
		fmt.Sprintf("主 selection 指标只纳入 facts_recorded=true、排名/输入顺序完整（观测版本 %s）、success、AI picks>0 且两组结果全部成熟非 forced 的批次；degraded、旧行、pending/forced/no_data/skipped 分列", rep.RankingVersion),
		"组级收益按标的汇总；所有比较差先在每批内计算，再以批次为重采样单位做固定 seed paired bootstrap，避免把同批多只股票当独立样本",
		"challenger 仅纳入 valid+ep1 且两份逐标的 JSON 完整的 run，并按 experiment_id 分开；matched-K=min(AI N, challenger 原生 K)，不补造标的",
		fmt.Sprintf("策略/regime/provider·model/prompt 分层至少 %d 个可比批次才评估，否则明确标记不确定", selectionSliceMinBatches),
		"本报表计算路径不调用 LLM，不改推荐、prompt、权重、门控或模型路由",
	}
	return rep, nil
}

type selectionPickFactWire struct {
	Symbol     *string `json:"symbol"`
	Order      *int    `json:"order"`
	Action     *string `json:"action"`
	Confidence *int    `json:"confidence"`
}

func parseSelectionPickFacts(raw string, want int, allowed map[string]bool) ([]selectionChallengerFact, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("逐标的 JSON 为空")
	}
	var wire []selectionPickFactWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, err
	}
	if wire == nil || len(wire) != want {
		return nil, fmt.Errorf("逐标的数量 %d 与聚合数量 %d 不一致", len(wire), want)
	}
	seen := map[string]bool{}
	out := make([]selectionChallengerFact, 0, len(wire))
	for i, v := range wire {
		if v.Symbol == nil || v.Order == nil || v.Action == nil || v.Confidence == nil {
			return nil, errors.New("逐标的字段不完整")
		}
		sym, action := strings.TrimSpace(*v.Symbol), strings.TrimSpace(*v.Action)
		if sym == "" || seen[sym] || !allowed[sym] || *v.Order != i+1 ||
			(action != model.RecActionBuy && action != model.RecActionWatch) ||
			*v.Confidence < 0 || *v.Confidence > 100 {
			return nil, errors.New("逐标的 symbol/order/action/confidence 非法")
		}
		seen[sym] = true
		out = append(out, selectionChallengerFact{Symbol: sym, Order: *v.Order,
			Action: action, Confidence: *v.Confidence})
	}
	return out, nil
}

func loadSelectionChallengerRuns(facts map[int64]*selectionBatchFacts,
	coverage *SelectionEvalCoverage) ([]selectionChallengerRun, error) {
	var runs []model.LLMExperimentRun
	if err := common.DB.Order("experiment_id, batch_id, id").Find(&runs).Error; err != nil {
		return nil, err
	}
	coverage.ChallengerRuns = len(runs)
	var experiments []model.LLMExperiment
	if err := common.DB.Select("id", "name").Find(&experiments).Error; err != nil {
		return nil, err
	}
	nameByID := map[int64]string{}
	for _, exp := range experiments {
		nameByID[exp.ID] = exp.Name
	}
	dups := map[string]int{}
	for _, run := range runs {
		dups[int64Key(run.ExperimentID)+":"+int64Key(run.BatchID)]++
	}
	out := make([]selectionChallengerRun, 0, len(runs))
	for _, run := range runs {
		cr := selectionChallengerRun{Run: run, Name: nameByID[run.ExperimentID]}
		bf := facts[run.BatchID]
		if dups[int64Key(run.ExperimentID)+":"+int64Key(run.BatchID)] != 1 {
			cr.Issue = "duplicate_experiment_batch_run"
		} else if !run.Valid || run.PickSchemaVersion != llmExperimentPickSchemaVersion {
			cr.Issue = "invalid_or_old_schema"
		} else if bf == nil || bf.Issue != "" || bf.Batch.Status != model.RecStatusSuccess ||
			run.UserID != bf.Batch.UserID {
			cr.Issue = "batch_not_comparable"
		} else {
			allowed := map[string]bool{}
			for _, ev := range bf.Opportunity {
				allowed[ev.Symbol] = true
			}
			champ, champErr := parseSelectionPickFacts(run.ChampionPicksJSON, run.ChampionPicks, allowed)
			chal, chalErr := parseSelectionPickFacts(run.ChallengerPicksJSON, run.PicksCount, allowed)
			if champErr != nil || chalErr != nil || len(champ) != len(bf.Picks) {
				cr.Issue = "incomplete_pick_json"
			} else {
				for i := range champ {
					if champ[i].Symbol != bf.Picks[i].Symbol {
						cr.Issue = "champion_not_final_pick_order"
						break
					}
				}
				if cr.Issue == "" {
					cr.Challenger = chal
				}
			}
		}
		if cr.Issue != "" {
			coverage.ChallengerInvalidRuns++
		} else {
			coverage.ChallengerValidRuns++
			if len(cr.Challenger) == 0 {
				coverage.ChallengerZeroPickRuns++
			}
		}
		out = append(out, cr)
	}
	return out, nil
}

func buildSelectionSection(recType string, horizon int, batches []selectionBatchFacts,
	outcomes map[string]model.RecommendationSelectionOutcome,
	planLabels map[selectionPlanLabelKey]selectionPlanLabelEntry,
	challengers []selectionChallengerRun) SelectionEvalSection {
	sec := SelectionEvalSection{RecType: recType, HorizonDays: horizon}
	var aiObs, quantObs []selectionObs
	var diffs []SelectionBatchDiff
	selectedAI, selectedQuant := 0, 0
	actionSelected := map[string]int{"buy": 0, "watch": 0, "reject": 0}
	actionObs := map[string][]selectionObs{"buy": {}, "watch": {}, "reject": {}}
	transitions := map[string]int{}
	batchFacts := map[int64]selectionBatchFacts{}

	for _, bf := range batches {
		if bf.Batch.Type != recType || bf.Issue != "" || bf.Batch.Status != model.RecStatusSuccess {
			continue
		}
		batchFacts[bf.Batch.ID] = bf
		pickBySymbol := map[string]model.Recommendation{}
		for _, pick := range bf.Picks {
			pickBySymbol[pick.Symbol] = pick
		}
		for _, ev := range bf.Opportunity {
			action := "reject"
			if pick, ok := pickBySymbol[ev.Symbol]; ok {
				action = pick.Action
				from := ev.RawAction
				if from == "" {
					from = action
				}
				transitions[from+"->"+action]++
			}
			actionSelected[action]++
			if row, ok := comparableSelectionOutcome(outcomes, bf.Batch.ID, ev.Symbol, horizon); ok {
				actionObs[action] = append(actionObs[action], outcomeObs(row))
			}
		}
		if len(bf.Picks) == 0 {
			continue
		}
		sec.Coverage.CandidateBatches++
		quant := quantTopN(bf.Opportunity, len(bf.Picks))
		selectedAI += len(bf.Picks)
		selectedQuant += len(quant)
		aiSymbols := recommendationSymbols(bf.Picks)
		quantSymbols := eventSymbols(quant)
		allSymbols := append(append([]string{}, aiSymbols...), quantSymbols...)
		reason := selectionOutcomeSetIssue(outcomes, bf.Batch.ID, allSymbols, horizon)
		view := SelectionBatchView{
			BatchID: bf.Batch.ID, SignalDate: batchSignalDate(bf.Batch), N: len(bf.Picks),
			AI: pickViews(bf.Picks, bf.Opportunity), Quant: quantViews(quant),
			Comparable: reason == "", Exclusion: reason,
		}
		sec.Batches = append(sec.Batches, view)
		if reason != "" {
			addSectionExclusion(&sec.Coverage, reason)
			continue
		}
		batchAI := outcomesForSymbols(outcomes, bf.Batch.ID, aiSymbols, horizon)
		batchQuant := outcomesForSymbols(outcomes, bf.Batch.ID, quantSymbols, horizon)
		aiObs = append(aiObs, batchAI...)
		quantObs = append(quantObs, batchQuant...)
		diffs = append(diffs, makeSelectionBatchDiff(bf.Batch, batchAI, batchQuant, aiSymbols, quantSymbols))
		sec.Coverage.ComparableBatches++
	}
	if sec.Coverage.CandidateBatches > 0 {
		sec.Coverage.CoveragePct = round2(float64(sec.Coverage.ComparableBatches) /
			float64(sec.Coverage.CandidateBatches) * 100)
	}
	sec.Groups = []SelectionMetric{
		makeSelectionMetric("ai", "AI 最终 picks", selectedAI, aiObs),
		makeSelectionMetric("quant", "Quant Top-N", selectedQuant, quantObs),
	}
	sec.Pairs = []SelectionPairedRow{makeSelectionPair("ai_minus_quant", "AI - Quant",
		"ai", "quant", diffs, recType+":"+intKey(horizon)+":ai_quant")}
	for _, action := range []struct{ key, label string }{
		{"buy", "最终 buy"}, {"watch", "最终 watch"}, {"reject", "未选 / veto"},
	} {
		sec.ActionVeto = append(sec.ActionVeto, SelectionActionRow{
			Action: action.key, Label: action.label,
			Metric: makeSelectionMetric(action.key, action.label, actionSelected[action.key], actionObs[action.key]),
		})
	}
	var transitionKeys []string
	for key := range transitions {
		transitionKeys = append(transitionKeys, key)
	}
	sort.Strings(transitionKeys)
	for _, key := range transitionKeys {
		parts := strings.SplitN(key, "->", 2)
		sec.ActionTransitions = append(sec.ActionTransitions, SelectionActionTransition{
			From: parts[0], To: parts[1], Count: transitions[key],
		})
	}
	sec.Plan = buildSelectionPlanPanel(recType, horizon, batches, outcomes, planLabels)
	sec.Challengers = buildSelectionChallengerEvals(recType, horizon, batchFacts, outcomes, challengers)
	sec.Slices = buildSelectionSlices(diffs, batchFacts, recType, horizon)
	sec.Notes = []string{
		"Selection：AI 取真实 recommendations；N 为该批有效 AI picks 数；Quant 仅从同批已审计机会集按 score_rank 取前 N",
		"Action / Veto：buy、watch、未选分别统计 fixed-hold 结果，不与 selection 配对结论合成一个胜率",
		"Plan：只在同一 AI picks 交集上比较 l2 计划结算与 so1 fixed-hold，属于辅助面板",
	}
	return sec
}

func batchSignalDate(batch model.RecommendationBatch) string {
	return batch.CreatedAt.In(time.Local).Format("2006-01-02")
}

func recommendationSymbols(picks []model.Recommendation) []string {
	out := make([]string, 0, len(picks))
	for _, pick := range picks {
		out = append(out, pick.Symbol)
	}
	return out
}

func eventSymbols(events []model.RecommendationCandidateEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Symbol)
	}
	return out
}

func pickViews(picks []model.Recommendation, opp []model.RecommendationCandidateEvent) []SelectionPickView {
	eventBySymbol := map[string]model.RecommendationCandidateEvent{}
	for _, ev := range opp {
		eventBySymbol[ev.Symbol] = ev
	}
	out := make([]SelectionPickView, 0, len(picks))
	for i, pick := range picks {
		ev := eventBySymbol[pick.Symbol]
		out = append(out, SelectionPickView{Symbol: pick.Symbol, Name: pick.Name, Action: pick.Action,
			Order: i + 1, ScoreRank: ev.ScoreRank})
	}
	return out
}

func quantViews(events []model.RecommendationCandidateEvent) []SelectionPickView {
	out := make([]SelectionPickView, 0, len(events))
	for i, ev := range events {
		out = append(out, SelectionPickView{Symbol: ev.Symbol, Name: ev.Name,
			Order: i + 1, ScoreRank: ev.ScoreRank})
	}
	return out
}

func comparableSelectionOutcome(outcomes map[string]model.RecommendationSelectionOutcome,
	batchID int64, symbol string, horizon int) (model.RecommendationSelectionOutcome, bool) {
	row, ok := outcomes[selectionOutcomeKey(batchID, symbol, horizon)]
	return row, ok && row.MaturityStatus == model.LabelMatured && !row.Forced
}

func selectionOutcomeSetIssue(outcomes map[string]model.RecommendationSelectionOutcome,
	batchID int64, symbols []string, horizon int) string {
	priority := map[string]int{"": 0, "pending": 1, "skipped": 2, "forced": 3, "no_data": 4, "missing": 5}
	reason := ""
	seen := map[string]bool{}
	for _, symbol := range symbols {
		if seen[symbol] {
			continue
		}
		seen[symbol] = true
		row, ok := outcomes[selectionOutcomeKey(batchID, symbol, horizon)]
		cur := ""
		switch {
		case !ok:
			cur = "missing"
		case row.MaturityStatus == model.LabelNoData:
			cur = "no_data"
		case row.Forced:
			cur = "forced"
		case row.MaturityStatus == model.LabelSkipped:
			cur = "skipped"
		case row.MaturityStatus != model.LabelMatured:
			cur = "pending"
		}
		if priority[cur] > priority[reason] {
			reason = cur
		}
	}
	return reason
}

func addSectionExclusion(c *SelectionSectionCoverage, reason string) {
	switch reason {
	case "missing":
		c.MissingExcluded++
	case "pending":
		c.PendingExcluded++
	case "skipped":
		c.SkippedExcluded++
	case "no_data":
		c.NoDataExcluded++
	case "forced":
		c.ForcedExcluded++
	}
}

func outcomeObs(row model.RecommendationSelectionOutcome) selectionObs {
	return selectionObs{BatchID: row.BatchID, Symbol: row.Symbol, Gross: row.GrossReturnPct,
		Net: row.NetReturnPct, Alpha: row.AlphaPct, HasAlpha: row.HasBench,
		MFE: row.MfePct, MAE: row.MaePct}
}

func outcomesForSymbols(outcomes map[string]model.RecommendationSelectionOutcome,
	batchID int64, symbols []string, horizon int) []selectionObs {
	out := make([]selectionObs, 0, len(symbols))
	for _, symbol := range symbols {
		out = append(out, outcomeObs(outcomes[selectionOutcomeKey(batchID, symbol, horizon)]))
	}
	return out
}

func makeSelectionMetric(group, label string, selected int, obs []selectionObs) SelectionMetric {
	m := SelectionMetric{Group: group, Label: label, SelectedSymbols: selected,
		SampleSymbols: len(obs), Evaluated: len(obs) > 0}
	if selected > 0 {
		m.CoveragePct = round2(float64(len(obs)) / float64(selected) * 100)
	}
	if len(obs) == 0 {
		return m
	}
	batches := map[int64]bool{}
	nets := make([]float64, 0, len(obs))
	alpha := make([]float64, 0, len(obs))
	mfes := make([]float64, 0, len(obs))
	maes := make([]float64, 0, len(obs))
	var grossSum, netSum, alphaSum, mfeSum, maeSum float64
	positive, alphaPositive, severe := 0, 0, 0
	for _, o := range obs {
		batches[o.BatchID] = true
		grossSum += o.Gross
		netSum += o.Net
		mfeSum += o.MFE
		maeSum += o.MAE
		nets = append(nets, o.Net)
		mfes = append(mfes, o.MFE)
		maes = append(maes, o.MAE)
		if o.Net > 0 {
			positive++
		}
		if o.Net < -5 {
			severe++
		}
		if o.HasAlpha {
			alpha = append(alpha, o.Alpha)
			alphaSum += o.Alpha
			if o.Alpha > 0 {
				alphaPositive++
			}
		}
	}
	sort.Float64s(nets)
	sort.Float64s(alpha)
	sort.Float64s(mfes)
	sort.Float64s(maes)
	m.Batches = len(batches)
	m.AvgGrossPct = round2(grossSum / float64(len(obs)))
	m.AvgNetPct = round2(netSum / float64(len(obs)))
	m.MedianNetPct = round2(median(nets))
	m.P10NetPct = round2(percentileSorted(nets, 0.10))
	m.NetPositivePct = round2(float64(positive) / float64(len(obs)) * 100)
	m.SevereLossPct = round2(float64(severe) / float64(len(obs)) * 100)
	m.AvgMfePct, m.MedianMfePct = round2(mfeSum/float64(len(obs))), round2(median(mfes))
	m.AvgMaePct, m.MedianMaePct = round2(maeSum/float64(len(obs))), round2(median(maes))
	m.AlphaSample = len(alpha)
	if len(alpha) > 0 {
		m.AvgAlphaPct = round2(alphaSum / float64(len(alpha)))
		m.MedianAlphaPct = round2(median(alpha))
		m.P10AlphaPct = round2(percentileSorted(alpha, 0.10))
		m.AlphaPositivePct = round2(float64(alphaPositive) / float64(len(alpha)) * 100)
	}
	return m
}

func aggregateBatch(obs []selectionObs) selectionBatchAgg {
	if len(obs) == 0 {
		return selectionBatchAgg{}
	}
	nets := make([]float64, 0, len(obs))
	var netSum, alphaSum, mfeSum, maeSum float64
	positive, severe, alphaN := 0, 0, 0
	for _, o := range obs {
		nets = append(nets, o.Net)
		netSum += o.Net
		mfeSum += o.MFE
		maeSum += o.MAE
		if o.Net > 0 {
			positive++
		}
		if o.Net < -5 {
			severe++
		}
		if o.HasAlpha {
			alphaSum += o.Alpha
			alphaN++
		}
	}
	sort.Float64s(nets)
	a := selectionBatchAgg{
		AvgNet: netSum / float64(len(obs)), MedianNet: median(nets),
		P10Net: percentileSorted(nets, 0.10), NetPositive: float64(positive) / float64(len(obs)) * 100,
		Severe: float64(severe) / float64(len(obs)) * 100,
		AvgMFE: mfeSum / float64(len(obs)), AvgMAE: maeSum / float64(len(obs)),
	}
	if alphaN > 0 {
		a.HasAlpha, a.AvgAlpha = true, alphaSum/float64(alphaN)
	}
	return a
}

func makeSelectionBatchDiff(batch model.RecommendationBatch, left, right []selectionObs,
	leftSymbols, rightSymbols []string) SelectionBatchDiff {
	l, r := aggregateBatch(left), aggregateBatch(right)
	return SelectionBatchDiff{
		BatchID: batch.ID, SignalDate: batchSignalDate(batch), K: min(len(left), len(right)),
		LeftSymbols: append([]string{}, leftSymbols...), RightSymbols: append([]string{}, rightSymbols...),
		AvgNetDiffPct: round2(l.AvgNet - r.AvgNet), MedianNetDiffPct: round2(l.MedianNet - r.MedianNet),
		P10NetDiffPct:      round2(l.P10Net - r.P10Net),
		NetPositiveDiffPct: round2(l.NetPositive - r.NetPositive),
		SevereLossDiffPct:  round2(l.Severe - r.Severe),
		AvgMfeDiffPct:      round2(l.AvgMFE - r.AvgMFE), AvgMaeDiffPct: round2(l.AvgMAE - r.AvgMAE),
		HasAlpha: l.HasAlpha && r.HasAlpha, AvgAlphaDiffPct: round2(l.AvgAlpha - r.AvgAlpha),
	}
}

type selectionDiffGetter func(SelectionBatchDiff) (float64, bool)

func makeSelectionPair(pair, label, left, right string, diffs []SelectionBatchDiff,
	salt string) SelectionPairedRow {
	out := SelectionPairedRow{Pair: pair, Label: label, LeftGroup: left, RightGroup: right,
		Batches: len(diffs), BatchDiffs: append([]SelectionBatchDiff{}, diffs...)}
	for _, d := range diffs {
		switch {
		case d.AvgNetDiffPct > 0.004:
			out.LeftWins++
		case d.AvgNetDiffPct < -0.004:
			out.RightWins++
		default:
			out.Ties++
		}
	}
	out.AvgNetPct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.AvgNetDiffPct, true
	}, salt+":avg_net")
	out.MedianNetPct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.MedianNetDiffPct, true
	}, salt+":median_net")
	out.P10NetPct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.P10NetDiffPct, true
	}, salt+":p10_net")
	out.NetPositivePct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.NetPositiveDiffPct, true
	}, salt+":positive")
	out.AvgAlphaPct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.AvgAlphaDiffPct, d.HasAlpha
	}, salt+":alpha")
	out.SevereLossPct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.SevereLossDiffPct, true
	}, salt+":severe")
	out.AvgMfePct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.AvgMfeDiffPct, true
	}, salt+":mfe")
	out.AvgMaePct = bootstrapSelectionDiff(diffs, func(d SelectionBatchDiff) (float64, bool) {
		return d.AvgMaeDiffPct, true
	}, salt+":mae")
	return out
}

func bootstrapSelectionDiff(diffs []SelectionBatchDiff, getter selectionDiffGetter,
	salt string) SelectionBootstrapCI {
	values := make([]float64, 0, len(diffs))
	for _, d := range diffs {
		if v, ok := getter(d); ok {
			values = append(values, v)
		}
	}
	out := SelectionBootstrapCI{SampleBatches: len(values)}
	if len(values) == 0 {
		return out
	}
	var total float64
	for _, v := range values {
		total += v
	}
	out.Estimate = round2(total / float64(len(values)))
	if len(values) == 1 {
		out.Low95, out.High95 = round2(values[0]), round2(values[0])
		return out
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	rng := rand.New(rand.NewSource(selectionBootstrapSeed + int64(h.Sum64())))
	boot := make([]float64, selectionBootstrapIterations)
	for i := range boot {
		var sum float64
		for j := 0; j < len(values); j++ {
			sum += values[rng.Intn(len(values))]
		}
		boot[i] = sum / float64(len(values))
	}
	sort.Float64s(boot)
	out.Low95 = round2(percentileSorted(boot, 0.025))
	out.High95 = round2(percentileSorted(boot, 0.975))
	return out
}

func buildSelectionPlanPanel(recType string, horizon int, batches []selectionBatchFacts,
	outcomes map[string]model.RecommendationSelectionOutcome,
	labels map[selectionPlanLabelKey]selectionPlanLabelEntry) SelectionPlanPanel {
	panel := SelectionPlanPanel{}
	var fixedObs, planObs []selectionObs
	var diffs []SelectionBatchDiff
	selected := 0
	for _, bf := range batches {
		if bf.Batch.Type != recType || bf.Issue != "" || bf.Batch.Status != model.RecStatusSuccess || len(bf.Picks) == 0 {
			continue
		}
		panel.Coverage.CandidateBatches++
		selected += len(bf.Picks)
		symbols := recommendationSymbols(bf.Picks)
		reason := selectionOutcomeSetIssue(outcomes, bf.Batch.ID, symbols, horizon)
		if reason == "" {
			for _, symbol := range symbols {
				entry := labels[selectionPlanLabelKey{BatchID: bf.Batch.ID, Symbol: symbol, Horizon: horizon}]
				cur := planLabelIssue(entry)
				if outcomeIssuePriority(cur) > outcomeIssuePriority(reason) {
					reason = cur
				}
			}
		}
		if reason != "" {
			addSectionExclusion(&panel.Coverage, reason)
			continue
		}
		batchFixed := outcomesForSymbols(outcomes, bf.Batch.ID, symbols, horizon)
		batchPlan := make([]selectionObs, 0, len(symbols))
		for _, symbol := range symbols {
			label := labels[selectionPlanLabelKey{BatchID: bf.Batch.ID, Symbol: symbol, Horizon: horizon}].Label
			batchPlan = append(batchPlan, labelObs(label))
		}
		fixedObs = append(fixedObs, batchFixed...)
		planObs = append(planObs, batchPlan...)
		diffs = append(diffs, makeSelectionBatchDiff(bf.Batch, batchPlan, batchFixed, symbols, symbols))
		panel.Coverage.ComparableBatches++
	}
	if panel.Coverage.CandidateBatches > 0 {
		panel.Coverage.CoveragePct = round2(float64(panel.Coverage.ComparableBatches) /
			float64(panel.Coverage.CandidateBatches) * 100)
	}
	panel.FixedHold = makeSelectionMetric("ai_fixed_hold", "同一 AI picks · so1 fixed-hold", selected, fixedObs)
	panel.PlanL2 = makeSelectionMetric("ai_plan_l2", "同一 AI picks · l2 计划结算", selected, planObs)
	panel.Pair = makeSelectionPair("plan_l2_minus_fixed", "l2 计划 - so1 fixed-hold",
		"ai_plan_l2", "ai_fixed_hold", diffs, recType+":"+intKey(horizon)+":plan")
	panel.Notes = []string{
		"仅比较同一批、同一 AI picks、同一 horizon 且 l2/so1 都成熟非 forced 的交集；缺标签不补算",
		"l2 可按 AI TP/SL 提前退出，so1 固定持有且不读 TP/SL；本面板只衡量计划执行差异，不代表 selection 增量",
	}
	return panel
}

func outcomeIssuePriority(reason string) int {
	switch reason {
	case "missing":
		return 5
	case "no_data":
		return 4
	case "forced":
		return 3
	case "skipped":
		return 2
	case "pending":
		return 1
	default:
		return 0
	}
}

func planLabelIssue(entry selectionPlanLabelEntry) string {
	if entry.Count != 1 || entry.Label.LabelVersion != labelVersion {
		return "missing"
	}
	label := entry.Label
	switch {
	case label.MaturityStatus == model.LabelNoData:
		return "no_data"
	case label.Forced:
		return "forced"
	case label.MaturityStatus == model.LabelSkipped:
		return "skipped"
	case label.MaturityStatus != model.LabelMatured:
		return "pending"
	default:
		return ""
	}
}

func labelObs(label model.RecommendationLabel) selectionObs {
	return selectionObs{BatchID: label.BatchID, Symbol: label.Symbol,
		Gross: label.GrossReturnPct, Net: label.NetReturnPct,
		Alpha: label.AlphaPct, HasAlpha: label.HasBench, MFE: label.MfePct, MAE: label.MaePct}
}

func buildSelectionChallengerEvals(recType string, horizon int,
	batchFacts map[int64]selectionBatchFacts,
	outcomes map[string]model.RecommendationSelectionOutcome,
	runs []selectionChallengerRun) []SelectionChallengerEval {
	byExperiment := map[int64][]selectionChallengerRun{}
	for _, run := range runs {
		bf, ok := batchFacts[run.Run.BatchID]
		if run.Issue == "" && ok && bf.Batch.Type == recType {
			byExperiment[run.Run.ExperimentID] = append(byExperiment[run.Run.ExperimentID], run)
		}
	}
	var ids []int64
	for id := range byExperiment {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]SelectionChallengerEval, 0, len(ids))
	for _, expID := range ids {
		list := byExperiment[expID]
		ev := SelectionChallengerEval{ExperimentID: expID, Name: list[0].Name}
		if ev.Name == "" {
			ev.Name = "实验 #" + int64Key(expID)
		}
		var nativeObs, matchedObs, aiObs, quantObs []selectionObs
		var chalAIDiffs, chalQuantDiffs []SelectionBatchDiff
		selectedNative, selectedMatched, selectedAI, selectedQuant := 0, 0, 0, 0
		nativeKTotal := 0
		for _, run := range list {
			bf := batchFacts[run.Run.BatchID]
			ev.Coverage.Runs++
			kNative := len(run.Challenger)
			nativeKTotal += kNative
			if ev.Coverage.Runs == 1 || kNative < ev.Coverage.NativeKMin {
				ev.Coverage.NativeKMin = kNative
			}
			if kNative > ev.Coverage.NativeKMax {
				ev.Coverage.NativeKMax = kNative
			}
			if kNative == 0 {
				ev.Coverage.ZeroMatched++
				continue
			}
			chalSymbols := challengerSymbols(run.Challenger)
			selectedNative += len(chalSymbols)
			nativeReason := selectionOutcomeSetIssue(outcomes, bf.Batch.ID, chalSymbols, horizon)
			outcomeExcluded := nativeReason != ""
			if nativeReason == "" {
				nativeObs = append(nativeObs, outcomesForSymbols(outcomes, bf.Batch.ID, chalSymbols, horizon)...)
				ev.Coverage.NativeEligible++
			}
			if len(bf.Picks) == 0 {
				ev.Coverage.ZeroMatched++
				if outcomeExcluded {
					ev.Coverage.OutcomeExcluded++
				}
				continue
			}

			matchedK := min(len(bf.Picks), len(run.Challenger))
			if matchedK <= 0 {
				ev.Coverage.ZeroMatched++
				if outcomeExcluded {
					ev.Coverage.OutcomeExcluded++
				}
				continue
			}
			aiSymbols := recommendationSymbols(bf.Picks[:matchedK])
			quantSymbols := eventSymbols(quantTopN(bf.Opportunity, matchedK))
			matchedSymbols := challengerSymbols(run.Challenger[:matchedK])
			selectedMatched += matchedK
			selectedAI += matchedK
			selectedQuant += matchedK
			union := append(append(append([]string{}, matchedSymbols...), aiSymbols...), quantSymbols...)
			matchedReason := selectionOutcomeSetIssue(outcomes, bf.Batch.ID, union, horizon)
			if matchedReason != "" {
				outcomeExcluded = true
			} else {
				batchChal := outcomesForSymbols(outcomes, bf.Batch.ID, matchedSymbols, horizon)
				batchAI := outcomesForSymbols(outcomes, bf.Batch.ID, aiSymbols, horizon)
				batchQuant := outcomesForSymbols(outcomes, bf.Batch.ID, quantSymbols, horizon)
				matchedObs = append(matchedObs, batchChal...)
				aiObs = append(aiObs, batchAI...)
				quantObs = append(quantObs, batchQuant...)
				chalAIDiffs = append(chalAIDiffs, makeSelectionBatchDiff(bf.Batch,
					batchChal, batchAI, matchedSymbols, aiSymbols))
				chalQuantDiffs = append(chalQuantDiffs, makeSelectionBatchDiff(bf.Batch,
					batchChal, batchQuant, matchedSymbols, quantSymbols))
				ev.Coverage.MatchedEligible++
			}
			if outcomeExcluded {
				ev.Coverage.OutcomeExcluded++
			}
		}
		if ev.Coverage.Runs > 0 {
			ev.Coverage.NativeKAvg = round2(float64(nativeKTotal) / float64(ev.Coverage.Runs))
		}
		ev.Groups = []SelectionMetric{
			makeSelectionMetric("challenger_native", "Challenger 原生 K", selectedNative, nativeObs),
			makeSelectionMetric("challenger_matched", "Challenger matched-K", selectedMatched, matchedObs),
			makeSelectionMetric("ai_matched", "AI matched-K", selectedAI, aiObs),
			makeSelectionMetric("quant_matched", "Quant matched-K", selectedQuant, quantObs),
		}
		salt := recType + ":" + intKey(horizon) + ":exp:" + int64Key(expID)
		ev.Pairs = []SelectionPairedRow{
			makeSelectionPair("challenger_minus_ai", "Challenger - AI（matched-K）",
				"challenger_matched", "ai_matched", chalAIDiffs, salt+":ai"),
			makeSelectionPair("challenger_minus_quant", "Challenger - Quant（matched-K）",
				"challenger_matched", "quant_matched", chalQuantDiffs, salt+":quant"),
		}
		ev.Notes = []string{
			"原生 K 保留 challenger 实际选择性；matched-K=min(AI N, challenger K)，AI 按最终顺序、Quant 按 score_rank、challenger 按 ep1 order 截取",
			"不同 experiment_id 不混池；K=0 只计拒选，不产生收益样本",
		}
		out = append(out, ev)
	}
	return out
}

func challengerSymbols(facts []selectionChallengerFact) []string {
	out := make([]string, 0, len(facts))
	for _, fact := range facts {
		out = append(out, fact.Symbol)
	}
	return out
}

func buildSelectionSlices(diffs []SelectionBatchDiff, batches map[int64]selectionBatchFacts,
	recType string, horizon int) []SelectionSliceGroup {
	dims := []struct {
		dim, label string
		key        func(selectionBatchFacts) string
	}{
		{"ranking_version", "排名版本", func(b selectionBatchFacts) string { return b.RankingVersion }},
		{"strategy", "策略", func(b selectionBatchFacts) string { return b.Batch.Strategy }},
		{"regime", "市场状态", func(b selectionBatchFacts) string { return b.Batch.Regime }},
		{"provider_model", "Provider · Model", func(b selectionBatchFacts) string {
			return strings.TrimSpace(b.Batch.Provider + " · " + b.Batch.Model)
		}},
		{"prompt_version", "Prompt 版本", func(b selectionBatchFacts) string { return b.Batch.PromptVersion }},
	}
	groups := make([]SelectionSliceGroup, 0, len(dims))
	for _, dim := range dims {
		byKey := map[string][]SelectionBatchDiff{}
		for _, d := range diffs {
			bf, ok := batches[d.BatchID]
			if !ok {
				continue
			}
			key := strings.TrimSpace(dim.key(bf))
			if key == "" || key == "·" {
				key = "（未知）"
			}
			byKey[key] = append(byKey[key], d)
		}
		var keys []string
		for key := range byKey {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if len(byKey[keys[i]]) == len(byKey[keys[j]]) {
				return keys[i] < keys[j]
			}
			return len(byKey[keys[i]]) > len(byKey[keys[j]])
		})
		group := SelectionSliceGroup{Dim: dim.dim, Label: dim.label}
		for _, key := range keys {
			list := byKey[key]
			row := SelectionSliceRow{Key: key, Batches: len(list), Evaluated: len(list) >= selectionSliceMinBatches}
			if row.Evaluated {
				ci := bootstrapSelectionDiff(list, func(d SelectionBatchDiff) (float64, bool) {
					return d.AvgNetDiffPct, true
				}, recType+":"+intKey(horizon)+":slice:"+dim.dim+":"+key)
				row.AvgNetPct = &ci
			} else {
				row.Note = fmt.Sprintf("可比批次 %d < %d，统计不确定", len(list), selectionSliceMinBatches)
			}
			group.Rows = append(group.Rows, row)
		}
		groups = append(groups, group)
	}
	return groups
}
