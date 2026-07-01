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
	svc *service.RecommendationService
}

func NewRecommendationController(svc *service.RecommendationService) *RecommendationController {
	return &RecommendationController{svc: svc}
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
