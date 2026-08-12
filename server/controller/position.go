package controller

import (
	"strconv"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// PositionController 已购入持仓（均限当前登录用户）。
type PositionController struct {
	svc *service.PositionService
}

func NewPositionController(svc *service.PositionService) *PositionController {
	return &PositionController{svc: svc}
}

// List GET /api/positions?status=holding|closed|all
func (pc *PositionController) List(c *gin.Context) {
	status := strings.ToLower(c.DefaultQuery("status", "all"))
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	list, err := pc.svc.ListByAccount(c.Request.Context(), currentUserID(c), account.ID, status)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, list)
}

// Overview GET /api/positions/overview —— 组合总览 + 个人风控信号。
func (pc *PositionController) Overview(c *gin.Context) {
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	ov, err := pc.svc.OverviewByAccount(c.Request.Context(), currentUserID(c), account.ID)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, ov)
}

// Create POST /api/positions
func (pc *PositionController) Create(c *gin.Context) {
	var in service.PositionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	p, err := pc.svc.CreateByAccount(c.Request.Context(), currentUserID(c), account.ID, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Update PUT /api/positions/:id
func (pc *PositionController) Update(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.PositionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil || service.ValidatePositionAccount(currentUserID(c), account.ID, id) != nil {
		common.ApiErrorMsg(c, "持仓不存在")
		return
	}
	p, err := pc.svc.Update(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Close POST /api/positions/:id/close
func (pc *PositionController) Close(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.CloseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil || service.ValidateWritablePositionAccount(currentUserID(c), account.ID, id) != nil {
		common.ApiErrorMsg(c, "持仓不存在")
		return
	}
	p, err := pc.svc.Close(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Delete DELETE /api/positions/:id
func (pc *PositionController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil || service.ValidateWritablePositionAccount(currentUserID(c), account.ID, id) != nil {
		common.ApiErrorMsg(c, "持仓不存在")
		return
	}
	if err := pc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// Trades GET /api/positions/:id/trades —— 加/减仓流水明细（B5）。
func (pc *PositionController) Trades(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil || service.ValidateWritablePositionAccount(currentUserID(c), account.ID, id) != nil {
		common.ApiErrorMsg(c, "持仓不存在")
		return
	}
	rows, err := pc.svc.ListTrades(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// AddTrade POST /api/positions/:id/trades —— 加仓 / 减仓（B5）。
// 减到 0 自动平仓，请求体可一并带复盘字段。
func (pc *PositionController) AddTrade(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.PositionTradeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil || service.ValidateWritablePositionAccount(currentUserID(c), account.ID, id) != nil {
		common.ApiErrorMsg(c, "持仓不存在")
		return
	}
	p, err := pc.svc.AddTrade(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// Stats GET /api/positions/stats?range= —— 个人交易复盘统计（B6，纯读时聚合）。
func (pc *PositionController) Stats(c *gin.Context) {
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	out, err := pc.svc.TradeStatsByAccount(c.Request.Context(), currentUserID(c), account.ID, c.Query("range"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}

// Curve GET /api/positions/curve?days= —— 真实持仓资产曲线（B7，读快照表）。
func (pc *PositionController) Curve(c *gin.Context) {
	days := 0
	if raw := c.Query("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			days = n
		}
	}
	account, err := service.ResolvePortfolioAccount(currentUserID(c), optionalAccountID(c), model.PortfolioKindReal)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	out, err := service.PortfolioCurveByAccount(currentUserID(c), account.ID, model.SnapshotKindReal, days)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}

// CorpAdjusts GET /api/positions/corp-adjusts?status= —— 除权除息待确认折算建议（B8）。
// 顺带按今日除权日生成一次（幂等），保证用户打开持仓页就能看到今天该确认的调整。
func (pc *PositionController) CorpAdjusts(c *gin.Context) {
	uid := currentUserID(c)
	account, err := service.ResolvePortfolioAccount(uid, optionalAccountID(c), model.PortfolioKindReal)
	if err != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	status := strings.ToLower(strings.TrimSpace(c.Query("status")))
	if status == "" || status == model.CorpAdjustPending {
		if _, err := service.GenerateCorpAdjustsForAccount(uid, account.ID, time.Now().Format("2006-01-02")); err != nil {
			common.SysWarn("生成除权调整建议失败 user=%d: %v", uid, err)
		}
	}
	rows, err := service.ListCorpAdjustsForAccount(uid, account.ID, status)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// CorpAdjustAction POST /api/positions/corp-adjusts/:id/:action —— confirm / revert / dismiss（B8）。
// 三个动作合并成一个受约束的枚举入口，避免路由分散后漏掉归属校验。
func (pc *PositionController) CorpAdjustAction(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	uid := currentUserID(c)
	account, accountErr := service.ResolvePortfolioAccount(uid, optionalAccountID(c), model.PortfolioKindReal)
	if accountErr != nil {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	var out *model.PositionCorpAdjust
	var err error
	switch strings.ToLower(c.Param("action")) {
	case "confirm":
		out, err = pc.svc.ConfirmCorpAdjustForAccount(uid, account.ID, id)
	case "revert":
		out, err = pc.svc.RevertCorpAdjustForAccount(uid, account.ID, id)
	case "dismiss":
		out, err = pc.svc.DismissCorpAdjustForAccount(uid, account.ID, id)
	default:
		common.ApiErrorMsg(c, "不支持的操作")
		return
	}
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}

// Calendar GET /api/events/calendar?days= —— 未来 N 天事件日历（B9）。
func Calendar(c *gin.Context) {
	days := 0
	if raw := c.Query("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			days = n
		}
	}
	out, err := service.EventCalendar(currentUserID(c), days)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}

// SellReviews GET /api/positions/sell-reviews?status= —— 卖出复核清单（D16）。
// status 默认 open；all / resolved / dismissed 可查历史。
func SellReviews(c *gin.Context) {
	rows, err := service.ListSellReviews(currentUserID(c), strings.ToLower(strings.TrimSpace(c.Query("status"))))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// SellReviewAction PUT /api/positions/sell-reviews/:id/status —— 标记已复核 / 忽略 / 恢复（D16）。
func SellReviewAction(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	out, err := service.SetSellReviewStatus(currentUserID(c), id, strings.ToLower(strings.TrimSpace(body.Status)))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}

// PositionAdviceController 持仓卖出决策 AI 建议（D17，走 llm_tasks 后台任务）。
type PositionAdviceController struct {
	svc *service.PositionAdviceService
}

func NewPositionAdviceController(svc *service.PositionAdviceService) *PositionAdviceController {
	return &PositionAdviceController{svc: svc}
}

// Advise POST /api/positions/advice —— 建后台任务，秒回任务 id 供前端轮询。
// 结果结构见 service.PositionAdviceResult（逐笔 hold|trim|exit + 理由 + 失效条件）。
func (ac *PositionAdviceController) Advise(c *gin.Context) {
	var req service.PositionAdviceRequest
	// 允许空请求体（不带任何参数 = 分析全部持仓、用默认 LLM 配置）。
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			common.ApiErrorMsg(c, "请求格式错误")
			return
		}
	}
	task, err := ac.svc.AdviseAsync(currentUserID(c), currentRole(c) == model.RoleAdmin, req)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, task)
}

// StockCorpEvents GET /api/markets/:market/stocks/:symbol/corp-events —— 个股解禁 / 分红（B9）。
// 公开市场信息，无用户隔离（与 lhb/orgview 同层）。
func StockCorpEvents(c *gin.Context) {
	out, err := service.StockCorpEventsFor(c.Param("market"), c.Param("symbol"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, out)
}
