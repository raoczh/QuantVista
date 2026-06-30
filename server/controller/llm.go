package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// LLMController 用户 LLM 配置增删改查与测试连接。
type LLMController struct {
	svc *service.LLMService
}

func NewLLMController(svc *service.LLMService) *LLMController {
	return &LLMController{svc: svc}
}

func (lc *LLMController) idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "非法的配置 id")
		return 0, false
	}
	return id, true
}

// List GET /api/llm-configs
func (lc *LLMController) List(c *gin.Context) {
	rows, err := lc.svc.List(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Create POST /api/llm-configs
func (lc *LLMController) Create(c *gin.Context) {
	var in service.LLMConfigInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	v, err := lc.svc.Create(currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Update PUT /api/llm-configs/:id
func (lc *LLMController) Update(c *gin.Context) {
	id, ok := lc.idParam(c)
	if !ok {
		return
	}
	var in service.LLMConfigInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	v, err := lc.svc.Update(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Delete DELETE /api/llm-configs/:id
func (lc *LLMController) Delete(c *gin.Context) {
	id, ok := lc.idParam(c)
	if !ok {
		return
	}
	if err := lc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// Test POST /api/llm-configs/:id/test —— 测试已保存配置。
func (lc *LLMController) Test(c *gin.Context) {
	id, ok := lc.idParam(c)
	if !ok {
		return
	}
	res, err := lc.svc.TestByID(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, res)
}

// TestDraft POST /api/llm-configs/test —— 测试未保存的表单配置。
func (lc *LLMController) TestDraft(c *gin.Context) {
	var in service.LLMConfigInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	res, err := lc.svc.TestByInput(in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, res)
}
