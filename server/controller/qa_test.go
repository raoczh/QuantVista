package controller

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestQaAskStreamUsesTaskJSONContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/qa/ask-stream", strings.NewReader("{"))
	c.Request.Header.Set("Content-Type", "application/json")

	// 非法 JSON 在触发 service 前失败，足以验证旧路由已走标准 JSON 包络、不再提前写
	// application/x-ndjson 响应头。svc 可安全置 nil。
	NewQaController(nil).AskStream(c)
	if strings.Contains(w.Header().Get("Content-Type"), "x-ndjson") {
		t.Fatalf("ask-stream 不应再使用请求绑定的 NDJSON 流: %s", w.Header().Get("Content-Type"))
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("应返回标准 JSON 包络: %v body=%q", err, w.Body.String())
	}
	if body["success"] != false {
		t.Fatalf("非法请求应返回失败包络: %s", w.Body.String())
	}
}
