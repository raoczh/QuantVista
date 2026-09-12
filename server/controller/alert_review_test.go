package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

func TestAlertCanceledReadActionsKeepUnreadEvents(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "single"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			setupPaperControllerReview(t)
			const userID int64 = 703
			event := model.AlertEvent{UserID: userID, RuleID: 703, Symbol: "600703", Market: "cn", Kind: model.AlertKindPrice,
				Message: "取消已读回归", TriggeredAt: time.Now(), Status: model.AlertEventUnread}
			if err := common.DB.Create(&event).Error; err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			ctx.Request = httptest.NewRequest(http.MethodPut, "/api/alerts/events/status", strings.NewReader(`{"status":"read"}`)).WithContext(requestContext)
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(event.ID, 10)}}
			controller := NewAlertController(service.NewAlertService(nil))
			if all {
				controller.ReadAllEvents(ctx)
			} else {
				controller.SetEventStatus(ctx)
			}
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Error("已取消请求仍返回已读成功")
			}
			if err := common.DB.First(&event, event.ID).Error; err != nil {
				t.Fatal(err)
			}
			if event.Status != model.AlertEventUnread {
				t.Errorf("已取消请求仍改写事件状态：%s", event.Status)
			}
		})
	}
}
