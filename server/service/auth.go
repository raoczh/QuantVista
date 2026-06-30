package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/oauth"
	"quantvista/setting"

	"gorm.io/gorm"
)

// RefreshTokenTTL 刷新令牌有效期。
const RefreshTokenTTL = 30 * 24 * time.Hour

// TokenPair 登录/换发成功返回的令牌对与用户信息。
type TokenPair struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresAt    int64       `json:"expires_at"` // access token 过期时间（unix 秒）
	User         *model.User `json:"user"`
}

type AuthService struct{}

func NewAuthService() *AuthService { return &AuthService{} }

// SetupNeeded 系统是否尚未初始化（无任何用户）。
func (s *AuthService) SetupNeeded() (bool, error) {
	var n int64
	if err := common.DB.Model(&model.User{}).Count(&n).Error; err != nil {
		return false, err
	}
	return n == 0, nil
}

// CreateAdmin 首启创建管理员（仅当系统无用户时允许）。
func (s *AuthService) CreateAdmin(username, password, ua string) (*TokenPair, error) {
	need, err := s.SetupNeeded()
	if err != nil {
		return nil, err
	}
	if !need {
		return nil, errors.New("系统已初始化，禁止重复创建管理员")
	}
	username = strings.TrimSpace(username)
	if len(username) < 3 {
		return nil, errors.New("用户名至少 3 个字符")
	}
	if len(password) < 8 {
		return nil, errors.New("密码至少 8 个字符")
	}
	hash, err := common.HashPassword(password)
	if err != nil {
		return nil, err
	}
	user := &model.User{
		Username:    username,
		Password:    hash,
		DisplayName: username,
		Role:        model.RoleAdmin,
		Status:      model.StatusEnabled,
	}
	if err := common.DB.Create(user).Error; err != nil {
		return nil, err
	}
	s.ensurePrefAndQuota(user.ID)
	return s.issueFor(user, ua)
}

// LoginByPassword 用户名 + 密码登录。
func (s *AuthService) LoginByPassword(username, password, ua string) (*TokenPair, error) {
	var user model.User
	if err := common.DB.Where("username = ?", username).First(&user).Error; err != nil {
		return nil, errors.New("用户名或密码错误")
	}
	if !common.CheckPassword(user.Password, password) {
		return nil, errors.New("用户名或密码错误")
	}
	if user.Status != model.StatusEnabled {
		return nil, errors.New("账号已被禁用")
	}
	return s.issueFor(&user, ua)
}

// GitHubAuthURL 构造 GitHub 授权地址（含签名 state）。
func (s *AuthService) GitHubAuthURL(redirectURI string) (string, error) {
	if !setting.GitHubOAuthEnabled() {
		return "", oauth.ErrOAuthDisabled
	}
	if redirectURI == "" {
		return "", errors.New("缺少 redirect_uri")
	}
	return oauth.AuthorizeURL(common.SignState(), redirectURI), nil
}

