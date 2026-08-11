package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	CandidateAuditVersion        = "cma1"
	candidateAuditOutcomeVersion = "ndo1"
	candidateAuditTimeout        = 8 * time.Minute
	candidateAuditPerCap         = 2e4
	candidateAuditTopK           = 20
	candidateAuditMinAmount      = 3e7
	candidateAuditLeaderNetPct   = 2.0
	candidateAuditLeaderAlphaPct = 0.0
	candidateAuditBadNetPct      = -3.0
	candidateAuditBadAlphaPct    = -2.0
	candidateAuditBadMaePct      = -5.0
	candidateAuditMinDays        = 10
	candidateAuditMinSamples     = 30
)

type candidateAuditParameters struct {
	AuditVersion           string  `json:"audit_version"`
	OutcomeVersion         string  `json:"outcome_version"`
	DiscoveryVersion       string  `json:"discovery_version"`
	DiscoveryParameterHash string  `json:"discovery_parameter_hash"`
	FactorVersion          string  `json:"factor_version"`
	SelectionVersion       string  `json:"selection_outcome_version"`
	LabelVersion           string  `json:"label_version"`
	OpportunityTopK        int     `json:"opportunity_top_k"`
	MinimumAmount          float64 `json:"minimum_amount"`
	LeaderNetPct           float64 `json:"leader_net_pct"`
	LeaderAlphaPct         float64 `json:"leader_alpha_pct"`
	FalsePositiveNetPct    float64 `json:"false_positive_net_pct"`
	FalsePositiveAlphaPct  float64 `json:"false_positive_alpha_pct"`
	FalsePositiveMaePct    float64 `json:"false_positive_mae_pct"`
	MinimumDays            int     `json:"minimum_days"`
	MinimumSamples         int     `json:"minimum_samples"`
}

func candidateAuditParams() candidateAuditParameters {
	return candidateAuditParameters{
		AuditVersion: CandidateAuditVersion, OutcomeVersion: candidateAuditOutcomeVersion,
		DiscoveryVersion: DiscoveryVersion, DiscoveryParameterHash: discoveryParameterHash(),
		FactorVersion:    factorSnapshotVersion,
		SelectionVersion: model.SelectionOutcomeVersion, LabelVersion: labelVersion,
		OpportunityTopK: candidateAuditTopK, MinimumAmount: candidateAuditMinAmount,
		LeaderNetPct: candidateAuditLeaderNetPct, LeaderAlphaPct: candidateAuditLeaderAlphaPct,
		FalsePositiveNetPct: candidateAuditBadNetPct, FalsePositiveAlphaPct: candidateAuditBadAlphaPct,
		FalsePositiveMaePct: candidateAuditBadMaePct, MinimumDays: candidateAuditMinDays,
		MinimumSamples: candidateAuditMinSamples,
	}
}

func candidateAuditParameterJSON() string {
	b, _ := json.Marshal(candidateAuditParams())
	return string(b)
}

func candidateAuditParameterHash() string {
	sum := sha256.Sum256([]byte(candidateAuditParameterJSON()))
	return hex.EncodeToString(sum[:])
}

type candidateAuditJobRequest struct {
	Version       int    `json:"version"`
	Market        string `json:"market"`
	SignalDate    string `json:"signal_date"`
	OutcomeDate   string `json:"outcome_date"`
	TriggerSource string `json:"trigger_source"`
	ParameterHash string `json:"parameter_hash"`
}

type auditSymbolObservation struct {
	Status            string
	SignalUniverseID  int64
	OutcomeUniverseID int64
	FactorSnapshotID  int64
	FactorReady       bool
	EntryDate         string
	EntryPrice        float64
	ClosePrice        float64
	GrossPct          float64
	NetPct            float64
	BenchPct          float64
	AlphaPct          float64
	HasBench          bool
	MFE               float64
	MAE               float64
	Name              string
}

type auditDiscoveryFact struct {
	Primary  model.CandidateDiscoveryItem
	Channels []string
}

type auditOpportunity struct {
	Symbol string
	Obs    auditSymbolObservation
	Rank   int
}

var candidateAuditRunMu sync.Mutex
var candidateAuditScheduleMu sync.Mutex
var candidateAuditSchedulePending = map[string]bool{}

func candidateAuditAdjacentSignalDate(market, outcomeDate string) (string, error) {
	if common.DB == nil {
		return "", errors.New("数据库不可用")
	}
	var current model.TradingCalendar
	if err := common.DB.Where("market = ? AND trade_date = ?", market, outcomeDate).First(&current).Error; err != nil {
		return "", fmt.Errorf("结果日交易日历缺失: %w", err)
	}
	if !current.IsOpen {
		return "", errors.New("结果日不是有效交易日")
	}
	var previous model.TradingCalendar
	if err := common.DB.Where("market = ? AND is_open = ? AND trade_date < ?", market, true, outcomeDate).
		Order("trade_date DESC").First(&previous).Error; err != nil {
		return "", fmt.Errorf("上一有效交易日不可用: %w", err)
	}
	return previous.TradeDate, nil
}

func auditDateStart(date string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", date, time.Local)
}

func candidateAuditDiscoveryRun(date string) (model.CandidateDiscoveryRun, error) {
	var run model.CandidateDiscoveryRun
	err := common.DB.Where("market = ? AND trade_date = ? AND owner_type = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ? AND status IN ?",
		"cn", date, model.JobOwnerSystem, DiscoveryVersion, factorSnapshotVersion, discoveryParameterHash(),
		[]string{DiscoveryRunStatusOK, DiscoveryRunStatusPart}).Order("id DESC").First(&run).Error
	return run, err
}

func auditMarshal(value any) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func auditMapIncrement(values map[string]int, key string) {
	if key == "" {
		key = "unknown"
	}
	values[key]++
}

