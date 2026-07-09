package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// MoodController M3a 扩展数据查询：个股资金流图 / 龙虎榜上榜记录。
type MoodController struct {
	svc *service.MoodService
}

func NewMoodController(svc *service.MoodService) *MoodController {
	return &MoodController{svc: svc}
}

// StockFundFlow GET /api/markets/:market/stocks/:symbol/fundflow?days=
// 个股详情「主力资金」块：逐日主力净额序列 + 今日/5/10/20 日汇总与连续净流入天数。
// 首次访问触发按需拉取（冷却 1h），非 A 股/基金返回空序列。
func (mc *MoodController) StockFundFlow(c *gin.Context) {
	days := 0
	if s := c.Query("days"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			days = n
		}
	}
	out, err := mc.svc.StockFundFlow(c.Request.Context(), c.Param("market"), c.Param("symbol"), days)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}

// StockLhb GET /api/markets/:market/stocks/:symbol/lhb?limit=
// 个股详情「龙虎榜上榜记录」块（本地缓存表查询，盘后 job 采集+近 30 天回填）。
func (mc *MoodController) StockLhb(c *gin.Context) {
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	common.ApiSuccess(c, mc.svc.StockLhbRecords(c.Param("symbol"), limit))
}
