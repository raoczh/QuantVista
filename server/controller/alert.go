package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// AlertController 条件提醒（均限当前登录用户）。
type AlertController struct {
	svc *service.AlertService
}

func NewAlertController(svc *service.AlertService) *AlertController {
	return &AlertController{svc: svc}
}

// List GET /api/alerts?status=
func (ac *AlertController) List(c *gin.Context) {
	rows, err := ac.svc.List(currentUserID(c), c.Query("status"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Create POST /api/alerts
func (ac *AlertController) Create(c *gin.Context) {
	var in service.AlertInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	rule, err := ac.svc.Create(c.Request.Context(), currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rule)
}

// Update PUT /api/alerts/:id
func (ac *AlertController) Update(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.AlertInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	rule, err := ac.svc.Update(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rule)
}

// SetStatus PUT /api/alerts/:id/status —— 暂停/恢复。
func (ac *AlertController) SetStatus(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	rule, err := ac.svc.SetStatus(currentUserID(c), id, body.Status)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rule)
}

// Delete DELETE /api/alerts/:id
func (ac *AlertController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := ac.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// Evaluate POST /api/alerts/evaluate —— 手动立即评估本人全部规则，返回命中数。
func (ac *AlertController) Evaluate(c *gin.Context) {
	n, err := ac.svc.EvaluateUser(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"hits": n})
}

// ListEvents GET /api/alerts/events?status=&limit= —— 命中历史（明细事件）。
func (ac *AlertController) ListEvents(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, err := ac.svc.ListEvents(currentUserID(c), c.Query("status"), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// SetEventStatus PUT /api/alerts/events/:id/status —— 标记已读/忽略/恢复未读。
func (ac *AlertController) SetEventStatus(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	ev, err := ac.svc.SetEventStatus(currentUserID(c), id, body.Status)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, ev)
}

// ReadAllEvents PUT /api/alerts/events/read-all —— 全部未读标记已读。
func (ac *AlertController) ReadAllEvents(c *gin.Context) {
	n, err := ac.svc.MarkAllEventsRead(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"updated": n})
}