func markCandidateAuditFailed(runID int64, err error) {
	if common.DB == nil || runID <= 0 || err == nil {
		return
	}
	now := time.Now()
	_ = common.DB.Model(&model.CandidateAuditRun{}).Where("id = ?", runID).Updates(map[string]any{
		"status": model.CandidateAuditStatusFailed, "error": truncate(err.Error(), 512),
		"finished_at": now, "updated_at": now,
	}).Error
}

func finishCandidateAuditWithoutItems(run *model.CandidateAuditRun, status string, gaps map[string]int) error {
	now := time.Now()
	run.Status, run.GapJSON, run.FinishedAt = status, auditMarshal(gaps), &now
	return common.DB.Model(&model.CandidateAuditRun{}).Where("id = ?", run.ID).Updates(map[string]any{
		"status": status, "gap_json": run.GapJSON, "coverage_json": auditMarshal(map[string]int{}),
		"data_as_of": run.DataAsOf,
		"item_count": 0, "missed_count": 0, "false_positive_count": 0,
		"error": "", "finished_at": now, "updated_at": now,
	}).Error
}

func auditBenchmarkObservation(ctx context.Context, market *MarketService, outcomeDate string) (float64, bool) {
	if market == nil {
		return 0, false
	}
	for _, bar := range NewBacktestService(market).fetchBench(ctx) {
		if bar.TradeDate == outcomeDate && bar.Open > 0 && bar.Close > 0 {
			return round2((bar.Close - bar.Open) / bar.Open * 100), true
		}
	}
	return 0, false
}

func loadAuditObservations(ctx context.Context, market *MarketService, signalDate, outcomeDate string,
	factorVersion string, coverage, gaps map[string]int) (map[string]auditSymbolObservation, error) {
	var signalUniverse, outcomeUniverse []model.StockUniverseDaily
	if err := common.DB.Where("market = ? AND trade_date = ?", "cn", signalDate).Find(&signalUniverse).Error; err != nil {
		return nil, err
	}
	if err := common.DB.Where("market = ? AND trade_date = ?", "cn", outcomeDate).Find(&outcomeUniverse).Error; err != nil {
		return nil, err
	}
	coverage["signal_universe"] = len(signalUniverse)
	coverage["outcome_universe"] = len(outcomeUniverse)
	if len(signalUniverse) == 0 {
		gaps["signal_universe_missing"]++
	}
	if len(outcomeUniverse) == 0 {
		gaps["outcome_universe_missing"]++
	}
	outcomeBy := make(map[string]model.StockUniverseDaily, len(outcomeUniverse))
	for _, row := range outcomeUniverse {
		outcomeBy[row.Symbol] = row
	}
	var factors []model.FactorSnapshotDaily
	if err := common.DB.Where("market = ? AND trade_date = ? AND factor_version = ?", "cn", signalDate, factorVersion).
		Find(&factors).Error; err != nil {
		return nil, err
	}
	factorBy := make(map[string]model.FactorSnapshotDaily, len(factors))
	for _, row := range factors {
		factorBy[row.Symbol] = row
	}
	coverage["factor_snapshots"] = len(factors)

	var barRows []model.DailyBar
	if err := common.DB.Where("market = ? AND trade_date IN ?", "cn", []string{signalDate, outcomeDate}).
		Order("symbol, trade_date").Find(&barRows).Error; err != nil {
		return nil, err
	}
	barsBy := map[string]map[string]datasource.Bar{}
	for _, row := range barRows {
		if barsBy[row.Symbol] == nil {
			barsBy[row.Symbol] = map[string]datasource.Bar{}
		}
		barsBy[row.Symbol][row.TradeDate] = datasource.Bar{TradeDate: row.TradeDate, Open: row.Open,
			High: row.High, Low: row.Low, Close: row.Close, Volume: row.Volume, Amount: row.Amount,
			TurnoverRate: row.TurnoverRate, Source: row.Source}
	}
	benchPct, hasBench := auditBenchmarkObservation(ctx, market, outcomeDate)
	if !hasBench {
		gaps["benchmark_missing"]++
	}

	out := make(map[string]auditSymbolObservation, len(signalUniverse))
	for _, signal := range signalUniverse {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		obs := auditSymbolObservation{Status: "no_data", SignalUniverseID: signal.ID, Name: signal.Name}
		outcome, outcomeOK := outcomeBy[signal.Symbol]
		if outcomeOK {
			obs.OutcomeUniverseID = outcome.ID
		}
		factor, factorOK := factorBy[signal.Symbol]
		if factorOK {
			obs.FactorSnapshotID = factor.ID
		}
		obs.FactorReady = factorOK && factor.LastBarDate == signalDate
		switch {
		case signal.IsST || (outcomeOK && outcome.IsST):
			obs.Status = "excluded_st"
			coverage["excluded_st"]++
		case signal.Suspended || (outcomeOK && outcome.Suspended):
			obs.Status = "excluded_suspended"
			coverage["excluded_suspended"]++
		case signal.Amount < candidateAuditMinAmount || (outcomeOK && outcome.Amount < candidateAuditMinAmount):
			obs.Status = "excluded_liquidity"
			coverage["excluded_liquidity"]++
		case !outcomeOK:
			obs.Status = "no_data"
			coverage["excluded_no_data"]++
		default:
			// 因子缺口本身正是“为什么没有被全市场发现”的可审计原因，不能把这类
			// 后来走强的股票从机会集分母里删掉，否则会静默抬高召回率。只要两日
			// 宇宙和行情足够完成可执行观测，就保留机会并把运行标为 partial。
			if !obs.FactorReady {
				coverage["factor_snapshot_missing"]++
				gaps["factor_snapshot_missing"]++
			}
			sigBar, sigOK := barsBy[signal.Symbol][signalDate]
			outBar, outOK := barsBy[signal.Symbol][outcomeDate]
			if !sigOK || !outOK {
				obs.Status = "no_data"
				coverage["excluded_no_data"]++
				break
			}
			result := simulateNextDayObservation([]datasource.Bar{sigBar, outBar}, 0,
				signal.Symbol, signal.Name, candidateAuditPerCap, outcomeDate)
			obs.Status, obs.EntryDate, obs.EntryPrice, obs.ClosePrice = result.Status, result.EntryDate,
				result.EntryPrice, result.ClosePrice
			obs.GrossPct, obs.NetPct, obs.MFE, obs.MAE = result.GrossPct, result.NetPct, result.MfePct, result.MaePct
			if result.Status == btObserved {
				obs.HasBench, obs.BenchPct = hasBench, benchPct
				if hasBench {
					obs.AlphaPct = round2(obs.NetPct - benchPct)
				}
				coverage["executable"]++
			} else if result.Status == btSkipLimitUp || result.Status == btSkipSuspend || result.Status == btSkipCash {
				coverage["excluded_unexecutable"]++
			} else {
				coverage["excluded_no_data"]++
			}
		}
		out[signal.Symbol] = obs
	}
	return out, nil
}

