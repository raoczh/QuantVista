package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ArtifactTypeAnalysis       = "analysis"
	ArtifactTypeRecommendation = "recommendation"
	ArtifactTypeDailyReport    = "daily_report"
	artifactSchemaVersion      = 1
)

func persistAnalysisArtifact(tx *gorm.DB, run *model.JobRun, rec model.AnalysisRecord, availableAt time.Time) error {
	return createResearchArtifact(tx, run, ArtifactTypeAnalysis,
		fmt.Sprintf("%s:%s:%s", rec.Module, rec.Market, rec.Symbol), rec.AsOf,
		"analysis_records:"+fmt.Sprint(rec.ID), rec.TraceID, rec, availableAt)
}

func persistRecommendationArtifact(tx *gorm.DB, run *model.JobRun, batch model.RecommendationBatch, availableAt time.Time) error {
	var items []model.Recommendation
	if err := tx.Where("batch_id = ? AND user_id = ?", batch.ID, batch.UserID).
		Order("sort_order ASC, id ASC").Find(&items).Error; err != nil {
		return err
	}
	content := struct {
		Batch model.RecommendationBatch `json:"batch"`
		Items []model.Recommendation    `json:"items"`
	}{Batch: batch, Items: items}
	return createResearchArtifact(tx, run, ArtifactTypeRecommendation,
		fmt.Sprintf("%s:%s:%s", batch.Type, batch.Market, batch.Strategy), "",
		"recommendation_batches:"+fmt.Sprint(batch.ID), batch.TraceID, content, availableAt)
}

func persistDailyReportArtifact(tx *gorm.DB, run *model.JobRun, report model.DailyReport, availableAt time.Time) error {
	return createResearchArtifact(tx, run, ArtifactTypeDailyReport,
		fmt.Sprintf("%s:%s", report.Market, report.TradeDate), report.TradeDate,
		"daily_reports:"+fmt.Sprint(report.ID), report.TraceID, report, availableAt)
}

func createResearchArtifact(tx *gorm.DB, run *model.JobRun, artifactType, subject, asOfText,
	storageRef, traceID string, persistedContent any, availableAt time.Time) error {
	if tx == nil || run == nil || run.ID <= 0 || run.OwnerType != model.JobOwnerUser || run.OwnerUserID == nil {
		return errors.New("ResearchArtifact 缺少有效用户作业 owner")
	}
	contentJSON, err := json.Marshal(persistedContent)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(contentJSON)
	if availableAt.IsZero() {
		availableAt = time.Now()
	}
	asOf := availableAt
	if parsed, parseErr := time.ParseInLocation("2006-01-02", asOfText, time.Local); parseErr == nil {
		asOf = parsed
	}
	sources, err := json.Marshal(map[string]any{
		"job_run_id":  run.ID,
		"result_type": run.ResultType,
		"result_id":   run.ResultID,
		"trace_id":    traceID,
	})
	if err != nil {
		return err
	}
	artifact := &model.ResearchArtifact{
		Type: artifactType, SchemaVersion: artifactSchemaVersion, Subject: subject,
		AsOf: &asOf, AvailableAt: availableAt, ContentHash: hex.EncodeToString(hash[:]),
		SourceRefs: string(sources), StorageRef: storageRef, JobRunID: run.ID,
		OwnerType: run.OwnerType, OwnerUserID: run.OwnerUserID,
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(artifact).Error
}
