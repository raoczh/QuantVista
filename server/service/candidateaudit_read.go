package service

import (
	"context"
	"errors"
	"fmt"

	"quantvista/model"

	"gorm.io/gorm"
)

type candidateAuditInputs struct {
	signal, outcome model.CandidateDiscoveryRun
	missingSignal   bool
	coverage, gaps  map[string]int
	observations    map[string]auditSymbolObservation
	discovery       map[string]auditDiscoveryFact
	batches         []model.RecommendationBatch
	events          []model.RecommendationCandidateEvent
	recommendations []model.Recommendation
	labels          []model.RecommendationLabel
	selections      []model.RecommendationSelectionOutcome
}

func readCandidateAuditInputs(ctx context.Context, market *MarketService, request candidateAuditJobRequest) (*candidateAuditInputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 基准行情可能走上游，先完成有界网络请求，再固定本地数据库读取快照。
	benchPct, hasBench := auditBenchmarkObservation(ctx, market, request.OutcomeDate)
	out := &candidateAuditInputs{coverage: map[string]int{}, gaps: map[string]int{}}
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		previous, err := candidateAuditAdjacentSignalDateDB(tx, request.Market, request.OutcomeDate)
		if err != nil {
			return err
		}
		if previous != request.SignalDate {
			return errors.New("审计相邻交易日已变化")
		}
		out.outcome, err = candidateAuditDiscoveryRunDB(tx, request.OutcomeDate)
		if err != nil {
			return fmt.Errorf("结果日发现事实不可用: %w", err)
		}
		out.signal, err = candidateAuditDiscoveryRunDB(tx, request.SignalDate)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			out.missingSignal = true
			return nil
		}
		if err != nil {
			return err
		}
		if out.signal.Status == DiscoveryRunStatusPart {
			out.gaps["signal_discovery_partial"]++
		}
		if out.outcome.Status == DiscoveryRunStatusPart {
			out.gaps["outcome_discovery_partial"]++
		}
		out.observations, err = loadAuditObservationsDB(ctx, tx, request.SignalDate, request.OutcomeDate,
			out.signal.FactorVersion, out.coverage, out.gaps, benchPct, hasBench)
		if err != nil {
			return err
		}
		out.discovery, err = loadAuditDiscoveryFactsDB(tx, out.signal)
		if err != nil {
			return err
		}
		start, err := auditDateStart(request.SignalDate)
		if err != nil {
			return err
		}
		end, err := auditDateStart(request.OutcomeDate)
		if err != nil {
			return err
		}
		if err := tx.Where("market = ? AND created_at >= ? AND created_at < ? AND status IN ?", "cn", start, end,
			[]string{model.RecStatusSuccess, model.RecStatusDegraded}).Order("id").Find(&out.batches).Error; err != nil {
			return err
		}
		if len(out.batches) == 0 {
			return nil
		}
		ids := make([]int64, 0, len(out.batches))
		for _, batch := range out.batches {
			ids = append(ids, batch.ID)
		}
		if err := tx.Where("batch_id IN ?", ids).Order("batch_id, id").Find(&out.events).Error; err != nil {
			return err
		}
		if err := tx.Where("batch_id IN ?", ids).Order("batch_id, sort_order, id").Find(&out.recommendations).Error; err != nil {
			return err
		}
		if err := tx.Where("batch_id IN ? AND horizon_days = ? AND entry_mode = ? AND label_version = ?",
			ids, 1, model.EntryModeNextOpen, labelVersion).Find(&out.labels).Error; err != nil {
			return err
		}
		return tx.Where("batch_id IN ? AND outcome_version = ?", ids, model.SelectionOutcomeVersion).Find(&out.selections).Error
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
