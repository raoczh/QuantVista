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
	analysisSvc := service.NewAnalysisService(marketSvc, watchlistSvc, positionSvc, llmSvc)
	recommendationSvc := service.NewRecommendationService(marketSvc, watchlistSvc, llmSvc)
	trackingSvc := service.NewTrackingService(marketSvc)
	alertSvc := service.NewAlertService(marketSvc)
	thesisSvc := service.NewThesisService(marketSvc)
	noteSvc := service.NewNoteService(marketSvc)
	todoSvc := service.NewTodoService(alertSvc, positionSvc, thesisSvc)
	qaSvc := service.NewQaService(marketSvc, llmSvc)
	compareSvc := service.NewCompareService(marketSvc, llmSvc)
	scoreSvc := service.NewScoreService(marketSvc)
	indicatorSvc := service.NewIndicatorService(marketSvc)
	chipSvc := service.NewChipService(marketSvc)
	paperSvc := service.NewPaperService(marketSvc)
	etfSvc := service.NewEtfService(marketSvc)
	notifySvc := service.NewNotifyService()
	promptSvc := service.NewPromptService()
	dailyReportSvc := service.NewDailyReportService(marketSvc, watchlistSvc, positionSvc, alertSvc, recommendationSvc, llmSvc, notifySvc)
	newsSvc := service.NewNewsService()
	financeSvc := service.NewFinanceService()

	// controllers
	marketCtl := controller.NewMarketController(marketSvc, scoreSvc, indicatorSvc, chipSvc)
	authCtl := controller.NewAuthController(authSvc)
	setupCtl := controller.NewSetupController(authSvc)
	userCtl := controller.NewUserController(userSvc)
	llmCtl := controller.NewLLMController(llmSvc)
	adminCtl := controller.NewAdminController(adminSvc)
	watchlistCtl := controller.NewWatchlistController(watchlistSvc)
	positionCtl := controller.NewPositionController(positionSvc)
	analysisCtl := controller.NewAnalysisController(analysisSvc)
	recommendationCtl := controller.NewRecommendationController(recommendationSvc, trackingSvc)
	alertCtl := controller.NewAlertController(alertSvc)
	todoCtl := controller.NewTodoController(todoSvc)
	qaCtl := controller.NewQaController(qaSvc)
	compareCtl := controller.NewCompareController(compareSvc)
	paperCtl := controller.NewPaperController(paperSvc)
	etfCtl := controller.NewEtfController(etfSvc)
	notifyCtl := controller.NewNotifyController(notifySvc)
	promptCtl := controller.NewPromptController(promptSvc)
	thesisCtl := controller.NewThesisController(thesisSvc)
	noteCtl := controller.NewNoteController(noteSvc)
	exportCtl := controller.NewExportController(service.NewExportService())
	dailyReportCtl := controller.NewDailyReportController(dailyReportSvc)
	newsCtl := controller.NewNewsController(newsSvc)
	financeCtl := controller.NewFinanceController(financeSvc)

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

		// 市场行情（公开市场数据；宽松限流防外部脚本刷接口——缓存 miss 时会
		// 直连上游并落库，被刷会导致服务器 IP 被数据源封禁 + stocks 表被灌满）
		markets := api.Group("/markets")
		markets.Use(middleware.RateLimit(120, time.Minute))
		{
			markets.GET("/:market/overview", marketCtl.GetOverview)
			markets.GET("/:market/stocks/:symbol/quote", marketCtl.GetQuote)
			markets.GET("/:market/stocks/:symbol/bars", marketCtl.GetDailyBars)
			markets.GET("/:market/stocks/:symbol/score", marketCtl.GetScore)
			markets.GET("/:market/stocks/:symbol/valuation", marketCtl.GetValuation)
			markets.GET("/:market/stocks/:symbol/indicators", marketCtl.GetIndicators)
			markets.GET("/:market/stocks/:symbol/chips", marketCtl.GetChips)
			markets.GET("/:market/stocks/:symbol/finance", financeCtl.StockFinance)
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
				// GitHub 绑定/解绑（发起绑定复用公开的 GET /oauth/github/url 拿授权地址）
				user.POST("/github/bind", middleware.RateLimit(10, time.Minute), authCtl.GitHubBind)
				user.DELETE("/github/bind", authCtl.GitHubUnbind)
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
				wlItems.GET("/missed", watchlistCtl.Missed) // 静态段先于 :id 注册
				wlItems.PUT("/:id", watchlistCtl.UpdateItem)
				wlItems.PUT("/:id/stage", watchlistCtl.SetItemStage)
				wlItems.DELETE("/:id", watchlistCtl.DeleteItem)
			}

			// 已购入持仓（按用户隔离）
			positions := authed.Group("/positions")
			{
				positions.GET("", positionCtl.List)
				positions.GET("/overview", positionCtl.Overview) // 静态段先于 :id
				positions.POST("", positionCtl.Create)
				positions.POST("/import", middleware.RateLimit(10, time.Minute), exportCtl.ImportPositions)
				positions.PUT("/:id", positionCtl.Update)
				positions.DELETE("/:id", positionCtl.Delete)
				positions.POST("/:id/close", positionCtl.Close)
			}

			// AI 分析中心（按用户隔离；发起分析限流，防止刷爆 LLM 配额与费用）
			analysis := authed.Group("/analysis")
			{
				analysis.POST("", middleware.RateLimit(20, time.Minute), analysisCtl.Create)
				analysis.GET("", analysisCtl.List)
				analysis.GET("/:id", analysisCtl.Get)
				analysis.GET("/:id/diff", analysisCtl.Diff)
				analysis.DELETE("/:id", analysisCtl.Delete)
			}

			// 短线/长线推荐（按用户隔离；生成走 LLM，限流控成本）
			recommendations := authed.Group("/recommendations")
			{
				recommendations.GET("/strategies", recommendationCtl.Strategies)
				recommendations.GET("/performance", recommendationCtl.Performance)
				recommendations.POST("", middleware.RateLimit(15, time.Minute), recommendationCtl.Generate)
				recommendations.GET("", recommendationCtl.List)
				recommendations.GET("/:id", recommendationCtl.Get)
				recommendations.POST("/:id/track", recommendationCtl.Track)
				recommendations.DELETE("/:id", recommendationCtl.Delete)
			}

			// 条件提醒（按用户隔离；命中落明细事件供待办/命中历史，配置了推送通道则额外推送）
			alerts := authed.Group("/alerts")
			{
				alerts.GET("", alertCtl.List)
				alerts.POST("", alertCtl.Create)
				alerts.POST("/evaluate", middleware.RateLimit(20, time.Minute), alertCtl.Evaluate)
				alerts.GET("/events", alertCtl.ListEvents)
				alerts.PUT("/events/read-all", alertCtl.ReadAllEvents)
				alerts.PUT("/events/:id/status", alertCtl.SetEventStatus)
				alerts.PUT("/:id", alertCtl.Update)
				alerts.PUT("/:id/status", alertCtl.SetStatus)
				alerts.DELETE("/:id", alertCtl.Delete)
			}

			// 今日待办/待复盘（聚合命中提醒 + 推荐/持仓复盘信号 + 逻辑卡到期）
			authed.GET("/todos", todoCtl.List)

			// 收盘日报（今日复盘 + 明日推荐；后台任务自动生成，手动重生成限流防连击烧 token）
			reports := authed.Group("/daily-reports")
			{
				reports.GET("", dailyReportCtl.List)
				reports.GET("/latest", dailyReportCtl.Latest)
				reports.GET("/:id", dailyReportCtl.Get)
				reports.POST("/generate", middleware.RateLimit(5, time.Minute), dailyReportCtl.Generate)
			}

			// 新闻/快讯（后台任务采集入库，此处只读查询）
			authed.GET("/news", newsCtl.List)

			// 个股公告（F1：后台按自选∪持仓每日采集；查询时可按需补拉，限流防刷上游）
			authed.GET("/announcements", middleware.RateLimit(60, time.Minute), financeCtl.Announcements)

			// 用户数据 CSV 导出（positions/watchlist/recommendations/analyses）
			authed.GET("/export/:kind", middleware.RateLimit(10, time.Minute), exportCtl.Export)

			// 投资逻辑卡片（结构化研究假设：核心逻辑/证据/风险/失效条件/复盘日期）
			thesis := authed.Group("/thesis-cards")
			{
				thesis.GET("", thesisCtl.List)
				thesis.GET("/checkup", thesisCtl.CheckUp)
				thesis.POST("", thesisCtl.Upsert)
				thesis.PUT("/:id/status", thesisCtl.SetStatus)
				thesis.DELETE("/:id", thesisCtl.Delete)
			}

			// 投资笔记/决策日志（自由笔记，可选绑定标的形成个股时间线）
			notes := authed.Group("/notes")
			{
				notes.GET("", noteCtl.List)
				notes.POST("", noteCtl.Create)
				notes.PUT("/:id", noteCtl.Update)
				notes.DELETE("/:id", noteCtl.Delete)
			}

			// 个股 AI 问答（多轮，复用数据快照；走 LLM，限流控成本）
			qa := authed.Group("/qa")
			{
				qa.POST("/ask", middleware.RateLimit(20, time.Minute), qaCtl.Ask)
				qa.POST("/ask-stream", middleware.RateLimit(20, time.Minute), qaCtl.AskStream)
				qa.GET("", qaCtl.List)
				qa.GET("/:id", qaCtl.Get)
				qa.GET("/:id/snapshot", qaCtl.Snapshot)
				qa.DELETE("/:id", qaCtl.Delete)
			}

			// 个股横向对比（多股并排 + 可选 AI 点评，走 LLM 限流）
			authed.POST("/compare", middleware.RateLimit(20, time.Minute), compareCtl.Compare)

			// 模拟交易（虚拟账户，用真实行情成交与估值）
			paper := authed.Group("/paper")
			{
				paper.GET("/overview", paperCtl.Overview)
				paper.POST("/trade", middleware.RateLimit(60, time.Minute), paperCtl.Trade)
				paper.GET("/trades", paperCtl.Trades)
				paper.POST("/reset", paperCtl.Reset)
			}

			// 指数 ETF 清单（精选宽基/行业/跨境，实时行情富化；交易复用 /paper/trade）
			authed.GET("/etf/list", etfCtl.List)

			// 推送通道（Server酱/webhook；提醒命中时主动推送）
			notify := authed.Group("/notify-channels")
			{
				notify.GET("", notifyCtl.List)
				notify.POST("", notifyCtl.Create)
				notify.PUT("/:id", notifyCtl.Update)
				notify.DELETE("/:id", notifyCtl.Delete)
				notify.POST("/:id/test", middleware.RateLimit(10, time.Minute), notifyCtl.Test)
			}

			// 自定义分析提示词模板（启用后覆盖对应模块默认指引）
			prompts := authed.Group("/prompt-templates")
			{
				prompts.GET("/modules", promptCtl.Modules)
				prompts.GET("", promptCtl.List)
				prompts.POST("", promptCtl.Upsert)
				prompts.DELETE("/:id", promptCtl.Delete)
			}

			// 管理员后台
			admin := authed.Group("/admin")
			admin.Use(middleware.AdminAuth())
			{
				admin.GET("/settings", adminCtl.GetSettings)
				admin.PUT("/settings", adminCtl.UpdateSettings)
				admin.GET("/users", adminCtl.ListUsers)
				admin.PUT("/users/:id/status", adminCtl.SetUserStatus)
				admin.GET("/users/:id/quota", adminCtl.GetUserQuota)
				admin.PUT("/users/:id/quota", adminCtl.UpdateUserQuota)

				// 数据源健康端点（S1 健康滑窗：每 (源,能力) success/empty/error 与冷却状态）
				admin.GET("/datasources", marketCtl.DataSources)

				// 市场数据维护（手动触发批量同步/日历回填/情绪快照）
				adminMarket := admin.Group("/market")
				{
					adminMarket.POST("/sync-bars", marketCtl.SyncBars)
					adminMarket.POST("/backfill-calendar", marketCtl.BackfillCalendar)
					adminMarket.POST("/snapshot", marketCtl.Snapshot)
					adminMarket.GET("/sync-logs", marketCtl.SyncLogs)
					// M1 全市场日线：增量同步 / 历史初始化（断点续传，可暂停）/ 覆盖状态
					adminMarket.POST("/wide-sync", marketCtl.WideSync)
					adminMarket.POST("/wide-init", marketCtl.WideInitStart)
					adminMarket.POST("/wide-init/pause", marketCtl.WideInitPause)
					adminMarket.GET("/wide-status", marketCtl.WideStatus)
				}
			}
		}
	}
}
