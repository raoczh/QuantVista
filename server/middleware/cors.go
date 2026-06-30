package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CORS 开发期跨域。骨架阶段前后端同源（后端 embed 前端），
// 仅本地 vite dev server 跨域调试时需要；生产同源可不依赖。
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Headers",
				"Origin, Content-Type, Authorization, Accept")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
