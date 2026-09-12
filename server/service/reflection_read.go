package service

import (
	"context"
	"errors"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func reflectionMaturedQuery(db *gorm.DB, asOf time.Time) *gorm.DB {
	return db.Model(&model.RecommendationLabel{}).
		Where("maturity_status = ? AND entry_mode = ? AND recommendation_id > 0 AND label_version = ? AND forced = ?",
			model.LabelMatured, model.EntryModeNextOpen, labelVersion, false).
		Where("updated_at <= ? AND signal_as_of <= ? AND signal_date <= ? AND exit_date <= ?", asOf, asOf, asOf.Format("2006-01-02"), asOf.Format("2006-01-02")).
		// 在 SQL 侧排除孤儿和血缘错配，不能让最新的孤儿永久挤占有限候选名额。
		Where(`EXISTS (SELECT 1 FROM recommendations r WHERE r.id = recommendation_labels.recommendation_id
			AND r.user_id = recommendation_labels.user_id AND r.batch_id = recommendation_labels.batch_id
			AND r.symbol = recommendation_labels.symbol AND r.market = recommendation_labels.market)`)
}

func loadReflectionCandidates(limit int, contexts ...context.Context) ([]reflectionCandidate, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var out []reflectionCandidate
	asOf := time.Now()
	err := readSnapshotTx(jobSubmissionContext(contexts...), func(tx *gorm.DB) error {
		var err error
		out, err = loadReflectionCandidatesDB(tx, asOf, limit)
		return err
	})
	return out, err
}

func loadReflectionCandidatesDB(db *gorm.DB, asOf time.Time, limit int) ([]reflectionCandidate, error) {
	if limit <= 0 {
		return []reflectionCandidate{}, nil
	}
	var labels []model.RecommendationLabel
	if err := reflectionMaturedQuery(db, asOf).
		Where("(type = ? AND horizon_days = 10) OR (type = ? AND horizon_days = 20)", model.RecTypeShortTerm, model.RecTypeLongTerm).
		Where("NOT EXISTS (SELECT 1 FROM recommendation_reflections rr WHERE rr.recommendation_id = recommendation_labels.recommendation_id AND rr.horizon_days = recommendation_labels.horizon_days)").
		// 相同结算时间按主键稳定排序，保证模型序号可复现。
		Order("updated_at DESC, id ASC").Limit(limit).Find(&labels).Error; err != nil {
		return nil, err
	}
	out := make([]reflectionCandidate, 0, len(labels))
	for _, label := range labels {
		var rec model.Recommendation
		if err := db.First(&rec, label.RecommendationID).Error; err != nil {
			return nil, err
		}
		out = append(out, reflectionCandidate{label: label, rec: rec})
	}
	return out, nil
}
