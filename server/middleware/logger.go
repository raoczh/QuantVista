package middleware

import (
	"time"

	"quantvista/common"

	"github.com/gin-gonic/gin"
)

// Logger 极简请求日志。
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		common.SysLog("%s %s %d %s",
			c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
	}
}

// Recovery 捕获 panic，返回统一错误而非裸 500。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				common.SysError("panic: %v", err)
				common.ApiErrorMsg(c, "服务器内部错误")
				c.Abort()
			}
		}()
		c.Next()
	}
}
