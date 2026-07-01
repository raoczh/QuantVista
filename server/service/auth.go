package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

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
	if n := utf8.RuneCountInString(username); n < 3 || n > 32 {
		return nil, errors.New("用户名长度需在 3~32 个字符之间")
	}
	if len(password) < 8 {
		return nil, errors.New("密码至少 8 个字符")
	}
	if len(password) > 72 {
		return nil, errors.New("密码过长（bcrypt 上限 72 字节）")
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
	// 事务 + options["initialized"] 主键唯一做并发闸：并发首启只会有一个成功。
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&model.User{}).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return errors.New("系统已初始化，禁止重复创建管理员")
		}
		if err := tx.Create(&model.Option{Key: "initialized", Value: "true"}).Error; err != nil {
			return errors.New("系统已初始化，禁止重复创建管理员")
		}
		if err := tx.Create(user).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.UserPreference{UserID: user.ID}).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.UserQuota{UserID: user.ID}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
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

// PruneRefreshTokens 删除已过期或已吊销的刷新令牌，防止表无界增长。返回删除条数。
func (s *AuthService) PruneRefreshTokens() (int64, error) {
	res := common.DB.Where("expires_at < ? OR revoked = ?", time.Now(), true).
		Delete(&model.RefreshToken{})
	return res.RowsAffected, res.Error
}

// StartRefreshTokenJanitor 启动即清理一次，之后每 6 小时清理一次过期/吊销令牌。
func StartRefreshTokenJanitor() {
	svc := NewAuthService()
	run := func() {
		if n, err := svc.PruneRefreshTokens(); err != nil {
			common.SysWarn("清理刷新令牌失败: %v", err)
		} else if n > 0 {
			common.SysLog("清理过期/吊销刷新令牌 %d 条", n)
		}
	}
	run()
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			run()
		}
	}()
}

// issueFor 为用户签发 access + refresh，并更新最后登录时间。
func (s *AuthService) issueFor(user *model.User, ua string) (*TokenPair, error) {
	access, exp, err := common.IssueAccessToken(user.ID, user.Role, user.TokenVersion)
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
