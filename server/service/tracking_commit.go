package service

import (
	"context"
	"errors"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 与删除共用批次锁；持锁后再作当前读，终态与新观察不能被在途旧评估覆盖。
func (s *TrackingService) commitStatus(ctx context.Context, st *model.RecommendationStatus, startedAt time.Time) (bool, error) {
	if common.DB == nil {
		return false, errors.New("数据库不可用")
	}
	written := false
	next := *st
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var batch model.RecommendationBatch
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", st.BatchID, st.UserID).First(&batch).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("推荐记录已删除，追踪结果未写入")
			}
			return err
		}
		if batch.Status != model.RecStatusSuccess && batch.Status != model.RecStatusDegraded {
			return errors.New("推荐批次尚不可追踪")
		}
		var rec model.Recommendation
		if err := tx.Where("id = ? AND batch_id = ? AND user_id = ? AND symbol = ? AND market = ?",
			st.RecommendationID, st.BatchID, st.UserID, st.Symbol, st.Market).First(&rec).Error; err != nil {
			return err
		}
		var previous model.RecommendationStatus
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("recommendation_id = ?", st.RecommendationID).First(&previous).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			if previous.UserID != st.UserID || previous.BatchID != st.BatchID {
				return errors.New("推荐追踪归属不一致")
			}
			if frozenTerminal(previous.Outcome) || previous.LastEvalDate > st.LastEvalDate || previous.UpdatedAt.After(startedAt) {
				return nil
			}
			next.ID = previous.ID
		} else {
			next.ID = 0
		}
		next.UpdatedAt = time.Now()
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "recommendation_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"batch_id", "user_id", "symbol", "market", "type", "action",
				"ref_price", "current_price", "period_high", "period_low",
				"return_pct", "max_gain_pct", "max_drawdown_pct", "bench_return_pct", "alpha_pct",
				"outcome", "review_needed", "hit_take_profit", "hit_stop_loss",
				"elapsed_trade_days", "valid_days", "bars_count", "last_eval_date", "note",
				"actual_buy_price", "actual_return_pct", "return_7d", "return_14d", "return_30d", "updated_at",
			}),
		}).Create(&next).Error; err != nil {
			return err
		}
		written = true
		return nil
	})
	if err != nil {
		return false, errors.Join(err, ctx.Err())
	}
	if written {
		*st = next
	}
	return written, nil
}
