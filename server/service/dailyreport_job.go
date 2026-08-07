package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type dailyReportJobRequest struct {
	Version   int    `json:"version"`
	TradeDate string `json:"trade_date"`
	Manual    bool   `json:"manual"`
}

func (s *DailyReportService) registerDurableJobHandler() {
	binding := durableJobBinding{
		resultType:               JobResultDailyReport,
		resultCommittedByHandler: true,
		create: func(tx *gorm.DB, run *model.JobRun, raw json.RawMessage) (int64, error) {
			if err := ensureEnabledJobUser(run.UserID); err != nil {
				return 0, err
			}
			var req dailyReportJobRequest
			if err := json.Unmarshal(raw, &req); err != nil || req.TradeDate == "" {
				return 0, fmt.Errorf("日报作业快照无效")
			}
			if _, err := time.ParseInLocation("2006-01-02", req.TradeDate, time.Local); err != nil {
				return 0, fmt.Errorf("日报日期无效")
			}
			var report model.DailyReport
			findErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("user_id = ? AND trade_date = ?", run.UserID, req.TradeDate).First(&report).Error
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				report = model.DailyReport{UserID: run.UserID, TradeDate: req.TradeDate, Market: "cn",
					Status: model.ReportStatusProcessing}
				if err := tx.Create(&report).Error; err != nil {
					return 0, err
				}
				return report.ID, nil
			}
			if findErr != nil {
				return 0, findErr
			}
			if report.Status == model.ReportStatusProcessing && report.PreviousStatus == "" {
				return report.ID, nil
			}
			previous := report.Status
			if previous == model.ReportStatusProcessing {
				previous = model.ReportStatusFailed
			}
			res := tx.Model(&model.DailyReport{}).Where("id = ? AND status = ?", report.ID, report.Status).
				Updates(map[string]any{"status": model.ReportStatusProcessing, "previous_status": previous, "updated_at": time.Now()})
			if res.Error != nil {
				return 0, res.Error
			}
			if res.RowsAffected != 1 {
				return 0, errors.New("日报已被其他作业接管")
			}
			return report.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, _ []byte, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("日报作业缺少结果引用")
			}
			var report model.DailyReport
			if err := tx.Where("id = ? AND user_id = ?", *run.ResultID, run.UserID).First(&report).Error; err != nil {
				return err
			}
			if report.Status != model.ReportStatusSuccess && report.Status != model.ReportStatusPartial {
				return errors.New("日报结果尚未落库")
			}
			if result.Status != "" && reportStatusToJobStatus(report.Status) != result.Status {
				return errors.New("日报结果状态与作业不一致")
			}
			if err := tx.Model(&model.DailyReport{}).Where("id = ?", report.ID).
				Updates(map[string]any{"previous_status": "", "updated_at": now}).Error; err != nil {
				return err
			}
			report.PreviousStatus = ""
			report.UpdatedAt = now
			return persistDailyReportArtifact(tx, run, report, now)
		},
		finishFailure: func(tx *gorm.DB, run *model.JobRun, _ string, code, message string, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("日报作业缺少结果引用")
			}
			var report model.DailyReport
			if err := tx.Where("id = ? AND user_id = ?", *run.ResultID, run.UserID).First(&report).Error; err != nil {
				return err
			}
			if report.Status != model.ReportStatusProcessing {
				// 业务执行可能已经完成了旧报告回滚；运行时失败 CAS 不能再把
				// 已恢复的 success/partial 覆盖成 failed。
				return nil
			}
			restore := report.PreviousStatus
			if restore == "" {
				restore = model.ReportStatusFailed
			}
			return tx.Model(&model.DailyReport{}).Where("id = ? AND user_id = ?", report.ID, run.UserID).
				Updates(map[string]any{"status": restore, "previous_status": "", "error": sanitizeJobError(message), "updated_at": now}).Error
		},
	}
	registerDurableBusinessJobHandler(JobKindDailyReport, reportJobTimeout, binding,
		func(ctx context.Context, userID int64, allowPrivate bool, raw json.RawMessage) (DurableJobResult, error) {
			if err := ensureEnabledJobUser(userID); err != nil {
				return DurableJobResult{}, err
			}
			var req dailyReportJobRequest
			if err := json.Unmarshal(raw, &req); err != nil || req.TradeDate == "" {
				return DurableJobResult{}, errors.New("日报作业快照无效")
			}
			cfg, apiKey, err := s.llm.ResolveForUse(userID, 0)
			if err != nil {
				return DurableJobResult{}, fmt.Errorf("未配置可用的 LLM：%w", err)
			}
			if err := checkQuota(userID); err != nil {
				return DurableJobResult{}, err
			}
			id, ok := currentJobResultID(ctx)
			if !ok {
				return DurableJobResult{}, errors.New("日报作业缺少结果定位")
			}
			var report model.DailyReport
			if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&report).Error; err != nil {
				return DurableJobResult{}, err
			}
			plan := reportGenPlan{userID: userID, date: req.TradeDate, manual: req.Manual,
				cfg: cfg, apiKey: apiKey, allowPrivate: llmAllowPrivate(allowPrivate, cfg),
				oldStatus: report.PreviousStatus}
			view, err := s.runGeneration(ctx, &report, plan)
			if err != nil {
				return DurableJobResult{}, err
			}
			if view == nil {
				return DurableJobResult{}, errors.New("日报作业未返回结果")
			}
			return DurableJobResult{Status: reportStatusToJobStatus(view.Status), TraceID: view.TraceID, Provider: view.Provider,
				Model: view.Model, TotalTokens: view.TotalTokens}, nil
		})
}

func reportStatusToJobStatus(status string) string {
	if status == model.ReportStatusPartial {
		return model.JobStatusDegraded
	}
	return status
}
