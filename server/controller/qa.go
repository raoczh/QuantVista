package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// QaController 个股 AI 问答（限当前登录用户）。
type QaController struct {
	svc *service.QaService
}

func NewQaController(svc *service.QaService) *QaController {
	return &QaController{svc: svc}
}

// Ask POST /api/qa/ask —— 新建会话或在已有会话上追问。
func (qc *QaController) Ask(c *gin.Context) {
	var req service.QaAskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	allowPrivate := currentRole(c) == model.RoleAdmin
	v, err := qc.svc.Ask(c.Request.Context(), currentUserID(c), allowPrivate, req)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// List GET /api/qa?limit=
func (qc *QaController) List(c *gin.Context) {
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := qc.svc.List(currentUserID(c), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Get GET /api/qa/:id
func (qc *QaController) Get(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	v, err := qc.svc.Get(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Snapshot GET /api/qa/:id/snapshot —— 会话固定数据快照原文（透明面板）。
func (qc *QaController) Snapshot(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	snap, err := qc.svc.Snapshot(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"data_snapshot": snap})
}

// Delete DELETE /api/qa/:id
func (qc *QaController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := qc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}
