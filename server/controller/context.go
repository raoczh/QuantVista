package controller

import "github.com/gin-gonic/gin"

// currentUserID 取鉴权中间件写入的当前用户 ID。
func currentUserID(c *gin.Context) int64 {
	if v, ok := c.Get("uid"); ok {
		if id, ok := v.(int64); ok {
			return id
		}
	}
	return 0
}

// currentRole 取当前用户角色。
func currentRole(c *gin.Context) string {
	return c.GetString("role")
}

// clientUA 取请求 User-Agent（落库刷新令牌时记录来源设备）。
func clientUA(c *gin.Context) string {
	return c.Request.UserAgent()
}
