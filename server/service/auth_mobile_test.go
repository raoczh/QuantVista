package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// mobileStateKey 按 service 层约定拼移动流 state 的一次性存储 key。
func mobileStateKey(state string) string { return "mstate:" + common.StateNonce(state) }

// TestMobileStateConsumedOnce 移动流 state 一次性：第一次回调即消费（且消费
// 发生在请求 GitHub 之前），同一 state 重放不再触发对 GitHub 的任何请求。
func TestMobileStateConsumedOnce(t *testing.T) {
	svc := NewAuthService()
	verifier := strings.Repeat("v", 43)
	state := common.SignState()
	storeOnce(mobileStateKey(state), pkceChallengeS256(verifier), mobileStateTTL)

	// 第一次：state 校验+消费通过，流程推进到换 GitHub token（测试环境 OAuth
	// 未配置凭证，在该步失败）——错误不是「已使用或过期」。
	_, err1 := svc.MobileGitHubCallback(context.Background(), "code-x", state, "https://site/login/callback?mode=mobile")
	if err1 == nil {
		t.Fatalf("测试环境未配置 GitHub 凭证，第一次回调应失败于换 token 步")
	}
	if strings.Contains(err1.Error(), "已使用或过期") {
		t.Fatalf("第一次回调不应报 state 已消费: %v", err1)
	}

	// 第二次（重放同一 state）：HMAC 仍有效，但一次性记录已被消费，
	// 必须在触发 GitHub 请求之前拒绝。
	_, err2 := svc.MobileGitHubCallback(context.Background(), "code-x", state, "https://site/login/callback?mode=mobile")
	if err2 == nil || !strings.Contains(err2.Error(), "已使用或过期") {
		t.Fatalf("state 重放应被一次性消费拦下，得到: %v", err2)
	}
}

// TestMobileExchangeReplay 短码重放：第一次兑换成功签发双 token，同一短码
// 第二次兑换必须失败（统一错误文案，不泄露短码是否存在过）。
func TestMobileExchangeReplay(t *testing.T) {
	setupTestDB(t)
	svc := NewAuthService()
	user := &model.User{Username: "mob_replay", Role: model.RoleUser, Status: model.StatusEnabled}
	if err := common.DB.Create(user).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	verifier := strings.Repeat("a", 64)
	authCode := "test-auth-code-replay"
	rec, _ := json.Marshal(mobileCodeRecord{UserID: user.ID, Challenge: pkceChallengeS256(verifier)})
	storeOnce("mcode:"+authCode, string(rec), mobileAuthCodeTTL)

	pair, err := svc.MobileGitHubExchange(authCode, verifier, "ua-test")
	if err != nil {
		t.Fatalf("首次兑换应成功: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" || pair.User == nil || pair.User.ID != user.ID {
		t.Fatalf("兑换结果不完整: %+v", pair)
	}
	if pair.User.Password != "" {
		t.Fatalf("兑换结果不得外泄密码哈希")
	}

	if _, err := svc.MobileGitHubExchange(authCode, verifier, "ua-test"); err == nil {
		t.Fatalf("短码重放应失败")
	} else if err.Error() != errMobileExchange.Error() {
		t.Fatalf("重放错误应为统一文案，得到: %v", err)
	}
}

// TestMobileExchangeWrongVerifier PKCE 校验：verifier 不匹配拒绝兑换；且短码
// 已随失败被消费，换正确 verifier 也无法再用（防截获短码后离线穷举）。
func TestMobileExchangeWrongVerifier(t *testing.T) {
	setupTestDB(t)
	svc := NewAuthService()
	user := &model.User{Username: "mob_pkce", Role: model.RoleUser, Status: model.StatusEnabled}
	if err := common.DB.Create(user).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	verifier := strings.Repeat("b", 43)
	authCode := "test-auth-code-pkce"
	rec, _ := json.Marshal(mobileCodeRecord{UserID: user.ID, Challenge: pkceChallengeS256(verifier)})
	storeOnce("mcode:"+authCode, string(rec), mobileAuthCodeTTL)

	if _, err := svc.MobileGitHubExchange(authCode, strings.Repeat("c", 43), "ua-test"); err == nil {
		t.Fatalf("verifier 错误应拒绝兑换")
	} else if err.Error() != errMobileExchange.Error() {
		t.Fatalf("verifier 错误应为统一文案（不泄露失败原因细分），得到: %v", err)
	}

	if _, err := svc.MobileGitHubExchange(authCode, verifier, "ua-test"); err == nil {
		t.Fatalf("verifier 校验失败后短码应已消费，正确 verifier 也不得复用")
	}
}

// TestTTLStoreExpiryAndAtomicity 进程内兜底存储：过期条目视同不存在；
// 消费即删除，同 key 只能成功一次。
func TestTTLStoreExpiryAndAtomicity(t *testing.T) {
	s := newTTLStore()

	s.set("k1", "v1", -time.Second) // 已过期
	if _, ok := s.consume("k1"); ok {
		t.Fatalf("过期条目不应可消费")
	}

	s.set("k2", "v2", time.Minute)
	v, ok := s.consume("k2")
	if !ok || v != "v2" {
		t.Fatalf("有效条目应可消费: %q %v", v, ok)
	}
	if _, ok := s.consume("k2"); ok {
		t.Fatalf("同一条目二次消费应失败")
	}
}