func buildAuditOpportunities(observations map[string]auditSymbolObservation) []auditOpportunity {
	rows := make([]auditOpportunity, 0)
	for symbol, obs := range observations {
		if obs.Status != btObserved || obs.NetPct < candidateAuditLeaderNetPct ||
			(obs.HasBench && obs.AlphaPct < candidateAuditLeaderAlphaPct) {
			continue
		}
		rows = append(rows, auditOpportunity{Symbol: symbol, Obs: obs})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Obs.NetPct != rows[j].Obs.NetPct {
			return rows[i].Obs.NetPct > rows[j].Obs.NetPct
		}
		if rows[i].Obs.HasBench && rows[j].Obs.HasBench && rows[i].Obs.AlphaPct != rows[j].Obs.AlphaPct {
			return rows[i].Obs.AlphaPct > rows[j].Obs.AlphaPct
		}
		return rows[i].Symbol < rows[j].Symbol
	})
	if len(rows) > candidateAuditTopK {
		rows = rows[:candidateAuditTopK]
	}
	for i := range rows {
		rows[i].Rank = i + 1
	}
	return rows
}

func loadAuditDiscoveryFacts(signalRun model.CandidateDiscoveryRun) (map[string]auditDiscoveryFact, error) {
	// 口径对齐：推荐建池消费的是「近 5 个有效交易日的发现记忆」（recentDiscoveryCandidates），
	// 审计判 discovered 若只看信号日单日事实，会把“3 天前被发现、信号日未复现且未进
	// 用户池”的漏选错归因为 not_discovered_marketwide，低估发现层召回。这里按同样的
	// 5 日窗口取事实（全部早于等于信号日，PIT 合规）；Primary 取最新一日的最优行。
	dates := recentOpenDates(signalRun.Market, signalRun.TradeDate, 5)
	if len(dates) == 0 || dates[len(dates)-1] != signalRun.TradeDate {
		dates = append(dates, signalRun.TradeDate)
	}
	if len(dates) > 5 {
		dates = dates[len(dates)-5:]
	}
	var runs []model.CandidateDiscoveryRun
	if err := common.DB.Where("market = ? AND trade_date IN ? AND owner_type = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ? AND status IN ?",
		signalRun.Market, dates, model.JobOwnerSystem, signalRun.DiscoveryVersion, signalRun.FactorVersion, signalRun.ParameterHash,
		[]string{DiscoveryRunStatusOK, DiscoveryRunStatusPart}).Find(&runs).Error; err != nil {
		return nil, err
	}
	runIDs := make([]int64, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
	}
	if len(runIDs) == 0 {
		return map[string]auditDiscoveryFact{}, nil
	}
	var rows []model.CandidateDiscoveryItem
	// rank 是 MySQL 8.0.2+ 保留字，裸 ORDER BY rank 生产必挂（SQLite 单测不覆盖），
	// 必须走 OrderByColumn 由方言加引号——同 mood.go 人气榜先例。
	if err := common.DB.Where("run_id IN ?", runIDs).Order("trade_date DESC").
		Order(clause.OrderByColumn{Column: clause.Column{Name: "rank"}}).
		Order("channel, symbol").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]auditDiscoveryFact{}
	for _, row := range rows {
		fact := out[row.Symbol]
		if fact.Primary.ID == 0 {
			fact.Primary = row
		}
		fact.Channels = append(fact.Channels, row.Channel)
		out[row.Symbol] = fact
	}
	for symbol, fact := range out {
		sort.Strings(fact.Channels)
		fact.Channels = uniqueAuditStrings(fact.Channels)
		out[symbol] = fact
	}
	return out, nil
}

func uniqueAuditStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func candidateAuditReason(stage string, ev *model.RecommendationCandidateEvent, discovered bool,
	factsReady, ambiguous bool) (string, string) {
	if !factsReady {
		return "unknown", "candidate_facts_missing"
	}
	if ambiguous {
		return "unknown", "candidate_facts_ambiguous"
	}
	if ev == nil {
		if discovered {
			return "absent", "discovered_not_in_user_pool"
		}
		return "absent", "not_discovered_marketwide"
	}
	stage = ev.CandidateStage
	switch stage {
	case model.CandStageFiltered:
		if ev.GateType == model.GateQuoteStale {
			return stage, "quote_stale"
		}
		reason := ev.RejectionReason
		for _, rule := range []struct{ text, code string }{
			{"黑名单", "user_blacklist"}, {"股价", "user_price_filter"},
			{"市值", "user_market_cap_filter"}, {"换手", "user_turnover_filter"},
			{"成交额", "liquidity_filter"}, {"流动性", "liquidity_filter"},
			{"ST", "st_or_delisted_filter"}, {"退市", "st_or_delisted_filter"},
			{"停牌", "suspended_filter"}, {"涨停", "unexecutable_filter"},
			{"不可交易", "unexecutable_filter"},
		} {
			if strings.Contains(reason, rule.text) {
				return stage, rule.code
			}
		}
		if strings.TrimSpace(reason) != "" {
			return stage, "filtered_recorded_other"
		}
		return stage, "unknown"
	case model.CandStagePoolFull:
		return stage, "pool_capacity"
	case model.CandStageScored:
		if ev.GateType == model.GateCorrelation {
			return stage, "correlation_gate"
		}
		if ev.GateType == model.GateIndustryCap {
			return stage, "industry_cap"
		}
		if ev.ScoreRank > 0 {
			return stage, "quant_rank_below_cutoff"
		}
		return stage, "unknown"
	case model.CandStageLLMList:
		if strings.TrimSpace(ev.RejectionReason) != "" {
			return stage, "llm_rejected_recorded"
		}
		return stage, "llm_not_selected"
	case model.CandStagePicked:
		return stage, "picked_observed"
	default:
		return "unknown", "unknown"
	}
}

