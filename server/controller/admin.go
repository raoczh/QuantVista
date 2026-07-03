package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// AdminController 管理员后台：系统设置与用户管理。
type AdminController struct {
	svc *service.AdminService
}

func NewAdminController(svc *service.AdminService) *AdminController {
	return &AdminController{svc: svc}
}

// GetSettings GET /api/admin/settings
func (ac *AdminController) GetSettings(c *gin.Context) {
	common.ApiSuccess(c, ac.svc.GetSettings())
}

// UpdateSettings PUT /api/admin/settings
func (ac *AdminController) UpdateSettings(c *gin.Context) {
	var in service.UpdateSettingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	v, err := ac.svc.UpdateSettings(in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// ListUsers GET /api/admin/users
func (ac *AdminController) ListUsers(c *gin.Context) {
	users, err := ac.svc.ListUsers()
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, users)
}

type setStatusReq struct {
	Status string `json:"status"`
}

// SetUserStatus PUT /api/admin/users/:id/status
func (ac *AdminController) SetUserStatus(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "非法的用户 id")
		return
	}
	var req setStatusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	if err := ac.svc.SetUserStatus(currentUserID(c), id, req.Status); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// GetUserQuota GET /api/admin/users/:id/quota —— 查看用户 AI 配额。
func (ac *AdminController) GetUserQuota(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	q, err := ac.svc.GetUserQuota(id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, q)
}

// UpdateUserQuota PUT /api/admin/users/:id/quota —— 调整 token 上限 / 清零已用量。
func (ac *AdminController) UpdateUserQuota(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.QuotaUpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	q, err := ac.svc.UpdateUserQuota(id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, q)
}
