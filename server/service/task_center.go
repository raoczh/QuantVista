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
	TaskSourceLLM            = "llm"
	TaskSourceDataSync       = "data_sync"

	TaskStatusProcessing = "processing"
	TaskStatusSuccess    = "success"
	TaskStatusDegraded   = "degraded"
	TaskStatusFailed     = "failed"

	defaultTaskCenterLimit = 50
	maxTaskCenterLimit     = 100
)

// TaskCenterItem 是跨业务表稳定的只读任务摘要。任务正文与结果只由原业务详情接口读取。
type TaskCenterItem struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	SourceID int64  `json:"source_id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Target   string `json:"target"`

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

	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskCenterListOptions 控制统一任务列表筛选。IncludeSystem 仍会在服务层校验管理员角色。
type TaskCenterListOptions struct {
	Source        string
	Kind          string
	Status        string
	Limit         int
	IncludeSystem bool
}

type TaskCenterService struct{}

func NewTaskCenterService() *TaskCenterService { return &TaskCenterService{} }

type taskCenterFilters struct {
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

	items := make([]TaskCenterItem, 0, filters.limit*4)
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
	// 防御性权限边界：调用方即使误传 IncludeSystem，非管理员也不会查询无用户归属的系统日志。
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
		source: strings.ToLower(strings.TrimSpace(options.Source)),
		kind:   strings.ToLower(strings.TrimSpace(options.Kind)),
		status: strings.ToLower(strings.TrimSpace(options.Status)),
		limit:  options.Limit,
	}
	if f.limit <= 0 {
		f.limit = defaultTaskCenterLimit
	} else if f.limit > maxTaskCenterLimit {
		f.limit = maxTaskCenterLimit
	}
	if _, ok := map[string]struct{}{
		"": {}, TaskSourceAnalysis: {}, TaskSourceRecommendation: {}, TaskSourceDailyReport: {},
		TaskSourceLLM: {}, TaskSourceDataSync: {},
	}[f.source]; !ok {
		return taskCenterFilters{}, errors.New("非法的任务来源筛选")
	}
	if len(f.kind) > 64 {
		return taskCenterFilters{}, errors.New("任务类型不能超过 64 个字符")
	}
	switch f.status {
	case "":
	case TaskStatusProcessing, TaskStatusSuccess, TaskStatusFailed:
		f.rawStatuses = []string{f.status}
	case TaskStatusDegraded:
		f.rawStatuses = []string{TaskStatusDegraded, model.ReportStatusPartial}
	default:
		return taskCenterFilters{}, errors.New("任务状态须为 processing、success、degraded 或 failed")
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
		Where("user_id = ?", userID)
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
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceAnalysis, row.ID), Source: TaskSourceAnalysis, SourceID: row.ID,
			Kind: row.Module, Title: taskTitle(row.Title, analysisTaskTitle(row.Module, target)), Target: target,
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
		Where("user_id = ?", userID)
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
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceRecommendation, row.ID), Source: TaskSourceRecommendation, SourceID: row.ID,
			Kind: row.Type, Title: taskTitle(row.Title, recommendationTaskTitle(row.Type)), Target: row.Market,
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
		Where("user_id = ?", userID)
	q = applyTaskStatusFilter(q, filters)
	var rows []dailyReportTaskRow
	if err := q.Order("created_at DESC, id DESC").Limit(filters.limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]TaskCenterItem, 0, len(rows))
	for _, row := range rows {
		status := normalizeTaskStatus(row.Status)
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceDailyReport, row.ID), Source: TaskSourceDailyReport, SourceID: row.ID,
			Kind: TaskSourceDailyReport, Title: taskTitle("", row.TradeDate+" 收盘日报"), Target: row.TradeDate,
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

func listLLMTasks(db *gorm.DB, userID int64, filters taskCenterFilters) ([]TaskCenterItem, error) {
	q := db.Model(&model.LLMTask{}).
		Select("id", "kind", "status", "error", "error_code", "created_at", "updated_at").
		Where("user_id = ?", userID)
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
		items = append(items, TaskCenterItem{
			ID: taskCompositeID(TaskSourceLLM, row.ID), Source: TaskSourceLLM, SourceID: row.ID,
			Kind: row.Kind, Title: llmTaskTitle(row.Kind), Status: status, RawStatus: row.Status,
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
		Select("id", "task", "market", "status", "total", "succeeded", "failed", "duration_ms", "message", "created_at")
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
			Kind: row.Task, Title: dataSyncTaskTitle(row.Task), Target: row.Market,
			Status: status, RawStatus: row.Status, Stage: taskStage(status), Error: errorMessage,
			LatencyMs: row.DurationMs, Total: row.Total, Succeeded: row.Succeeded, Failed: row.Failed,
			CreatedAt: row.CreatedAt, UpdatedAt: row.CreatedAt,
		})
	}
	return items, nil
}

func normalizeTaskStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case TaskStatusProcessing:
		return TaskStatusProcessing
	case TaskStatusSuccess:
		return TaskStatusSuccess
	case TaskStatusDegraded, model.ReportStatusPartial:
		return TaskStatusDegraded
	default:
		return TaskStatusFailed
	}
}

func taskStage(status string) string {
	if status == TaskStatusProcessing {
		return "processing"
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
	case "qa":
		return "AI 问答"
	case "compare":
		return "个股对比"
	case "position_advice":
		return "持仓建议"
	case "screener_parse":
		return "选股条件解析"
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
	default:
		return "系统数据任务"
	}
}
