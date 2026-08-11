package controller

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDailyAuditReportAPIRequiresUserAndReturnsStableEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:candidate-audit-controller?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.CandidateAuditRun{}, &model.CandidateAuditItem{}); err != nil {
		t.Fatal(err)
	}
	previous := common.DB
	common.DB = db
	t.Cleanup(func() { common.DB = previous })

	call := func(userID *int64) map[string]any {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/api/recommendations/daily-audits?type=short_term", nil)
		if userID != nil {
			c.Set("uid", *userID)
		}
		(&RecommendationController{}).DailyAuditReport(c)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是 JSON: %s err=%v", w.Body.String(), err)
		}
		return body
	}

	withoutUser := call(nil)
	if success, _ := withoutUser["success"].(bool); success || withoutUser["message"] != "用户身份无效" {
		t.Fatalf("缺用户身份必须拒绝: %+v", withoutUser)
	}
	uid := int64(7)
	withUser := call(&uid)
	if success, _ := withUser["success"].(bool); !success {
		t.Fatalf("合法用户空报表应稳定成功: %+v", withUser)
	}
	data, ok := withUser["data"].(map[string]any)
	if !ok || data["audit_version"] == "" || data["outcome_version"] == "" {
		t.Fatalf("响应缺少版本化契约: %+v", withUser)
	}
}
