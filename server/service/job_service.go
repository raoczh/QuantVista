package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

type JobStepView struct {
	ID         int64      `json:"id"`
	Sequence   int        `json:"sequence"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	ErrorCode  string     `json:"error_code,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// JobRunView 刻意不含 RequestSnapshot、RequestHash、ActiveKey 与 UserID。
type JobRunView struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	ParentID *int64 `json:"parent_id,omitempty"`
	Status   string `json:"status"`

	ResultType string `json:"result_type,omitempty"`
	ResultID   *int64 `json:"result_id,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`

	PromptTokens     int   `json:"prompt_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	TotalTokens      int   `json:"total_tokens"`
	LatencyMs        int64 `json:"latency_ms"`
	Total            int   `json:"total"`
	Succeeded        int   `json:"succeeded"`
	Failed           int   `json:"failed"`

	CancelRequested bool          `json:"cancel_requested"`
	Steps           []JobStepView `json:"steps,omitempty"`
	QueuedAt        time.Time     `json:"queued_at"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	FinishedAt      *time.Time    `json:"finished_at,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

type JobEventView struct {
	ID        int64     `json:"id"`
	JobRunID  int64     `json:"job_run_id"`
	Type      string    `json:"type"`
	Status    string    `json:"status,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *TaskCenterService) GetJob(userID, id int64) (*JobRunView, error) {
	return GetJobRun(userID, id, true)
}

func (s *TaskCenterService) CancelJob(userID, id int64) (*JobRunView, error) {
	return CancelJobRun(userID, id)
}

func (s *TaskCenterService) RetryJob(userID, id int64) (*JobRunView, error) {
	return RetryJobRun(userID, id)
}

func (s *TaskCenterService) Events(userID, afterID, limit int64) ([]JobEventView, error) {
	return ListJobEvents(userID, afterID, limit)
}

func StartDurableLLMTask(userID int64, kind string, request any, allowPrivate bool) (*LLMTaskView, error) {
	if !isDurableJobKind(kind) {
		return nil, fmt.Errorf("%w: %s", ErrJobKindUnsupported, kind)
	}
	return defaultJobRuntime.start(userID, kind, request, allowPrivate, nil, nil)
}

func GetJobRun(userID, id int64, withSteps bool) (*JobRunView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 || id <= 0 {
		return nil, ErrJobNotFound
	}
	var run model.JobRun
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	view := jobRunView(run)
	if withSteps {
		steps, err := listJobSteps([]int64{run.ID})
		if err != nil {
			return nil, err
		}
		view.Steps = steps[run.ID]
	}
	return view, nil
}

func CancelJobRun(userID, id int64) (*JobRunView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 || id <= 0 {
		return nil, ErrJobNotFound
	}
	now := time.Now()
	runningCanceled := false
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var run model.JobRun
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&run).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrJobNotFound
			}
			return err
		}
		switch run.Status {
		case model.JobStatusQueued:
			res := tx.Model(&model.JobRun{}).
				Where("id = ? AND user_id = ? AND status = ?", id, userID, model.JobStatusQueued).
				Updates(map[string]any{
					"status": model.JobStatusCanceled, "cancel_requested": true, "active_key": nil,
					"error": "作业已取消", "error_code": JobErrorCanceled,
					"finished_at": now, "updated_at": now,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrJobNotCancelable
			}
			if run.ResultID != nil {
				if err := failCompatibleLLMTask(tx, *run.ResultID, userID, JobErrorCanceled, "作业已取消", now); err != nil {
					return err
				}
			}
			if err := finishRunningJobSteps(tx, id, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
				return err
			}
			return appendJobEvent(tx, userID, id, "status", model.JobStatusCanceled)
		case model.JobStatusRunning:
			res := tx.Model(&model.JobRun{}).
				Where("id = ? AND user_id = ? AND status = ? AND cancel_requested = ?",
					id, userID, model.JobStatusRunning, false).
				Updates(map[string]any{"cancel_requested": true, "updated_at": now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrJobNotCancelable
			}
			runningCanceled = true
			return appendJobEvent(tx, userID, id, "cancel_requested", model.JobStatusRunning)
		default:
			return ErrJobNotCancelable
		}
	})
	if err != nil {
		return nil, err
	}
	if runningCanceled {
		defaultJobRuntime.signalCancel(id)
	}
	return GetJobRun(userID, id, true)
}

