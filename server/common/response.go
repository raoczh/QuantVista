package common

import (
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

func ApiError(c *gin.Context, err error) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": err.Error(),
	})
}

func ApiErrorMsg(c *gin.Context, msg string) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": msg,
	})
}
