package controller

import (
	"strings"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// ThesisController 投资逻辑卡片（均限当前登录用户）。
type ThesisController struct {
	svc *service.ThesisService
}

func NewThesisController(svc *service.ThesisService) *ThesisController {
	return &ThesisController{svc: svc}
}

// List GET /api/thesis-cards?status=&symbol=&market=
// 带 symbol 时返回该标的单卡（无卡返回 null），供自选/持仓行内入口探测。
func (tc *ThesisController) List(c *gin.Context) {
	if symbol := strings.TrimSpace(c.Query("symbol")); symbol != "" {
		market := strings.TrimSpace(c.Query("market"))
		if market == "" {
			market = "cn"
		}
		card, err := tc.svc.GetBySymbol(currentUserID(c), symbol, market)
		if err != nil {
			common.ApiErrorMsg(c, err.Error())
			return
		}
		common.ApiSuccess(c, card)
		return
	}
	rows, err := tc.svc.List(currentUserID(c), c.Query("status"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Upsert POST /api/thesis-cards —— 按 symbol+market 唯一，存在即更新。
func (tc *ThesisController) Upsert(c *gin.Context) {
	var in service.ThesisUpsertRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	card, err := tc.svc.Upsert(c.Request.Context(), currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, card)
}

// SetStatus PUT /api/thesis-cards/:id/status —— active/invalidated/archived。
func (tc *ThesisController) SetStatus(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	card, err := tc.svc.SetStatus(currentUserID(c), id, body.Status, body.Reason)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, card)
}

// Delete DELETE /api/thesis-cards/:id
func (tc *ThesisController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := tc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"deleted": true})
}

// CheckUp GET /api/thesis-cards/checkup —— 一键体检 active 卡。
func (tc *ThesisController) CheckUp(c *gin.Context) {
	items, err := tc.svc.CheckUp(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, items)
}
