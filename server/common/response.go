package common

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 统一响应包络：{success, message, data}，与 new-api 对齐，
// healthcheck 依赖 "success": true。

func ApiSuccess(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    data,
	})
}

// refusalCoder 机读拒答码接口（service.RefusalError 实现；接口探测避免 common→service 反向依赖）。
type refusalCoder interface{ RefusalCode() string }

// ApiError 错误包络；错误链中带机读拒答码（service.RefusalError）时附加 code 字段，
// 前端按 code 程序化分支（如 stale_quote 弹「按历史数据解释」确认），不再解析中文文案。
func ApiError(c *gin.Context, err error) {
	h := gin.H{
		"success": false,
		"message": err.Error(),
	}
	var rc refusalCoder
	if errors.As(err, &rc) {
		if code := rc.RefusalCode(); code != "" {
			h["code"] = code
		}
	}
	c.JSON(http.StatusOK, h)
}

func ApiErrorMsg(c *gin.Context, msg string) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": msg,
	})
}
