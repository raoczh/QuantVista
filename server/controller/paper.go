package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/model"
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
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindPaper)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	ov, err := pc.svc.OverviewByAccount(c.Request.Context(), currentUserID(c), account.ID)
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
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindPaper)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	t, err := pc.svc.TradeByAccount(c.Request.Context(), currentUserID(c), account.ID, in)
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
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindPaper)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	rows, err := pc.svc.TradesByAccount(currentUserID(c), account.ID, limit)
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
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindPaper)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	acc, err := pc.svc.ResetByAccount(currentUserID(c), account.ID, body.InitialCash)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, acc)
}

// Curve GET /api/paper/curve?days= —— 模拟盘资产曲线（B7，读快照表）。
func (pc *PaperController) Curve(c *gin.Context) {
	days := 0
	if raw := c.Query("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			days = n
		}
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindPaper)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	out, err := service.PortfolioCurveByAccount(currentUserID(c), account.ID, model.SnapshotKindPaper, days)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}