func RetryJobRun(userID, id int64) (*JobRunView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	var parent model.JobRun
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&parent).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if parent.Status != model.JobStatusFailed {
		return nil, errors.New("只有失败作业可以重跑")
	}
	if !isDurableJobKind(parent.Kind) || !defaultJobRuntime.hasHandler(parent.Kind) {
		return nil, fmt.Errorf("%w: %s", ErrJobKindUnsupported, parent.Kind)
	}
	snapshot, err := decodePersistedJobSnapshot(parent)
	if err != nil {
		return nil, err
	}
	task, err := defaultJobRuntime.start(
		userID, parent.Kind, json.RawMessage(snapshot.Request), false, &parent.ID, nil,
	)
	if err != nil {
		return nil, err
	}
	var compatibility model.LLMTask
	if err := common.DB.Select("job_run_id").Where("id = ? AND user_id = ?", task.ID, userID).
		First(&compatibility).Error; err != nil {
		return nil, err
	}
	if compatibility.JobRunID == nil {
		return nil, errors.New("重跑作业缺少事实关联")
	}
	return GetJobRun(userID, *compatibility.JobRunID, true)
}

func ListJobEvents(userID, afterID, limit int64) ([]JobEventView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 {
		return nil, errors.New("非法的用户 ID")
	}
	if afterID < 0 {
		afterID = 0
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var events []model.JobEvent
	if err := common.DB.Select("id", "job_run_id", "type", "status", "created_at").
		Where("user_id = ? AND id > ?", userID, afterID).
		Order("id ASC").Limit(int(limit)).Find(&events).Error; err != nil {
		return nil, err
	}
	views := make([]JobEventView, 0, len(events))
	for _, event := range events {
		views = append(views, JobEventView{
			ID: event.ID, JobRunID: event.JobRunID, Type: event.Type,
			Status: event.Status, CreatedAt: event.CreatedAt,
		})
	}
	return views, nil
}

func listJobSteps(jobIDs []int64) (map[int64][]JobStepView, error) {
	grouped := make(map[int64][]JobStepView, len(jobIDs))
	if len(jobIDs) == 0 {
		return grouped, nil
	}
	var steps []model.JobStep
	if err := common.DB.Select("id", "job_run_id", "sequence", "name", "status", "error", "error_code", "started_at", "finished_at").
		Where("job_run_id IN ?", jobIDs).Order("job_run_id ASC, sequence ASC").Find(&steps).Error; err != nil {
		return nil, err
	}
	for _, step := range steps {
		grouped[step.JobRunID] = append(grouped[step.JobRunID], JobStepView{
			ID: step.ID, Sequence: step.Sequence, Name: step.Name, Status: step.Status,
			Error: step.Error, ErrorCode: step.ErrorCode,
			StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
		})
	}
	return grouped, nil
}

func jobRunView(run model.JobRun) *JobRunView {
	return &JobRunView{
		ID: run.ID, Kind: run.Kind, ParentID: run.ParentID, Status: run.Status,
		ResultType: run.ResultType, ResultID: run.ResultID,
		Error: run.Error, ErrorCode: run.ErrorCode, TraceID: run.TraceID,
		Provider: run.Provider, Model: run.Model,
		PromptTokens: run.PromptTokens, CompletionTokens: run.CompletionTokens,
		TotalTokens: run.TotalTokens, LatencyMs: run.LatencyMs,
		Total: run.Total, Succeeded: run.Succeeded, Failed: run.Failed,
		CancelRequested: run.CancelRequested, QueuedAt: run.QueuedAt,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
}

func isDurableJobKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case JobKindQA, JobKindCompare, JobKindPositionAdvice, JobKindScreenerParse:
		return true
	default:
		return false
	}
}
