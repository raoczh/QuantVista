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
