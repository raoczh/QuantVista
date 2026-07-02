package controller

import (
	"strings"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// PositionController 已购入持仓（均限当前登录用户）。
type PositionController struct {
	svc *service.PositionService
}

func NewPositionController(svc *service.PositionService) *PositionController {
	return &PositionController{svc: svc}
}

// List GET /api/positions?status=holding|closed|all
func (pc *PositionController) List(c *gin.Context) {
	status := strings.ToLower(c.DefaultQuery("status", "all"))
	list, err := pc.svc.List(c.Request.Context(), currentUserID(c), status)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, list)
}

// Overview GET /api/positions/overview —— 组合总览 + 个人风控信号。
func (pc *PositionController) Overview(c *gin.Context) {
	ov, err := pc.svc.Overview(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, ov)
}

// Create POST /api/positions
func (pc *PositionController) Create(c *gin.Context) {
	var in service.PositionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := pc.svc.Create(c.Request.Context(), currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Update PUT /api/positions/:id
func (pc *PositionController) Update(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.PositionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := pc.svc.Update(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Close POST /api/positions/:id/close
func (pc *PositionController) Close(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.CloseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := pc.svc.Close(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Delete DELETE /api/positions/:id
func (pc *PositionController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := pc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}
