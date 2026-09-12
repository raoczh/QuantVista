package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"quantvista/model"

	"github.com/gin-gonic/gin"
)

func TestLLMExperimentActionRejectsMalformedBodyBeforeMutation(t *testing.T) {
	db := reviewControllerDB(t, &model.LLMExperiment{})
	for _, body := range []string{`{"conclusion":17,"failure_reason":"review"}`, `{"failure_reason":`} {
		exp := model.LLMExperiment{Status: model.ExpStatusDraft}
		if err := db.Create(&exp).Error; err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(exp.ID, 10)}, {Key: "action", Value: "abandon"}}
		c.Request = httptest.NewRequest(http.MethodPost, "/experiment/abandon", strings.NewReader(body))
		(&AdminController{}).LLMExperimentAction(c)
		var result struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if err := db.First(&exp, exp.ID).Error; err != nil {
			t.Fatal(err)
		}
		if result.Success || exp.Status != model.ExpStatusDraft {
			t.Errorf("非法请求改变了实验：body=%s status=%s response=%s", body, exp.Status, w.Body.String())
		}
	}
}

func TestLLMExperimentDetailRejectsAuditReadFailure(t *testing.T) {
	// 特意不创建审计工件表，验证读取故障不能伪装为没有发布审计。
	db := reviewControllerDB(t, &model.LLMExperiment{}, &model.LLMExperimentRun{})
	exp := model.LLMExperiment{Status: model.ExpStatusCompleted}
	if err := db.Create(&exp).Error; err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(exp.ID, 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/experiment", nil)
	(&AdminController{}).GetLLMExperiment(c)
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatalf("审计查询失败不能返回成功空工件：%s", w.Body.String())
	}
}
