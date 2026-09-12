package middleware

import (
	"errors"
	"net/http"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// JWTAuth 校验 Authorization: Bearer <access token>，把 uid/role 写入 context。
// 除签名/过期外，还查库校验用户状态与 token_version，使禁用/改密后旧 access token 即时失效。
// 校验失败返回 401，供前端拦截器触发刷新令牌流程。
func JWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if status, message := ValidateJWTSession(c); status != 0 {
			abort(c, status, message)
			return
		}
		c.Next()
	}
}

// ValidateJWTSession 复验当前请求令牌及用户状态；成功返回 (0, "") 并更新身份上下文。
// 不写响应，长连接也可在 HTTP 头已发送后调用，并在失效时关闭连接。
func ValidateJWTSession(c *gin.Context) (int, string) {
	tokenStr := extractBearer(c)
	if tokenStr == "" {
		return http.StatusUnauthorized, "未登录"
	}
	claims, err := common.ParseAccessToken(tokenStr)
	if err != nil {
		return http.StatusUnauthorized, "登录状态无效或已过期"
	}
	if common.DB == nil {
		return http.StatusServiceUnavailable, "账号状态暂时不可用，请稍后重试"
	}
	var u struct {
		Status       string
		Role         string
		TokenVersion int
	}
	if err := common.DB.WithContext(c.Request.Context()).Model(&model.User{}).
		Select("status", "role", "token_version").
		Where("id = ?", claims.UserID).Take(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return http.StatusUnauthorized, "账号不存在"
		}
		return http.StatusServiceUnavailable, "账号状态暂时不可用，请稍后重试"
	}
	if u.Status != model.StatusEnabled {
		return http.StatusUnauthorized, "账号已被禁用"
	}
	if claims.Ver != u.TokenVersion {
		return http.StatusUnauthorized, "登录状态已失效，请重新登录"
	}
	c.Set("uid", claims.UserID)
	c.Set("role", u.Role)
	return 0, ""
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
