package service

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

const (
	selectionFactFactsMissing  = "facts_not_recorded"
	selectionFactEventsMissing = "candidate_events_missing"
	selectionFactRankingOld    = "ranking_or_order_incomplete"
	selectionFactDuplicate     = "duplicate_or_noncontiguous_facts"
	selectionFactPickMismatch  = "ai_pick_fact_mismatch"
	selectionForcedExecution   = "execution_forced_exit"
	selectionForcedNoData      = "no_data_timeout_forced_close"
)

// selectionBatchFacts 是一批生成时事实的内存视图。Issue 非空时整批不得进入精确
// selection 配对，也不得从当前行情或聚合计数猜测缺失事实。
type selectionBatchFacts struct {
	Batch          model.RecommendationBatch
	RankingVersion string
	Opportunity    []model.RecommendationCandidateEvent // 按 llm_input_order 升序
	Picks          []model.Recommendation               // 按真实 sort_order 升序
	Issue          string
}

// selectionRankingVersionSupported 只列出已经审计过、可精确复原机会集的版本。
// 后续候选排序升级必须显式扩展这里，并补充新旧版本对拍，不能自动跟随当前版本。
func selectionRankingVersionSupported(version string) bool {
	switch version {
	case "cr1", "cr2":
		return true
	default:
		return false
	}
}

func loadSelectionBatchFacts() ([]selectionBatchFacts, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var batches []model.RecommendationBatch
	if err := common.DB.Where("status IN ?", []string{model.RecStatusSuccess, model.RecStatusDegraded}).
		Order("created_at, id").Find(&batches).Error; err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return []selectionBatchFacts{}, nil
	}
	ids := make([]int64, 0, len(batches))
	for _, b := range batches {
		ids = append(ids, b.ID)
	}
	var events []model.RecommendationCandidateEvent
	if err := common.DB.Where("batch_id IN ?", ids).Order("batch_id, id").Find(&events).Error; err != nil {
		return nil, err
	}
	var picks []model.Recommendation
	if err := common.DB.Where("batch_id IN ?", ids).
		Order("batch_id, sort_order, id").Find(&picks).Error; err != nil {
		return nil, err
	}
	eventsByBatch := make(map[int64][]model.RecommendationCandidateEvent, len(batches))
	for _, ev := range events {
		eventsByBatch[ev.BatchID] = append(eventsByBatch[ev.BatchID], ev)
	}
	picksByBatch := make(map[int64][]model.Recommendation, len(batches))
	for _, p := range picks {
		picksByBatch[p.BatchID] = append(picksByBatch[p.BatchID], p)
	}

	out := make([]selectionBatchFacts, 0, len(batches))
	for _, batch := range batches {
		bf := selectionBatchFacts{Batch: batch, Picks: picksByBatch[batch.ID]}
		allEvents := eventsByBatch[batch.ID]
		switch {
		case !batch.FactsRecorded:
			bf.Issue = selectionFactFactsMissing
		case len(allEvents) == 0:
			bf.Issue = selectionFactEventsMissing
		default:
			bf.Opportunity, bf.Issue = validateSelectionFacts(allEvents, bf.Picks)
			if bf.Issue == "" && len(bf.Opportunity) > 0 {
				bf.RankingVersion = bf.Opportunity[0].RankingVersion
			}
		}
		out = append(out, bf)
	}
	return out, nil
}

// validateSelectionFacts 校验一批生成时机会集。所有实际送模标的必须属于同一个已
// 审计排名版本、有正 score_rank、连续 llm_input_order，且 symbol/rank/order 均唯一。
func validateSelectionFacts(events []model.RecommendationCandidateEvent, picks []model.Recommendation) ([]model.RecommendationCandidateEvent, string) {
	opp := make([]model.RecommendationCandidateEvent, 0, len(events))
	seenSym := map[string]bool{}
	seenRank := map[int]bool{}
	seenOrder := map[int]bool{}
	pickedEvents := map[string]bool{}
	rankingVersion := ""
	for _, ev := range events {
		if ev.CandidateStage == model.CandStagePicked {
			if pickedEvents[ev.Symbol] {
				return nil, selectionFactDuplicate
			}
			pickedEvents[ev.Symbol] = true
		}
		if !ev.SentToLLM && ev.LLMInputOrder <= 0 {
			continue
		}
		if !ev.SentToLLM || !selectionRankingVersionSupported(ev.RankingVersion) ||
			ev.ScoreRank <= 0 || ev.LLMInputOrder <= 0 || ev.Symbol == "" {
			return nil, selectionFactRankingOld
		}
		if rankingVersion == "" {
			rankingVersion = ev.RankingVersion
		} else if ev.RankingVersion != rankingVersion {
			return nil, selectionFactRankingOld
		}
		if seenSym[ev.Symbol] || seenRank[ev.ScoreRank] || seenOrder[ev.LLMInputOrder] {
			return nil, selectionFactDuplicate
		}
		seenSym[ev.Symbol] = true
		seenRank[ev.ScoreRank] = true
		seenOrder[ev.LLMInputOrder] = true
		opp = append(opp, ev)
	}
	if len(opp) == 0 {
		return nil, selectionFactEventsMissing
	}
	sort.Slice(opp, func(i, j int) bool {
		if opp[i].LLMInputOrder == opp[j].LLMInputOrder {
			return opp[i].Symbol < opp[j].Symbol
		}
		return opp[i].LLMInputOrder < opp[j].LLMInputOrder
	})
	for i := range opp {
		if opp[i].LLMInputOrder != i+1 {
			return nil, selectionFactDuplicate
		}
	}

	seenPick := map[string]bool{}
	for _, p := range picks {
		if p.Symbol == "" || seenPick[p.Symbol] || !seenSym[p.Symbol] ||
			(p.Action != model.RecActionBuy && p.Action != model.RecActionWatch) || !pickedEvents[p.Symbol] {
			return nil, selectionFactPickMismatch
		}
		seenPick[p.Symbol] = true
	}
	if len(seenPick) != len(pickedEvents) {
		return nil, selectionFactPickMismatch
	}
	return opp, ""
}

