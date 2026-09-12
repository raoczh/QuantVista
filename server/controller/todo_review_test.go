package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

func TestTodoCanceledActionKeepsUnreadSource(t *testing.T) {
	setupPaperControllerReview(t)
	const userID int64 = 702
	event := model.AlertEvent{UserID: userID, RuleID: 702, Symbol: "600702", Market: "cn", Kind: model.AlertKindPrice,
		Message: "取消回归", TriggeredAt: time.Now(), Status: model.AlertEventUnread}
	if err := common.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	svc := service.NewTodoService(service.NewAlertService(nil), service.NewPositionService(nil), nil)
	inbox, err := svc.BuildInbox(t.Context(), userID, service.TodoListOptions{Scope: service.TodoScopeAll})
	if err != nil || inbox.Total != 1 {
		t.Fatalf("读取本机模拟事项：inbox=%+v err=%v", inbox, err)
	}
	child := inbox.Items[0].Children[0]
	body, err := json.Marshal(service.TodoActionRequest{Action: service.TodoActionRead, Items: []service.TodoSourceRef{{
		SourceKind: child.SourceKind, SourceID: child.SourceID, SourceVersion: child.SourceVersion,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", userID)
	requestContext, cancel := context.WithCancel(t.Context())
	cancel()
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/todos/actions", strings.NewReader(string(body))).WithContext(requestContext)
	ctx.Request.Header.Set("Content-Type", "application/json")
	NewTodoController(svc).Action(ctx)
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("已取消请求仍成功收下事项")
	}
	if err := common.DB.First(&event, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if event.Status != model.AlertEventUnread {
		t.Errorf("已取消请求仍修改原业务状态：%s", event.Status)
	}
}
