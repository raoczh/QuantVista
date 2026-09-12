package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"github.com/gin-gonic/gin"
)

func TestSellReviewCanceledHTTPPreservesStatus(t *testing.T) {
	setupPaperControllerReview(t)
	row := model.SellReview{UserID: 14130, PositionID: 1, Symbol: "600061", Market: "cn", Trigger: model.SellReviewLift, TradeDate: "2026-09-11", Status: model.SellReviewStatusOpen}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"read", "resolve"} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Set("uid", row.UserID)
		requestContext, cancel := context.WithCancel(t.Context())
		cancel()
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/positions/sell-reviews", strings.NewReader(`{"status":"resolved"}`)).WithContext(requestContext)
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(row.ID, 10)}}
		if action == "read" {
			SellReviews(ctx)
		} else {
			SellReviewAction(ctx)
		}
		var result struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Success {
			t.Errorf("已取消请求不能返回成功：action=%s body=%s err=%v", action, response.Body.String(), err)
		}
	}
	if err := common.DB.First(&row, row.ID).Error; err != nil || row.Status != model.SellReviewStatusOpen || row.ResolvedAt != nil {
		t.Fatalf("取消不能改变待复核状态：row=%+v err=%v", row, err)
	}
}
