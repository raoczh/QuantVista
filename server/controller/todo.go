package controller

import (
	"strconv"

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

// List GET /api/todos —— 聚合当前用户的统一收件箱投影。
func (tc *TodoController) List(c *gin.Context) {
	res, err := tc.svc.BuildInbox(c.Request.Context(), currentUserID(c), service.TodoListOptions{
		Scope: c.Query("scope"), Status: c.Query("status"), Source: c.Query("source"),
		Page: queryInt(c, "page"), PageSize: queryInt(c, "page_size"),
		HistoryDays: queryInt(c, "history_days"), Limit: queryInt(c, "limit"),
	})
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, res)
}

// Action PUT /api/todos/actions —— 批量收下、明天再说或当日同股同类静默。
func (tc *TodoController) Action(c *gin.Context) {
	var req service.TodoActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求参数无效")
		return
	}
	if err := tc.svc.ApplyInboxAction(currentUserID(c), req); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

func queryInt(c *gin.Context, key string) int {
	value, _ := strconv.Atoi(c.Query(key))
	return value
}
