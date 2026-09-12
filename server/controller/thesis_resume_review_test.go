package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestThesisHTTPCancellationPreservesCard(t *testing.T) {
	for _, action := range []string{"list", "symbol", "status", "delete", "upsert", "checkup"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			card := model.ThesisCard{UserID: 12101, Symbol: "600001", Market: "cn", Thesis: "原假设", Status: model.ThesisStatusActive}
			if err := common.DB.Create(&card).Error; err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			c.Set("uid", card.UserID)
			c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(card.ID, 10)}}
			path := "/api/thesis-cards"
			if action == "symbol" {
				path += "?symbol=600001&market=cn"
			}
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"status":"archived","symbol":"600001","market":"cn","thesis":"新假设"}`)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			ctl := NewThesisController(service.NewThesisService(nil))
			switch action {
			case "list", "symbol":
				ctl.List(c)
			case "status":
				ctl.SetStatus(c)
			case "delete":
				ctl.Delete(c)
			case "upsert":
				ctl.Upsert(c)
			case "checkup":
				ctl.CheckUp(c)
			}
			var reply struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil || reply.Success {
				t.Errorf("已取消请求不能报告成功：body=%s err=%v", recorder.Body.String(), err)
			}
			var stored model.ThesisCard
			if err := common.DB.First(&stored, card.ID).Error; err != nil || stored.Thesis != card.Thesis || stored.Status != card.Status {
				t.Fatalf("已取消请求改动了逻辑卡：stored=%+v err=%v", stored, err)
			}
		})
	}
}

func TestThesisHTTPDoesNotExposeStorageError(t *testing.T) {
	setupPaperControllerReview(t)
	const hook = "review_thesis_private_storage_error"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "thesis_cards" {
			tx.AddError(errors.New("sqlite: internal-review-details"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(hook) })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("uid", int64(12101))
	c.Request = httptest.NewRequest(http.MethodGet, "/api/thesis-cards", nil)
	NewThesisController(service.NewThesisService(nil)).List(c)
	if strings.Contains(recorder.Body.String(), "internal-review-details") {
		t.Fatalf("接口泄漏内部存储诊断：%s", recorder.Body.String())
	}
}
