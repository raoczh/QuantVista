package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func maintenanceTestContext(body string) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reqBody *strings.Reader
	if body == "<nil>" {
		reqBody = strings.NewReader("")
		c.Request = httptest.NewRequest("POST", "/api/admin/market/sync-bars", nil)
		return c
	}
	reqBody = strings.NewReader(body)
	c.Request = httptest.NewRequest("POST", "/api/admin/market/sync-bars", reqBody)
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func TestBindMaintenanceRequestLegacyNoBodyCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := maintenanceTestContext("<nil>")
	_, hasBody, err := bindMaintenanceRequest(c)
	if err != nil || hasBody {
		t.Fatalf("完全无 body 必须走旧兼容路径: hasBody=%v err=%v", hasBody, err)
	}
	// 显式空 JSON 对象属于新契约，执行时会被 plan_hash 校验拦住，不能冒充旧请求。
	c = maintenanceTestContext(`{}`)
	_, hasBody, err = bindMaintenanceRequest(c)
	if err != nil || !hasBody {
		t.Fatalf("{} 应识别为新 body: hasBody=%v err=%v", hasBody, err)
	}
}

func TestBindMaintenanceRequestRejectsUnknownAndLargeBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := maintenanceTestContext(`{"dry_run":true,"api_key":"不得进入审计"}`)
	if _, _, err := bindMaintenanceRequest(c); err == nil {
		t.Fatal("未知/敏感字段必须拒绝")
	}
	c = maintenanceTestContext(`{"dry_run":true}` + strings.Repeat(" ", maintenanceBodyMaxBytes+1))
	if _, _, err := bindMaintenanceRequest(c); err == nil {
		t.Fatal("超过 4KiB 的正文必须拒绝")
	}
	c = maintenanceTestContext(`{"dry_run":true}` + strings.Repeat(" ", maintenanceBodyMaxBytes+1))
	c.Request.ContentLength = -1 // 模拟 chunked/未知长度，仍必须按实读字节执行硬上限。
	if _, _, err := bindMaintenanceRequest(c); err == nil {
		t.Fatal("未知 Content-Length 也不得绕过 4KiB 硬上限")
	}
	for _, body := range []string{"null", "[]", "true"} {
		c = maintenanceTestContext(body)
		if _, _, err := bindMaintenanceRequest(c); err == nil {
			t.Fatalf("非 JSON 对象正文必须拒绝: %s", body)
		}
	}
}
