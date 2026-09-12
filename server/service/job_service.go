package service

import (
	"context"
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
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Owner       string `json:"owner"`
	OwnerUserID *int64 `json:"owner_user_id,omitempty"`
	TriggeredBy *int64 `json:"triggered_by,omitempty"`
	ParentID    *int64 `json:"parent_id,omitempty"`
	Status      string `json:"status"`

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

func (s *TaskCenterService) GetJob(userID, id int64, contexts ...context.Context) (*JobRunView, error) {
	return GetJobRun(userID, id, true, contexts...)
}

func (s *TaskCenterService) CancelJob(userID, id int64, contexts ...context.Context) (*JobRunView, error) {
	return CancelJobRun(userID, id, contexts...)
}

func (s *TaskCenterService) RetryJob(userID, id int64, contexts ...context.Context) (*JobRunView, error) {
	return RetryJobRun(userID, id, contexts...)
}

func (s *TaskCenterService) Events(userID, afterID, limit int64, contexts ...context.Context) ([]JobEventView, error) {
	return ListJobEvents(userID, afterID, limit, contexts...)
}

func (s *TaskCenterService) Metrics(actorID int64) (*JobRuntimeMetrics, error) {
	return GetJobRuntimeMetrics(actorID)
}

func StartDurableLLMTask(userID int64, kind string, request any, allowPrivate bool, contexts ...context.Context) (*LLMTaskView, error) {
	if !isLegacyDurableJobKind(kind) {
		return nil, fmt.Errorf("%w: %s", ErrJobKindUnsupported, kind)
	}
	return defaultJobRuntime.start(userID, kind, request, allowPrivate, nil, nil, contexts...)
}

// startDurableBusinessJob 保留内部调用兼容入口。
func startDurableBusinessJob(userID int64, kind string, request any, allowPrivate bool) (*model.JobRun, error) {
	return startDurableBusinessJobContext(context.Background(), userID, kind, request, allowPrivate, nil)
}

// 新作业的业务回执在创建事务内读取；复用已有作业时只读其已有引用。
func startDurableBusinessJobContext(ctx context.Context, userID int64, kind string, request any, allowPrivate bool, receipt func(*gorm.DB, *model.JobRun) error) (*model.JobRun, error) {
	ctx = jobSubmissionContext(ctx)
	if !isBusinessDurableJobKind(kind) {
		return nil, fmt.Errorf("%w: %s", ErrJobKindUnsupported, kind)
	}
	var bindingOverride *durableJobBinding
	var receiptRunID int64
	if receipt != nil {
		handler, ok := defaultJobRuntime.handler(kind)
		if !ok {
			return nil, ErrJobKindUnsupported
		}
		binding := handler.binding
		binding.readSubmission = func(tx *gorm.DB, run *model.JobRun) error {
			if err := receipt(tx, run); err != nil {
				return err
			}
			receiptRunID = run.ID
			return nil
		}
		bindingOverride = &binding
	}
	run, err := defaultJobRuntime.startWithBinding(userID, kind, request, allowPrivate, nil, nil, bindingOverride, ctx)
	if err != nil {
		return nil, err
	}
	if receipt != nil && receiptRunID != run.ID {
		if err := readSnapshotTx(ctx, func(tx *gorm.DB) error { return receipt(tx, run) }); err != nil {
			return nil, err
		}
	}
	return run, nil
}

func GetJobRun(userID, id int64, withSteps bool, contexts ...context.Context) (*JobRunView, error) {
	ctx := jobSubmissionContext(contexts...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 || id <= 0 {
		return nil, ErrJobNotFound
	}
	var view *JobRunView
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		var readErr error
		view, readErr = getJobRunDB(tx, userID, id, withSteps)
		return readErr
	})
	return view, err
}

func getJobRunDB(db *gorm.DB, userID, id int64, withSteps bool) (*JobRunView, error) {
	run, err := loadAuthorizedJobRun(db, userID, id)
	if err != nil {
		return nil, err
	}
	view := jobRunView(*run)
	if withSteps {
		steps, err := listJobStepsDB(db, []int64{run.ID})
		if err != nil {
			return nil, err
		}
		view.Steps = steps[run.ID]
	}
	return view, nil
}

func CancelJobRun(userID, id int64, contexts ...context.Context) (*JobRunView, error) {
	ctx := jobSubmissionContext(contexts...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 || id <= 0 {
		return nil, ErrJobNotFound
	}
	now := time.Now()
	runningCanceled := false
	var view *JobRunView
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := func() error {
			runPtr, err := loadAuthorizedJobRun(tx, userID, id)
			if err != nil {
				return err
			}
			run := *runPtr
			switch run.Status {
			case model.JobStatusQueued:
				res := tx.Model(&model.JobRun{}).
					Where("id = ? AND status = ?", id, model.JobStatusQueued).
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
					binding := defaultJobRuntime.bindingForRun(run)
					if binding.finishFailure == nil {
						return errors.New("作业结果绑定不可用")
					}
					if err := binding.finishFailure(tx, &run, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
						return err
					}
				}
				if err := finishRunningJobSteps(tx, id, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
					return err
				}
				return appendJobEventForRun(tx, &run, "status", model.JobStatusCanceled)
			case model.JobStatusRunning:
				res := tx.Model(&model.JobRun{}).
					Where("id = ? AND status = ? AND cancel_requested = ?",
						id, model.JobStatusRunning, false).
					Updates(map[string]any{"cancel_requested": true, "updated_at": now})
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					return ErrJobNotCancelable
				}
				runningCanceled = true
				return appendJobEventForRun(tx, &run, "cancel_requested", model.JobStatusRunning)
			default:
				return ErrJobNotCancelable
			}
		}(); err != nil {
			return err
		}
		var err error
		view, err = getJobRunDB(tx, userID, id, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	if runningCanceled {
		defaultJobRuntime.signalCancel(id)
	}
	return view, nil
}

