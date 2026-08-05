package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

type taskCenterLister interface {
	List(userID int64, role string, options service.TaskCenterListOptions) ([]service.TaskCenterItem, error)
}

// TaskCenterController 提供当前用户跨业务域的只读任务列表。
type TaskCenterController struct {
	svc taskCenterLister
}

func NewTaskCenterController(svc taskCenterLister) *TaskCenterController {
	return &TaskCenterController{svc: svc}
}

// List GET /api/tasks?source=&kind=&status=&limit=&include_system=1
func (tc *TaskCenterController) List(c *gin.Context) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	items, err := tc.svc.List(currentUserID(c), currentRole(c), service.TaskCenterListOptions{
		Source:        c.Query("source"),
		Kind:          c.Query("kind"),
		Status:        c.Query("status"),
		Limit:         limit,
		IncludeSystem: c.Query("include_system") == "1" && currentRole(c) == model.RoleAdmin,
	})
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, items)
}
