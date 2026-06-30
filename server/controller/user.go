package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// UserController 当前用户信息与偏好。
type UserController struct {
	svc *service.UserService
}

func NewUserController(svc *service.UserService) *UserController {
	return &UserController{svc: svc}
}

// GetSelf GET /api/user/self
func (uc *UserController) GetSelf(c *gin.Context) {
	u, err := uc.svc.GetByID(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, u)
}

// GetPreference GET /api/user/preference
func (uc *UserController) GetPreference(c *gin.Context) {
	p, err := uc.svc.GetPreference(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// UpdatePreference PUT /api/user/preference
func (uc *UserController) UpdatePreference(c *gin.Context) {
	var in service.PreferenceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	p, err := uc.svc.UpdatePreference(currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, p)
}

// GetQuota GET /api/user/quota
func (uc *UserController) GetQuota(c *gin.Context) {
	q, err := uc.svc.GetQuota(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, q)
}
