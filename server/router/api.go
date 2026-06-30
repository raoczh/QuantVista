package router

import (
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

	// controllers
	marketCtl := controller.NewMarketController(marketSvc)
	authCtl := controller.NewAuthController(authSvc)
	setupCtl := controller.NewSetupController(authSvc)
	userCtl := controller.NewUserController(userSvc)
	llmCtl := controller.NewLLMController(llmSvc)
	adminCtl := controller.NewAdminController(adminSvc)

	api := r.Group("/api")
	{
		api.GET("/status", controller.Status)

		// 首启引导（公开）
		setup := api.Group("/setup")
		{
			setup.GET("/status", setupCtl.Status)
			setup.POST("/admin", setupCtl.CreateAdmin)
		}

		// 认证（公开）
		auth := api.Group("/auth")
		{
			auth.POST("/login", authCtl.Login)
			auth.POST("/refresh", authCtl.Refresh)
			auth.POST("/logout", authCtl.Logout)
		}
		gh := api.Group("/oauth/github")
		{
			gh.GET("/url", authCtl.GitHubURL)
			gh.POST("", authCtl.GitHubCallback)
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

			// 管理员后台
			admin := authed.Group("/admin")
			admin.Use(middleware.AdminAuth())
			{
				admin.GET("/settings", adminCtl.GetSettings)
				admin.PUT("/settings", adminCtl.UpdateSettings)
				admin.GET("/users", adminCtl.ListUsers)
				admin.PUT("/users/:id/status", adminCtl.SetUserStatus)
			}
		}
	}
}