// LoginByGitHub OAuth 回调：校验 state、换 token、取用户、查或建。
func (s *AuthService) LoginByGitHub(ctx context.Context, code, state, redirectURI, ua string) (*TokenPair, error) {
	if !common.VerifyState(state) {
		return nil, errors.New("state 校验失败（可能过期或被篡改）")
	}
	accessToken, err := oauth.ExchangeToken(ctx, code, redirectURI)
	if err != nil {
		return nil, err
	}
	gu, err := oauth.GetUser(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	// 已绑定的 GitHub 用户：直接登录。
	var user model.User
	err = common.DB.Where("github_id = ?", gu.GithubID).First(&user).Error
	if err == nil {
		if user.Status != model.StatusEnabled {
			return nil, errors.New("账号已被禁用")
		}
		return s.issueFor(&user, ua)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	// 新用户：首个用户强制 admin；否则需开放注册。
	need, err := s.SetupNeeded()
	if err != nil {
		return nil, err
	}
	if !need && !setting.RegistrationOpen() {
		return nil, errors.New("当前未开放注册")
	}
	role := model.RoleUser
	if need {
		role = model.RoleAdmin
	}
	newUser := &model.User{
		GithubID:    gu.GithubID,
		Username:    s.uniqueUsername(gu.Username, gu.GithubID),
		DisplayName: gu.DisplayName,
		Email:       gu.Email,
		AvatarURL:   gu.AvatarURL,
		Role:        role,
		Status:      model.StatusEnabled,
	}
	if newUser.DisplayName == "" {
		newUser.DisplayName = newUser.Username
	}
	if err := common.DB.Create(newUser).Error; err != nil {
		return nil, err
	}
	s.ensurePrefAndQuota(newUser.ID)
	return s.issueFor(newUser, ua)
}

// Refresh 用 refresh token 换发新令牌（轮换：吊销旧的、签发新的）。
func (s *AuthService) Refresh(rawRefresh, ua string) (*TokenPair, error) {
	if rawRefresh == "" {
		return nil, errors.New("缺少 refresh token")
	}
	var rt model.RefreshToken
	if err := common.DB.Where("token_hash = ?", common.SHA256Hex(rawRefresh)).First(&rt).Error; err != nil {
		return nil, errors.New("refresh token 无效")
	}
	if rt.Revoked || time.Now().After(rt.ExpiresAt) {
		return nil, errors.New("refresh token 已失效，请重新登录")
	}
	var user model.User
	if err := common.DB.First(&user, rt.UserID).Error; err != nil {
		return nil, errors.New("用户不存在")
	}
	if user.Status != model.StatusEnabled {
		return nil, errors.New("账号已被禁用")
	}
	common.DB.Model(&rt).Update("revoked", true) // 轮换吊销旧令牌
	return s.issueFor(&user, ua)
}

// Logout 吊销单个 refresh token。
func (s *AuthService) Logout(rawRefresh string) error {
	if rawRefresh == "" {
		return nil
	}
	return common.DB.Model(&model.RefreshToken{}).
		Where("token_hash = ?", common.SHA256Hex(rawRefresh)).
		Update("revoked", true).Error
}

// RevokeAllForUser 吊销某用户全部刷新令牌（强制登出所有设备）。
func (s *AuthService) RevokeAllForUser(userID int64) error {
	return common.DB.Model(&model.RefreshToken{}).
		Where("user_id = ? AND revoked = ?", userID, false).
		Update("revoked", true).Error
}

// issueFor 为用户签发 access + refresh，并更新最后登录时间。
func (s *AuthService) issueFor(user *model.User, ua string) (*TokenPair, error) {
	access, exp, err := common.IssueAccessToken(user.ID, user.Role)
	if err != nil {
		return nil, err
	}
	raw := common.RandomString(48)
	rt := &model.RefreshToken{
		UserID:    user.ID,
		TokenHash: common.SHA256Hex(raw),
		ExpiresAt: time.Now().Add(RefreshTokenTTL),
		UserAgent: truncate(ua, 256),
	}
	if err := common.DB.Create(rt).Error; err != nil {
		return nil, err
	}
	common.DB.Model(user).Update("last_login_at", time.Now())
	user.Password = "" // 绝不外泄
	return &TokenPair{AccessToken: access, RefreshToken: raw, ExpiresAt: exp.Unix(), User: user}, nil
}

// ensurePrefAndQuota 为新用户建默认偏好与配额（已存在则忽略）。
func (s *AuthService) ensurePrefAndQuota(userID int64) {
	common.DB.FirstOrCreate(&model.UserPreference{}, model.UserPreference{UserID: userID})
	common.DB.FirstOrCreate(&model.UserQuota{}, model.UserQuota{UserID: userID})
}

// uniqueUsername 在用户名冲突或为空时追加后缀保证唯一。
func (s *AuthService) uniqueUsername(preferred, githubID string) string {
	base := strings.TrimSpace(preferred)
	if base == "" {
		base = "gh_" + githubID
	}
	candidate := base
	for i := 1; ; i++ {
		var n int64
		common.DB.Model(&model.User{}).Where("username = ?", candidate).Count(&n)
		if n == 0 {
			return candidate
		}
		candidate = base + "_" + githubID[:min(4, len(githubID))]
		if i > 1 {
			candidate = base + "_" + common.RandomString(4)
		}
	}
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