func auditHoldingPeriod(recType string) int {
	if recType == model.RecTypeLongTerm {
		return 20
	}
	return 5
}

func auditAdverse(obs auditSymbolObservation) bool {
	return obs.Status == btObserved && (obs.NetPct <= candidateAuditBadNetPct ||
		(obs.HasBench && obs.AlphaPct <= candidateAuditBadAlphaPct) || obs.MAE <= candidateAuditBadMaePct)
}

func auditItemFromFacts(run *model.CandidateAuditRun, batch model.RecommendationBatch, symbol string,
	obs auditSymbolObservation, discovery auditDiscoveryFact, ev *model.RecommendationCandidateEvent,
	rec *model.Recommendation, label *model.RecommendationLabel,
	selection *model.RecommendationSelectionOutcome) model.CandidateAuditItem {
	item := model.CandidateAuditItem{
		RunID: run.ID, UserID: batch.UserID, BatchID: batch.ID, Symbol: symbol,
		Market: run.Market, SignalDate: run.SignalDate, OutcomeDate: run.OutcomeDate,
		Name: obs.Name, RecType: batch.Type, Regime: batch.Regime,
		DiscoveryVersion: run.DiscoveryVersion, HoldingPeriodDays: auditHoldingPeriod(batch.Type),
		OutcomeStatus: obs.Status, EntryDate: obs.EntryDate, EntryPrice: obs.EntryPrice,
		ObservationPrice: obs.ClosePrice, GrossReturnPct: obs.GrossPct, NetReturnPct: obs.NetPct,
		BenchReturnPct: obs.BenchPct, AlphaPct: obs.AlphaPct, HasBench: obs.HasBench,
		MfePct: obs.MFE, MaePct: obs.MAE,
		DiscoveryItemID: discovery.Primary.ID, Channel: discovery.Primary.Channel,
		ChannelSet: strings.Join(discovery.Channels, ","),
	}
	if item.Name == "" {
		item.Name = discovery.Primary.Name
	}
	if ev != nil {
		item.CandidateEventID, item.RankingVersion = ev.ID, ev.RankingVersion
		item.Source, item.SourceSet = ev.Source, ev.SourceSet
		item.ScoreRank, item.LLMInputOrder = ev.ScoreRank, ev.LLMInputOrder
		item.RejectionReason, item.RefPrice = ev.RejectionReason, ev.RefPrice
		item.Action = ev.PostGateAction
		if item.Action == "" {
			item.Action = ev.RawAction
		}
	}
	if rec != nil {
		item.RecommendationID = rec.ID
		if item.Action == "" {
			item.Action = rec.Action
		}
		if item.Name == "" {
			item.Name = rec.Name
		}
	}
	if label != nil {
		item.RecommendationLabelID = label.ID
	}
	if selection != nil {
		item.SelectionOutcomeID = selection.ID
		item.SelectionMaturityStatus = selection.MaturityStatus
		item.Forced = selection.Forced
	}
	item.InputRefsJSON = auditMarshal(map[string]any{
		"batch_id": batch.ID, "candidate_event_id": item.CandidateEventID,
		"discovery_item_id": item.DiscoveryItemID, "factor_snapshot_id": obs.FactorSnapshotID,
		"signal_universe_id": obs.SignalUniverseID, "audit_parameter_hash": run.ParameterHash,
	})
	item.OutcomeRefsJSON = auditMarshal(map[string]any{
		"outcome_universe_id": obs.OutcomeUniverseID, "outcome_date": run.OutcomeDate,
		"observation_version": run.OutcomeVersion, "selection_outcome_id": item.SelectionOutcomeID,
		"recommendation_label_id": item.RecommendationLabelID,
	})
	return item
}

func auditMapKey(batchID int64, symbol string) string {
	return int64Key(batchID) + "\x00" + symbol
}

