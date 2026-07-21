package controller

import (
	"strconv"

	"quantvista/common"

	"github.com/gin-gonic/gin"
)

// ListLLMCalls GET /api/admin/llm-calls
func (ac *AdminController) ListLLMCalls(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	result, err := ac.svc.ListLLMCalls(userID, c.Query("module"), c.Query("status"), c.Query("trace"), page, pageSize)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, result)
}

// GetLLMCall GET /api/admin/llm-calls/:id
func (ac *AdminController) GetLLMCall(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	result, err := ac.svc.GetLLMCall(id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, result)
}
