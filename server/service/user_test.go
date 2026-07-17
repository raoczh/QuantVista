package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestRecommendationTypeForHorizon(t *testing.T) {
	cases := []struct {
		name    string
		horizon string
		want    string
	}{
		{name: "short term", horizon: HorizonShortTerm, want: RecommendationTypeShortTerm},
		{name: "mid term", horizon: HorizonMidTerm, want: RecommendationTypeLongTerm},
		{name: "long term", horizon: HorizonLongTerm, want: RecommendationTypeLongTerm},
		{name: "unknown defaults to long term", horizon: "", want: RecommendationTypeLongTerm},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecommendationTypeForHorizon(tc.horizon); got != tc.want {
				t.Fatalf("RecommendationTypeForHorizon(%q) = %q, want %q", tc.horizon, got, tc.want)
			}
		})
	}
}

// TestChangePasswordTransaction 改密三步（写新密码 + token_version+1 + 吊销刷新令牌）原子生效（#6）。
func TestChangePasswordTransaction(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM users")
	common.DB.Exec("DELETE FROM refresh_tokens")
	svc := &UserService{}

	hash, _ := common.HashPassword("oldpass123")
	u := model.User{Username: "u1", Password: hash, TokenVersion: 1}
	if err := common.DB.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	common.DB.Create(&model.RefreshToken{UserID: u.ID, TokenHash: "rthash1", Revoked: false})

	// 旧密码错误 → 报错。
	if err := svc.ChangePassword(u.ID, "wrong", "newpass123"); err == nil {
		t.Fatal("旧密码错误应报错")
	}
	// 新密码过短 → 报错。
	if err := svc.ChangePassword(u.ID, "oldpass123", "short"); err == nil {
		t.Fatal("过短新密码应报错")
	}

	// 正常改密：三步全落。
	if err := svc.ChangePassword(u.ID, "oldpass123", "newpass123"); err != nil {
		t.Fatalf("改密失败: %v", err)
	}
	var after model.User
	common.DB.First(&after, u.ID)
	if !common.CheckPassword(after.Password, "newpass123") {
		t.Fatal("新密码未生效")
	}
	if after.TokenVersion != 2 {
		t.Fatalf("token_version 应 +1 到 2，得到 %d", after.TokenVersion)
	}
	var rt model.RefreshToken
	common.DB.Where("user_id = ?", u.ID).First(&rt)
	if !rt.Revoked {
		t.Fatal("刷新令牌应被吊销")
	}
}
