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
	Version            int                               `json:"version"`
	Request            RecommendRequest                  `json:"request"`
	Manual             bool                              `json:"manual"`
	PreferenceSnapshot *recommendationPreferenceSnapshot `json:"preference_snapshot,omitempty"`
	RuntimeHash        string                            `json:"runtime_hash"`
}

func recommendationJobRequestFromPlan(req RecommendRequest, plan *recGenPlan, manual bool) recommendationJobRequest {
	filters := plan.filters
	snapshot := plan.preference
	return recommendationJobRequest{Version: 4, Manual: manual, PreferenceSnapshot: &snapshot, RuntimeHash: plan.runtime.Hash, Request: RecommendRequest{
		Type: plan.recType, Market: plan.market, Strategy: plan.strat.Key, Count: plan.count,
		StrategyRevisionID: plan.strat.StrategyRevisionID,
		Filters:            &filters, Verify: plan.verify, BearCheck: boolPtr(plan.bear), LLMConfigID: req.LLMConfigID,
	}}
}

func boolPtr(v bool) *bool { return &v }

func decodeRecommendationJobRequest(raw json.RawMessage) (recommendationJobRequest, error) {
	var wrapped recommendationJobRequest
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Request.Type != "" {
		if wrapped.Version == 0 {
			wrapped.Version = 1
		}
		if wrapped.Version < 4 {
			return recommendationJobRequest{}, errors.New("旧推荐任务未冻结完整算法版本，请重新生成；已完成的历史结果不变")
		}
		if wrapped.Version != 4 || len(wrapped.RuntimeHash) != 64 {
			return recommendationJobRequest{}, errors.New("推荐作业版本不支持或缺少完整运行快照")
		}
		return wrapped, nil
	}
	// 兼容升级前可能已经进入队列的纯 RecommendRequest 快照。
	var req RecommendRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return recommendationJobRequest{}, err
	}
	return recommendationJobRequest{}, errors.New("旧推荐任务未冻结完整算法版本，请重新生成；已完成的历史结果不变")
}

func (s *RecommendationService) recommendationJobBinding() durableJobBinding {
	return durableJobBinding{
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
			// 重试从同用户的原结果事实复制完整计划；JobRun 只保存版本摘要。
			if run.ParentID == nil {
				return 0, errors.New("推荐重试缺少原任务计划")
			}
			var parent model.JobRun
			if err := tx.Where("id = ? AND user_id = ? AND kind = ? AND result_type = ?", *run.ParentID, run.UserID, JobKindRecommendation, JobResultRecommendation).First(&parent).Error; err != nil || parent.ResultID == nil {
				return 0, errors.New("原推荐任务引用不可用")
			}
			var previous model.RecommendationBatch
			if err := tx.Where("id = ? AND user_id = ?", *parent.ResultID, run.UserID).First(&previous).Error; err != nil {
				return 0, err
			}
			if err := attachRecommendationJobRuntime(&jobReq, previous); err != nil {
				return 0, err
			}
			plan, err := s.prepareGenerationWithSnapshot(run.UserID, isAdminUser(run.UserID), jobReq.Request,
				jobReq.Manual, jobReq.PreferenceSnapshot, tx.Statement.Context)
			if err != nil {
				return 0, err
			}
			batch := plan.newProcessingBatch()
			if err := tx.Create(batch).Error; err != nil {
				return 0, err
			}
			return batch.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, _ []byte, now time.Time) error {
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
			return persistRecommendationArtifact(tx, run, batch, now)
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
}

func attachRecommendationJobRuntime(job *recommendationJobRequest, batch model.RecommendationBatch) error {
	var runtime recommendationRuntimeSnapshot
	if len(batch.RuntimeSnapshot) > 128<<10 || json.Unmarshal([]byte(batch.RuntimeSnapshot), &runtime) != nil || runtime.Hash != job.RuntimeHash || runtime.UserID != batch.UserID || batch.ScoringVersion != runtime.Scoring.Algorithm || batch.ScoringArtifactHash != runtime.Scoring.ArtifactHash {
		return errors.New("批次与已接受任务的运行快照不一致")
	}
	if _, err := thawRecommendationRuntime(&runtime, batch.UserID, job.Request); err != nil {
		return err
	}
	job.Request.runtimeSnapshot = &runtime
	return nil
}

func (s *RecommendationService) recommendationSeedBinding(plan *recGenPlan) durableJobBinding {
	binding := s.recommendationJobBinding()
	binding.create = func(tx *gorm.DB, run *model.JobRun, raw json.RawMessage) (int64, error) {
		if err := ensureEnabledJobUser(run.UserID, tx.Statement.Context); err != nil {
			return 0, err
		}
		job, err := decodeRecommendationJobRequest(raw)
		if err != nil {
			return 0, err
		}
		if run.UserID != plan.userID || job.RuntimeHash != plan.runtime.Hash {
			return 0, errors.New("提交计划与作业摘要不一致")
		}
		batch := plan.newProcessingBatch()
		if err := tx.Create(batch).Error; err != nil {
			return 0, err
		}
		return batch.ID, nil
	}
	return binding
}

func (s *RecommendationService) registerDurableJobHandler() {
	binding := s.recommendationJobBinding()
	registerDurableBusinessJobHandler(JobKindRecommendation, recJobTimeout, binding,
		func(ctx context.Context, userID int64, allowPrivate bool, raw json.RawMessage) (DurableJobResult, error) {
			if err := ensureEnabledJobUser(userID); err != nil {
				return DurableJobResult{}, err
			}
			jobReq, err := decodeRecommendationJobRequest(raw)
			if err != nil {
				return DurableJobResult{}, fmt.Errorf("推荐作业快照无效: %w", err)
			}
			id, ok := currentJobResultID(ctx)
			if !ok {
				return DurableJobResult{}, errors.New("推荐作业缺少结果定位")
			}
			var batch model.RecommendationBatch
			if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&batch).Error; err != nil {
				return DurableJobResult{}, err
			}
			if err := attachRecommendationJobRuntime(&jobReq, batch); err != nil {
				return DurableJobResult{}, err
			}
			plan, err := s.prepareGenerationWithSnapshot(userID, allowPrivate, jobReq.Request, jobReq.Manual, jobReq.PreferenceSnapshot, ctx)
			if err != nil {
				return DurableJobResult{}, err
			}
			batch.LLMConfigID, batch.Provider, batch.Model = plan.cfg.ID, plan.cfg.Provider, plan.cfg.Model
			// 模板按执行时配置读取，审计版本必须与实际发送正文一致。
			batch.PromptVersion = plan.prompt.Version(recPromptVersion)
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
