package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// LLMTaskController 提供通用 LLM 后台任务的只读状态接口。
type LLMTaskController struct{}

func NewLLMTaskController() *LLMTaskController { return &LLMTaskController{} }

// Get GET /api/llm-tasks/:id
func (lc *LLMTaskController) Get(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	view, err := service.GetAsyncLLMTask(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, view)
}

// List GET /api/llm-tasks?kind=&status=&limit=
func (lc *LLMTaskController) List(c *gin.Context) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	views, err := service.ListAsyncLLMTasks(
		currentUserID(c), c.Query("kind"), c.Query("status"), limit,
	)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, views)
}
