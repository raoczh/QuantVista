package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// PromptController 用户自定义分析提示词模板（限当前登录用户）。
type PromptController struct {
	svc *service.PromptService
}

func NewPromptController(svc *service.PromptService) *PromptController {
	return &PromptController{svc: svc}
}

// Modules GET /api/prompt-templates/modules —— 可自定义的模块及默认指引。
func (pc *PromptController) Modules(c *gin.Context) {
	common.ApiSuccess(c, pc.svc.Modules())
}

// List GET /api/prompt-templates
func (pc *PromptController) List(c *gin.Context) {
	rows, err := pc.svc.List(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Upsert POST /api/prompt-templates —— 新建或更新某模块模板。
// P0-6：响应含 template 与 warnings（占位符/内容 lint 诊断，不阻断保存）。
func (pc *PromptController) Upsert(c *gin.Context) {
	var in service.PromptInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	tpl, warnings, err := pc.svc.Upsert(currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"template": tpl, "warnings": warnings})
}

// Delete DELETE /api/prompt-templates/:id —— 删除（恢复默认）。
func (pc *PromptController) Delete(c *gin.Context) {
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
