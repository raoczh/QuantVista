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

// dummyPasswordHash 登录时用户不存在也执行一次同代价的 bcrypt 比较，
// 抹平响应时间差，防止通过计时侧信道枚举已注册用户名。
var dummyPasswordHash = func() string {
	h, err := common.HashPassword("quantvista-timing-pad")
	if err != nil {
		return ""
	}
	return h
}()

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
		common.CheckPassword(dummyPasswordHash, password) // 哑比较抹平计时差
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

// GitHubAuthURL 构造 GitHub 授权地址（含签名 state）。第二返回值为 state 的
// nonce，调用方须种进 HttpOnly cookie，回调时 double-submit 比对（防登录 CSRF）。
func (s *AuthService) GitHubAuthURL(redirectURI string) (string, string, error) {
	if !setting.GitHubOAuthEnabled() {
		return "", "", oauth.ErrOAuthDisabled
	}
	if redirectURI == "" {
		return "", "", errors.New("缺少 redirect_uri")
	}
	state := common.SignState()
	return oauth.AuthorizeURL(state, redirectURI), common.StateNonce(state), nil
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
		// 明确指出「查无绑定」而非笼统的未开放注册：绑定过却走到这里，
		// 说明本库中无该 GitHub 账号的绑定关系（换了 GitHub 账号 / 换了环境库）。
		return nil, errors.New("该 GitHub 账号未绑定任何已有用户，且当前未开放注册；若你绑定过，请确认授权的是同一个 GitHub 账号")
	}
	newUser := &model.User{
		GithubID:    gu.GithubID,
		Username:    s.uniqueUsername(gu.Username, gu.GithubID),
		DisplayName: gu.DisplayName,
		Email:       gu.Email,
		AvatarURL:   gu.AvatarURL,
		Role:        model.RoleUser,
		Status:      model.StatusEnabled,
	}
	if newUser.DisplayName == "" {
		newUser.DisplayName = newUser.Username
	}
	if need {
		// 首用户授予 admin 须过与 CreateAdmin 相同的并发闸（options["initialized"]
		// 主键唯一）：两个并发的 GitHub 首登只允许一个成为管理员。
		newUser.Role = model.RoleAdmin
		err = common.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&model.Option{Key: "initialized", Value: "true"}).Error; err != nil {
				return errInitialized
			}
			return tx.Create(newUser).Error
		})
		if errors.Is(err, errInitialized) {
			// 竞争失败：系统已被并发请求初始化，按普通注册路径重试。
			if !setting.RegistrationOpen() {
				return nil, errors.New("当前未开放注册")
			}
			newUser.Role = model.RoleUser
			err = common.DB.Create(newUser).Error
		}
		if err != nil {
			return nil, err
		}
	} else if err := common.DB.Create(newUser).Error; err != nil {
		return nil, err
	}
	s.ensurePrefAndQuota(newUser.ID)
	return s.issueFor(newUser, ua)
}

// errInitialized 首用户闸竞争失败（系统已被并发请求初始化）。
var errInitialized = errors.New("系统已初始化")

// BindGitHub 已登录用户绑定 GitHub：校验 state、换 token、取 GitHub 用户，
// 该 GitHub 账号未被其他用户占用时写入当前用户（解决"密码登录的管理员再用
// GitHub 登录会开出第二个账号"的问题——绑定后同一 GitHub 直接登录本账号）。
func (s *AuthService) BindGitHub(ctx context.Context, userID int64, code, state, redirectURI string) (*model.User, error) {
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
	var user model.User
	if err := common.DB.First(&user, userID).Error; err != nil {
		return nil, errors.New("用户不存在")
	}
	if user.GithubID == gu.GithubID {
		user.Password = ""
		return &user, nil // 幂等：重复绑定同一 GitHub 直接成功
	}
	var n int64
	common.DB.Model(&model.User{}).Where("github_id = ? AND id <> ?", gu.GithubID, userID).Count(&n)
	if n > 0 {
		return nil, errors.New("该 GitHub 账号已绑定其他用户，请先在对方账号解绑")
	}
	updates := map[string]any{"github_id": gu.GithubID}
	// 空缺信息用 GitHub 资料补齐，不覆盖已有值。
	if user.AvatarURL == "" && gu.AvatarURL != "" {
		updates["avatar_url"] = gu.AvatarURL
	}
	if user.Email == "" && gu.Email != "" {
		updates["email"] = gu.Email
	}
	if err := common.DB.Model(&user).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := common.DB.First(&user, userID).Error; err != nil {
		return nil, err
	}
	user.Password = ""
	return &user, nil
}

// UnbindGitHub 解绑 GitHub。未设密码的纯 OAuth 账号拒绝解绑（会失去唯一登录方式）。
func (s *AuthService) UnbindGitHub(userID int64) (*model.User, error) {
	var user model.User
	if err := common.DB.First(&user, userID).Error; err != nil {
		return nil, errors.New("用户不存在")
	}
	if user.GithubID == "" {
		return nil, errors.New("当前未绑定 GitHub")
	}
	if user.Password == "" {
		return nil, errors.New("该账号未设置密码，解绑 GitHub 将无法登录；请先设置密码")
	}
	if err := common.DB.Model(&user).Update("github_id", "").Error; err != nil {
		return nil, err
	}
	user.GithubID = ""
	user.Password = ""
	return &user, nil
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
	// 轮换吊销旧令牌：条件更新 + RowsAffected 校验，保证并发重放同一 refresh token
	// 时只有一次换发成功（先查后改的 TOCTOU 窗口内，第二个请求在这里会失败）。
	res := common.DB.Model(&model.RefreshToken{}).
		Where("id = ? AND revoked = ?", rt.ID, false).
		Update("revoked", true)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, errors.New("refresh token 已失效，请重新登录")
	}
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
