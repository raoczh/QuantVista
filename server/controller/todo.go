package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// TodoController 今日待办/待复盘清单（限当前登录用户）。
type TodoController struct {
	svc *service.TodoService
}

func NewTodoController(svc *service.TodoService) *TodoController {
	return &TodoController{svc: svc}
}

// List GET /api/todos?scope= —— 聚合当前用户的待办清单。
// scope 默认 ledger（只看与我的账本有关的）；research 为推荐追踪页的复盘区所用；
// market 为打新等全市场机会；all 为全量。
func (tc *TodoController) List(c *gin.Context) {
	res, err := tc.svc.Build(c.Request.Context(), currentUserID(c), c.Query("scope"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, res)
}
