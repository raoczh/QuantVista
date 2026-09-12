package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestOrgViewHTTPDoesNotReportStorageFailureAsEmpty(t *testing.T) {
	setupPaperControllerReview(t)
	day := time.Now().Format("2006-01-02")
	for _, row := range []any{
		&model.ReportRating{Market: "cn", Symbol: "600901", InfoCode: "TEST", ReportDate: day, Rating: "买入"},
		&model.OrgSurvey{Market: "cn", Symbol: "600901", SurveyDate: day, NoticeDate: day, OrgCount: 1},
	} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	const callback = "review_org_http_storage_error"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "report_ratings" {
			tx.AddError(errors.New("SELECT private_column FROM report_ratings failed"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/markets/cn/stocks/600901/orgview", nil)
	c.Params = gin.Params{{Key: "market", Value: "cn"}, {Key: "symbol", Value: "600901"}}
	NewOrgViewController(service.NewOrgViewService()).StockOrgView(c)
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.Success {
		t.Fatalf("存储故障应明确失败：%s %v", recorder.Body.String(), err)
	}
	if strings.Contains(recorder.Body.String(), "private_column") || strings.Contains(recorder.Body.String(), "report_ratings") {
		t.Fatalf("不应暴露存储细节：%s", recorder.Body.String())
	}
}
