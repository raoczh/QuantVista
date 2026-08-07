package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

type taskCenterLister interface {
	List(userID int64, role string, options service.TaskCenterListOptions) ([]service.TaskCenterItem, error)
}

type taskCenterJobs interface {
	GetJob(userID, id int64) (*service.JobRunView, error)
	CancelJob(userID, id int64) (*service.JobRunView, error)
	RetryJob(userID, id int64) (*service.JobRunView, error)
	Events(userID, afterID, limit int64) ([]service.JobEventView, error)
}

type taskCenterMetrics interface {
	Metrics(actorID int64) (*service.JobRuntimeMetrics, error)
}

var (
	taskEventPollInterval      = time.Second
	taskEventHeartbeatInterval = 15 * time.Second
)

// TaskCenterController 提供当前用户跨业务域的只读任务列表。
type TaskCenterController struct {
	svc     taskCenterLister
	jobs    taskCenterJobs
	metrics taskCenterMetrics
}

func NewTaskCenterController(svc taskCenterLister) *TaskCenterController {
	controller := &TaskCenterController{svc: svc}
	if jobs, ok := svc.(taskCenterJobs); ok {
		controller.jobs = jobs
	}
	if metrics, ok := svc.(taskCenterMetrics); ok {
		controller.metrics = metrics
	}
	return controller
}

// Metrics GET /api/admin/jobs/metrics。聚合查询不选择请求快照或业务正文。
func (tc *TaskCenterController) Metrics(c *gin.Context) {
	if tc.metrics == nil {
		common.ApiErrorMsg(c, "作业指标服务不可用")
		return
	}
	metrics, err := tc.metrics.Metrics(currentUserID(c))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, metrics)
}

// List GET /api/tasks?source=&kind=&status=&limit=&include_system=1
func (tc *TaskCenterController) List(c *gin.Context) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	items, err := tc.svc.List(currentUserID(c), currentRole(c), service.TaskCenterListOptions{
		Source:        c.Query("source"),
		Kind:          c.Query("kind"),
		Status:        c.Query("status"),
		Limit:         limit,
		IncludeSystem: c.Query("include_system") == "1" && currentRole(c) == model.RoleAdmin,
		IncludeSteps:  c.Query("include_steps") == "1",
	})
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, items)
}

// Get GET /api/tasks/:id，当前只接受统一 JobRun 的数字 ID。
func (tc *TaskCenterController) Get(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if tc.jobs == nil {
		common.ApiErrorMsg(c, "作业服务不可用")
		return
	}
	view, err := tc.jobs.GetJob(currentUserID(c), id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, view)
}

// Cancel POST /api/tasks/:id/cancel。queued 直接终止，running 设置协作取消标志。
func (tc *TaskCenterController) Cancel(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if tc.jobs == nil {
		common.ApiErrorMsg(c, "作业服务不可用")
		return
	}
	view, err := tc.jobs.CancelJob(currentUserID(c), id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, view)
}

// Retry POST /api/tasks/:id/retry。从失败作业的持久快照创建新 run，旧 run 不变。
func (tc *TaskCenterController) Retry(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if tc.jobs == nil {
		common.ApiErrorMsg(c, "作业服务不可用")
		return
	}
	view, err := tc.jobs.RetryJob(currentUserID(c), id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, view)
}

// Events GET /api/tasks/events。Authorization 由外层 JWT 中间件读取；续传位置只从
// 标准 Last-Event-ID 请求头读取，避免令牌或游标与认证信息混入 URL。
func (tc *TaskCenterController) Events(c *gin.Context) {
	if tc.jobs == nil {
		common.ApiErrorMsg(c, "作业服务不可用")
		return
	}
	afterID := int64(0)
	if raw := strings.TrimSpace(c.GetHeader("Last-Event-ID")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			common.ApiErrorMsg(c, "Last-Event-ID 必须是非负整数")
			return
		}
		afterID = parsed
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return
	}
	_, _ = fmt.Fprint(c.Writer, "retry: 3000\n\n")
	flusher.Flush()

	poll := time.NewTicker(taskEventPollInterval)
	heartbeat := time.NewTicker(taskEventHeartbeatInterval)
	defer poll.Stop()
	defer heartbeat.Stop()

	writeEvents := func() error {
		events, err := tc.jobs.Events(currentUserID(c), afterID, 100)
		if err != nil {
			return err
		}
		for _, event := range events {
			payload, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(c.Writer, "id: %d\nevent: job\ndata: %s\n\n", event.ID, payload); err != nil {
				return err
			}
			afterID = event.ID
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		return nil
	}
	if err := writeEvents(); err != nil {
		return
	}
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-poll.C:
			if err := writeEvents(); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(c.Writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
