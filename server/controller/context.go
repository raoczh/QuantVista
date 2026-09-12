package controller

import (
	"math"
	"strconv"

	"quantvista/common"

	"github.com/gin-gonic/gin"
)

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

func optionalAccountID(c *gin.Context) int64 {
	raw := c.Query("account_id")
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return -1
	}
	return id
}

// clientUA 取请求 User-Agent（落库刷新令牌时记录来源设备）。
func clientUA(c *gin.Context) string {
	return c.Request.UserAgent()
}

func optionalNonnegativeFloat(c *gin.Context, key string) (float64, bool) {
	raw := c.Query(key)
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		common.ApiErrorMsg(c, key+" 须为有限的非负数")
		return 0, false
	}
	return value, true
}
