package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// NotifyController 推送通道管理（限当前登录用户）。
type NotifyController struct {
	svc     *service.NotifyService
	browser *service.BrowserNotificationService
}

func NewNotifyController(svc *service.NotifyService) *NotifyController {
	return &NotifyController{svc: svc, browser: service.NewBrowserNotificationService()}
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

// BrowserConfig GET /api/browser-notifications/config
func (nc *NotifyController) BrowserConfig(c *gin.Context) {
	view, err := nc.browser.Config(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, view)
}

// UpdateBrowserSettings PUT /api/browser-notifications/settings
func (nc *NotifyController) UpdateBrowserSettings(c *gin.Context) {
	var in service.BrowserNotificationSettingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	view, err := nc.browser.UpdateSettings(currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, view)
}

// UpsertBrowserSubscription POST /api/browser-notifications/subscriptions
func (nc *NotifyController) UpsertBrowserSubscription(c *gin.Context) {
	var in service.BrowserSubscriptionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	view, err := nc.browser.UpsertSubscription(currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, view)
}

// RemoveBrowserDevice DELETE /api/browser-notifications/subscriptions/:id
func (nc *NotifyController) RemoveBrowserDevice(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := nc.browser.RemoveDevice(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// BrowserEvents GET /api/browser-notifications/events
func (nc *NotifyController) BrowserEvents(c *gin.Context) {
	afterID, _ := strconv.ParseInt(c.Query("after_id"), 10, 64)
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, err := nc.browser.PendingEvents(currentUserID(c), c.Query("device_key"), afterID, limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// AckBrowserEvent PUT /api/browser-notifications/events/:id/ack
func (nc *NotifyController) AckBrowserEvent(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		DeviceKey string `json:"device_key"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	if err := nc.browser.Ack(currentUserID(c), id, in.DeviceKey); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// TestBrowserNotification POST /api/browser-notifications/test
func (nc *NotifyController) TestBrowserNotification(c *gin.Context) {
	var in struct {
		DeviceKey string `json:"device_key"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	if err := nc.browser.Test(currentUserID(c), in.DeviceKey); err != nil {
		common.ApiErrorMsg(c, "测试通知失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}
