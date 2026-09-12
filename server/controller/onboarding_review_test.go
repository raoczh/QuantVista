package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"github.com/gin-gonic/gin"
)

func runOnboardingReviewRequest(t *testing.T, action, body string, canceled bool) bool {
	t.Helper()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Set("uid", int64(716))
	c.Params = gin.Params{{Key: "step", Value: "preference"}}
	ctx := t.Context()
	if canceled {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/onboarding/"+action, strings.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	controller := NewOnboardingController()
	handlers := map[string]func(*gin.Context){"get": controller.Get, "skip": controller.Skip, "finish": controller.Finish, "restart": controller.Restart, "defer": controller.Defer}
	handlers[action](c)
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Success
}

func seedOnboardingReviewProgress(t *testing.T, run int, terminal bool) model.OnboardingProgress {
	t.Helper()
	status := model.OnboardingStepNotStarted
	if terminal {
		status = model.OnboardingStepSkipped
	}
	progress := model.OnboardingProgress{UserID: 716, Version: 1, Run: run, Status: model.OnboardingStatusInProgress,
		PreferenceStatus: status, PortfolioStatus: status, AlertStatus: status}
	if err := common.DB.Create(&progress).Error; err != nil {
		t.Fatal(err)
	}
	return progress
}

func onboardingReviewRows(t *testing.T) []model.OnboardingProgress {
	t.Helper()
	var rows []model.OnboardingProgress
	if err := common.DB.Where("user_id = ?", 716).Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestOnboardingCanceledHTTPRequestsCannotChangeProgress(t *testing.T) {
	for _, action := range []string{"get", "skip", "finish", "restart", "defer"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			var id int64
			if action != "get" {
				id = seedOnboardingReviewProgress(t, 1, action == "finish").ID
			}
			before := onboardingReviewRows(t)
			if runOnboardingReviewRequest(t, action, fmt.Sprintf(`{"progress_id":%d}`, id), true) {
				t.Error("已取消的引导请求仍返回成功")
			}
			if after := onboardingReviewRows(t); !reflect.DeepEqual(before, after) {
				t.Errorf("取消请求仍改变引导事实：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOnboardingOldPageCannotMutateAnotherRun(t *testing.T) {
	for _, action := range []string{"skip", "finish", "restart", "defer"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			old := seedOnboardingReviewProgress(t, 1, true)
			seedOnboardingReviewProgress(t, 2, action == "finish")
			before := onboardingReviewRows(t)
			if runOnboardingReviewRequest(t, action, fmt.Sprintf(`{"progress_id":%d}`, old.ID), false) {
				t.Error("旧页面仍成功操作了另一轮引导")
			}
			if after := onboardingReviewRows(t); !reflect.DeepEqual(before, after) {
				t.Errorf("旧页面请求改变新轮次：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOnboardingHTTPRequiresViewedProgress(t *testing.T) {
	for _, body := range []string{`{}`, `{"progress_id":0}`, `{"progress_id":-1}`, `{"progress_id":"bad"}`} {
		t.Run(body, func(t *testing.T) {
			setupPaperControllerReview(t)
			seedOnboardingReviewProgress(t, 1, false)
			before := onboardingReviewRows(t)
			if runOnboardingReviewRequest(t, "skip", body, false) {
				t.Error("缺失或无效的引导轮次仍被当作当前轮次操作")
			}
			if !reflect.DeepEqual(before, onboardingReviewRows(t)) {
				t.Error("非法轮次改变了引导事实")
			}
		})
	}
}
