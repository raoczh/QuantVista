package service

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

const (
	taskProcessingStaleAfter = 15 * time.Minute

	TaskSourceAnalysis       = "analysis"
	TaskSourceRecommendation = "recommendation"
	TaskSourceDailyReport    = "daily_report"
	TaskSourceJob            = "job"
	TaskSourceLLM            = "llm"
	TaskSourceDataSync       = "data_sync"

	TaskStatusQueued     = "queued"
	TaskStatusRunning    = "running"
	TaskStatusProcessing = "processing" // 仅用于兼容旧业务表 raw_status
	TaskStatusSuccess    = "success"
	TaskStatusDegraded   = "degraded"
	TaskStatusFailed     = "failed"
	TaskStatusCanceled   = "canceled"

	defaultTaskCenterLimit = 50
	maxTaskCenterLimit     = 100
)

// TaskCenterItem 是跨业务表稳定的只读任务摘要。任务正文与结果只由原业务详情接口读取。
type TaskCenterItem struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	SourceID    int64  `json:"source_id"`
	ResultID    int64  `json:"result_id,omitempty"`
	ParentID    *int64 `json:"parent_id,omitempty"`
	Kind        string `json:"kind"`
	Owner       string `json:"owner"`
	OwnerUserID *int64 `json:"owner_user_id,omitempty"`
	TriggeredBy *int64 `json:"triggered_by,omitempty"`
	Title       string `json:"title"`
	Target      string `json:"target"`

	Status           string `json:"status"`
	RawStatus        string `json:"raw_status"`
	Stage            string `json:"stage"`
	Error            string `json:"error"`
	ErrorCode        string `json:"error_code"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	LatencyMs        int64  `json:"latency_ms"`
	TraceID          string `json:"trace_id"`

	Total           int           `json:"total"`
	Succeeded       int           `json:"succeeded"`
	Failed          int           `json:"failed"`
	CanCancel       bool          `json:"can_cancel"`
	CanRetry        bool          `json:"can_retry"`
	CancelRequested bool          `json:"cancel_requested"`
	Steps           []JobStepView `json:"steps,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskCenterListOptions 控制统一任务列表筛选。IncludeSystem 仍会在服务层校验管理员角色。
type TaskCenterListOptions struct {
	JobID         int64
	Source        string
	Kind          string
	Status        string
	Limit         int
	IncludeSystem bool
	IncludeSteps  bool
}

type TaskCenterService struct{}

func NewTaskCenterService() *TaskCenterService { return &TaskCenterService{} }

type taskCenterFilters struct {
	jobID       int64
	source      string
	kind        string
	status      string
	rawStatuses []string
	limit       int
}

