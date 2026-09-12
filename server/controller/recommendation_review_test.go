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

func TestRecommendationCanceledHTTPPreservesRecords(t *testing.T) {
	for _, action := range []string{"delete", "ack"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			batch := model.RecommendationBatch{UserID: 738, Type: model.RecTypeShortTerm, Market: "cn", Status: model.RecStatusSuccess}
			if err := common.DB.Create(&batch).Error; err != nil {
				t.Fatal(err)
			}
			status := model.RecommendationStatus{UserID: 738, BatchID: batch.ID, RecommendationID: 123, Symbol: "600000", Market: "cn", Outcome: model.RecOutcomeStopLoss, ReviewNeeded: true}
			if err := common.DB.Create(&status).Error; err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Set("uid", int64(738))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/recommendations/1", nil).WithContext(ctx)
			ctl := NewRecommendationController(service.NewRecommendationService(nil, nil, nil), service.NewTrackingService(nil), nil)
			if action == "delete" {
				c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(batch.ID, 10)}}
				ctl.Delete(c)
			} else {
				c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(status.ID, 10)}}
				ctl.AckReview(c)
			}
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Error("取消请求仍报告修改成功")
			}
			var stored model.RecommendationStatus
			if err := common.DB.First(&stored, status.ID).Error; err != nil || stored.ReviewAck {
				t.Errorf("取消请求删除或更改了复盘事实: stored=%+v err=%v", stored, err)
			}
			if err := common.DB.First(&model.RecommendationBatch{}, batch.ID).Error; err != nil {
				t.Errorf("取消请求删除了批次: %v", err)
			}
		})
	}
}

func TestRecommendationHTTPDoesNotExposeStorageErrors(t *testing.T) {
	setupPaperControllerReview(t)
	const callback = "review_recommendation_private_error"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_batches" {
			tx.AddError(errors.New("SELECT private_column FROM recommendation_batches: storage failed"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Set("uid", int64(738))
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/recommendations/1", nil)
	NewRecommendationController(service.NewRecommendationService(nil, nil, nil), nil, nil).Get(c)
	if body := response.Body.String(); strings.Contains(body, "private_column") || strings.Contains(body, "recommendation_batches") {
		t.Errorf("业务回复泄露存储细节: %s", body)
	}
}

func TestDiscoveryStatusHidesStoredInternalError(t *testing.T) {
	setupPaperControllerReview(t)
	const callback = "review_discovery_stored_error"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*model.CandidateDiscoveryRun); ok {
			*row = model.CandidateDiscoveryRun{ID: 19, Status: service.DiscoveryRunStatusFail,
				Error: "SELECT private_column FROM candidate_discovery_runs: storage failed"}
			tx.Error, tx.RowsAffected = nil, 1
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/recommendations/discovery-status", nil)
	NewRecommendationController(nil, nil, nil).DiscoveryStatus(c)
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Success {
		t.Fatalf("运行失败状态仍应正常显示：body=%s err=%v", response.Body.String(), err)
	}
	if strings.Contains(response.Body.String(), "private_column") || strings.Contains(response.Body.String(), "candidate_discovery_runs") {
		t.Fatalf("状态回执不能泄露存储细节：%s", response.Body.String())
	}
}
