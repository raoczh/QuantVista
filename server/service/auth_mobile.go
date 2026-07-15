package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/oauth"
	"quantvista/setting"
)

// 移动 GitHub OAuth 流（阶段 B，设计见 docs/ANDROID_APP_PLAN.md §5）。
// 与 Web 流的差异只在「会话如何跨端防重放」：
//   Web 流  ：HttpOnly cookie double-submit（发起与回调同一浏览器）。
//   移动流  ：发起在 App WebView、回调落在系统浏览器，cookie 不共享——
//             state 改服务端一次性消费；回 App 用一次性短码 + PKCE
//             （quantvista:// scheme 可被恶意 App 抢注，verifier 校验保证
//             截获短码也换不到 token）。

// mobileAuthCodeTTL 一次性短码有效期：只覆盖「回调页 → 深链回 App → 兑换」
// 一瞬，给足弱网余量即可。
const mobileAuthCodeTTL = 60 * time.Second

// mobileStateTTL 移动流 state 一次性记录有效期，与 common.SignState 的
// HMAC 时效同为 10min（HMAC 管防篡改与时效，服务端记录管一次性）。
const mobileStateTTL = 10 * time.Minute

// errMobileExchange 短码兑换的统一错误：短码不存在/过期/已用/verifier 不符
// 一律同文案，不泄露短码是否存在（防枚举探测）。
var errMobileExchange = errors.New("登录凭证无效或已过期，请回到 App 重新发起 GitHub 登录")

// mobileCodeRecord 短码兑换记录：短码必须绑定 PKCE challenge 与用户。
type mobileCodeRecord struct {
	UserID    int64  `json:"user_id"`
	Challenge string `json:"challenge"`
}

// GitHubAuthURLMobile 移动流授权地址：SignState 照旧，但 nonce 不种 cookie，
// 改存服务端一次性记录并绑定 PKCE challenge（S256）。
func (s *AuthService) GitHubAuthURLMobile(redirectURI, codeChallenge string) (string, error) {
	if !setting.GitHubOAuthEnabled() {
		return "", oauth.ErrOAuthDisabled
	}
	if redirectURI == "" {
		return "", errors.New("缺少 redirect_uri")
	}
	// RFC 7636：base64url(SHA256) 定长 43；容忍到 128 以兼容 plain 长度上限。
	if n := len(codeChallenge); n < 43 || n > 128 {
		return "", errors.New("code_challenge 缺失或格式非法")
	}
	state := common.SignState()
	storeOnce("mstate:"+common.StateNonce(state), codeChallenge, mobileStateTTL)
	return oauth.AuthorizeURL(state, redirectURI), nil
}

// MobileGitHubCallback 系统浏览器回调页调用：校验并一次性消费 state、
// 换 GitHub token、查/建本地用户，签发绑定 challenge 的一次性短码。
// 返回的短码经 quantvista:// 深链带回 App，本体绝不含任何令牌。
func (s *AuthService) MobileGitHubCallback(ctx context.Context, code, state, redirectURI string) (string, error) {
	if !common.VerifyState(state) {
		return "", errors.New("state 校验失败（可能过期或被篡改）")
	}
	// 一次性消费须在换 token 之前：重放的 state 不应触发对 GitHub 的请求。
	challenge, ok := consumeOnce("mstate:" + common.StateNonce(state))
	if !ok {
		return "", errors.New("登录会话已使用或过期，请回到 App 重新发起 GitHub 登录")
	}
	accessToken, err := oauth.ExchangeToken(ctx, code, redirectURI)
	if err != nil {
		return "", err
	}
	gu, err := oauth.GetUser(ctx, accessToken)
	if err != nil {
		return "", err
	}
	user, err := s.userForGitHub(gu)
	if err != nil {
		return "", err
	}
	authCode := common.RandomString(48)
	rec, err := json.Marshal(mobileCodeRecord{UserID: user.ID, Challenge: challenge})
	if err != nil {
		return "", err
	}
	storeOnce("mcode:"+authCode, string(rec), mobileAuthCodeTTL)
	return authCode, nil
}

// MobileGitHubExchange App 深链收到短码后调用：一次性消费短码、校验
// PKCE verifier，签发现有 JWT 双 token。任何失败都不可重试同一短码
// （已被消费），须整个流程重来。
func (s *AuthService) MobileGitHubExchange(authCode, codeVerifier, ua string) (*TokenPair, error) {
	if authCode == "" || codeVerifier == "" {
		return nil, errMobileExchange
	}
	raw, ok := consumeOnce("mcode:" + authCode)
	if !ok {
		return nil, errMobileExchange
	}
	var rec mobileCodeRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, errMobileExchange
	}
	if subtle.ConstantTimeCompare([]byte(pkceChallengeS256(codeVerifier)), []byte(rec.Challenge)) != 1 {
		return nil, errMobileExchange
	}
	var user model.User
	if err := common.DB.First(&user, rec.UserID).Error; err != nil {
		return nil, errMobileExchange
	}
	if user.Status != model.StatusEnabled {
		return nil, errors.New("账号已被禁用")
	}
	return s.issueFor(&user, ua)
}

// pkceChallengeS256 RFC 7636 S256：base64url(SHA256(verifier))，无填充。
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