// List 聚合当前用户的业务任务；data_sync_logs 没有 user_id，仅管理员显式请求时可见。
func (s *TaskCenterService) List(userID int64, role string, options TaskCenterListOptions) ([]TaskCenterItem, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 {
		return nil, errors.New("非法的用户 ID")
	}
	filters, err := normalizeTaskCenterFilters(options)
	if err != nil {
		return nil, err
	}
	if err := expireStaleUserTasks(userID); err != nil {
		return nil, fmt.Errorf("收敛遗留任务失败: %w", err)
	}
	if filters.jobID > 0 {
		rows, err := listJobRunTasks(common.DB, userID, filters, options.IncludeSteps)
		if err != nil {
			return nil, fmt.Errorf("查询统一作业失败: %w", err)
		}
		return rows, nil
	}

	items := make([]TaskCenterItem, 0, filters.limit*4)
	if taskSourceSelected(filters.source, TaskSourceJob) {
		rows, err := listJobRunTasks(common.DB, userID, filters, options.IncludeSteps)
		if err != nil {
			return nil, fmt.Errorf("查询统一作业失败: %w", err)
		}
		items = append(items, rows...)
	}
	if role == model.RoleAdmin && options.IncludeSystem && taskSourceSelected(filters.source, TaskSourceDataSync) {
		rows, err := listSystemJobRunTasks(common.DB, filters, options.IncludeSteps)
		if err != nil {
			return nil, fmt.Errorf("查询统一系统作业失败: %w", err)
		}
		items = append(items, rows...)
	}
	if taskSourceSelected(filters.source, TaskSourceAnalysis) {
		rows, err := listAnalysisTasks(common.DB, userID, filters)
		if err != nil {
			return nil, fmt.Errorf("查询分析任务失败: %w", err)
		}
		items = append(items, rows...)
	}
	if taskSourceSelected(filters.source, TaskSourceRecommendation) {
		rows, err := listRecommendationTasks(common.DB, userID, filters)
		if err != nil {
			return nil, fmt.Errorf("查询推荐任务失败: %w", err)
		}
		items = append(items, rows...)
	}
	if taskSourceSelected(filters.source, TaskSourceDailyReport) && taskKindSelected(filters.kind, TaskSourceDailyReport) {
		rows, err := listDailyReportTasks(common.DB, userID, filters)
		if err != nil {
			return nil, fmt.Errorf("查询日报任务失败: %w", err)
		}
		items = append(items, rows...)
	}
	if taskSourceSelected(filters.source, TaskSourceLLM) {
		rows, err := listLLMTasks(common.DB, userID, filters)
		if err != nil {
			return nil, fmt.Errorf("查询通用任务失败: %w", err)
		}
		items = append(items, rows...)
	}
	// 旧日志只作兼容投影；统一 JobRun 已经在上方展示，按 job_run_id 排除重复。
	if role == model.RoleAdmin && options.IncludeSystem && taskSourceSelected(filters.source, TaskSourceDataSync) {
		rows, err := listDataSyncTasks(common.DB, filters)
		if err != nil {
			return nil, fmt.Errorf("查询系统任务失败: %w", err)
		}
		items = append(items, rows...)
	}

	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		if items[i].Source != items[j].Source {
			return items[i].Source < items[j].Source
		}
		return items[i].SourceID > items[j].SourceID
	})
	if len(items) > filters.limit {
		items = items[:filters.limit]
	}
	return items, nil
}

func normalizeTaskCenterFilters(options TaskCenterListOptions) (taskCenterFilters, error) {
	f := taskCenterFilters{
		jobID:  options.JobID,
		source: strings.ToLower(strings.TrimSpace(options.Source)),
		kind:   strings.ToLower(strings.TrimSpace(options.Kind)),
		status: strings.ToLower(strings.TrimSpace(options.Status)),
		limit:  options.Limit,
	}
	if f.jobID < 0 {
		return taskCenterFilters{}, errors.New("任务 ID 必须是正整数")
	}
	if f.limit <= 0 {
		f.limit = defaultTaskCenterLimit
	} else if f.limit > maxTaskCenterLimit {
		f.limit = maxTaskCenterLimit
	}
	if _, ok := map[string]struct{}{
		"": {}, TaskSourceAnalysis: {}, TaskSourceRecommendation: {}, TaskSourceDailyReport: {}, TaskSourceJob: {},
		TaskSourceLLM: {}, TaskSourceDataSync: {},
	}[f.source]; !ok {
		return taskCenterFilters{}, errors.New("非法的任务来源筛选")
	}
	if len(f.kind) > 64 {
		return taskCenterFilters{}, errors.New("任务类型不能超过 64 个字符")
	}
	switch f.status {
	case "":
	case TaskStatusRunning:
		f.rawStatuses = []string{TaskStatusRunning, TaskStatusProcessing}
	case TaskStatusQueued, TaskStatusCanceled, TaskStatusSuccess, TaskStatusFailed:
		f.rawStatuses = []string{f.status}
	case TaskStatusDegraded:
		f.rawStatuses = []string{TaskStatusDegraded, model.ReportStatusPartial}
	default:
		return taskCenterFilters{}, errors.New("任务状态须为 queued、running、success、degraded、failed 或 canceled")
	}
	return f, nil
}

func expireStaleUserTasks(userID int64) error {
	if err := expireStaleAnalyses(userID); err != nil {
		return err
	}
	if err := expireStaleRecommendationBatches(userID); err != nil {
		return err
	}
	if err := expireStaleDailyReports(userID); err != nil {
		return err
	}
	return expireStaleLLMTasks(userID)
}

func taskSourceSelected(filter, source string) bool { return filter == "" || filter == source }
func taskKindSelected(filter, kind string) bool     { return filter == "" || filter == kind }