func ExecuteCandidateAudit(ctx context.Context, market *MarketService, expectedResultID int64,
	request candidateAuditJobRequest) (*model.CandidateAuditRun, error) {
	candidateAuditRunMu.Lock()
	defer candidateAuditRunMu.Unlock()
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if request.Market == "" {
		request.Market = "cn"
	}
	if request.ParameterHash == "" {
		request.ParameterHash = candidateAuditParameterHash()
	}
	if request.Market != "cn" || request.SignalDate == "" || request.OutcomeDate == "" {
		return nil, errors.New("审计交易日对或市场无效")
	}
	previous, err := candidateAuditAdjacentSignalDate(request.Market, request.OutcomeDate)
	if err != nil {
		return nil, fmt.Errorf("审计日期不是相邻有效交易日: signal=%s outcome=%s: %w",
			request.SignalDate, request.OutcomeDate, err)
	}
	if previous != request.SignalDate {
		return nil, fmt.Errorf("审计日期不是相邻有效交易日: signal=%s outcome=%s previous=%s",
			request.SignalDate, request.OutcomeDate, previous)
	}

	var run model.CandidateAuditRun
	created := false
	err = common.DB.Where("market = ? AND signal_date = ? AND outcome_date = ? AND audit_version = ? AND parameter_hash = ?",
		request.Market, request.SignalDate, request.OutcomeDate, CandidateAuditVersion, request.ParameterHash).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		now := time.Now()
		run = model.CandidateAuditRun{OwnerType: model.JobOwnerSystem, Market: request.Market,
			SignalDate: request.SignalDate, OutcomeDate: request.OutcomeDate,
			AuditVersion: CandidateAuditVersion, ParameterHash: request.ParameterHash,
			DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion,
			OutcomeVersion: candidateAuditOutcomeVersion, ParameterJSON: candidateAuditParameterJSON(),
			DataAsOf: now, Status: model.CandidateAuditStatusProcessing, StartedAt: now}
		if err := common.DB.Create(&run).Error; err != nil {
			return nil, err
		}
		created = true
	} else if err != nil {
		return nil, err
	}
	if expectedResultID > 0 && run.ID != expectedResultID {
		return nil, fmt.Errorf("审计作业结果引用错配: expected=%d actual=%d", expectedResultID, run.ID)
	}
	// success 与「已封存明细的 partial」是终态：观测一旦落库就是 PIT 事实，结果日
	// 数据被订正（前复权重锚等）后重算会让历史报表漂移。只有**空运行 partial**
	//（如信号日发现缺失，未封存任何明细）允许在上游数据补齐后幂等重算，否则一次
	// 瞬时缺口会把该日对永久冻结成空报表。
	if !created && run.FinishedAt != nil &&
		(run.Status == model.CandidateAuditStatusSuccess ||
			(run.Status == model.CandidateAuditStatusPartial && run.ItemCount > 0)) {
		return &run, nil
	}
	if !created {
		now := time.Now()
		if err := common.DB.Model(&run).Updates(map[string]any{"status": model.CandidateAuditStatusProcessing,
			"error": "", "gap_json": "", "started_at": now, "finished_at": nil, "updated_at": now}).Error; err != nil {
			return nil, err
		}
		if err := common.DB.Where("run_id = ?", run.ID).Delete(&model.CandidateAuditItem{}).Error; err != nil {
			return nil, err
		}
	}

	outcomeDiscovery, err := candidateAuditDiscoveryRun(request.OutcomeDate)
	if err != nil {
		markCandidateAuditFailed(run.ID, err)
		return &run, errors.New("结果日发现事实不可用")
	}
	run.DataAsOf = outcomeDiscovery.AsOf
	signalDiscovery, signalErr := candidateAuditDiscoveryRun(request.SignalDate)
	if signalErr != nil {
		// 只有「确实不存在信号日发现事实」才是合法降级；瞬时 DB 错误必须让作业
		// 失败重试，不能固化成 partial 空运行。
		if !errors.Is(signalErr, gorm.ErrRecordNotFound) {
			markCandidateAuditFailed(run.ID, signalErr)
			return &run, signalErr
		}
		gaps := map[string]int{"signal_discovery_missing": 1}
		if err := finishCandidateAuditWithoutItems(&run, model.CandidateAuditStatusPartial, gaps); err != nil {
			return &run, err
		}
		return &run, nil
	}
	run.SignalDataAsOf = &signalDiscovery.AsOf

	coverage, gaps := map[string]int{}, map[string]int{}
	if signalDiscovery.Status == DiscoveryRunStatusPart {
		gaps["signal_discovery_partial"]++
	}
	if outcomeDiscovery.Status == DiscoveryRunStatusPart {
		gaps["outcome_discovery_partial"]++
	}
	observations, err := loadAuditObservations(ctx, market, request.SignalDate, request.OutcomeDate,
		signalDiscovery.FactorVersion, coverage, gaps)
	if err != nil {
		markCandidateAuditFailed(run.ID, err)
		return &run, err
	}
	opportunities := buildAuditOpportunities(observations)
	discoveryBy, err := loadAuditDiscoveryFacts(signalDiscovery)
	if err != nil {
		markCandidateAuditFailed(run.ID, err)
		return &run, err
	}

	start, err := auditDateStart(request.SignalDate)
	if err != nil {
		markCandidateAuditFailed(run.ID, err)
		return &run, err
	}
	end, err := auditDateStart(request.OutcomeDate)
	if err != nil {
		markCandidateAuditFailed(run.ID, err)
		return &run, err
	}
	var batches []model.RecommendationBatch
	if err := common.DB.Where("market = ? AND created_at >= ? AND created_at < ? AND status IN ?", "cn", start, end,
		[]string{model.RecStatusSuccess, model.RecStatusDegraded}).Order("id").Find(&batches).Error; err != nil {
		markCandidateAuditFailed(run.ID, err)
		return &run, err
	}
	batchIDs := make([]int64, 0, len(batches))
	for _, batch := range batches {
		batchIDs = append(batchIDs, batch.ID)
	}
	var events []model.RecommendationCandidateEvent
	var recommendations []model.Recommendation
	var labels []model.RecommendationLabel
	var selections []model.RecommendationSelectionOutcome
	if len(batchIDs) > 0 {
		if err := common.DB.Where("batch_id IN ?", batchIDs).Order("batch_id, id").Find(&events).Error; err != nil {
			markCandidateAuditFailed(run.ID, err)
			return &run, err
		}
		if err := common.DB.Where("batch_id IN ?", batchIDs).Order("batch_id, sort_order, id").Find(&recommendations).Error; err != nil {
			markCandidateAuditFailed(run.ID, err)
			return &run, err
		}
		if err := common.DB.Where("batch_id IN ? AND horizon_days = ? AND entry_mode = ? AND label_version = ?",
			batchIDs, 1, model.EntryModeNextOpen, labelVersion).Find(&labels).Error; err != nil {
			markCandidateAuditFailed(run.ID, err)
			return &run, err
		}
		if err := common.DB.Where("batch_id IN ? AND outcome_version = ?", batchIDs,
			model.SelectionOutcomeVersion).Find(&selections).Error; err != nil {
			markCandidateAuditFailed(run.ID, err)
			return &run, err
		}
	}
	eventBy := map[string][]model.RecommendationCandidateEvent{}
	for _, event := range events {
		eventBy[auditMapKey(event.BatchID, event.Symbol)] = append(eventBy[auditMapKey(event.BatchID, event.Symbol)], event)
	}
	recBy := map[string]model.Recommendation{}
	for _, rec := range recommendations {
		recBy[auditMapKey(rec.BatchID, rec.Symbol)] = rec
	}
	labelBy := map[string]model.RecommendationLabel{}
	for _, label := range labels {
		labelBy[auditMapKey(label.BatchID, label.Symbol)] = label
	}
	selectionBy := map[string]model.RecommendationSelectionOutcome{}
	for _, selection := range selections {
		want := auditHoldingPeriod(selection.Type)
		if selection.HorizonDays == want {
			selectionBy[auditMapKey(selection.BatchID, selection.Symbol)] = selection
		}
	}

	items := make([]model.CandidateAuditItem, 0, len(batches)*(len(opportunities)+16))
	users := map[int64]bool{}
	rankingVersions := map[string]bool{}
	for _, batch := range batches {
		if ctx.Err() != nil {
			markCandidateAuditFailed(run.ID, ctx.Err())
			return &run, ctx.Err()
		}
		users[batch.UserID] = true
		if !batch.FactsRecorded {
			gaps["batch_facts_missing"]++
		}
		batchEvents := map[string][]model.RecommendationCandidateEvent{}
		for _, event := range events {
			if event.BatchID == batch.ID {
				batchEvents[event.Symbol] = append(batchEvents[event.Symbol], event)
			}
		}
		if batch.FactsRecorded && len(batchEvents) == 0 {
			gaps["batch_events_missing"]++
		}
		leaderSymbols := map[string]bool{}
		for _, opportunity := range opportunities {
			leaderSymbols[opportunity.Symbol] = true
			key := auditMapKey(batch.ID, opportunity.Symbol)
			facts := eventBy[key]
			ambiguous := len(facts) > 1
			var event *model.RecommendationCandidateEvent
			if len(facts) == 1 {
				event = &facts[0]
			}
			discovery, discovered := discoveryBy[opportunity.Symbol]
			stage, reason := candidateAuditReason("", event, discovered, batch.FactsRecorded && len(batchEvents) > 0, ambiguous)
			if event == nil && batch.FactsRecorded && len(batchEvents) > 0 && !opportunity.Obs.FactorReady {
				stage, reason = "absent", "signal_factor_snapshot_missing"
			}
			rec, recOK := recBy[key]
			label, labelOK := labelBy[key]
			selection, selectionOK := selectionBy[key]
			item := auditItemFromFacts(&run, batch, opportunity.Symbol, opportunity.Obs, discovery, event,
				optionalRecommendation(rec, recOK), optionalRecommendationLabel(label, labelOK),
				optionalSelectionOutcome(selection, selectionOK))
			item.OpportunityRank, item.FunnelStage, item.PrimaryReasonCode = opportunity.Rank, stage, reason
			item.AuditType, item.ConclusionCode = model.CandidateAuditTypeMissedLeader, "missed_leader"
			if event != nil && event.CandidateStage == model.CandStagePicked {
				item.AuditType, item.ConclusionCode = model.CandidateAuditTypeLeaderHit, "leader_recalled"
			}
			item.ReasonFactsJSON = auditMarshal(map[string]any{"recorded_rejection_reason": item.RejectionReason,
				"gate_type":  valueOrEmpty(event, func(e *model.RecommendationCandidateEvent) string { return e.GateType }),
				"score_rank": item.ScoreRank, "llm_input_order": item.LLMInputOrder})
			if item.RankingVersion != "" {
				rankingVersions[item.RankingVersion] = true
			}
			items = append(items, item)
		}

		batchSymbols := make([]string, 0, len(batchEvents))
		for symbol := range batchEvents {
			batchSymbols = append(batchSymbols, symbol)
		}
		sort.Strings(batchSymbols)
		for _, symbol := range batchSymbols {
			facts := batchEvents[symbol]
			if leaderSymbols[symbol] || len(facts) == 0 {
				continue
			}
			ambiguous := len(facts) != 1
			event := facts[0]
			if ambiguous {
				relevant := false
				for _, fact := range facts {
					if fact.CandidateStage == model.CandStageScored || fact.CandidateStage == model.CandStageLLMList ||
						fact.CandidateStage == model.CandStagePicked {
						relevant = true
						break
					}
				}
				if !relevant {
					continue
				}
			}
			if event.CandidateStage != model.CandStageScored && event.CandidateStage != model.CandStageLLMList &&
				event.CandidateStage != model.CandStagePicked && !ambiguous {
				continue
			}
			obs, ok := observations[symbol]
			if !ok {
				obs = auditSymbolObservation{Status: "no_data", Name: event.Name}
			}
			key := auditMapKey(batch.ID, symbol)
			rec, recOK := recBy[key]
			label, labelOK := labelBy[key]
			selection, selectionOK := selectionBy[key]
			discovery, discovered := discoveryBy[symbol]
			var eventRef *model.RecommendationCandidateEvent
			if !ambiguous {
				eventRef = &event
			}
			stage, reason := candidateAuditReason(event.CandidateStage, eventRef, discovered, true, ambiguous)
			item := auditItemFromFacts(&run, batch, symbol, obs, discovery, eventRef,
				optionalRecommendation(rec, recOK), optionalRecommendationLabel(label, labelOK),
				optionalSelectionOutcome(selection, selectionOK))
			item.AuditType, item.ConclusionCode = model.CandidateAuditTypeObservation, "candidate_observation"
			item.FunnelStage, item.PrimaryReasonCode = stage, reason
			if !ambiguous && event.CandidateStage == model.CandStagePicked && auditAdverse(obs) {
				item.AuditType = model.CandidateAuditTypeFalsePositive
				item.PrimaryReasonCode = "picked_adverse_next_day"
				if batch.Type == model.RecTypeLongTerm {
					item.ConclusionCode = "early_adverse_observation"
				} else {
					item.ConclusionCode = "false_positive_next_day"
				}
			}
			item.ReasonFactsJSON = auditMarshal(map[string]any{
				"ambiguous_event_count": len(facts), "recorded_rejection_reason": event.RejectionReason, "gate_type": event.GateType,
				"net_pct": obs.NetPct, "alpha_pct": obs.AlphaPct, "mae_pct": obs.MAE,
				"thresholds": map[string]float64{"net_pct": candidateAuditBadNetPct,
					"alpha_pct": candidateAuditBadAlphaPct, "mae_pct": candidateAuditBadMaePct},
			})
			if item.RankingVersion != "" {
				rankingVersions[item.RankingVersion] = true
			}
			items = append(items, item)
		}
	}

	versions := make([]string, 0, len(rankingVersions))
	for version := range rankingVersions {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	missed, falsePos := 0, 0
	for _, item := range items {
		if item.AuditType == model.CandidateAuditTypeMissedLeader {
			missed++
		}
		if item.ConclusionCode == "false_positive_next_day" {
			falsePos++
		}
	}
	contentHash := sha256.Sum256([]byte(auditMarshal(items)))
	finished := time.Now()
	status := model.CandidateAuditStatusSuccess
	if len(gaps) > 0 {
		status = model.CandidateAuditStatusPartial
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if len(items) > 0 {
			if err := tx.CreateInBatches(items, 300).Error; err != nil {
				return err
			}
		}
		updates := map[string]any{
			"status": status, "signal_data_as_of": signalDiscovery.AsOf, "data_as_of": outcomeDiscovery.AsOf,
			"ranking_versions": auditMarshal(versions), "universe_count": len(observations),
			"scanned_count": len(observations), "executable_count": coverage["executable"],
			"opportunity_count": len(opportunities), "batch_count": len(batches), "user_count": len(users),
			"item_count": len(items), "missed_count": missed, "false_positive_count": falsePos,
			"excluded_st": coverage["excluded_st"], "excluded_suspended": coverage["excluded_suspended"],
			"excluded_liquidity":    coverage["excluded_liquidity"],
			"excluded_unexecutable": coverage["excluded_unexecutable"],
			"excluded_no_data":      coverage["excluded_no_data"], "coverage_json": auditMarshal(coverage),
			"gap_json": auditMarshal(gaps), "content_hash": hex.EncodeToString(contentHash[:]), "error": "",
			"finished_at": finished, "updated_at": finished,
		}
		return tx.Model(&model.CandidateAuditRun{}).Where("id = ?", run.ID).Updates(updates).Error
	})
	if err != nil {
		markCandidateAuditFailed(run.ID, err)
		return &run, err
	}
	if err := common.DB.First(&run, run.ID).Error; err != nil {
		return &run, err
	}
	common.SysLog("每日候选审计完成 market=%s signal=%s outcome=%s run_id=%d status=%s batches=%d missed=%d false_positive=%d",
		run.Market, run.SignalDate, run.OutcomeDate, run.ID, run.Status, run.BatchCount, run.MissedCount, run.FalsePositiveCount)
	return &run, nil
}

