package controller

import (
	"net/http"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// oauthStateCookie OAuth state 的 double-submit cookie 名。
const oauthStateCookie = "qv_oauth_state"

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

// GitHubBind POST /api/user/github/bind —— 已登录用户把 GitHub 绑到当前账号。
// state 校验与登录回调同一套 double-submit cookie（发起时同样走 GET /oauth/github/url）。
func (ac *AuthController) GitHubBind(c *gin.Context) {
	var req githubCallbackReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	cookieNonce, cErr := c.Cookie(oauthStateCookie)
	if cErr != nil || cookieNonce == "" || cookieNonce != common.StateNonce(req.State) {
		common.ApiErrorMsg(c, "绑定会话校验失败，请从设置页重新发起 GitHub 绑定")
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(oauthStateCookie, "", -1, "/", "", false, true)
	user, err := ac.svc.BindGitHub(c.Request.Context(), currentUserID(c), req.Code, req.State, req.RedirectURI)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, user)
}

// GitHubUnbind DELETE /api/user/github/bind —— 解绑 GitHub（须已设密码）。
func (ac *AuthController) GitHubUnbind(c *gin.Context) {
	user, err := ac.svc.UnbindGitHub(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, user)
}

// GitHubURL GET /api/oauth/github/url?redirect_uri=... —— 返回 GitHub 授权跳转地址。
func (ac *AuthController) GitHubURL(c *gin.Context) {
	redirectURI := c.Query("redirect_uri")
	url, nonce, err := ac.svc.GitHubAuthURL(redirectURI)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	// double-submit：nonce 同时种进 HttpOnly cookie，回调时与 state 比对。
	// 仅验 HMAC 防不了登录 CSRF——攻击者可用自己申请的合法 state+code 诱导
	// 受害者浏览器完成回调、静默登进攻击者账号；cookie 绑定发起浏览器后不可行。
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(oauthStateCookie, nonce, 600, "/", "", false, true)
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
	cookieNonce, cErr := c.Cookie(oauthStateCookie)
	if cErr != nil || cookieNonce == "" || cookieNonce != common.StateNonce(req.State) {
		common.ApiErrorMsg(c, "登录会话校验失败，请从本站登录页重新发起 GitHub 登录")
		return
	}
	// 一次性：校验通过即失效，防止同一 state 在 TTL 内重放。
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(oauthStateCookie, "", -1, "/", "", false, true)
	pair, err := ac.svc.LoginByGitHub(c.Request.Context(), req.Code, req.State, req.RedirectURI, clientUA(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, pair)
}
