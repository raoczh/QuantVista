package service

import (
	"errors"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

const candidateAuditReportMaxRuns = 30

type CandidateAuditRunView struct {
	ID               int64      `json:"id"`
	SignalDate       string     `json:"signal_date"`
	OutcomeDate      string     `json:"outcome_date"`
	Status           string     `json:"status"`
	AuditVersion     string     `json:"audit_version"`
	ParameterHash    string     `json:"parameter_hash"`
	DataAsOf         time.Time  `json:"data_as_of"`
	SignalDataAsOf   *time.Time `json:"signal_data_as_of,omitempty"`
	OpportunityCount int        `json:"opportunity_count"`
	BatchCount       int        `json:"batch_count"`
	ItemCount        int        `json:"item_count"`
	MissedCount      int        `json:"missed_count"`
	AdverseCount     int        `json:"adverse_count"`
	GapJSON          string     `json:"gap_json"`
}

type CandidateAuditMetric struct {
	Samples       int     `json:"samples"`
	OutcomeDays   int     `json:"outcome_days"`
	Evaluated     bool    `json:"evaluated"`
	AlphaSamples  int     `json:"alpha_samples"`
	HasAlpha      bool    `json:"has_alpha"`
	AvgNetPct     float64 `json:"avg_net_pct"`
	AvgAlphaPct   float64 `json:"avg_alpha_pct"`
	AvgMfePct     float64 `json:"avg_mfe_pct"`
	AvgMaePct     float64 `json:"avg_mae_pct"`
	NoData        int     `json:"no_data"`
	Forced        int     `json:"forced"`
	Pending       int     `json:"pending"`
	EarlyAdverse  int     `json:"early_adverse"`
	FalsePositive int     `json:"false_positive"`
	Note          string  `json:"note,omitempty"`
}

type CandidateAuditRateRow struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Total       int     `json:"total"`
	Missed      int     `json:"missed"`
	MissedPct   float64 `json:"missed_pct"`
	OutcomeDays int     `json:"outcome_days"`
	Evaluated   bool    `json:"evaluated"`
	Note        string  `json:"note,omitempty"`
}

type CandidateAuditFunnelRow struct {
	Stage      string `json:"stage"`
	ReasonCode string `json:"reason_code"`
	Count      int    `json:"count"`
}

type CandidateAuditSliceRow struct {
	Dimension   string  `json:"dimension"`
	Key         string  `json:"key"`
	Samples     int     `json:"samples"`
	OutcomeDays int     `json:"outcome_days"`
	Missed      int     `json:"missed"`
	Adverse     int     `json:"adverse"`
	AvgNetPct   float64 `json:"avg_net_pct"`
	Evaluated   bool    `json:"evaluated"`
	Note        string  `json:"note,omitempty"`
}

type CandidateAuditDetailView struct {
	ID              int64   `json:"id"`
	BatchID         int64   `json:"batch_id"`
	Symbol          string  `json:"symbol"`
	Name            string  `json:"name"`
	SignalDate      string  `json:"signal_date"`
	OutcomeDate     string  `json:"outcome_date"`
	AuditType       string  `json:"audit_type"`
	ConclusionCode  string  `json:"conclusion_code"`
	RecType         string  `json:"rec_type"`
	FunnelStage     string  `json:"funnel_stage"`
	PrimaryReason   string  `json:"primary_reason_code"`
	Source          string  `json:"source"`
	SourceSet       string  `json:"source_set"`
	Channel         string  `json:"channel"`
	ChannelSet      string  `json:"channel_set"`
	OpportunityRank int     `json:"opportunity_rank"`
	ScoreRank       int     `json:"score_rank"`
	LLMInputOrder   int     `json:"llm_input_order"`
	OutcomeStatus   string  `json:"outcome_status"`
	NetReturnPct    float64 `json:"net_return_pct"`
	AlphaPct        float64 `json:"alpha_pct"`
	HasBench        bool    `json:"has_bench"`
	MfePct          float64 `json:"mfe_pct"`
	MaePct          float64 `json:"mae_pct"`
	ReasonFactsJSON string  `json:"reason_facts_json"`
	InputRefsJSON   string  `json:"input_refs_json"`
	OutcomeRefsJSON string  `json:"outcome_refs_json"`
}

