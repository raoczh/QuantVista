package router

import (
	"time"

	"quantvista/controller"
	"quantvista/datasource"
	"quantvista/middleware"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// SetApiRouter 注册所有 /api 路由。
func SetApiRouter(r *gin.Engine, mgr *datasource.Manager) {
	// services
	marketSvc := service.NewMarketService(mgr)
	authSvc := service.NewAuthService()
	userSvc := service.NewUserService()
	llmSvc := service.NewLLMService()
	adminSvc := service.NewAdminService()
	watchlistSvc := service.NewWatchlistService(marketSvc)
	positionSvc := service.NewPositionService(marketSvc)

	// controllers
	marketCtl := controller.NewMarketController(marketSvc)
	authCtl := controller.NewAuthController(authSvc)
	setupCtl := controller.NewSetupController(authSvc)
	userCtl := controller.NewUserController(userSvc)
	llmCtl := controller.NewLLMController(llmSvc)
	adminCtl := controller.NewAdminController(adminSvc)
	watchlistCtl := controller.NewWatchlistController(watchlistSvc)
	positionCtl := controller.NewPositionController(positionSvc)

	api := r.Group("/api")
	{
		api.GET("/status", controller.Status)

		// 首启引导（公开）
		setup := api.Group("/setup")
		{
			setup.GET("/status", setupCtl.Status)
			setup.POST("/admin", middleware.RateLimit(5, time.Minute), setupCtl.CreateAdmin)
		}

		// 认证（公开）
		auth := api.Group("/auth")
		{
			auth.POST("/login", middleware.RateLimit(10, time.Minute), authCtl.Login)
			auth.POST("/refresh", middleware.RateLimit(30, time.Minute), authCtl.Refresh)
			auth.POST("/logout", authCtl.Logout)
		}
		gh := api.Group("/oauth/github")
		{
			gh.GET("/url", authCtl.GitHubURL)
			gh.POST("", middleware.RateLimit(20, time.Minute), authCtl.GitHubCallback)
		}

		// 市场行情（公开，公开市场数据）
		markets := api.Group("/markets")
		{
			markets.GET("/:market/overview", marketCtl.GetOverview)
			markets.GET("/:market/stocks/:symbol/quote", marketCtl.GetQuote)
			markets.GET("/:market/stocks/:symbol/bars", marketCtl.GetDailyBars)
		}

		// 需登录
		authed := api.Group("")
		authed.Use(middleware.JWTAuth())
		{
			user := authed.Group("/user")
			{
				user.GET("/self", userCtl.GetSelf)
				user.GET("/preference", userCtl.GetPreference)
				user.PUT("/preference", userCtl.UpdatePreference)
				user.GET("/quota", userCtl.GetQuota)
				user.PUT("/password", userCtl.ChangePassword)
			}

			llm := authed.Group("/llm-configs")
			{
				llm.GET("", llmCtl.List)
				llm.POST("", llmCtl.Create)
				llm.PUT("/:id", llmCtl.Update)
				llm.DELETE("/:id", llmCtl.Delete)
				llm.POST("/:id/test", llmCtl.Test)
			}
			// 草稿测试单独成路径，避免与 /llm-configs/:id 的参数段冲突。
			authed.POST("/llm-config-test", llmCtl.TestDraft)

			// 自选股（分组 + 条目，按用户隔离）
			watchlists := authed.Group("/watchlists")
			{
				watchlists.GET("", watchlistCtl.List)
				watchlists.POST("", watchlistCtl.CreateGroup)
				watchlists.PUT("/:id", watchlistCtl.UpdateGroup)
				watchlists.DELETE("/:id", watchlistCtl.DeleteGroup)
				watchlists.POST("/:id/items", watchlistCtl.AddItem)
			}
			// 条目改删用独立前缀，避免与 /watchlists/:id 的参数段语义混淆。
			wlItems := authed.Group("/watchlist-items")
			{
				wlItems.PUT("/:id", watchlistCtl.UpdateItem)
				wlItems.DELETE("/:id", watchlistCtl.DeleteItem)
			}

			// 已购入持仓（按用户隔离）
			positions := authed.Group("/positions")
			{
				positions.GET("", positionCtl.List)
				positions.POST("", positionCtl.Create)
				positions.PUT("/:id", positionCtl.Update)
				positions.DELETE("/:id", positionCtl.Delete)
				positions.POST("/:id/close", positionCtl.Close)
			}

			// 管理员后台
			admin := authed.Group("/admin")
			admin.Use(middleware.AdminAuth())
			{
				admin.GET("/settings", adminCtl.GetSettings)
				admin.PUT("/settings", adminCtl.UpdateSettings)
				admin.GET("/users", adminCtl.ListUsers)
				admin.PUT("/users/:id/status", adminCtl.SetUserStatus)

				// 市场数据维护（手动触发批量同步/日历回填/情绪快照）
				adminMarket := admin.Group("/market")
				{
					adminMarket.POST("/sync-bars", marketCtl.SyncBars)
					adminMarket.POST("/backfill-calendar", marketCtl.BackfillCalendar)
					adminMarket.POST("/snapshot", marketCtl.Snapshot)
					adminMarket.GET("/sync-logs", marketCtl.SyncLogs)
				}
			}
		}
	}
}
