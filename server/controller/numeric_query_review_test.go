package controller

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPriceQueriesRejectNonfiniteValuesBeforeReadingData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, target := range []string{
		"/analysis/1/hindsight?stop_price=Inf",
		"/analysis/1/hindsight?target_price=NaN",
		"/analysis/1/hindsight?target_price=bad",
		"/orgview?price=Inf", "/orgview?price=-1",
	} {
		t.Run(target, func(t *testing.T) {
			r := gin.New()
			// 不配置服务：无效价格须在任何数据库/上游读取之前被明确拒绝。
			r.GET("/analysis/:id/hindsight", (&AnalysisController{}).Hindsight)
			r.GET("/orgview", (&OrgViewController{}).StockOrgView)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
			var result struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Success || result.Message == "" {
				t.Fatalf("无效价格必须返回完整错误 JSON：body=%s err=%v", w.Body.String(), err)
			}
		})
	}
}
