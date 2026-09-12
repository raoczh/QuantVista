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

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RefreshTokenTTL 刷新令牌有效期。
const RefreshTokenTTL = 30 * 24 * time.Hour

var (
	ErrRefreshTokenInvalid = errors.New("登录状态已失效，请重新登录")
	ErrAuthAccountInactive = errors.New("账号不存在或已被禁用")
)

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
	var pair *TokenPair
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
			if isDuplicateRecordError(err) {
				return errInitialized
			}
			return err
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
		// 首启成功同时代表已登录；会话签发失败时不能留下阻止原操作重试的初始化闸。
		var issueErr error
		pair, issueErr = s.issueForTx(tx, user, ua)
		return issueErr
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
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
// state 防重放由 controller 层 cookie double-submit 承担（Web 流）。
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
	user, err := s.userForGitHub(gu)
	if err != nil {
		return nil, err
	}
	return s.issueFor(user, ua)
}

// userForGitHub 「GitHub 用户→本地用户」公共段：已绑定直登，未绑定按
// 首用户 admin 闸/开放注册规则建号。Web 流（LoginByGitHub）与移动流
// （MobileGitHubCallback）共用，改注册/绑定规则只改这里。
func (s *AuthService) userForGitHub(gu *oauth.GitHubUser) (*model.User, error) {
	if gu == nil || strings.TrimSpace(gu.GithubID) == "" {
		return nil, errors.New("GitHub 身份无效")
	}
	// 已绑定的 GitHub 用户：直接登录。
	var user model.User
	err := common.DB.Where("github_id = ?", gu.GithubID).First(&user).Error
	if err == nil {
		if user.Status != model.StatusEnabled {
			return nil, errors.New("账号已被禁用")
		}
		return &user, nil
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
	username, err := s.uniqueUsername(gu.Username, gu.GithubID)
	if err != nil {
		return nil, err
	}
	newUser := &model.User{
		GithubID:    gu.GithubID,
		Username:    username,
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
				if isDuplicateRecordError(err) {
					return errInitialized
				}
				return err
			}
			return tx.Create(newUser).Error
		})
		if errors.Is(err, errInitialized) {
			// 同一 GitHub 身份可能已由并发首次登录建立，先重读唯一绑定。
			var existing model.User
			if lookupErr := common.DB.Where("github_id = ?", gu.GithubID).First(&existing).Error; lookupErr == nil {
				if existing.Status != model.StatusEnabled {
					return nil, ErrAuthAccountInactive
				}
				return &existing, nil
			} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return nil, lookupErr
			}
			// 竞争失败：系统已被并发请求初始化，按普通注册路径重试。
			if !setting.RegistrationOpen() {
				return nil, errors.New("当前未开放注册")
			}
			newUser.Role = model.RoleUser
			err = common.DB.Create(newUser).Error
		}
	} else {
		err = common.DB.Create(newUser).Error
	}
	if err != nil {
		// 两个回调可同时看到未注册；唯一索引的竞争失败者复用已提交的账号。
		var existing model.User
		if lookupErr := common.DB.Where("github_id = ?", gu.GithubID).First(&existing).Error; lookupErr != nil {
			return nil, err
		}
		if existing.Status != model.StatusEnabled {
			return nil, ErrAuthAccountInactive
		}
		return &existing, nil
	}
	s.ensurePrefAndQuota(newUser.ID)
	return newUser, nil
}

// errInitialized 首用户闸竞争失败（系统已被并发请求初始化）。
var errInitialized = errors.New("系统已初始化")

func isDuplicateRecordError(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) {
		return mysqlError.Number == 1062
	}
	var sqliteError interface{ Code() int }
	if errors.As(err, &sqliteError) {
		return sqliteError.Code() == 1555 || sqliteError.Code() == 2067 // 主键/唯一索引冲突
	}
	return false
}

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
	var n int64
	if err := common.DB.WithContext(ctx).Model(&model.User{}).Where("github_id = ? AND id <> ?", gu.GithubID, userID).Count(&n).Error; err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, errors.New("该 GitHub 账号已绑定其他用户，请先在对方账号解绑")
	}
	var user *model.User
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		user, err = lockEnabledAuthUser(tx, userID)
		if err != nil {
			return err
		}
		if user.GithubID == gu.GithubID {
			return nil
		}
		updates := map[string]any{"github_id": gu.GithubID}
		// 空缺信息用 GitHub 资料补齐，不覆盖锁定后读到的当前资料。
		if user.AvatarURL == "" && gu.AvatarURL != "" {
			updates["avatar_url"] = gu.AvatarURL
		}
		if user.Email == "" && gu.Email != "" {
			updates["email"] = gu.Email
		}
		// 上面的 Count 只提供友好提示，数据库唯一索引才是并发绑定的最终约束。
		return tx.Model(user).Updates(updates).Error
	})
	if err != nil {
		return nil, err
	}
	user.Password = ""
	return user, nil
}

