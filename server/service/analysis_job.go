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
)

func (s *AnalysisService) registerDurableJobHandler() {
	binding := durableJobBinding{
		resultType:               JobResultAnalysis,
		resultCommittedByHandler: true,
		create: func(tx *gorm.DB, run *model.JobRun, raw json.RawMessage) (int64, error) {
			if err := ensureEnabledJobUser(run.UserID); err != nil {
				return 0, err
			}
			var req AnalyzeRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return 0, fmt.Errorf("分析作业快照无效: %w", err)
			}
			plan, err := s.prepareAnalysis(run.UserID, isAdminUser(run.UserID), req)
			if err != nil {
				return 0, err
			}
			rec := plan.newProcessingRecord(s)
			if err := tx.Create(rec).Error; err != nil {
				return 0, err
			}
			return rec.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, _ []byte, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("分析作业缺少结果引用")
			}
			var rec model.AnalysisRecord
			if err := tx.Where("id = ? AND user_id = ?", *run.ResultID, run.UserID).First(&rec).Error; err != nil {
				return err
			}
			if rec.Status != model.AnalysisStatusSuccess && rec.Status != model.AnalysisStatusDegraded {
				return errors.New("分析结果尚未落库")
			}
			if result.Status != "" && rec.Status != result.Status {
				return errors.New("分析结果状态与作业不一致")
			}
			return persistAnalysisArtifact(tx, run, rec, now)
		},
		finishFailure: func(tx *gorm.DB, run *model.JobRun, _ string, code, message string, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("分析作业缺少结果引用")
			}
			res := tx.Model(&model.AnalysisRecord{}).
				Where("id = ? AND user_id = ? AND status = ?", *run.ResultID, run.UserID, model.AnalysisStatusProcessing).
				Updates(map[string]any{"status": model.AnalysisStatusFailed, "summary": "分析失败",
					"error": sanitizeJobError(message), "error_code": code, "updated_at": now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				var rec model.AnalysisRecord
				if err := tx.Select("id").Where("id = ? AND user_id = ?", *run.ResultID, run.UserID).First(&rec).Error; err != nil {
					return err
				}
			}
			return nil
		},
	}
	registerDurableBusinessJobHandler(JobKindAnalysis, analysisJobTimeout,
		binding, func(ctx context.Context, userID int64, allowPrivate bool, raw json.RawMessage) (DurableJobResult, error) {
			if err := ensureEnabledJobUser(userID); err != nil {
				return DurableJobResult{}, err
			}
			var req AnalyzeRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return DurableJobResult{}, fmt.Errorf("分析作业快照无效: %w", err)
			}
			plan, err := s.prepareAnalysis(userID, allowPrivate, req)
			if err != nil {
				return DurableJobResult{}, err
			}
			id, ok := currentJobResultID(ctx)
			if !ok {
				return DurableJobResult{}, errors.New("分析作业缺少结果定位")
			}
			var rec model.AnalysisRecord
			if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&rec).Error; err != nil {
				return DurableJobResult{}, err
			}
			// 配置、角色和模板均在执行时重读；旧提交参数只负责定位同一业务请求。
			rec.LLMConfigID, rec.Provider, rec.Model = plan.cfg.ID, plan.cfg.Provider, plan.cfg.Model
			rec.PromptVersion = plan.prompt.Version(analysisPromptVersion)
			view, err := s.runAnalysis(ctx, plan, &rec)
			if err != nil {
				return DurableJobResult{}, err
			}
			if view == nil {
				return DurableJobResult{}, errors.New("分析作业未返回结果")
			}
			return DurableJobResult{Status: view.Status, TraceID: view.TraceID, Provider: view.Provider,
				Model: view.Model, PromptTokens: view.PromptTokens, CompletionTokens: view.CompletionTokens,
				TotalTokens: view.TotalTokens}, nil
		})
}

func ensureEnabledJobUser(userID int64) error {
	var user model.User
	if common.DB == nil {
		return errors.New("账号已禁用，作业未执行")
	}
	err := common.DB.Select("status").First(&user, userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errors.New("账号不存在或已禁用，作业未执行")
	}
	if err != nil {
		return fmt.Errorf("读取账号状态失败: %w", err)
	}
	if user.Status != model.StatusEnabled {
		return errors.New("账号已禁用，作业未执行")
	}
	return nil
}