func applyTaskStatusFilter(q *gorm.DB, filters taskCenterFilters) *gorm.DB {
	if len(filters.rawStatuses) > 0 {
		q = q.Where("status IN ?", filters.rawStatuses)
	}
	return q
}

type analysisTaskRow struct {
	ID               int64
	Module           string
	Symbol           string
	Target           string
	Title            string
	Status           string
	Error            string
	ErrorCode        string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	LatencyMs        int64
	TraceID          string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func listAnalysisTasks(db *gorm.DB, userID int64, filters taskCenterFilters) ([]TaskCenterItem, error) {
	q := db.Model(&model.AnalysisRecord{}).
		Select("id", "module", "symbol", "target", "title", "status", "error", "error_code",
			"provider", "model", "prompt_tokens", "completion_tokens", "total_tokens", "latency_ms",
			"trace_id", "created_at", "updated_at").
		Where("user_id = ?", userID).
		Where("NOT EXISTS (SELECT 1 FROM job_runs jr WHERE jr.user_id = analysis_records.user_id AND jr.result_type = ? AND jr.result_id = analysis_records.id)", JobResultAnalysis)
	if filters.kind != "" {
		q = q.Where("module = ?", filters.kind)
	}
	q = applyTaskStatusFilter(q, filters)
	var rows []analysisTaskRow
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]TaskCenterItem, 0, len(rows))
	for _, row := range rows {
		target := strings.TrimSpace(row.Target)
		if target == "" {
			target = row.Symbol
		}
		status := normalizeTaskStatus(row.Status)
		ownerUserID := userID
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceAnalysis, row.ID), Source: TaskSourceAnalysis, SourceID: row.ID,
			Kind: row.Module, Owner: model.JobOwnerUser, OwnerUserID: &ownerUserID,
			Title: taskTitle(row.Title, analysisTaskTitle(row.Module, target)), Target: target,
			Status: status, RawStatus: row.Status, Stage: taskStage(status), Error: row.Error,
			ErrorCode: taskErrorCode(status, row.ErrorCode, row.Error),
			Provider:  row.Provider, Model: row.Model, PromptTokens: row.PromptTokens,
			CompletionTokens: row.CompletionTokens, TotalTokens: row.TotalTokens, LatencyMs: row.LatencyMs,
			TraceID: row.TraceID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return items, nil
}

type recommendationTaskRow struct {
	ID               int64
	Type             string
	Market           string
	Title            string
	Status           string
	Error            string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	LatencyMs        int64
	TraceID          string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func listRecommendationTasks(db *gorm.DB, userID int64, filters taskCenterFilters) ([]TaskCenterItem, error) {
	q := db.Model(&model.RecommendationBatch{}).
		Select("id", "type", "market", "title", "status", "error", "provider", "model",
			"prompt_tokens", "completion_tokens", "total_tokens", "latency_ms", "trace_id", "created_at", "updated_at").
		Where("user_id = ?", userID).
		Where("NOT EXISTS (SELECT 1 FROM job_runs jr WHERE jr.user_id = recommendation_batches.user_id AND jr.result_type = ? AND jr.result_id = recommendation_batches.id)", JobResultRecommendation)
	if filters.kind != "" {
		q = q.Where("type = ?", filters.kind)
	}
	q = applyTaskStatusFilter(q, filters)
	var rows []recommendationTaskRow
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]TaskCenterItem, 0, len(rows))
	for _, row := range rows {
		status := normalizeTaskStatus(row.Status)
		ownerUserID := userID
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceRecommendation, row.ID), Source: TaskSourceRecommendation, SourceID: row.ID,
			Kind: row.Type, Owner: model.JobOwnerUser, OwnerUserID: &ownerUserID,
			Title: taskTitle(row.Title, recommendationTaskTitle(row.Type)), Target: row.Market,
			Status: status, RawStatus: row.Status, Stage: taskStage(status), Error: row.Error,
			ErrorCode: taskErrorCode(status, "", row.Error),
			Provider:  row.Provider, Model: row.Model, PromptTokens: row.PromptTokens,
			CompletionTokens: row.CompletionTokens, TotalTokens: row.TotalTokens, LatencyMs: row.LatencyMs,
			TraceID: row.TraceID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return items, nil
}