type CandidateAuditUserReport struct {
	GeneratedAt    string                     `json:"generated_at"`
	AuditVersion   string                     `json:"audit_version"`
	OutcomeVersion string                     `json:"outcome_version"`
	ParameterHash  string                     `json:"parameter_hash"`
	Parameters     candidateAuditParameters   `json:"parameters"`
	Runs           []CandidateAuditRunView    `json:"runs"`
	Outcome        CandidateAuditMetric       `json:"outcome"`
	Funnel         []CandidateAuditFunnelRow  `json:"funnel"`
	SourceRecall   []CandidateAuditRateRow    `json:"source_recall"`
	ChannelRecall  []CandidateAuditRateRow    `json:"channel_recall"`
	Slices         []CandidateAuditSliceRow   `json:"slices"`
	Items          []CandidateAuditDetailView `json:"items"`
	Notes          []string                   `json:"notes"`
}

type CandidateAuditAdminReport struct {
	GeneratedAt    string                    `json:"generated_at"`
	AuditVersion   string                    `json:"audit_version"`
	OutcomeVersion string                    `json:"outcome_version"`
	Runs           int                       `json:"runs"`
	OutcomeDays    int                       `json:"outcome_days"`
	Users          int                       `json:"users"`
	Batches        int                       `json:"batches"`
	Outcome        CandidateAuditMetric      `json:"outcome"`
	LLMRejected    CandidateAuditMetric      `json:"llm_rejected"`
	SourceRecall   []CandidateAuditRateRow   `json:"source_recall"`
	ChannelRecall  []CandidateAuditRateRow   `json:"channel_recall"`
	Funnel         []CandidateAuditFunnelRow `json:"funnel"`
	Slices         []CandidateAuditSliceRow  `json:"slices"`
	Notes          []string                  `json:"notes"`
}

func loadCandidateAuditFacts(userID int64, recType string, maxRuns int) ([]model.CandidateAuditRun, []model.CandidateAuditItem, error) {
	if common.DB == nil {
		return nil, nil, errors.New("数据库不可用")
	}
	if maxRuns <= 0 || maxRuns > candidateAuditReportMaxRuns {
		maxRuns = candidateAuditReportMaxRuns
	}
	var runs []model.CandidateAuditRun
	if err := common.DB.Where("owner_type = ? AND audit_version = ? AND parameter_hash = ? AND status IN ?",
		model.JobOwnerSystem, CandidateAuditVersion, candidateAuditParameterHash(),
		[]string{model.CandidateAuditStatusSuccess, model.CandidateAuditStatusPartial}).
		Order("outcome_date DESC, id DESC").Limit(maxRuns).Find(&runs).Error; err != nil {
		return nil, nil, err
	}
	if len(runs) == 0 {
		return runs, []model.CandidateAuditItem{}, nil
	}
	runIDs := make([]int64, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
	}
	q := common.DB.Where("run_id IN ?", runIDs)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	if recType != "" {
		q = q.Where("rec_type = ?", recType)
	}
	var items []model.CandidateAuditItem
	if err := q.Order("outcome_date DESC, batch_id DESC, opportunity_rank, symbol").Find(&items).Error; err != nil {
		return nil, nil, err
	}
	return runs, items, nil
}

func LoadCandidateAuditUserReport(userID int64, recType string, maxRuns int) (*CandidateAuditUserReport, error) {
	if userID <= 0 {
		return nil, errors.New("用户身份无效")
	}
	runs, items, err := loadCandidateAuditFacts(userID, recType, maxRuns)
	if err != nil {
		return nil, err
	}
	rep := &CandidateAuditUserReport{
		GeneratedAt: time.Now().Format(time.RFC3339), AuditVersion: CandidateAuditVersion,
		OutcomeVersion: candidateAuditOutcomeVersion, ParameterHash: candidateAuditParameterHash(),
		Parameters: candidateAuditParams(), Notes: candidateAuditReportNotes(),
	}
	populateCandidateAuditReport(runs, items, &rep.Outcome, &rep.Funnel, &rep.SourceRecall,
		&rep.ChannelRecall, &rep.Slices)
	rep.Runs = candidateAuditUserRunViews(runs, items)
	for _, item := range items {
		if item.AuditType != model.CandidateAuditTypeMissedLeader && item.AuditType != model.CandidateAuditTypeFalsePositive {
			continue
		}
		rep.Items = append(rep.Items, candidateAuditDetail(item))
	}
	return rep, nil
}

