package middleware

import (
	"net/http"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"github.com/gin-gonic/gin"
)

// JWTAuth 校验 Authorization: Bearer <access token>，把 uid/role 写入 context。
// 校验失败返回 401，供前端拦截器触发刷新令牌流程。
func JWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := extractBearer(c)
		if tokenStr == "" {
			abort(c, http.StatusUnauthorized, "未登录")
			return
		}
		claims, err := common.ParseAccessToken(tokenStr)
		if err != nil {
			abort(c, http.StatusUnauthorized, "登录状态无效或已过期")
			return
		}
		c.Set("uid", claims.UserID)
		c.Set("role", claims.Role)
		c.Next()
	}
}

// AdminAuth 要求当前用户为管理员，必须挂在 JWTAuth 之后。
func AdminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("role") != model.RoleAdmin {
			abort(c, http.StatusForbidden, "需要管理员权限")
			return
		}
		c.Next()
	}
}

func extractBearer(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(h) // 容忍直接传 token
}

func abort(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "message": msg})
}
