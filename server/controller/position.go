package controller

import (
	"strconv"
	"strings"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// PositionController 已购入持仓（均限当前登录用户）。
type PositionController struct {
	svc *service.PositionService
}

func NewPositionController(svc *service.PositionService) *PositionController {
	return &PositionController{svc: svc}
}

// List GET /api/positions?status=holding|closed|all
func (pc *PositionController) List(c *gin.Context) {
	status := strings.ToLower(c.DefaultQuery("status", "all"))
	list, err := pc.svc.List(c.Request.Context(), currentUserID(c), status)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, list)
}

// Overview GET /api/positions/overview —— 组合总览 + 个人风控信号。
func (pc *PositionController) Overview(c *gin.Context) {
	ov, err := pc.svc.Overview(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, ov)
}

// Create POST /api/positions
func (pc *PositionController) Create(c *gin.Context) {
	var in service.PositionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := pc.svc.Create(c.Request.Context(), currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Update PUT /api/positions/:id
func (pc *PositionController) Update(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.PositionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := pc.svc.Update(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Close POST /api/positions/:id/close
func (pc *PositionController) Close(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.CloseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := pc.svc.Close(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Delete DELETE /api/positions/:id
func (pc *PositionController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := pc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// Trades GET /api/positions/:id/trades —— 加/减仓流水明细（B5）。
func (pc *PositionController) Trades(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	rows, err := pc.svc.ListTrades(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// AddTrade POST /api/positions/:id/trades —— 加仓 / 减仓（B5）。
// 减到 0 自动平仓，请求体可一并带复盘字段。
func (pc *PositionController) AddTrade(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.PositionTradeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := pc.svc.AddTrade(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Stats GET /api/positions/stats?range= —— 个人交易复盘统计（B6，纯读时聚合）。
func (pc *PositionController) Stats(c *gin.Context) {
	out, err := pc.svc.TradeStats(c.Request.Context(), currentUserID(c), c.Query("range"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}

// Curve GET /api/positions/curve?days= —— 真实持仓资产曲线（B7，读快照表）。
func (pc *PositionController) Curve(c *gin.Context) {
	days := 0
	if raw := c.Query("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			days = n
		}
	}
	out, err := service.PortfolioCurve(currentUserID(c), model.SnapshotKindReal, days)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}
