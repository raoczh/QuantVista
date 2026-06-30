package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// AuthController 登录、令牌换发、GitHub OAuth。
type AuthController struct {
	svc *service.AuthService
}

func NewAuthController(svc *service.AuthService) *AuthController {
	return &AuthController{svc: svc}
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login POST /api/auth/login —— 用户名+密码登录。
func (ac *AuthController) Login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	pair, err := ac.svc.LoginByPassword(req.Username, req.Password, clientUA(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, pair)
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh POST /api/auth/refresh —— 用 refresh token 换发新令牌。
func (ac *AuthController) Refresh(c *gin.Context) {
	var req refreshReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	pair, err := ac.svc.Refresh(req.RefreshToken, clientUA(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, pair)
}

// Logout POST /api/auth/logout —— 吊销当前 refresh token。
func (ac *AuthController) Logout(c *gin.Context) {
	var req refreshReq
	_ = c.ShouldBindJSON(&req)
	if err := ac.svc.Logout(req.RefreshToken); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

// GitHubURL GET /api/oauth/github/url?redirect_uri=... —— 返回 GitHub 授权跳转地址。
func (ac *AuthController) GitHubURL(c *gin.Context) {
	redirectURI := c.Query("redirect_uri")
	url, err := ac.svc.GitHubAuthURL(redirectURI)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"url": url})
}

type githubCallbackReq struct {
	Code        string `json:"code"`
	State       string `json:"state"`
	RedirectURI string `json:"redirect_uri"`
}

// GitHubCallback POST /api/oauth/github —— 前端回调页用 code 换登录令牌。
func (ac *AuthController) GitHubCallback(c *gin.Context) {
	var req githubCallbackReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	pair, err := ac.svc.LoginByGitHub(c.Request.Context(), req.Code, req.State, req.RedirectURI, clientUA(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, pair)
}