type dailyReportTaskRow struct {
	ID          int64
	TradeDate   string
	Market      string
	Status      string
	Error       string
	Provider    string
	Model       string
	TotalTokens int
	LatencyMs   int64
	TraceID     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func listDailyReportTasks(db *gorm.DB, userID int64, filters taskCenterFilters) ([]TaskCenterItem, error) {
	q := db.Model(&model.DailyReport{}).
		Select("id", "trade_date", "market", "status", "error", "provider", "model", "total_tokens",
			"latency_ms", "trace_id", "created_at", "updated_at").
		Where("user_id = ?", userID).
		Where("NOT EXISTS (SELECT 1 FROM job_runs jr WHERE jr.user_id = daily_reports.user_id AND jr.result_type = ? AND jr.result_id = daily_reports.id)", JobResultDailyReport)
	q = applyTaskStatusFilter(q, filters)
	var rows []dailyReportTaskRow
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]TaskCenterItem, 0, len(rows))
	for _, row := range rows {
		status := normalizeTaskStatus(row.Status)
		ownerUserID := userID
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceDailyReport, row.ID), Source: TaskSourceDailyReport, SourceID: row.ID,
			Kind: TaskSourceDailyReport, Owner: model.JobOwnerUser, OwnerUserID: &ownerUserID,
			Title: taskTitle("", row.TradeDate+" 收盘日报"), Target: row.TradeDate,
			Status: status, RawStatus: row.Status, Stage: taskStage(status), Error: row.Error,
			ErrorCode: taskErrorCode(status, "", row.Error),
			Provider:  row.Provider, Model: row.Model, TotalTokens: row.TotalTokens, LatencyMs: row.LatencyMs,
			TraceID: row.TraceID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return items, nil
}

type llmTaskCenterRow struct {
	ID        int64
	Kind      string
	Status    string
	Error     string
	ErrorCode string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func listJobRunTasks(db *gorm.DB, userID int64, filters taskCenterFilters, includeSteps bool) ([]TaskCenterItem, error) {
	q := db.Model(&model.JobRun{}).
		Select("id", "owner_type", "user_id", "triggered_by", "kind", "parent_id", "status", "result_id", "error", "error_code",
			"trace_id", "provider", "model", "prompt_tokens", "completion_tokens", "total_tokens",
			"latency_ms", "total", "succeeded", "failed", "cancel_requested", "created_at", "updated_at").
		Where("user_id = ?", userID)
	if filters.jobID > 0 {
		q = q.Where("id = ?", filters.jobID)
	}
	if filters.kind != "" {
		q = q.Where("kind = ?", filters.kind)
	}
	if filters.jobID == 0 {
		q = applyTaskStatusFilter(q, filters)
	}
	var runs []model.JobRun
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&runs).Error; err != nil {
		return nil, err
	}
	var steps map[int64][]JobStepView
	if includeSteps {
		ids := make([]int64, 0, len(runs))
		for _, run := range runs {
			ids = append(ids, run.ID)
		}
		var err error
		steps, err = listJobSteps(ids)
		if err != nil {
			return nil, err
		}
	}
	items := make([]TaskCenterItem, 0, len(runs))
	for _, run := range runs {
		resultID := int64(0)
		if run.ResultID != nil {
			resultID = *run.ResultID
		}
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceJob, run.ID), Source: TaskSourceJob, SourceID: run.ID,
			ResultID: resultID, ParentID: run.ParentID, Kind: run.Kind, Owner: model.JobOwnerUser,
			OwnerUserID: run.OwnerUserID, Title: llmTaskTitle(run.Kind),
			Status: run.Status, RawStatus: run.Status, Stage: taskStage(run.Status),
			Error: run.Error, ErrorCode: run.ErrorCode, TraceID: run.TraceID,
			Provider: run.Provider, Model: run.Model, PromptTokens: run.PromptTokens,
			CompletionTokens: run.CompletionTokens, TotalTokens: run.TotalTokens,
			LatencyMs: run.LatencyMs, Total: run.Total, Succeeded: run.Succeeded, Failed: run.Failed,
			CanCancel:       (run.Status == model.JobStatusQueued || run.Status == model.JobStatusRunning) && !run.CancelRequested,
			CanRetry:        run.Status == model.JobStatusFailed && isDurableJobKind(run.Kind),
			CancelRequested: run.CancelRequested,
			Steps:           steps[run.ID], CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		})
	}
	return items, nil
}