func optionalRecommendation(value model.Recommendation, ok bool) *model.Recommendation {
	if !ok {
		return nil
	}
	return &value
}

func optionalRecommendationLabel(value model.RecommendationLabel, ok bool) *model.RecommendationLabel {
	if !ok {
		return nil
	}
	return &value
}

func optionalSelectionOutcome(value model.RecommendationSelectionOutcome, ok bool) *model.RecommendationSelectionOutcome {
	if !ok {
		return nil
	}
	return &value
}

func valueOrEmpty[T any](value *T, getter func(*T) string) string {
	if value == nil {
		return ""
	}
	return getter(value)
}

func registerCandidateAuditBinding() durableJobBinding {
	return durableJobBinding{resultType: JobResultCandidateAudit, resultCommittedByHandler: true,
		create: func(tx *gorm.DB, job *model.JobRun, raw json.RawMessage) (int64, error) {
			var req candidateAuditJobRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return 0, err
			}
			if req.Market == "" {
				req.Market = "cn"
			}
			if req.ParameterHash == "" {
				req.ParameterHash = candidateAuditParameterHash()
			}
			var row model.CandidateAuditRun
			err := tx.Where("market = ? AND signal_date = ? AND outcome_date = ? AND audit_version = ? AND parameter_hash = ?",
				req.Market, req.SignalDate, req.OutcomeDate, CandidateAuditVersion, req.ParameterHash).First(&row).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				now := time.Now()
				dataAsOf := now
				var outcomeDiscovery model.CandidateDiscoveryRun
				if tx.Where("market = ? AND trade_date = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ?",
					req.Market, req.OutcomeDate, DiscoveryVersion, factorSnapshotVersion, discoveryParameterHash()).
					Order("id DESC").First(&outcomeDiscovery).Error == nil {
					dataAsOf = outcomeDiscovery.AsOf
				}
				row = model.CandidateAuditRun{OwnerType: model.JobOwnerSystem, JobRunID: &job.ID,
					Market: req.Market, SignalDate: req.SignalDate, OutcomeDate: req.OutcomeDate,
					AuditVersion: CandidateAuditVersion, ParameterHash: req.ParameterHash,
					DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion,
					OutcomeVersion: candidateAuditOutcomeVersion, ParameterJSON: candidateAuditParameterJSON(),
					DataAsOf: dataAsOf, Status: model.CandidateAuditStatusProcessing, StartedAt: now}
				if err := tx.Create(&row).Error; err != nil {
					return 0, err
				}
			} else if err != nil {
				return 0, err
			} else if row.Status != model.CandidateAuditStatusSuccess &&
				!(row.Status == model.CandidateAuditStatusPartial && row.ItemCount > 0) {
				if err := tx.Model(&row).Updates(map[string]any{"job_run_id": job.ID,
					"status": model.CandidateAuditStatusProcessing, "error": "", "finished_at": nil}).Error; err != nil {
					return 0, err
				}
			}
			return row.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, job *model.JobRun, _ DurableJobResult, _ []byte, _ time.Time) error {
			if job.ResultID == nil {
				return errors.New("候选审计作业缺少结果引用")
			}
			var row model.CandidateAuditRun
			if err := tx.First(&row, *job.ResultID).Error; err != nil {
				return err
			}
			if row.Status != model.CandidateAuditStatusSuccess && row.Status != model.CandidateAuditStatusPartial {
				return errors.New("候选审计结果尚未落库")
			}
			return nil
		},
		finishFailure: func(tx *gorm.DB, job *model.JobRun, _ string, _ string, message string, now time.Time) error {
			if job.ResultID == nil {
				return errors.New("候选审计作业缺少结果引用")
			}
			return tx.Model(&model.CandidateAuditRun{}).Where("id = ?", *job.ResultID).Updates(map[string]any{
				"status": model.CandidateAuditStatusFailed, "error": truncate(message, 512), "finished_at": now,
			}).Error
		},
	}
}

