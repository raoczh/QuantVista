package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// NotifyController 推送通道管理（限当前登录用户）。
type NotifyController struct {
	svc *service.NotifyService
}

func NewNotifyController(svc *service.NotifyService) *NotifyController {
	return &NotifyController{svc: svc}
}

// List GET /api/notify-channels
func (nc *NotifyController) List(c *gin.Context) {
	rows, err := nc.svc.List(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Create POST /api/notify-channels
func (nc *NotifyController) Create(c *gin.Context) {
	var in service.NotifyChannelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	v, err := nc.svc.Create(currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Update PUT /api/notify-channels/:id
func (nc *NotifyController) Update(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.NotifyChannelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	v, err := nc.svc.Update(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Delete DELETE /api/notify-channels/:id
func (nc *NotifyController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := nc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// Test POST /api/notify-channels/:id/test
func (nc *NotifyController) Test(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := nc.svc.Test(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, "推送失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}
