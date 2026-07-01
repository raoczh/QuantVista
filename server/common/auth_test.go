package common

import "testing"

func TestAccessTokenRoundTrip(t *testing.T) {
	SessionSecret = "test-session-secret"
	defer func() { SessionSecret = "" }()

	tok, exp, err := IssueAccessToken(42, "admin", 0)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if exp.IsZero() {
		t.Fatal("过期时间不应为零值")
	}
	claims, err := ParseAccessToken(tok)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if claims.UserID != 42 || claims.Role != "admin" {
		t.Fatalf("载荷不一致: uid=%d role=%s", claims.UserID, claims.Role)
	}
}

// RoleAdminForTest 避免 import model 造成循环，这里用字面量。
func RoleAdminForTest() string { return "admin" }

func TestParseRejectsTamperedToken(t *testing.T) {
	SessionSecret = "k1"
	tok, _, _ := IssueAccessToken(1, "user", 0)
	SessionSecret = "k2" // 换密钥后签名应失配
	defer func() { SessionSecret = "" }()
	if _, err := ParseAccessToken(tok); err == nil {
		t.Fatal("密钥不匹配时应解析失败")
	}
}

func TestStateSignVerify(t *testing.T) {
	SessionSecret = "k"
	defer func() { SessionSecret = "" }()
	s := SignState()
	if !VerifyState(s) {
		t.Fatal("自签 state 应校验通过")
	}
	if VerifyState(s + "x") {
		t.Fatal("被篡改的 state 应校验失败")
	}
	if VerifyState("garbage") {
		t.Fatal("非法格式 state 应校验失败")
	}
}
