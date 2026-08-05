package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

const maintenanceBodyMaxBytes = 4096

// bindMaintenanceRequest 区分“完全无 body”的旧客户端与新 JSON 契约。只接受白名单字段，
// 且硬限 4KiB，后续审计只使用解析后的摘要，不保存原始正文。
func bindMaintenanceRequest(c *gin.Context) (service.MaintenanceRequest, bool, error) {
	var req service.MaintenanceRequest
	if c.Request.Body == nil {
		return req, false, nil
	}
	if c.Request.ContentLength > maintenanceBodyMaxBytes {
		return req, true, errors.New("补采请求正文超过 4KiB 上限")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maintenanceBodyMaxBytes+1))
	if err != nil {
		return req, true, err
	}
	if len(body) == 0 {
		return req, false, nil
	}
	if len(body) > maintenanceBodyMaxBytes {
		return req, true, errors.New("补采请求正文超过 4KiB 上限")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return req, true, errors.New("补采请求正文必须是 JSON 对象")
		}
		return req, true, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("请求只能包含一个 JSON 对象")
		}
		return req, true, err
	}
	return req, true, nil
}

func maintenanceRequest(c *gin.Context) (service.MaintenanceRequest, bool, error) {
	req, hasBody, err := bindMaintenanceRequest(c)
	if err != nil {
		return req, hasBody, err
	}
	if req.Market == "" {
		req.Market = strings.ToLower(c.DefaultQuery("market", "cn"))
	}
	return req, hasBody, nil
}

// MarketController 行情相关接口。
type MarketController struct {
	svc       *service.MarketService
	score     *service.ScoreService
	indicator *service.IndicatorService
	chip      *service.ChipService
	intraday  *service.IntradayService
}

