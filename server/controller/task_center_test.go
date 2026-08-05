package controller

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

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
		Status: service.TaskStatusProcessing, RawStatus: service.TaskStatusProcessing, Stage: "processing",
	}}}
	controller := NewTaskCenterController(stub)
	c, w := taskCenterTestContext(
		"/api/tasks?source=llm&kind=qa&status=processing&limit=120&include_system=1",
		model.RoleUser,
	)
	controller.List(c)
	if stub.userID != 7 || stub.role != model.RoleUser {
		t.Fatalf("当前用户上下文未透传: uid=%d role=%s", stub.userID, stub.role)
	}
	if stub.options.Source != service.TaskSourceLLM || stub.options.Kind != "qa" ||
		stub.options.Status != service.TaskStatusProcessing || stub.options.Limit != 120 {
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
