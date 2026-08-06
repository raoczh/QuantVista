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

type recommendationJobRequest struct {
	Version int              `json:"version"`
	Request RecommendRequest `json:"request"`
	Manual  bool             `json:"manual"`
}

func recommendationJobRequestFromPlan(req RecommendRequest, plan *recGenPlan, manual bool) recommendationJobRequest {
	filters := plan.filters
	return recommendationJobRequest{Version: 1, Manual: manual, Request: RecommendRequest{
		Type: plan.recType, Market: plan.market, Strategy: plan.strat.Key, Count: plan.count,
		Filters: &filters, Verify: plan.verify, BearCheck: boolPtr(plan.bear), LLMConfigID: req.LLMConfigID,
	}}
}

func boolPtr(v bool) *bool { return &v }

func decodeRecommendationJobRequest(raw json.RawMessage) (recommendationJobRequest, error) {
	var wrapped recommendationJobRequest
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Request.Type != "" {
		if wrapped.Version == 0 {
			wrapped.Version = 1
		}
		return wrapped, nil
	}
	// 兼容升级前可能已经进入队列的纯 RecommendRequest 快照。
	var req RecommendRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return recommendationJobRequest{}, err
	}
	return recommendationJobRequest{Version: 1, Request: req, Manual: true}, nil
}

func (s *RecommendationService) registerDurableJobHandler() {
	binding := durableJobBinding{
		resultType:               JobResultRecommendation,
		resultCommittedByHandler: true,
		create: func(tx *gorm.DB, run *model.JobRun, raw json.RawMessage) (int64, error) {
			if err := ensureEnabledJobUser(run.UserID); err != nil {
				return 0, err
			}
			jobReq, err := decodeRecommendationJobRequest(raw)
			if err != nil {
				return 0, fmt.Errorf("推荐作业快照无效: %w", err)
			}
			plan, err := s.prepareGeneration(run.UserID, isAdminUser(run.UserID), jobReq.Request, jobReq.Manual)
			if err != nil {
				return 0, err
			}
			batch := plan.newProcessingBatch()
			if err := tx.Create(batch).Error; err != nil {
				return 0, err
			}
			return batch.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, _ []byte, _ time.Time) error {
			if run.ResultID == nil {
				return errors.New("推荐作业缺少结果引用")
			}
			var batch model.RecommendationBatch
			if err := tx.Where("id = ? AND user_id = ?", *run.ResultID, run.UserID).First(&batch).Error; err != nil {
				return err
			}
			if batch.Status != model.RecStatusSuccess && batch.Status != model.RecStatusDegraded {
				return errors.New("推荐结果尚未落库")
			}
			if result.Status != "" && batch.Status != result.Status {
				return errors.New("推荐结果状态与作业不一致")
			}
			return nil
		},
		finishFailure: func(tx *gorm.DB, run *model.JobRun, _ string, code, message string, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("推荐作业缺少结果引用")
			}
			res := tx.Model(&model.RecommendationBatch{}).
				Where("id = ? AND user_id = ? AND status = ?", *run.ResultID, run.UserID, model.RecStatusProcessing).
				Updates(map[string]any{"status": model.RecStatusFailed, "error": sanitizeJobError(message), "updated_at": now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				var batch model.RecommendationBatch
				if err := tx.Select("id").Where("id = ? AND user_id = ?", *run.ResultID, run.UserID).First(&batch).Error; err != nil {
					return err
				}
			}
			return nil
		},
	}
	registerDurableBusinessJobHandler(JobKindRecommendation, recJobTimeout, binding,
		func(ctx context.Context, userID int64, allowPrivate bool, raw json.RawMessage) (DurableJobResult, error) {
			if err := ensureEnabledJobUser(userID); err != nil {
				return DurableJobResult{}, err
			}
			jobReq, err := decodeRecommendationJobRequest(raw)
			if err != nil {
				return DurableJobResult{}, fmt.Errorf("推荐作业快照无效: %w", err)
			}
			plan, err := s.prepareGeneration(userID, allowPrivate, jobReq.Request, jobReq.Manual)
			if err != nil {
				return DurableJobResult{}, err
			}
			id, ok := currentJobResultID(ctx)
			if !ok {
				return DurableJobResult{}, errors.New("推荐作业缺少结果定位")
			}
			var batch model.RecommendationBatch
			if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&batch).Error; err != nil {
				return DurableJobResult{}, err
			}
			batch.LLMConfigID, batch.Provider, batch.Model = plan.cfg.ID, plan.cfg.Provider, plan.cfg.Model
			view, err := s.runGeneration(ctx, &batch, plan)
			if err != nil {
				return DurableJobResult{}, err
			}
			if view == nil {
				return DurableJobResult{}, errors.New("推荐作业未返回结果")
			}
			return DurableJobResult{Status: view.Status, TraceID: view.TraceID, Provider: view.Provider,
				Model: view.Model, PromptTokens: view.PromptTokens, CompletionTokens: view.CompletionTokens,
				TotalTokens: view.TotalTokens}, nil
		})
}
