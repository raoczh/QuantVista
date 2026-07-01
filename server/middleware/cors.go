package middleware

import (
	"net/http"

	"quantvista/common"

	"github.com/gin-gonic/gin"
)

// CORS 跨域策略：
//   - 开发环境（非生产）：反射任意 Origin，方便本地 vite dev server 调试。
//   - 生产环境：仅放行 ALLOWED_ORIGINS 白名单中的 Origin；未配置则不发 CORS 头
//     （前端与后端同源 embed，正常无需跨域）。
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" && corsAllowed(origin) {
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

func corsAllowed(origin string) bool {
	if !common.Production {
		return true // 开发环境放行
	}
	for _, o := range common.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}
