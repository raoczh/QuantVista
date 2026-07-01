package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// AnalysisController AI 分析中心（均限当前登录用户）。
type AnalysisController struct {
	svc *service.AnalysisService
}

func NewAnalysisController(svc *service.AnalysisService) *AnalysisController {
	return &AnalysisController{svc: svc}
}

// Create POST /api/analysis —— 发起一次分析。
func (ac *AnalysisController) Create(c *gin.Context) {
	var req service.AnalyzeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	// allowPrivate：仅管理员可让分析调用触达内网自建模型（与 LLM 测试连接一致，防 SSRF）。
	allowPrivate := currentRole(c) == model.RoleAdmin
	v, err := ac.svc.Analyze(c.Request.Context(), currentUserID(c), allowPrivate, req)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// List GET /api/analysis?module=&limit= —— 分析历史。
func (ac *AnalysisController) List(c *gin.Context) {
	module := c.Query("module")
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := ac.svc.History(currentUserID(c), module, limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Get GET /api/analysis/:id —— 分析详情（含结构化结果与数据快照）。
func (ac *AnalysisController) Get(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	v, err := ac.svc.Get(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Delete DELETE /api/analysis/:id
func (ac *AnalysisController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := ac.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}
