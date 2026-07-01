package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// PaperController 模拟交易（限当前登录用户）。
type PaperController struct {
	svc *service.PaperService
}

func NewPaperController(svc *service.PaperService) *PaperController {
	return &PaperController{svc: svc}
}

// Overview GET /api/paper/overview —— 账户总览（现金 + 持仓估值 + 盈亏）。
func (pc *PaperController) Overview(c *gin.Context) {
	ov, err := pc.svc.Overview(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, ov)
}

// Trade POST /api/paper/trade —— 模拟买/卖。
func (pc *PaperController) Trade(c *gin.Context) {
	var in service.TradeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	t, err := pc.svc.Trade(c.Request.Context(), currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, t)
}

// Trades GET /api/paper/trades?limit= —— 成交流水。
func (pc *PaperController) Trades(c *gin.Context) {
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := pc.svc.Trades(currentUserID(c), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Reset POST /api/paper/reset —— 重置账户（可指定初始资金）。
func (pc *PaperController) Reset(c *gin.Context) {
	var body struct {
		InitialCash float64 `json:"initial_cash"`
	}
	_ = c.ShouldBindJSON(&body)
	acc, err := pc.svc.Reset(currentUserID(c), body.InitialCash)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, acc)
}
