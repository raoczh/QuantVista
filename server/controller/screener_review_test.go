package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

func TestScreenerCanceledHTTPDoesNotSaveOrArchive(t *testing.T) {
	for _, action := range []string{"create", "edit", "archive"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			svc := service.NewScreenerService()
			value := 0.0
			saved, err := svc.SaveStrategy(713, service.SaveStrategyRequest{Name: "原策略", Tree: &service.CondNode{Factor: "chg_pct", Op: ">", Value: &value}})
			if err != nil {
				t.Fatal(err)
			}
			id, revision := int64(0), int64(0)
			if action == "edit" {
				id, revision = saved.ID, saved.CurrentRevisionID
			}
			body := fmt.Sprintf(`{"id":%d,"base_revision_id":%d,"name":"取消后的更改","tree":{"factor":"chg_pct","op":">","value":1}}`, id, revision)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Set("uid", int64(713))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/api/screener/strategies", strings.NewReader(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			ctl := NewScreenerController(svc, nil)
			if action == "archive" {
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(saved.ID)}}
				ctl.DeleteStrategy(c)
			} else {
				ctl.SaveStrategy(c)
			}
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Error("已取消请求仍报告写入成功")
			}
			var rows []model.ScreenerStrategy
			if err := common.DB.Where("user_id = ?", 713).Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Name != "原策略" || rows[0].ArchivedAt != nil || rows[0].CurrentRevisionID != saved.CurrentRevisionID {
				t.Errorf("取消请求仍改变策略事实: %+v", rows)
			}
		})
	}
}
