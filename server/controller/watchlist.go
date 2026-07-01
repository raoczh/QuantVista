package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// WatchlistController 自选股分组与条目（均限当前登录用户）。
type WatchlistController struct {
	svc *service.WatchlistService
}

func NewWatchlistController(svc *service.WatchlistService) *WatchlistController {
	return &WatchlistController{svc: svc}
}

// List GET /api/watchlists
func (wc *WatchlistController) List(c *gin.Context) {
	groups, err := wc.svc.List(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, groups)
}

type groupReq struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

// CreateGroup POST /api/watchlists
func (wc *WatchlistController) CreateGroup(c *gin.Context) {
	var req groupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	g, err := wc.svc.CreateGroup(currentUserID(c), req.Name)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, g)
}

// UpdateGroup PUT /api/watchlists/:id
func (wc *WatchlistController) UpdateGroup(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var req groupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	g, err := wc.svc.UpdateGroup(currentUserID(c), id, req.Name, req.SortOrder)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, g)
}

// DeleteGroup DELETE /api/watchlists/:id
func (wc *WatchlistController) DeleteGroup(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := wc.svc.DeleteGroup(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// AddItem POST /api/watchlists/:id/items
func (wc *WatchlistController) AddItem(c *gin.Context) {
	groupID, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.WatchlistItemInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	item, err := wc.svc.AddItem(c.Request.Context(), currentUserID(c), groupID, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, item)
}

// UpdateItem PUT /api/watchlist-items/:id
func (wc *WatchlistController) UpdateItem(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.WatchlistItemInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	item, err := wc.svc.UpdateItem(currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, item)
}

// DeleteItem DELETE /api/watchlist-items/:id
func (wc *WatchlistController) DeleteItem(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := wc.svc.DeleteItem(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// parseIDParam 解析并校验路径 :param 为正整数 ID，失败时直接写错误响应并返回 false。
func parseIDParam(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "非法的 ID")
		return 0, false
	}
	return id, true
}
