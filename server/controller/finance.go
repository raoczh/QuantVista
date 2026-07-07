package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// FinanceController F1 财报日历/公告查询。
type FinanceController struct {
	svc *service.FinanceService
}

func NewFinanceController(svc *service.FinanceService) *FinanceController {
	return &FinanceController{svc: svc}
}

// Announcements GET /api/announcements?symbol=&limit=
// 个股详情「公告」块数据源；库中无该股记录时按需实时补拉一次（冷却 1h）。
func (fc *FinanceController) Announcements(c *gin.Context) {
	symbol := c.Query("symbol")
	if symbol == "" {
		common.ApiErrorMsg(c, "缺少 symbol 参数")
		return
	}
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := fc.svc.ListAnnouncements(c.Request.Context(), symbol, limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// StockFinance GET /api/markets/:market/stocks/:symbol/finance
// 个股详情「财务」块：最近 8 期主要指标与三表关键科目（升序）。首次访问触发
// 按需同步（F10 单请求 + 三表约 7 次上游请求，冷却 1h），非 A 股口径返回空集。
func (fc *FinanceController) StockFinance(c *gin.Context) {
	out, err := fc.svc.FinanceOverview(c.Request.Context(), c.Param("symbol"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}