func LoadCandidateAuditAdminReport(maxRuns int) (*CandidateAuditAdminReport, error) {
	runs, items, err := loadCandidateAuditFacts(0, "", maxRuns)
	if err != nil {
		return nil, err
	}
	rep := &CandidateAuditAdminReport{GeneratedAt: time.Now().Format(time.RFC3339),
		AuditVersion: CandidateAuditVersion, OutcomeVersion: candidateAuditOutcomeVersion,
		Runs: len(runs), Notes: candidateAuditReportNotes()}
	populateCandidateAuditReport(runs, items, &rep.Outcome, &rep.Funnel, &rep.SourceRecall,
		&rep.ChannelRecall, &rep.Slices)
	users, batches, days := map[int64]bool{}, map[int64]bool{}, map[string]bool{}
	var rejected []model.CandidateAuditItem
	for _, item := range items {
		users[item.UserID], batches[item.BatchID], days[item.OutcomeDate] = true, true, true
		if item.FunnelStage == model.CandStageLLMList {
			rejected = append(rejected, item)
		}
	}
	rep.Users, rep.Batches, rep.OutcomeDays = len(users), len(batches), len(days)
	rep.LLMRejected = candidateAuditMetric(rejected)
	rep.Notes = append(rep.Notes, "管理员报表仅返回跨用户聚合，不返回 user_id、batch_id、symbol 或筛选偏好")
	return rep, nil
}

func populateCandidateAuditReport(runs []model.CandidateAuditRun, items []model.CandidateAuditItem,
	metric *CandidateAuditMetric, funnel *[]CandidateAuditFunnelRow, sources, channels *[]CandidateAuditRateRow,
	slices *[]CandidateAuditSliceRow) {
	*metric = candidateAuditMetric(items)
	*funnel = candidateAuditFunnel(items)
	*sources = candidateAuditRates(items, true)
	*channels = candidateAuditRates(items, false)
	*slices = candidateAuditSlices(items)
}

func candidateAuditMetric(items []model.CandidateAuditItem) CandidateAuditMetric {
	metric := CandidateAuditMetric{}
	days := map[string]bool{}
	var net, alpha, mfe, mae float64
	alphaN := 0
	for _, item := range items {
		if item.SelectionMaturityStatus == model.LabelPending {
			metric.Pending++
		}
		if item.Forced {
			metric.Forced++
			continue
		}
		if item.OutcomeStatus != btObserved {
			metric.NoData++
			continue
		}
		metric.Samples++
		days[item.OutcomeDate] = true
		net, mfe, mae = net+item.NetReturnPct, mfe+item.MfePct, mae+item.MaePct
		if item.HasBench {
			alpha += item.AlphaPct
			alphaN++
		}
		if item.ConclusionCode == "early_adverse_observation" {
			metric.EarlyAdverse++
		}
		if item.ConclusionCode == "false_positive_next_day" {
			metric.FalsePositive++
		}
	}
	metric.OutcomeDays = len(days)
	if metric.Samples > 0 {
		metric.AvgNetPct, metric.AvgMfePct, metric.AvgMaePct = round2(net/float64(metric.Samples)),
			round2(mfe/float64(metric.Samples)), round2(mae/float64(metric.Samples))
	}
	if alphaN > 0 {
		metric.AvgAlphaPct = round2(alpha / float64(alphaN))
	}
	metric.AlphaSamples, metric.HasAlpha = alphaN, alphaN > 0
	metric.Evaluated = metric.OutcomeDays >= candidateAuditMinDays && metric.Samples >= candidateAuditMinSamples
	if !metric.Evaluated {
		metric.Note = "未评估：至少需要 10 个结果日且 30 条可执行观测"
	}
	return metric
}