func RegisterCandidateAuditJobHandler(market *MarketService) {
	binding := registerCandidateAuditBinding()
	registerDurableBusinessJobHandler(JobKindCandidateAudit, candidateAuditTimeout, binding,
		func(ctx context.Context, _ int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
			var req candidateAuditJobRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return DurableJobResult{}, err
			}
			resultID, ok := currentJobResultID(ctx)
			if !ok {
				return DurableJobResult{}, errors.New("候选审计作业缺少结果定位")
			}
			run, err := ExecuteCandidateAudit(ctx, market, resultID, req)
			if err != nil {
				return DurableJobResult{}, err
			}
			status := model.JobStatusSuccess
			if run.Status == model.CandidateAuditStatusPartial {
				status = model.JobStatusDegraded
			}
			return DurableJobResult{Status: status, Total: run.ItemCount, Succeeded: run.ItemCount}, nil
		})
}

func candidateAuditNeedsBackfill(signalDate, outcomeDate string) bool {
	if common.DB == nil || signalDate == "" || outcomeDate == "" {
		return false
	}
	var run model.CandidateAuditRun
	err := common.DB.Where("market = ? AND signal_date = ? AND outcome_date = ? AND audit_version = ? AND parameter_hash = ?",
		"cn", signalDate, outcomeDate, CandidateAuditVersion, candidateAuditParameterHash()).First(&run).Error
	// 空运行 partial（未封存明细）不算完成：缺口可能已被补齐，补跑幂等重算；
	// 已封存明细的 partial 与 success 一样是 PIT 终态。
	return err != nil || run.FinishedAt == nil ||
		(run.Status != model.CandidateAuditStatusSuccess &&
			!(run.Status == model.CandidateAuditStatusPartial && run.ItemCount > 0))
}