func quantTopN(opp []model.RecommendationCandidateEvent, n int) []model.RecommendationCandidateEvent {
	if n <= 0 {
		return []model.RecommendationCandidateEvent{}
	}
	out := append([]model.RecommendationCandidateEvent(nil), opp...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].ScoreRank == out[j].ScoreRank {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].ScoreRank < out[j].ScoreRank
	})
	if n < len(out) {
		out = out[:n]
	}
	return out
}

type selectionOutcomeBuildStats struct {
	Inserted  int
	Updated   int
	Unchanged int
}

func advanceSelectionOutcomes(ctx context.Context, market *MarketService, batches []selectionBatchFacts, now time.Time) (selectionOutcomeBuildStats, error) {
	var stats selectionOutcomeBuildStats
	if common.DB == nil {
		return stats, errors.New("数据库不可用")
	}
	today := now.In(time.Local).Format("2006-01-02")
	axis, benchClose, _ := NewBacktestService(market).marketAxis(ctx, today)
	marketLast := ""
	if len(axis) > 0 {
		marketLast = axis[len(axis)-1]
	}

	var existing []model.RecommendationSelectionOutcome
	if err := common.DB.Where("outcome_version = ?", model.SelectionOutcomeVersion).Find(&existing).Error; err != nil {
		return stats, err
	}
	existingByKey := make(map[string]model.RecommendationSelectionOutcome, len(existing))
	for _, row := range existing {
		existingByKey[selectionOutcomeKey(row.BatchID, row.Symbol, row.HorizonDays)] = row
	}
	barsBySymbol := map[string][]datasource.Bar{}
	for _, bf := range batches {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		if bf.Issue != "" {
			continue
		}
		for _, ev := range bf.Opportunity {
			bars, ok := barsBySymbol[ev.Symbol]
			if !ok {
				bars = cnDailyBarsAsc(ev.Symbol)
				barsBySymbol[ev.Symbol] = bars
			}
			for _, horizon := range model.SelectionOutcomeHorizons {
				key := selectionOutcomeKey(bf.Batch.ID, ev.Symbol, horizon)
				old, exists := existingByKey[key]
				if exists && selectionOutcomeTerminal(old.MaturityStatus) &&
					old.SchemaVersion == model.SelectionOutcomeSchemaVersion &&
					old.RankingVersion == ev.RankingVersion {
					stats.Unchanged++
					continue
				}
				row := computeSelectionOutcome(bf.Batch, ev, horizon, bars, axis, benchClose, marketLast, today)
				if exists {
					row.ID, row.CreatedAt = old.ID, old.CreatedAt
					if selectionOutcomeSame(old, row) {
						stats.Unchanged++
						continue
					}
					if err := common.DB.Save(&row).Error; err != nil {
						return stats, err
					}
					stats.Updated++
				} else {
					if err := common.DB.Create(&row).Error; err != nil {
						return stats, err
					}
					stats.Inserted++
				}
				existingByKey[key] = row
			}
		}
	}
	return stats, nil
}

func selectionOutcomeKey(batchID int64, symbol string, horizon int) string {
	return int64Key(batchID) + ":" + symbol + ":" + intKey(horizon)
}

func int64Key(v int64) string {
	// strconv.FormatInt 放在小 helper 中，避免各个配对 map 自行拼不同格式。
	return strconv.FormatInt(v, 10)
}

func selectionOutcomeTerminal(status string) bool {
	return status == model.LabelMatured || status == model.LabelNoData || status == model.LabelSkipped
}