func candidateAuditFunnel(items []model.CandidateAuditItem) []CandidateAuditFunnelRow {
	counts := map[string]int{}
	for _, item := range items {
		if item.AuditType == model.CandidateAuditTypeMissedLeader {
			counts[item.FunnelStage+"\x00"+item.PrimaryReasonCode]++
		}
	}
	rows := make([]CandidateAuditFunnelRow, 0, len(counts))
	for key, count := range counts {
		parts := strings.SplitN(key, "\x00", 2)
		rows = append(rows, CandidateAuditFunnelRow{Stage: parts[0], ReasonCode: parts[1], Count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Stage+rows[i].ReasonCode < rows[j].Stage+rows[j].ReasonCode
	})
	return rows
}

func candidateAuditRates(items []model.CandidateAuditItem, bySource bool) []CandidateAuditRateRow {
	type bucket struct {
		total, missed int
		days          map[string]bool
	}
	buckets := map[string]*bucket{}
	for _, item := range items {
		if item.AuditType != model.CandidateAuditTypeMissedLeader && item.AuditType != model.CandidateAuditTypeLeaderHit {
			continue
		}
		values := auditAttributionValues(item, bySource)
		for _, value := range values {
			b := buckets[value]
			if b == nil {
				b = &bucket{days: map[string]bool{}}
				buckets[value] = b
			}
			b.total++
			b.days[item.OutcomeDate] = true
			if item.AuditType == model.CandidateAuditTypeMissedLeader {
				b.missed++
			}
		}
	}
	rows := make([]CandidateAuditRateRow, 0, len(buckets))
	for key, b := range buckets {
		row := CandidateAuditRateRow{Key: key, Label: candidateAuditBucketLabel(key), Total: b.total,
			Missed: b.missed, OutcomeDays: len(b.days)}
		if b.total > 0 {
			row.MissedPct = round2(float64(b.missed) / float64(b.total) * 100)
		}
		row.Evaluated = row.OutcomeDays >= candidateAuditMinDays && row.Total >= candidateAuditMinSamples
		if !row.Evaluated {
			row.Note = "未评估"
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

func auditAttributionValues(item model.CandidateAuditItem, bySource bool) []string {
	raw := item.ChannelSet
	if raw == "" {
		raw = item.Channel
	}
	if bySource {
		raw = item.SourceSet
		if raw == "" {
			raw = item.Source
		}
	}
	values := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		fallback := "channel_unknown"
		if bySource {
			fallback = "source_unknown"
			if item.CandidateEventID == 0 {
				if item.DiscoveryItemID > 0 {
					fallback = "discovered_not_in_user_pool"
				} else {
					fallback = "not_discovered_marketwide"
				}
			}
		} else if item.DiscoveryItemID == 0 {
			fallback = "not_discovered_marketwide"
		}
		values = append(values, fallback)
	}
	sort.Strings(values)
	return uniqueAuditStrings(values)
}

func candidateAuditBucketLabel(key string) string {
	switch key {
	case "not_discovered_marketwide":
		return "未被全市场发现"
	case "discovered_not_in_user_pool":
		return "已发现但未进用户池"
	case "source_unknown":
		return "来源事实缺失"
	case "channel_unknown":
		return "发现通道事实缺失"
	default:
		return sourceLabelOrKey(key)
	}
}

func candidateAuditSlices(items []model.CandidateAuditItem) []CandidateAuditSliceRow {
	type acc struct {
		items []model.CandidateAuditItem
	}
	groups := map[string]*acc{}
	for _, item := range items {
		values := map[string]string{"recommendation_type": item.RecType, "market_regime": item.Regime,
			"discovery_version": item.DiscoveryVersion, "ranking_version": item.RankingVersion,
			"holding_period": candidateAuditIntKey(item.HoldingPeriodDays)}
		for dim, key := range values {
			if key == "" {
				key = "unknown"
			}
			groupKey := dim + "\x00" + key
			if groups[groupKey] == nil {
				groups[groupKey] = &acc{}
			}
			groups[groupKey].items = append(groups[groupKey].items, item)
		}
	}
	rows := make([]CandidateAuditSliceRow, 0, len(groups))
	for groupKey, group := range groups {
		parts := strings.SplitN(groupKey, "\x00", 2)
		metric := candidateAuditMetric(group.items)
		row := CandidateAuditSliceRow{Dimension: parts[0], Key: parts[1], Samples: metric.Samples,
			OutcomeDays: metric.OutcomeDays, AvgNetPct: metric.AvgNetPct, Evaluated: metric.Evaluated, Note: metric.Note}
		for _, item := range group.items {
			if item.AuditType == model.CandidateAuditTypeMissedLeader {
				row.Missed++
			}
			if item.AuditType == model.CandidateAuditTypeFalsePositive {
				row.Adverse++
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Dimension+"\x00"+rows[i].Key < rows[j].Dimension+"\x00"+rows[j].Key
	})
	return rows
}

func candidateAuditUserRunViews(runs []model.CandidateAuditRun, items []model.CandidateAuditItem) []CandidateAuditRunView {
	byRun := map[int64][]model.CandidateAuditItem{}
	for _, item := range items {
		byRun[item.RunID] = append(byRun[item.RunID], item)
	}
	views := make([]CandidateAuditRunView, 0, len(runs))
	for _, run := range runs {
		rows := byRun[run.ID]
		if len(rows) == 0 {
			continue
		}
		batches, missed, adverse := map[int64]bool{}, 0, 0
		for _, item := range rows {
			batches[item.BatchID] = true
			if item.AuditType == model.CandidateAuditTypeMissedLeader {
				missed++
			}
			if item.AuditType == model.CandidateAuditTypeFalsePositive {
				adverse++
			}
		}
		views = append(views, CandidateAuditRunView{ID: run.ID, SignalDate: run.SignalDate,
			OutcomeDate: run.OutcomeDate, Status: run.Status, AuditVersion: run.AuditVersion,
			ParameterHash: run.ParameterHash, DataAsOf: run.DataAsOf, SignalDataAsOf: run.SignalDataAsOf,
			OpportunityCount: run.OpportunityCount, BatchCount: len(batches), ItemCount: len(rows),
			MissedCount: missed, AdverseCount: adverse, GapJSON: run.GapJSON})
	}
	return views
}

func candidateAuditDetail(item model.CandidateAuditItem) CandidateAuditDetailView {
	return CandidateAuditDetailView{ID: item.ID, BatchID: item.BatchID, Symbol: item.Symbol, Name: item.Name,
		SignalDate: item.SignalDate, OutcomeDate: item.OutcomeDate, AuditType: item.AuditType,
		ConclusionCode: item.ConclusionCode, RecType: item.RecType, FunnelStage: item.FunnelStage,
		PrimaryReason: item.PrimaryReasonCode, Source: item.Source, SourceSet: item.SourceSet,
		Channel: item.Channel, ChannelSet: item.ChannelSet, OpportunityRank: item.OpportunityRank,
		ScoreRank: item.ScoreRank, LLMInputOrder: item.LLMInputOrder, OutcomeStatus: item.OutcomeStatus,
		NetReturnPct: item.NetReturnPct, AlphaPct: item.AlphaPct, HasBench: item.HasBench,
		MfePct: item.MfePct, MaePct: item.MaePct, ReasonFactsJSON: item.ReasonFactsJSON,
		InputRefsJSON: item.InputRefsJSON, OutcomeRefsJSON: item.OutcomeRefsJSON}
}

func candidateAuditReportNotes() []string {
	return []string{
		"机会集仅使用信号日历史宇宙、因子与日线；结果日数据只用于次日可执行观测和收益标签",
		"次日收益使用统一执行模拟器的次日开盘成交、扣费净收益；一字涨停、停牌、ST 与低流动性标的排除",
		"每条漏斗记录只有一个主原因；历史事实缺失或冲突时固定为 unknown，不调用 LLM 重释历史",
		"来源与通道为多值归因，同一标的可进入多个来源分母；小样本一律显示未评估",
		"本报表只测量和归因，不修改发现阈值、量化权重、prompt、模型路由或 AI 权限",
	}
}

func candidateAuditIntKey(value int) string {
	if value == 0 {
		return "unknown"
	}
	return int64Key(int64(value))
}