func ScheduleCandidateAudit(outcomeDate, trigger string) {
	if common.DB == nil || outcomeDate == "" {
		return
	}
	signalDate, err := candidateAuditAdjacentSignalDate("cn", outcomeDate)
	if err != nil {
		common.SysWarn("每日候选审计未提交 outcome=%s：%v", outcomeDate, err)
		return
	}
	if !candidateAuditNeedsBackfill(signalDate, outcomeDate) {
		return
	}
	req := candidateAuditJobRequest{Version: 1, Market: "cn", SignalDate: signalDate,
		OutcomeDate: outcomeDate, TriggerSource: trigger, ParameterHash: candidateAuditParameterHash()}
	scheduleRetry := func() {
		key := signalDate + ":" + outcomeDate + ":" + req.ParameterHash
		candidateAuditScheduleMu.Lock()
		if !candidateAuditSchedulePending[key] {
			candidateAuditSchedulePending[key] = true
			time.AfterFunc(systemScheduleRetryDelay, func() {
				candidateAuditScheduleMu.Lock()
				delete(candidateAuditSchedulePending, key)
				candidateAuditScheduleMu.Unlock()
				ScheduleCandidateAudit(outcomeDate, trigger)
			})
		}
		candidateAuditScheduleMu.Unlock()
	}
	run, err := defaultJobRuntime.startSystemWithBinding(nil, JobKindCandidateAudit, req, nil)
	if err != nil {
		if errors.Is(err, ErrJobQueueBusy) || errors.Is(err, ErrJobAlreadyRunning) {
			scheduleRetry()
			return
		}
		common.SysWarn("提交每日候选审计作业失败: %v", err)
		return
	}
	// system 同 kind 全局互斥：命中在途作业时 runtime 返回的是**原 run**（可能是
	// 另一日对的请求），静默当“已受理”会让本日对审计永久漏掉（startup backfill
	// 只补最新一对）。比对快照身份，不一致必须延迟补交。
	var accepted candidateAuditJobRequest
	if run == nil || json.Unmarshal(snapshotJSONRequest([]byte(run.RequestSnapshot)), &accepted) != nil ||
		accepted.SignalDate != req.SignalDate || accepted.OutcomeDate != req.OutcomeDate ||
		accepted.ParameterHash != req.ParameterHash {
		scheduleRetry()
	}
}

func ScheduleCandidateAuditStartupBackfill() {
	if common.DB == nil {
		return
	}
	go func() {
		var latest model.CandidateDiscoveryRun
		if err := common.DB.Where("owner_type = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ? AND status IN ?",
			model.JobOwnerSystem, DiscoveryVersion, factorSnapshotVersion, discoveryParameterHash(),
			[]string{DiscoveryRunStatusOK, DiscoveryRunStatusPart}).Order("trade_date DESC, id DESC").First(&latest).Error; err != nil {
			return
		}
		ScheduleCandidateAudit(latest.TradeDate, "startup")
	}()
}