// UnbindGitHub 解绑 GitHub。未设密码的纯 OAuth 账号拒绝解绑（会失去唯一登录方式）。
func (s *AuthService) UnbindGitHub(userID int64) (*model.User, error) {
	var user *model.User
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		user, err = lockEnabledAuthUser(tx, userID)
		if err != nil {
			return err
		}
		if user.GithubID == "" {
			return errors.New("当前未绑定 GitHub")
		}
		if user.Password == "" {
			return errors.New("该账号未设置密码，解绑 GitHub 将无法登录；请先设置密码")
		}
		return tx.Model(user).Update("github_id", nil).Error
	})
	if err != nil {
		return nil, err
	}
	user.GithubID = ""
	user.Password = ""
	return user, nil
}

// Refresh 用 refresh token 换发新令牌（轮换：吊销旧的、签发新的）。
func (s *AuthService) Refresh(rawRefresh, ua string) (*TokenPair, error) {
	if rawRefresh == "" {
		return nil, ErrRefreshTokenInvalid
	}
	var rt model.RefreshToken
	if err := common.DB.Where("token_hash = ?", common.SHA256Hex(rawRefresh)).First(&rt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRefreshTokenInvalid
		}
		return nil, err
	}
	if rt.Revoked || time.Now().After(rt.ExpiresAt) {
		return nil, ErrRefreshTokenInvalid
	}
	var pair *TokenPair
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		// 与改密/禁用共用用户行锁；旧令牌吊销与替换令牌创建必须同时提交。
		user, err := lockEnabledAuthUser(tx, rt.UserID)
		if err != nil {
			return err
		}
		res := tx.Model(&model.RefreshToken{}).
			Where("id = ? AND revoked = ? AND expires_at > ?", rt.ID, false, time.Now()).
			Update("revoked", true)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrRefreshTokenInvalid
		}
		pair, err = s.issueForTx(tx, user, ua)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
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
	var pair *TokenPair
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		current, err := lockEnabledAuthUser(tx, user.ID)
		if err != nil {
			return err
		}
		if current.TokenVersion != user.TokenVersion || current.GithubID != user.GithubID {
			return errors.New("登录状态已变更，请重新登录")
		}
		pair, err = s.issueForTx(tx, current, ua)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

func lockEnabledAuthUser(tx *gorm.DB, userID int64) (*model.User, error) {
	query := tx
	if tx.Dialector.Name() == "mysql" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var user model.User
	if err := query.First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAuthAccountInactive
		}
		return nil, err
	}
	if user.Status != model.StatusEnabled {
		return nil, ErrAuthAccountInactive
	}
	return &user, nil
}

// issueForTx 仅在用户行已经锁定、认证版本已核验的事务内签发。
func (s *AuthService) issueForTx(tx *gorm.DB, user *model.User, ua string) (*TokenPair, error) {
	access, exp, err := common.IssueAccessToken(user.ID, user.Role, user.TokenVersion)
	if err != nil {
		return nil, err
	}
	raw := common.RandomString(48)
	rt := &model.RefreshToken{
		UserID:    user.ID,
		TokenHash: common.SHA256Hex(raw),
		ExpiresAt: time.Now().Add(RefreshTokenTTL),
		UserAgent: truncateRunes(ua, 256),
	}
	if err := tx.Create(rt).Error; err != nil {
		return nil, err
	}
	if err := tx.Model(user).Update("last_login_at", time.Now()).Error; err != nil {
		return nil, err
	}
	user.Password = "" // 绝不外泄
	return &TokenPair{AccessToken: access, RefreshToken: raw, ExpiresAt: exp.Unix(), User: user}, nil
}

// ensurePrefAndQuota 为新用户建默认偏好与配额（已存在则忽略）。
func (s *AuthService) ensurePrefAndQuota(userID int64) {
	common.DB.FirstOrCreate(&model.UserPreference{}, model.UserPreference{UserID: userID})
	common.DB.FirstOrCreate(&model.UserQuota{}, model.UserQuota{UserID: userID})
}

// uniqueUsername 在用户名冲突或为空时追加后缀保证唯一。
func (s *AuthService) uniqueUsername(preferred, githubID string) (string, error) {
	base := strings.TrimSpace(preferred)
	if base == "" {
		base = "gh_" + githubID
	}
	candidate := base
	for i := 1; ; i++ {
		var n int64
		if err := common.DB.Model(&model.User{}).Where("username = ?", candidate).Count(&n).Error; err != nil {
			return "", err
		}
		if n == 0 {
			return candidate, nil
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