func computeSelectionOutcome(batch model.RecommendationBatch, ev model.RecommendationCandidateEvent,
	horizon int, bars []datasource.Bar, axis []string, benchClose map[string]float64,
	marketLast, today string) model.RecommendationSelectionOutcome {
	signalDate := batch.CreatedAt.In(time.Local).Format("2006-01-02")
	row := model.RecommendationSelectionOutcome{
		BatchID: batch.ID, CandidateEventID: ev.ID, UserID: batch.UserID,
		Symbol: ev.Symbol, Market: ev.Market, Name: ev.Name, Type: batch.Type,
		HorizonDays: horizon, SignalDate: signalDate, SignalAsOf: batch.CreatedAt,
		EntryMode: model.EntryModeNextOpen, RankingVersion: ev.RankingVersion,
		OutcomeVersion: model.SelectionOutcomeVersion,
		SchemaVersion:  model.SelectionOutcomeSchemaVersion,
		MaturityStatus: model.LabelPending,
	}
	i := -1
	for k := len(bars) - 1; k >= 0; k-- {
		if bars[k].TradeDate <= signalDate {
			i = k
			break
		}
	}
	if i < 0 || len(bars) == 0 {
		if daysBetween(signalDate, today) > labelNoDataAfterDays {
			row.MaturityStatus = model.LabelNoData
			row.NoDataReason = "signal_bar_missing_after_timeout"
		}
		return row
	}
	nextDate, sellDate := labelAxisDates(axis, signalDate, horizon)
	out := simulateLabelHold(bars, i, ev.Symbol, ev.Name, horizon, labelPerCap, 0, 0,
		nextDate, sellDate, marketLast)
	forcedReason := ""
	if out.Status == btPending && daysBetween(signalDate, today) > labelNoDataAfterDays {
		tmp := model.RecommendationLabel{EntryMode: model.EntryModeNextOpen, Symbol: ev.Symbol}
		if out.BuyDate != "" && forceCloseStaleLabel(&tmp, &out, bars) {
			forcedReason = selectionForcedNoData
		} else {
			row.MaturityStatus = model.LabelNoData
			row.NoDataReason = "execution_data_missing_after_timeout"
			return row
		}
	}
	row.EntryDate, row.EntryPrice = out.BuyDate, round2(out.BuyPrice)
	row.ExitDate, row.ExitPrice = out.SellDate, round2(out.SellPrice)
	row.GrossReturnPct, row.NetReturnPct = out.GrossPct, out.NetPct
	row.MfePct, row.MaePct = out.MfePct, out.MaePct
	row.Deferred, row.Forced = out.Deferred, out.Forced
	if out.Forced {
		row.ForcedReason = forcedReason
		if row.ForcedReason == "" {
			row.ForcedReason = selectionForcedExecution
		}
	}
	switch out.Status {
	case btTraded:
		row.MaturityStatus = model.LabelMatured
	case btPending:
		row.MaturityStatus = model.LabelPending
	default:
		row.MaturityStatus = model.LabelSkipped
		row.SkipReason = out.Status
	}
	if row.MaturityStatus == model.LabelMatured {
		if b0, ok0 := benchClose[out.BuyDate]; ok0 && b0 > 0 {
			if b1, ok1 := benchClose[out.SellDate]; ok1 && b1 > 0 {
				row.BenchReturnPct = round2((b1 - b0) / b0 * 100)
				row.AlphaPct = round2(row.NetReturnPct - row.BenchReturnPct)
				row.HasBench = true
			}
		}
	}
	return row
}

func selectionOutcomeSame(a, b model.RecommendationSelectionOutcome) bool {
	return a.CandidateEventID == b.CandidateEventID && a.UserID == b.UserID &&
		a.Market == b.Market && a.Name == b.Name && a.Type == b.Type &&
		a.RankingVersion == b.RankingVersion && a.SchemaVersion == b.SchemaVersion &&
		a.EntryMode == b.EntryMode && a.SignalDate == b.SignalDate && a.SignalAsOf.Equal(b.SignalAsOf) &&
		a.EntryDate == b.EntryDate && a.EntryPrice == b.EntryPrice &&
		a.ExitDate == b.ExitDate && a.ExitPrice == b.ExitPrice &&
		a.GrossReturnPct == b.GrossReturnPct && a.NetReturnPct == b.NetReturnPct &&
		a.BenchReturnPct == b.BenchReturnPct && a.AlphaPct == b.AlphaPct &&
		a.HasBench == b.HasBench && a.MfePct == b.MfePct && a.MaePct == b.MaePct &&
		a.MaturityStatus == b.MaturityStatus && a.SkipReason == b.SkipReason &&
		a.NoDataReason == b.NoDataReason && a.Forced == b.Forced &&
		a.ForcedReason == b.ForcedReason && a.Deferred == b.Deferred
}
