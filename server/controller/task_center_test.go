package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

type taskCenterListerStub struct {
	userID  int64
	role    string
	options service.TaskCenterListOptions
	items   []service.TaskCenterItem
	err     error
}

func (s *taskCenterListerStub) List(userID int64, role string, options service.TaskCenterListOptions) ([]service.TaskCenterItem, error) {
	s.userID = userID
	s.role = role
	s.options = options
	return s.items, s.err
}

func taskCenterTestContext(target, role string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", target, nil)
	c.Set("uid", int64(7))
	c.Set("role", role)
	return c, w
}

func TestTaskCenterControllerParsesFiltersAndGuardsSystemTasks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &taskCenterListerStub{items: []service.TaskCenterItem{{
		ID: "llm:9", Source: service.TaskSourceLLM, SourceID: 9, Kind: "qa",
		Status: service.TaskStatusRunning, RawStatus: service.TaskStatusProcessing, Stage: "running",
	}}}
	controller := NewTaskCenterController(stub)
	c, w := taskCenterTestContext(
		"/api/tasks?job_id=42&source=llm&kind=qa&status=running&limit=120&include_system=1&include_steps=1",
		model.RoleUser,
	)
	controller.List(c)
	if stub.userID != 7 || stub.role != model.RoleUser {
		t.Fatalf("当前用户上下文未透传: uid=%d role=%s", stub.userID, stub.role)
	}
	if stub.options.JobID != 42 || stub.options.Source != service.TaskSourceLLM || stub.options.Kind != "qa" ||
		stub.options.Status != service.TaskStatusRunning || stub.options.Limit != 120 || !stub.options.IncludeSteps {
		t.Fatalf("筛选参数解析错误: %+v", stub.options)
	}
	if stub.options.IncludeSystem {
		t.Fatal("普通用户的 include_system 必须在 controller 层清除")
	}
	var body struct {
		Success bool                     `json:"success"`
		Data    []service.TaskCenterItem `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Success ||
		len(body.Data) != 1 || body.Data[0].ID != "llm:9" {
		t.Fatalf("成功包络错误: body=%s err=%v", w.Body.String(), err)
	}

	adminStub := &taskCenterListerStub{items: []service.TaskCenterItem{}}
	adminController := NewTaskCenterController(adminStub)
	adminContext, _ := taskCenterTestContext("/api/tasks?include_system=1&limit=bad", model.RoleAdmin)
	adminController.List(adminContext)
	if !adminStub.options.IncludeSystem || adminStub.options.Limit != 0 || adminStub.role != model.RoleAdmin {
		t.Fatalf("管理员 include_system 或非法 limit 默认值处理错误: %+v role=%s", adminStub.options, adminStub.role)
	}
}

type taskCenterJobsStub struct {
	taskCenterListerStub
	afterIDs []int64
	events   []service.JobEventView
}

func (s *taskCenterJobsStub) GetJob(userID, id int64) (*service.JobRunView, error) {
	return &service.JobRunView{ID: id, Kind: service.JobKindQA, Status: model.JobStatusRunning}, nil
}

func (s *taskCenterJobsStub) CancelJob(userID, id int64) (*service.JobRunView, error) {
	return &service.JobRunView{ID: id, Kind: service.JobKindQA, Status: model.JobStatusCanceled}, nil
}

func (s *taskCenterJobsStub) RetryJob(userID, id int64) (*service.JobRunView, error) {
	return &service.JobRunView{ID: id + 1, ParentID: &id, Kind: service.JobKindQA, Status: model.JobStatusQueued}, nil
}

func (s *taskCenterJobsStub) Events(userID, afterID, limit int64) ([]service.JobEventView, error) {
	s.afterIDs = append(s.afterIDs, afterID)
	rows := make([]service.JobEventView, 0, len(s.events))
	for _, event := range s.events {
		if event.ID > afterID {
			rows = append(rows, event)
		}
	}
	return rows, nil
}

func TestTaskCenterEventStreamResumesAndHeartbeats(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &taskCenterJobsStub{events: []service.JobEventView{{
		ID: 9, JobRunID: 3, Type: "status", Status: model.JobStatusSuccess, CreatedAt: time.Now(),
	}}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	base := httptest.NewRequest("GET", "/api/tasks/events", nil)
	base.Header.Set("Last-Event-ID", "8")
	ctx, cancel := context.WithCancel(base.Context())
	c.Request = base.WithContext(ctx)
	c.Set("uid", int64(7))
	c.Set("role", model.RoleUser)

	oldPoll, oldHeartbeat := taskEventPollInterval, taskEventHeartbeatInterval
	taskEventPollInterval = time.Hour
	taskEventHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		taskEventPollInterval = oldPoll
		taskEventHeartbeatInterval = oldHeartbeat
	})
	time.AfterFunc(100*time.Millisecond, cancel)
	NewTaskCenterController(stub).Events(c)

	body := w.Body.String()
	if len(stub.afterIDs) == 0 || stub.afterIDs[0] != 8 {
		t.Fatalf("未从 Last-Event-ID 续传: after=%v", stub.afterIDs)
	}
	if !strings.Contains(body, "id: 9\n") || !strings.Contains(body, "event: job\n") ||
		!strings.Contains(body, ": heartbeat\n\n") {
		t.Fatalf("SSE 事件 ID/heartbeat 不完整: %q", body)
	}
	if contentType := w.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("SSE Content-Type 错误: %s", contentType)
	}
}

func TestTaskCenterControllerErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &taskCenterListerStub{err: errors.New("查询失败")}
	c, w := taskCenterTestContext("/api/tasks", model.RoleUser)
	NewTaskCenterController(stub).List(c)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误包络不是 JSON: %v body=%s", err, w.Body.String())
	}
	if body["success"] != false || body["message"] != "查询失败" {
		t.Fatalf("错误包络不正确: %s", w.Body.String())
	}
}
