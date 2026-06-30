package router

import (
	"quantvista/controller"
	"quantvista/datasource"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// SetApiRouter 注册所有 /api 路由。
func SetApiRouter(r *gin.Engine, mgr *datasource.Manager) {
	marketSvc := service.NewMarketService(mgr)
	marketCtl := controller.NewMarketController(marketSvc)

	api := r.Group("/api")
	{
		api.GET("/status", controller.Status)

		markets := api.Group("/markets")
		{
			// 市场首页概览：GET /api/markets/:market/overview
			markets.GET("/:market/overview", marketCtl.GetOverview)
			// 行情：GET /api/markets/:market/stocks/:symbol/quote
			markets.GET("/:market/stocks/:symbol/quote", marketCtl.GetQuote)
			markets.GET("/:market/stocks/:symbol/bars", marketCtl.GetDailyBars)
		}

		// TODO(阶段1+)：/oauth /user /watchlists /positions /ai /recommendations /settings
	}
}
