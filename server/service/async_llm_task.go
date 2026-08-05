package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

const (
	AsyncLLMTaskErrorFailed       = "task_failed"
	AsyncLLMTaskErrorTimeout      = "task_timeout"
	AsyncLLMTaskErrorPanic        = "task_panic"
	AsyncLLMTaskErrorStale        = "task_stale"
	AsyncLLMTaskErrorResultEncode = "result_encode_failed"
)

// LLMTaskView 是通用后台任务的 API 视图。Result 只在详情/启动响应需要时出现；
// UserID、RequestHash 与数据库中的 ResultJSON 不直接暴露。
type LLMTaskView struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// StartAsyncLLMTask 是旧调用点的兼容入口，执行同样进入统一有界队列。override 只在
// 当前进程有效，因此生产首批四类任务应使用 StartDurableLLMTask 注册可恢复处理器。
func StartAsyncLLMTask(userID int64, kind string, request any, timeout time.Duration, runner func(context.Context) (any, error)) (*LLMTaskView, error) {
	if timeout <= 0 {
		return nil, errors.New("任务超时须大于 0")
	}
	if runner == nil {
		return nil, errors.New("任务执行函数不能为空")
	}
	handler := durableJobHandler{timeout: timeout, run: func(ctx context.Context, _ int64, _ bool, _ json.RawMessage) (DurableJobResult, error) {
		value, err := runner(ctx)
		return DurableJobResult{Value: value, Status: model.JobStatusSuccess}, err
	}}
	return defaultJobRuntime.start(userID, kind, request, false, nil, &handler)
}

// StartAsyncLLMTaskSnapshot 是需要读取不可变请求快照时使用的变体。runner 每次收到
// 独立的 RawMessage 副本，调用方后续修改原请求不会影响后台执行。
func StartAsyncLLMTaskSnapshot(userID int64, kind string, request any, timeout time.Duration, runner func(context.Context, json.RawMessage) (any, error)) (*LLMTaskView, error) {
	if timeout <= 0 {
		return nil, errors.New("任务超时须大于 0")
	}
	if runner == nil {
		return nil, errors.New("任务执行函数不能为空")
	}
	handler := durableJobHandler{timeout: timeout, run: func(ctx context.Context, _ int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
		value, err := runner(ctx, append(json.RawMessage(nil), raw...))
		return DurableJobResult{Value: value, Status: model.JobStatusSuccess}, err
	}}
	return defaultJobRuntime.start(userID, kind, request, false, nil, &handler)
}

func asyncLLMTaskErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return AsyncLLMTaskErrorTimeout
	case RefusalCodeOf(err) != "":
		return RefusalCodeOf(err)
	default:
		return AsyncLLMTaskErrorFailed
	}
}

// GetAsyncLLMTask 返回当前用户的一条任务详情，包含成功结果。
func GetAsyncLLMTask(userID, id int64) (*LLMTaskView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if err := expireStaleLLMTasks(userID); err != nil {
		return nil, err
	}
	var task model.LLMTask
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("LLM 任务不存在")
		}
		return nil, err
	}
	return llmTaskView(task, true), nil
}

// ListAsyncLLMTasks 返回当前用户的任务摘要；ResultJSON 与 RequestHash 均不读取。
func ListAsyncLLMTasks(userID int64, kind, status string, limit int) ([]LLMTaskView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if err := expireStaleLLMTasks(userID); err != nil {
		return nil, err
	}
	kind = strings.TrimSpace(kind)
	if len(kind) > 64 {
		return nil, errors.New("任务类型不能超过 64 个字符")
	}
	status = strings.TrimSpace(status)
	if status != "" && status != model.LLMTaskStatusProcessing &&
		status != model.LLMTaskStatusSuccess && status != model.LLMTaskStatusFailed {
		return nil, errors.New("任务状态须为 processing、success 或 failed")
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}

	q := common.DB.Model(&model.LLMTask{}).Where("user_id = ?", userID)
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var tasks []model.LLMTask
	err := q.Select("id", "kind", "status", "error", "error_code", "created_at", "updated_at").
		Order("id DESC").Limit(limit).Find(&tasks).Error
	if err != nil {
		return nil, err
	}
	views := make([]LLMTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, *llmTaskView(task, false))
	}
	return views, nil
}

func expireStaleLLMTasks(userID int64) error {
	if userID <= 0 {
		return errors.New("非法的用户 ID")
	}
	return common.DB.Model(&model.LLMTask{}).
		Where("user_id = ? AND job_run_id IS NULL AND status = ? AND updated_at < ?",
			userID, model.LLMTaskStatusProcessing, time.Now().Add(-taskProcessingStaleAfter)).
		Updates(map[string]any{
			"status":     model.LLMTaskStatusFailed,
			"active_key": nil,
			"error":      "任务中断（服务重启或执行超时），请重新发起",
			"error_code": AsyncLLMTaskErrorStale,
			"updated_at": time.Now(),
		}).Error
}

func llmTaskView(task model.LLMTask, withResult bool) *LLMTaskView {
	view := &LLMTaskView{
		ID: task.ID, Kind: task.Kind, Status: task.Status,
		Error: task.Error, ErrorCode: task.ErrorCode,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
	if withResult && task.ResultJSON != "" && json.Valid([]byte(task.ResultJSON)) {
		view.Result = append(json.RawMessage(nil), task.ResultJSON...)
	}
	return view
}
