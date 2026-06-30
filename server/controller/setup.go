package controller

import (
	"quantvista/common"
	"quantvista/service"
	"quantvista/setting"

	"github.com/gin-gonic/gin"
)

// SetupController 首启引导：检查初始化状态、创建首个管理员。
type SetupController struct {
	svc *service.AuthService
}

func NewSetupController(svc *service.AuthService) *SetupController {
	return &SetupController{svc: svc}
}

// Status GET /api/setup/status —— 公开，供前端决定显示首启页还是登录页。
func (sc *SetupController) Status(c *gin.Context) {
	need, err := sc.svc.SetupNeeded()
	if err != nil {
		common.ApiErrorMsg(c, "读取初始化状态失败")
		return
	}
	common.ApiSuccess(c, gin.H{
		"initialized":          !need,
		"github_oauth_enabled": setting.GitHubOAuthEnabled(),
		"registration_open":    setting.RegistrationOpen(),
	})
}

type createAdminReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// CreateAdmin POST /api/setup/admin —— 仅系统无用户时可用，创建管理员并直接登录。
func (sc *SetupController) CreateAdmin(c *gin.Context) {
	var req createAdminReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	pair, err := sc.svc.CreateAdmin(req.Username, req.Password, clientUA(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, pair)
}
