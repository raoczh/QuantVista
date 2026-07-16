package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// RecommendationController 短线/长线推荐（均限当前登录用户）。
type RecommendationController struct {
	svc      *service.RecommendationService
	tracking *service.TrackingService
	alerts   *service.AlertService
}

func NewRecommendationController(svc *service.RecommendationService, tracking *service.TrackingService, alerts *service.AlertService) *RecommendationController {
	return &RecommendationController{svc: svc, tracking: tracking, alerts: alerts}
}

// Strategies GET /api/recommendations/strategies?type=short_term|long_term
func (rc *RecommendationController) Strategies(c *gin.Context) {
	recType := c.DefaultQuery("type", model.RecTypeShortTerm)
	common.ApiSuccess(c, service.StrategiesFor(recType))
}

// Generate POST /api/recommendations —— 生成一批推荐。
func (rc *RecommendationController) Generate(c *gin.Context) {
	var req service.RecommendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	allowPrivate := currentRole(c) == model.RoleAdmin
	v, err := rc.svc.Generate(c.Request.Context(), currentUserID(c), allowPrivate, req)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// List GET /api/recommendations?type=&limit= —— 推荐历史。
func (rc *RecommendationController) List(c *gin.Context) {
	recType := c.Query("type")
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := rc.svc.History(currentUserID(c), recType, limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Get GET /api/recommendations/:id —— 详情（含条目）。
func (rc *RecommendationController) Get(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	v, err := rc.svc.Get(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Delete DELETE /api/recommendations/:id
func (rc *RecommendationController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := rc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// AckReview PUT /api/recommendations/review-ack/:id —— 标记推荐复盘提示已读
//（:id 为追踪状态行 id，今日待办 rec_review 条目的 ref_id）。
func (rc *RecommendationController) AckReview(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := rc.tracking.AckReview(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// Performance GET /api/recommendations/performance?type= —— 推荐历史表现统计（带样本量）。
func (rc *RecommendationController) Performance(c *gin.Context) {
	stats, err := rc.tracking.Performance(currentUserID(c), c.Query("type"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, stats)
}

// Attribution GET /api/recommendations/attribution?type=&horizon= —— S0-6 确定性错误
// 归因报表（成熟标签按入场特征桶×regime×策略×来源×行业分组）。
func (rc *RecommendationController) Attribution(c *gin.Context) {
	horizon := 10
	if s := c.Query("horizon"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			horizon = n
		}
	}
	rep, err := service.RecAttribution(currentUserID(c), c.Query("type"), horizon)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rep)
}

// StopLossAlert POST /api/recommendations/items/:id/stop-alert —— S1-4 执行纪律：
// 对推荐条目的止损价一键创建到价提醒。
func (rc *RecommendationController) StopLossAlert(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	rule, err := rc.svc.CreateStopLossAlert(c.Request.Context(), currentUserID(c), id, rc.alerts)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rule)
}

// Track POST /api/recommendations/:id/track —— 手动刷新该批次的推荐追踪状态，返回最新详情。
func (rc *RecommendationController) Track(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	uid := currentUserID(c)
	if _, err := rc.tracking.RefreshBatch(c.Request.Context(), uid, id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	v, err := rc.svc.Get(uid, id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}
