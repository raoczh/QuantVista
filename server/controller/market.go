package controller

import (
	"strconv"
	"strings"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// MarketController 行情相关接口。
type MarketController struct {
	svc *service.MarketService
}

func NewMarketController(svc *service.MarketService) *MarketController {
	return &MarketController{svc: svc}
}

// GetQuote GET /api/markets/:market/stocks/:symbol/quote
func (mc *MarketController) GetQuote(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	symbol := strings.TrimSpace(c.Param("symbol"))
	if symbol == "" {
		common.ApiErrorMsg(c, "symbol 不能为空")
		return
	}
	q, err := mc.svc.GetQuote(c.Request.Context(), market, symbol)
	if err != nil {
		common.ApiErrorMsg(c, "获取行情失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, q)
}

// GetDailyBars GET /api/markets/:market/stocks/:symbol/bars?limit=120
func (mc *MarketController) GetDailyBars(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	symbol := strings.TrimSpace(c.Param("symbol"))
	if symbol == "" {
		common.ApiErrorMsg(c, "symbol 不能为空")
		return
	}
	limit := 120
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	bars, err := mc.svc.GetDailyBars(c.Request.Context(), market, symbol, limit)
	if err != nil {
		common.ApiErrorMsg(c, "获取日线失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, bars)
}
