package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// MarketController 行情相关接口。
type MarketController struct {
	svc       *service.MarketService
	score     *service.ScoreService
	indicator *service.IndicatorService
	chip      *service.ChipService
}

func NewMarketController(svc *service.MarketService, score *service.ScoreService,
	indicator *service.IndicatorService, chip *service.ChipService) *MarketController {
	return &MarketController{svc: svc, score: score, indicator: indicator, chip: chip}
}

// GetOverview GET /api/markets/:market/overview
func (mc *MarketController) GetOverview(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	if market == "" {
		market = "cn"
	}
	ov := mc.svc.GetOverview(c.Request.Context(), market)
	common.ApiSuccess(c, ov)
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

// GetScore GET /api/markets/:market/stocks/:symbol/score
func (mc *MarketController) GetScore(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	symbol := strings.TrimSpace(c.Param("symbol"))
	if symbol == "" {
		common.ApiErrorMsg(c, "symbol 不能为空")
		return
	}
	v, err := mc.score.Score(c.Request.Context(), market, symbol)
	if err != nil {
		common.ApiErrorMsg(c, "评分失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// GetValuation GET /api/markets/:market/stocks/:symbol/valuation
func (mc *MarketController) GetValuation(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	symbol := strings.TrimSpace(c.Param("symbol"))
	if symbol == "" {
		common.ApiErrorMsg(c, "symbol 不能为空")
		return
	}
	v, err := mc.svc.GetValuation(c.Request.Context(), market, symbol)
	if err != nil {
		common.ApiErrorMsg(c, "获取估值失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// GetIndicators GET /api/markets/:market/stocks/:symbol/indicators?limit=120
// 返回与 K 线对齐的 MACD/BOLL/RSI/ATR 序列（详情页副图；后端统一口径计算）。
func (mc *MarketController) GetIndicators(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	symbol := strings.TrimSpace(c.Param("symbol"))
	if symbol == "" {
		common.ApiErrorMsg(c, "symbol 不能为空")
		return
	}
	limit := 120
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 250 {
			limit = n
		}
	}
	view, err := mc.indicator.Series(c.Request.Context(), market, symbol, limit)
	if err != nil {
		common.ApiErrorMsg(c, "获取指标失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, view)
}

// GetChips GET /api/markets/:market/stocks/:symbol/chips
// 筹码分布本地复算（210 根日线 + 换手率三角衰减模型）。
func (mc *MarketController) GetChips(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	symbol := strings.TrimSpace(c.Param("symbol"))
	if symbol == "" {
		common.ApiErrorMsg(c, "symbol 不能为空")
		return
	}
	view, err := mc.chip.Distribution(c.Request.Context(), market, symbol)
	if err != nil {
		common.ApiErrorMsg(c, "获取筹码分布失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, view)
}

// --- 管理员：市场数据维护 ---

// SyncBars POST /api/admin/market/sync-bars
// 批量同步已跟踪股票日线。耗时较长，异步执行，立即返回"已启动"。
func (mc *MarketController) SyncBars(c *gin.Context) {
	market := strings.ToLower(c.DefaultQuery("market", "cn"))
	// 预检：已有一轮在跑就如实返回 started:false（原实现无条件 started:true，把后台被吞的
	// ErrSyncInProgress 掩盖成「又启动了」）。
	if service.IsSyncingBars() {
		common.ApiSuccess(c, gin.H{"started": false, "task": "sync_daily_bars", "market": market})
		return
	}
	// 用后台上下文，避免请求结束即取消这个长任务。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if _, err := mc.svc.SyncTrackedDailyBars(ctx, market, 120); err != nil &&
			!errors.Is(err, service.ErrSyncInProgress) {
			common.SysWarn("手动批量同步日线失败: %v", err)
		}
	}()
	common.ApiSuccess(c, gin.H{"started": true, "task": "sync_daily_bars", "market": market})
}

// BackfillCalendar POST /api/admin/market/backfill-calendar
func (mc *MarketController) BackfillCalendar(c *gin.Context) {
	market := strings.ToLower(c.DefaultQuery("market", "cn"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	log, err := mc.svc.BackfillCalendar(ctx, market)
	if err != nil {
		common.ApiErrorMsg(c, "回填交易日历失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, log)
}

// Snapshot POST /api/admin/market/snapshot
func (mc *MarketController) Snapshot(c *gin.Context) {
	market := strings.ToLower(c.DefaultQuery("market", "cn"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	snap, err := mc.svc.SnapshotMarket(ctx, market)
	if err != nil {
		common.ApiErrorMsg(c, "生成市场情绪快照失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, snap)
}

// SyncLogs GET /api/admin/market/sync-logs?limit=50
func (mc *MarketController) SyncLogs(c *gin.Context) {
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	logs, err := mc.svc.RecentSyncLogs(limit)
	if err != nil {
		common.ApiErrorMsg(c, "查询同步日志失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, logs)
}

// DataSources GET /api/admin/datasources —— 数据源健康端点：每 (源,能力) 的
// 滑窗统计（success/empty/error/平均延迟）与冷却状态（S1 健康滑窗）。
func (mc *MarketController) DataSources(c *gin.Context) {
	common.ApiSuccess(c, gin.H{"health": mc.svc.DataSourceHealth()})
}

// --- 管理员：全市场日线（M1） ---

// WideSync POST /api/admin/market/wide-sync
// 手动触发全市场日线增量（clist 快照落当日 bar + 除权初筛）。异步执行，立即返回。
func (mc *MarketController) WideSync(c *gin.Context) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := mc.svc.SyncMarketWide(ctx); err != nil &&
			!errors.Is(err, service.ErrSyncInProgress) {
			common.SysWarn("手动全市场增量失败: %v", err)
		}
	}()
	common.ApiSuccess(c, gin.H{"started": true, "task": "sync_market_wide"})
}

// WideInitStart POST /api/admin/market/wide-init
// 启动/续跑全市场历史初始化（断点续传，已在跑则报错）。
func (mc *MarketController) WideInitStart(c *gin.Context) {
	if err := mc.svc.StartMarketWideInit(); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"started": true, "task": "init_market_history"})
}

// WideInitPause POST /api/admin/market/wide-init/pause
// 暂停历史初始化（进度在表内，再次启动即从断点续跑）。
func (mc *MarketController) WideInitPause(c *gin.Context) {
	common.ApiSuccess(c, gin.H{"paused": mc.svc.PauseMarketWideInit()})
}

// WideStatus GET /api/admin/market/wide-status
// 全市场覆盖状态：宇宙内 pending/done/failed 计数、任务运行标志、最近增量/初始化日志。
func (mc *MarketController) WideStatus(c *gin.Context) {
	v, err := mc.svc.MarketWideStatus()
	if err != nil {
		common.ApiErrorMsg(c, "查询全市场状态失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// FactorIC GET /api/admin/market/factor-ic?refresh=1 —— S3-4 因子 RankIC 验证报表
//（管理端只读页）。默认返回进程内缓存；无缓存或 refresh=1 时全量重算（数秒级，
// 全局互斥）。纯程序计算零 LLM 调用。
func (mc *MarketController) FactorIC(c *gin.Context) {
	if c.Query("refresh") != "1" {
		if rep := service.CachedFactorICReport(); rep != nil {
			common.ApiSuccess(c, rep)
			return
		}
	}
	rep, err := service.RunFactorIC(c.Request.Context(), mc.svc)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rep)
}

// WalkForward GET /api/admin/market/walk-forward?refresh=1 —— S3-5 walk-forward
// 评估基线报表（管理端只读页）。默认返回进程内缓存；无缓存或 refresh=1 时全量
// 重算（每信号日一次全市场 as-of 因子重算，数十秒级，全局互斥）。纯程序计算
// 零 LLM 调用，不改写任何推荐行为。
func (mc *MarketController) WalkForward(c *gin.Context) {
	if c.Query("refresh") != "1" {
		if rep := service.CachedWalkForwardReport(); rep != nil {
			common.ApiSuccess(c, rep)
			return
		}
	}
	rep, err := service.RunWalkForward(c.Request.Context(), mc.svc)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rep)
}