func listSystemJobRunTasks(db *gorm.DB, filters taskCenterFilters, includeSteps bool) ([]TaskCenterItem, error) {
	q := db.Model(&model.JobRun{}).
		Select("id", "owner_type", "user_id", "triggered_by", "kind", "parent_id", "status", "result_id", "error", "error_code",
			"latency_ms", "total", "succeeded", "failed", "cancel_requested", "created_at", "updated_at").
		Where("owner_type = ? AND user_id IS NULL", model.JobOwnerSystem)
	if filters.kind != "" {
		q = q.Where("kind = ?", filters.kind)
	}
	q = applyTaskStatusFilter(q, filters)
	var runs []model.JobRun
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&runs).Error; err != nil {
		return nil, err
	}
	var steps map[int64][]JobStepView
	if includeSteps {
		ids := make([]int64, 0, len(runs))
		for _, run := range runs {
			ids = append(ids, run.ID)
		}
		var err error
		steps, err = listJobSteps(ids)
		if err != nil {
			return nil, err
		}
	}
	items := make([]TaskCenterItem, 0, len(runs))
	for _, run := range runs {
		resultID := int64(0)
		if run.ResultID != nil {
			resultID = *run.ResultID
		}
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceJob, run.ID), Source: TaskSourceDataSync, SourceID: run.ID,
			ResultID: resultID, ParentID: run.ParentID, Kind: run.Kind, Owner: model.JobOwnerSystem,
			TriggeredBy: run.TriggeredBy, Title: dataSyncTaskTitle(run.Kind), Target: "cn",
			Status: run.Status, RawStatus: run.Status, Stage: taskStage(run.Status),
			Error: run.Error, ErrorCode: run.ErrorCode, LatencyMs: run.LatencyMs,
			Total: run.Total, Succeeded: run.Succeeded, Failed: run.Failed,
			CanCancel:       (run.Status == model.JobStatusQueued || run.Status == model.JobStatusRunning) && !run.CancelRequested,
			CanRetry:        run.Status == model.JobStatusFailed && isSystemDurableJobKind(run.Kind),
			CancelRequested: run.CancelRequested, Steps: steps[run.ID], CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		})
	}
	return items, nil
}

func listLLMTasks(db *gorm.DB, userID int64, filters taskCenterFilters) ([]TaskCenterItem, error) {
	q := db.Model(&model.LLMTask{}).
		Select("id", "kind", "status", "error", "error_code", "created_at", "updated_at").
		Where("user_id = ? AND job_run_id IS NULL", userID)
	if filters.kind != "" {
		q = q.Where("kind = ?", filters.kind)
	}
	q = applyTaskStatusFilter(q, filters)
	var rows []llmTaskCenterRow
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]TaskCenterItem, 0, len(rows))
	for _, row := range rows {
		status := normalizeTaskStatus(row.Status)
		ownerUserID := userID
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceLLM, row.ID), Source: TaskSourceLLM, SourceID: row.ID,
			Kind: row.Kind, Owner: model.JobOwnerUser, OwnerUserID: &ownerUserID,
			Title: llmTaskTitle(row.Kind), Status: status, RawStatus: row.Status,
			Stage: taskStage(status), Error: row.Error, ErrorCode: taskErrorCode(status, row.ErrorCode, row.Error),
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return items, nil
}

type dataSyncTaskRow struct {
	ID         int64
	Task       string
	Market     string
	Status     string
	Total      int
	Succeeded  int
	Failed     int
	DurationMs int64
	Message    string
	CreatedAt  time.Time
}

