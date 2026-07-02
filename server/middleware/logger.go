package middleware

import (
	"net/http"
	"runtime/debug"
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

// Recovery 捕获 panic：记录堆栈、返回 500（监控依赖状态码识别异常，不能回 200）。
// http.ErrAbortHandler 是 net/http 约定的「静默中断」哨兵，原样重抛交给标准库处理。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				if err == http.ErrAbortHandler {
					panic(err)
				}
				common.SysError("panic: %v\n%s", err, debug.Stack())
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"success": false,
					"message": "服务器内部错误",
				})
			}
		}()
		c.Next()
	}
}
