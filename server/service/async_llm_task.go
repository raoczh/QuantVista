package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

const (
	asyncLLMTaskStaleAfter = 15 * time.Minute

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

// startAsyncLLMTaskMu 封住“查询在途任务 -> 创建 processing 行”的进程内竞态。
// 多实例部署若需要跨进程严格去重，应升级为数据库锁或独立任务队列。
var startAsyncLLMTaskMu sync.Mutex

// StartAsyncLLMTask 创建一个通用 LLM 后台任务。request 只用于生成稳定哈希，不会落库；
// runner 在脱离 HTTP 请求的 context.Background() 上执行。
func StartAsyncLLMTask(userID int64, kind string, request any, timeout time.Duration, runner func(context.Context) (any, error)) (*LLMTaskView, error) {
	return startAsyncLLMTask(userID, kind, request, timeout, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return runner(ctx)
	}, runner == nil)
}

// StartAsyncLLMTaskSnapshot 是需要读取不可变请求快照时使用的变体。runner 每次收到
// 独立的 RawMessage 副本，调用方后续修改原请求不会影响后台执行。
func StartAsyncLLMTaskSnapshot(userID int64, kind string, request any, timeout time.Duration, runner func(context.Context, json.RawMessage) (any, error)) (*LLMTaskView, error) {
	return startAsyncLLMTask(userID, kind, request, timeout, runner, runner == nil)
}

func startAsyncLLMTask(userID int64, kind string, request any, timeout time.Duration,
	runner func(context.Context, json.RawMessage) (any, error), runnerNil bool,
) (*LLMTaskView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 {
		return nil, errors.New("非法的用户 ID")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" || len(kind) > 64 {
		return nil, errors.New("任务类型不能为空且不能超过 64 个字符")
	}
	if timeout <= 0 {
		return nil, errors.New("任务超时须大于 0")
	}
	if runnerNil {
		return nil, errors.New("任务执行函数不能为空")
	}

	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("任务请求无法序列化: %w", err)
	}
	sum := sha256.Sum256(requestJSON)
	requestHash := hex.EncodeToString(sum[:])
	activeSum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", userID, kind, requestHash)))
	activeKey := hex.EncodeToString(activeSum[:])

	startAsyncLLMTaskMu.Lock()
	if err := expireStaleLLMTasks(userID); err != nil {
		startAsyncLLMTaskMu.Unlock()
		return nil, err
	}
	var existing model.LLMTask
	err = common.DB.Where("user_id = ? AND kind = ? AND request_hash = ? AND status = ?",
		userID, kind, requestHash, model.LLMTaskStatusProcessing).
		Order("id DESC").First(&existing).Error
	if err == nil {
		startAsyncLLMTaskMu.Unlock()
		return llmTaskView(existing, false), nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		startAsyncLLMTaskMu.Unlock()
		return nil, err
	}

	task := model.LLMTask{
		UserID: userID, Kind: kind, RequestHash: requestHash,
		ActiveKey: &activeKey, Status: model.LLMTaskStatusProcessing,
	}
	if err := common.DB.Create(&task).Error; err != nil {
		// 另一实例可能刚在查询后、创建前抢先落行；唯一 ActiveKey 冲突时回读复用。
		var raced model.LLMTask
		if qerr := common.DB.Where("user_id = ? AND kind = ? AND request_hash = ? AND status = ?",
			userID, kind, requestHash, model.LLMTaskStatusProcessing).
			Order("id DESC").First(&raced).Error; qerr == nil {
			startAsyncLLMTaskMu.Unlock()
			return llmTaskView(raced, false), nil
		}
		startAsyncLLMTaskMu.Unlock()
		return nil, err
	}
	startAsyncLLMTaskMu.Unlock()

	// 后台只持有标量 ID、timeout 与独立请求字节，不与返回给调用方的 view 共享可变对象。
	requestSnapshot := append(json.RawMessage(nil), requestJSON...)
	go runAsyncLLMTask(task.ID, timeout, requestSnapshot, runner)
	return llmTaskView(task, false), nil
}

func runAsyncLLMTask(taskID int64, timeout time.Duration, request json.RawMessage,
	runner func(context.Context, json.RawMessage) (any, error),
) {
	defer func() {
		if r := recover(); r != nil {
			finishAsyncLLMTaskFailed(taskID, AsyncLLMTaskErrorPanic,
				fmt.Sprintf("任务异常终止: %v", r))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := runner(ctx, append(json.RawMessage(nil), request...))
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		finishAsyncLLMTaskFailed(taskID, asyncLLMTaskErrorCode(err), err.Error())
		return
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		finishAsyncLLMTaskFailed(taskID, AsyncLLMTaskErrorResultEncode,
			"任务结果无法序列化: "+err.Error())
		return
	}
	res := common.DB.Model(&model.LLMTask{}).
		Where("id = ? AND status = ?", taskID, model.LLMTaskStatusProcessing).
		Updates(map[string]any{
			"status":      model.LLMTaskStatusSuccess,
			"active_key":  nil,
			"result_json": string(resultJSON),
			"error":       "",
			"error_code":  "",
			"updated_at":  time.Now(),
		})
	if res.Error != nil {
		common.SysWarn("LLM 后台任务成功状态回写失败 task=%d: %v", taskID, res.Error)
	}
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

func finishAsyncLLMTaskFailed(taskID int64, code, message string) {
	res := common.DB.Model(&model.LLMTask{}).
		Where("id = ? AND status = ?", taskID, model.LLMTaskStatusProcessing).
		Updates(map[string]any{
			"status":     model.LLMTaskStatusFailed,
			"active_key": nil,
			"error":      truncateRunes(message, 512),
			"error_code": code,
			"updated_at": time.Now(),
		})
	if res.Error != nil {
		common.SysWarn("LLM 后台任务失败状态回写失败 task=%d: %v", taskID, res.Error)
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
		Where("user_id = ? AND status = ? AND updated_at < ?",
			userID, model.LLMTaskStatusProcessing, time.Now().Add(-asyncLLMTaskStaleAfter)).
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