func listDataSyncTasks(db *gorm.DB, filters taskCenterFilters) ([]TaskCenterItem, error) {
	q := db.Model(&model.DataSyncLog{}).
		Select("id", "task", "market", "status", "total", "succeeded", "failed", "duration_ms", "message", "created_at").
		Where("job_run_id IS NULL")
	if filters.kind != "" {
		q = q.Where("task = ?", filters.kind)
	}
	q = applyTaskStatusFilter(q, filters)
	var rows []dataSyncTaskRow
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]TaskCenterItem, 0, len(rows))
	for _, row := range rows {
		status := normalizeTaskStatus(row.Status)
		errorMessage := ""
		if status == TaskStatusDegraded || status == TaskStatusFailed {
			errorMessage = row.Message
		}
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceDataSync, row.ID), Source: TaskSourceDataSync, SourceID: row.ID,
			Kind: row.Task, Owner: model.JobOwnerSystem, Title: dataSyncTaskTitle(row.Task), Target: row.Market,
			Status: status, RawStatus: row.Status, Stage: taskStage(status), Error: errorMessage,
			LatencyMs: row.DurationMs, Total: row.Total, Succeeded: row.Succeeded, Failed: row.Failed,
			CreatedAt: row.CreatedAt, UpdatedAt: row.CreatedAt,
		})
	}
	return items, nil
}

func normalizeTaskStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case TaskStatusQueued:
		return TaskStatusQueued
	case TaskStatusProcessing, TaskStatusRunning:
		return TaskStatusRunning
	case TaskStatusSuccess:
		return TaskStatusSuccess
	case TaskStatusDegraded, model.ReportStatusPartial:
		return TaskStatusDegraded
	case TaskStatusCanceled:
		return TaskStatusCanceled
	default:
		return TaskStatusFailed
	}
}

func taskStage(status string) string {
	if status == TaskStatusQueued {
		return TaskStatusQueued
	}
	if status == TaskStatusRunning {
		return TaskStatusRunning
	}
	return "finished"
}

func taskErrorCode(status, stored, message string) string {
	if stored = strings.TrimSpace(stored); stored != "" {
		return stored
	}
	if status == TaskStatusFailed && strings.HasPrefix(message, "任务中断（服务重启或执行超时）") {
		return AsyncLLMTaskErrorStale
	}
	return ""
}

func taskCompositeID(source string, sourceID int64) string {
	return fmt.Sprintf("%s:%d", source, sourceID)
}

func taskTitle(stored, fallback string) string {
	if stored = strings.TrimSpace(stored); stored != "" {
		return stored
	}
	return fallback
}

func analysisTaskTitle(kind, target string) string {
	labels := map[string]string{
		model.AnalysisModuleMarket: "市场分析", model.AnalysisModuleSector: "板块分析",
		model.AnalysisModuleStock: "个股分析", model.AnalysisModuleWatchlist: "自选股分析",
		model.AnalysisModulePosition: "持仓分析",
	}
	title := labels[kind]
	if title == "" {
		title = "AI 分析"
	}
	if target != "" {
		return title + " · " + target
	}
	return title
}

func recommendationTaskTitle(kind string) string {
	if kind == model.RecTypeShortTerm {
		return "短线推荐"
	}
	if kind == model.RecTypeLongTerm {
		return "长线推荐"
	}
	return "选股推荐"
}

func llmTaskTitle(kind string) string {
	switch kind {
	case JobKindAnalysis:
		return "AI 分析"
	case JobKindRecommendation:
		return "选股推荐"
	case JobKindDailyReport:
		return "收盘日报"
	case "qa":
		return "AI 问答"
	case "compare":
		return "个股对比"
	case "position_advice":
		return "持仓建议"
	case "screener_parse":
		return "选股条件解析"
	case JobKindScreenerScan:
		return "策略扫描"
	case JobKindStrategyBacktest:
		return "策略回测"
	default:
		return "AI 任务"
	}
}

func dataSyncTaskTitle(kind string) string {
	switch kind {
	case "sync_daily_bars":
		return "同步日线数据"
	case "backfill_calendar":
		return "回填交易日历"
	case "snapshot_market":
		return "生成市场快照"
	case "sync_market_wide":
		return "同步全市场数据"
	case "init_market_history":
		return "初始化市场历史"
	case JobKindDailyDiscovery:
		return "全市场候选发现"
	default:
		return "系统数据任务"
	}
}
