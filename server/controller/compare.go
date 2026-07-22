package controller

import (
	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// CompareController 个股横向对比（限当前登录用户；AI 点评走 LLM）。
type CompareController struct {
	svc *service.CompareService
}

func NewCompareController(svc *service.CompareService) *CompareController {
	return &CompareController{svc: svc}
}

// Compare POST /api/compare —— 多只股票横向对比，可选 AI 一句话点评。
func (cc *CompareController) Compare(c *gin.Context) {
	var req service.CompareRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	allowPrivate := currentRole(c) == model.RoleAdmin
	if req.WithAI {
		task, err := cc.svc.CompareAsync(currentUserID(c), allowPrivate, req)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		common.ApiSuccess(c, task)
		return
	}
	res, err := cc.svc.Compare(c.Request.Context(), currentUserID(c), allowPrivate, req)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, res)
}
