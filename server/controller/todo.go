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

// List GET /api/todos —— 聚合当前用户的待办清单。
func (tc *TodoController) List(c *gin.Context) {
	res, err := tc.svc.Build(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, res)
}