func RetryJobRun(userID, id int64, contexts ...context.Context) (*JobRunView, error) {
	ctx := jobSubmissionContext(contexts...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	parentPtr, err := loadAuthorizedJobRun(common.DB.WithContext(ctx), userID, id)
	if err != nil {
		return nil, err
	}
	parent := *parentPtr
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
	var child *model.JobRun
	if parent.OwnerType == model.JobOwnerSystem {
		triggeredBy := userID
		child, err = defaultJobRuntime.startSystemWithBinding(&triggeredBy, parent.Kind, json.RawMessage(snapshot.Request), &parent.ID, ctx)
	} else if isBusinessDurableJobKind(parent.Kind) {
		child, err = defaultJobRuntime.startWithBinding(userID, parent.Kind, json.RawMessage(snapshot.Request), false, &parent.ID, nil, nil, ctx)
	} else {
		var task *LLMTaskView
		task, err = defaultJobRuntime.start(userID, parent.Kind, json.RawMessage(snapshot.Request), false, &parent.ID, nil, ctx)
		if err == nil {
			var compatibility model.LLMTask
			if lookupErr := common.DB.WithContext(ctx).Select("job_run_id").Where("id = ? AND user_id = ?", task.ID, userID).First(&compatibility).Error; lookupErr != nil {
				return nil, lookupErr
			}
			if compatibility.JobRunID == nil {
				return nil, errors.New("重跑作业缺少事实关联")
			}
			return GetJobRun(userID, *compatibility.JobRunID, true, ctx)
		}
	}
	if err != nil {
		return nil, err
	}
	if child == nil || child.ID == 0 {
		return nil, errors.New("重跑作业缺少事实关联")
	}
	return GetJobRun(userID, child.ID, true, ctx)
}

func ListJobEvents(userID, afterID, limit int64, contexts ...context.Context) ([]JobEventView, error) {
	ctx := jobSubmissionContext(contexts...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	if err := common.DB.WithContext(ctx).Select("id", "job_run_id", "type", "status", "created_at").
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
	return listJobStepsDB(common.DB, jobIDs)
}

func listJobStepsDB(db *gorm.DB, jobIDs []int64) (map[int64][]JobStepView, error) {
	grouped := make(map[int64][]JobStepView, len(jobIDs))
	if len(jobIDs) == 0 {
		return grouped, nil
	}
	var steps []model.JobStep
	if err := db.Select("id", "job_run_id", "sequence", "name", "status", "error", "error_code", "started_at", "finished_at").
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
		ID: run.ID, Kind: run.Kind, Owner: run.OwnerType, OwnerUserID: run.OwnerUserID,
		TriggeredBy: run.TriggeredBy, ParentID: run.ParentID, Status: run.Status,
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
	return isLegacyDurableJobKind(kind) || isBusinessDurableJobKind(kind) || isSystemDurableJobKind(kind)
}

func isSystemDurableJobKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case JobKindSyncDailyBars, JobKindBackfillCalendar, JobKindSnapshotMarket,
		JobKindSyncMarketWide, JobKindInitMarketHistory, JobKindFactorRebuild, JobKindDailyDiscovery,
		JobKindCandidateAudit:
		return true
	default:
		return false
	}
}

func loadAuthorizedJobRun(db *gorm.DB, actorID, id int64) (*model.JobRun, error) {
	if actorID <= 0 || id <= 0 {
		return nil, ErrJobNotFound
	}
	var run model.JobRun
	if err := db.First(&run, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if run.OwnerType == model.JobOwnerUser && run.OwnerUserID != nil && *run.OwnerUserID == actorID {
		return &run, nil
	}
	if run.OwnerType == model.JobOwnerSystem {
		var actor model.User
		if err := db.Select("role", "status").First(&actor, actorID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrJobNotFound
			}
			return nil, err
		}
		if actor.Role == model.RoleAdmin && actor.Status == model.StatusEnabled {
			return &run, nil
		}
	}
	return nil, ErrJobNotFound
}

func isLegacyDurableJobKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case JobKindQA, JobKindCompare, JobKindPositionAdvice, JobKindScreenerParse:
		return true
	default:
		return false
	}
}

func isBusinessDurableJobKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case JobKindAnalysis, JobKindRecommendation, JobKindDailyReport,
		JobKindScreenerScan, JobKindStrategyBacktest:
		return true
	default:
		return false
	}
}