func NewMarketController(svc *service.MarketService, score *service.ScoreService,
	indicator *service.IndicatorService, chip *service.ChipService,
	intraday *service.IntradayService) *MarketController {
	return &MarketController{svc: svc, score: score, indicator: indicator, chip: chip, intraday: intraday}
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
// 走新鲜行情链路（主源旧价不当成功，逐源找当前有效行情），响应统一携带 freshness 块
// （captured_at/source_data_time/expected_as_of/market_state/freshness_status/stale_reason），
// 前端据此区分「请求成功」与「数据仍然有效」。
func (mc *MarketController) GetQuote(c *gin.Context) {
	market := strings.ToLower(c.Param("market"))
	symbol := strings.TrimSpace(c.Param("symbol"))
	if symbol == "" {
		common.ApiErrorMsg(c, "symbol 不能为空")
		return
	}
	q, _, err := mc.svc.GetFreshQuote(c.Request.Context(), market, symbol)
	if err != nil {
		common.ApiErrorMsg(c, "获取行情失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, struct {
		*datasource.Quote
		Freshness *service.QuoteFreshnessView `json:"freshness"`
	}{q, mc.svc.FreshnessView(market, q)})
}

// GetMinuteLine GET /api/markets/:market/stocks/:symbol/minute
// 腾讯 m1 分时线按需读取，不落历史；非交易时段返回上游最近交易日并带 trade_date。
func (mc *MarketController) GetMinuteLine(c *gin.Context) {
	line, err := mc.intraday.MinuteLine(c.Request.Context(), c.Param("market"), c.Param("symbol"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, line)
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
	req, hasBody, err := maintenanceRequest(c)
	if err != nil {
		common.ApiErrorMsg(c, "补采参数无效: "+err.Error())
		return
	}
	audit := service.AdminSyncAudit(currentUserID(c), req, !hasBody)
	if hasBody && req.DryRun {
		plan, err := mc.svc.PlanMaintenance(service.MaintenanceSyncBars, req)
		if err != nil {
			common.ApiErrorMsg(c, "生成日线补采计划失败: "+err.Error())
			return
		}
		common.ApiSuccess(c, gin.H{"dry_run": true, "plan": plan})
		return
	}
	// 预检：已有一轮在跑就如实返回 started:false（原实现无条件 started:true，把后台被吞的
	// ErrSyncInProgress 掩盖成「又启动了」）。
	if service.IsSyncingBars() {
		common.ApiSuccess(c, gin.H{"started": false, "task": service.MaintenanceSyncBars, "market": req.Market})
		return
	}
	if hasBody {
		if err := mc.svc.ValidateMaintenancePlan(service.MaintenanceSyncBars, req); err != nil {
			mc.svc.RecordMaintenanceFailure(service.MaintenanceSyncBars, req.Market, audit, err)
			common.ApiErrorMsg(c, err.Error())
			return
		}
	}
	// 用后台上下文，避免请求结束即取消这个长任务。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		var logErr error
		if hasBody {
			var log any
			log, logErr = mc.svc.RunSyncBarsPlan(ctx, req, audit)
			if log == nil && logErr != nil {
				mc.svc.RecordMaintenanceFailure(service.MaintenanceSyncBars, req.Market, audit, logErr)
			}
		} else {
			_, logErr = mc.svc.SyncTrackedDailyBarsWithAudit(ctx, req.Market, 120, audit)
		}
		if logErr != nil &&
			!errors.Is(logErr, service.ErrSyncInProgress) {
			common.SysWarn("手动批量同步日线失败: %v", logErr)
		}
	}()
	common.ApiSuccess(c, gin.H{"started": true, "task": service.MaintenanceSyncBars, "market": req.Market, "plan_hash": req.PlanHash})
}

// BackfillCalendar POST /api/admin/market/backfill-calendar
func (mc *MarketController) BackfillCalendar(c *gin.Context) {
	req, hasBody, err := maintenanceRequest(c)
	if err != nil {
		common.ApiErrorMsg(c, "补采参数无效: "+err.Error())
		return
	}
	audit := service.AdminSyncAudit(currentUserID(c), req, !hasBody)
	if hasBody && req.DryRun {
		plan, err := mc.svc.PlanMaintenance(service.MaintenanceBackfillCalendar, req)
		if err != nil {
			common.ApiErrorMsg(c, "生成日历回填计划失败: "+err.Error())
			return
		}
		common.ApiSuccess(c, gin.H{"dry_run": true, "plan": plan})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	var log any
	if hasBody {
		if err := mc.svc.ValidateMaintenancePlan(service.MaintenanceBackfillCalendar, req); err != nil {
			mc.svc.RecordMaintenanceFailure(service.MaintenanceBackfillCalendar, req.Market, audit, err)
			common.ApiErrorMsg(c, err.Error())
			return
		}
		log, err = mc.svc.RunCalendarPlan(ctx, req, audit)
	} else {
		log, err = mc.svc.BackfillCalendarWithAudit(ctx, req.Market, audit)
	}
	if err != nil {
		common.ApiErrorMsg(c, "回填交易日历失败: "+err.Error())
		return
	}
	if hasBody {
		common.ApiSuccess(c, gin.H{"dry_run": false, "plan_hash": req.PlanHash, "log": log})
	} else {
		common.ApiSuccess(c, log)
	}
}

// Snapshot POST /api/admin/market/snapshot
func (mc *MarketController) Snapshot(c *gin.Context) {
	market := strings.ToLower(c.DefaultQuery("market", "cn"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	audit := service.SyncAudit{TriggerSource: "admin", UserID: currentUserID(c), ParameterSummary: "manual_snapshot=true"}
	snap, err := mc.svc.SnapshotMarketWithAudit(ctx, market, audit)
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

// DataHealth GET /api/admin/data-health —— P1 数据健康总览：各数据域的
// expected/observed 日期、落后开市日数、覆盖率与最近任务日志（对账入口，
// 补跑走既有 wide-sync/wide-init/sync-bars/snapshot/factor-rebuild 接口）。
func (mc *MarketController) DataHealth(c *gin.Context) {
	days := service.DataHealthDefaultDays
	if raw := c.Query("days"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			days = parsed
		}
	}
	common.ApiSuccess(c, service.BuildDataHealthReportForDays(days))
}

// --- 管理员：全市场日线（M1） ---

// WideSync POST /api/admin/market/wide-sync
// 手动触发全市场日线增量（clist 快照落当日 bar + 除权初筛）。异步执行，立即返回。
func (mc *MarketController) WideSync(c *gin.Context) {
	req, hasBody, err := maintenanceRequest(c)
	if err != nil {
		common.ApiErrorMsg(c, "补采参数无效: "+err.Error())
		return
	}
	audit := service.AdminSyncAudit(currentUserID(c), req, !hasBody)
	if hasBody && req.DryRun {
		plan, err := mc.svc.PlanMaintenance(service.MaintenanceWideSync, req)
		if err != nil {
			common.ApiErrorMsg(c, "生成全市场同步计划失败: "+err.Error())
			return
		}
		common.ApiSuccess(c, gin.H{"dry_run": true, "plan": plan})
		return
	}
	if hasBody {
		if err := mc.svc.ValidateMaintenancePlan(service.MaintenanceWideSync, req); err != nil {
			mc.svc.RecordMaintenanceFailure(service.MaintenanceWideSync, req.Market, audit, err)
			common.ApiErrorMsg(c, err.Error())
			return
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var log any
		var runErr error
		if hasBody {
			log, runErr = mc.svc.RunMarketWidePlan(ctx, req, audit)
		} else {
			log, runErr = mc.svc.SyncMarketWideWithAudit(ctx, audit)
		}
		if log == nil && runErr != nil {
			mc.svc.RecordMaintenanceFailure(service.MaintenanceWideSync, req.Market, audit, runErr)
		}
		if runErr != nil && !errors.Is(runErr, service.ErrSyncInProgress) {
			common.SysWarn("手动全市场增量失败: %v", runErr)
		}
	}()
	common.ApiSuccess(c, gin.H{"started": true, "task": service.MaintenanceWideSync, "plan_hash": req.PlanHash})
}

// WideInitStart POST /api/admin/market/wide-init
// 启动/续跑全市场历史初始化（断点续传，已在跑则报错）。
func (mc *MarketController) WideInitStart(c *gin.Context) {
	audit := service.SyncAudit{TriggerSource: "admin", UserID: currentUserID(c), ParameterSummary: "resume=true"}
	if err := mc.svc.StartMarketWideInitWithAudit(audit); err != nil {
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
// （管理端只读页）。默认返回进程内缓存；无缓存或 refresh=1 时全量重算（数秒级，
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
