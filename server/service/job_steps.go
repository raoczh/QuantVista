package service

import (
	"context"
	"errors"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// jobExecution 只存在于当前 worker 的 context，不进入持久快照。
type jobExecution struct {
	runtime    *jobRuntime
	jobID      int64
	resultType string
	resultID   *int64
}

type jobExecutionKey struct{}

func withJobExecution(ctx context.Context, execution jobExecution) context.Context {
	return context.WithValue(ctx, jobExecutionKey{}, execution)
}

// withoutJobExecution 保留取消/超时链路，但隔离嵌套业务调用的步骤写入。
// 日报的推荐子流程复用 RecommendationService，却不应把 candidate_pool 等
// 子步骤伪装成 daily_report 的顶层阶段；子流程自身仍由日报 dual_generation 阶段承载。
func withoutJobExecution(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithValue(ctx, jobExecutionKey{}, jobExecution{})
}

func currentJobExecution(ctx context.Context) (jobExecution, bool) {
	if ctx == nil {
		return jobExecution{}, false
	}
	execution, ok := ctx.Value(jobExecutionKey{}).(jobExecution)
	return execution, ok && execution.runtime != nil && execution.jobID > 0
}

func currentJobResultID(ctx context.Context) (int64, bool) {
	execution, ok := currentJobExecution(ctx)
	if !ok || execution.resultID == nil || *execution.resultID <= 0 {
		return 0, false
	}
	return *execution.resultID, true
}

// JobStepTransition 记录一个真实发生的业务阶段：前一阶段若仍运行则收敛为成功，
// 再开启指定阶段。它只接受代码实际调用的阶段名，不计算或写入百分比。
func JobStepTransition(ctx context.Context, name string) error {
	execution, ok := currentJobExecution(ctx)
	if !ok {
		return nil // 同步服务调用没有 JobRun，保持原语义
	}
	return execution.runtime.transitionStep(execution.jobID, name)
}

// JobStepFinish 收敛当前阶段。失败时通常由运行时统一写入错误，此函数只用于
// 成功或明确降级的阶段边界。
func JobStepFinish(ctx context.Context, name, status string) error {
	execution, ok := currentJobExecution(ctx)
	if !ok {
		return nil
	}
	if status == "" {
		status = model.JobStatusSuccess
	}
	if status != model.JobStatusSuccess && status != model.JobStatusDegraded {
		return errors.New("非法作业步骤终态")
	}
	now := time.Now()
	return common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.JobStep{}).
			Where("job_run_id = ? AND name = ? AND status = ?", execution.jobID, name, model.JobStatusRunning).
			Updates(map[string]any{"status": status, "finished_at": now, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		return appendJobEvent(tx, execution.userID(), execution.jobID, "step", status)
	})
}

func (e jobExecution) userID() int64 {
	var run model.JobRun
	if common.DB != nil && common.DB.Select("user_id").First(&run, e.jobID).Error == nil {
		return run.UserID
	}
	return 0
}

func (r *jobRuntime) transitionStep(jobID int64, name string) error {
	if name == "" || len(name) > 24 || common.DB == nil {
		return errors.New("作业步骤名称无效")
	}
	now := time.Now()
	return common.DB.Transaction(func(tx *gorm.DB) error {
		// 已完成的同名步骤不重复创建；重试/恢复只追加事实，不覆盖历史。
		var existing model.JobStep
		if err := tx.Where("job_run_id = ? AND name = ?", jobID, name).Order("sequence DESC").First(&existing).Error; err == nil {
			if existing.Status == model.JobStatusRunning {
				return nil
			}
			if existing.Status == model.JobStatusSuccess || existing.Status == model.JobStatusDegraded {
				return nil
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Model(&model.JobStep{}).
			Where("job_run_id = ? AND status = ?", jobID, model.JobStatusRunning).
			Updates(map[string]any{"status": model.JobStatusSuccess, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		var maxSeq int
		if err := tx.Model(&model.JobStep{}).Where("job_run_id = ?", jobID).Select("COALESCE(MAX(sequence), 0)").Scan(&maxSeq).Error; err != nil {
			return err
		}
		var run model.JobRun
		if err := tx.Select("user_id").First(&run, jobID).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.JobStep{JobRunID: jobID, Sequence: maxSeq + 1, Name: name,
			Status: model.JobStatusRunning, StartedAt: &now}).Error; err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, jobID, "step", model.JobStatusRunning)
	})
}
