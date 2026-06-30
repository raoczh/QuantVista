package controller

import (
	"time"

	"quantvista/common"

	"github.com/gin-gonic/gin"
)

var startTime = time.Now()

// Status 健康检查。docker healthcheck 依赖返回体中的 "success": true。
func Status(c *gin.Context) {
	common.ApiSuccess(c, gin.H{
		"version":     common.Version,
		"uptime_sec":  int(time.Since(startTime).Seconds()),
		"db":          common.DB != nil,
		"redis":       common.RedisEnabled(),
		"server_time": time.Now().Format(time.RFC3339),
	})
}
