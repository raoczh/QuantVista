package router

import (
	"testing"

	"quantvista/datasource"

	"github.com/gin-gonic/gin"
)

func TestTaskCenterRouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	SetApiRouter(r, &datasource.Manager{})

	for _, route := range r.Routes() {
		if route.Method == "GET" && route.Path == "/api/tasks" {
			return
		}
	}
	t.Fatal("GET /api/tasks 未注册")
}
