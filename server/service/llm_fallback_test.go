package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestResolveForUseAdminFallback 普通用户没有任何自有 LLM 配置时，回退到首个启用
// 管理员的默认配置（管理员代付 key，配额仍按发起用户）；有自有配置时优先自己的；
// 管理员本人无配置、或管理员也未配置时保持"请先在设置中添加"的引导报错。
func TestResolveForUseAdminFallback(t *testing.T) {
	setupTestDB(t)
	common.EncryptionKey = "unit-test-key"
	svc := NewLLMService()

	admin := &model.User{Username: "fb-admin", Role: model.RoleAdmin, Status: model.StatusEnabled}
	user := &model.User{Username: "fb-user", Role: model.RoleUser, Status: model.StatusEnabled}
	if err := common.DB.Create(admin).Error; err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	if err := common.DB.Create(user).Error; err != nil {
		t.Fatalf("建普通用户失败: %v", err)
	}
	cipher, _ := common.Encrypt("admin-key")
	adminCfg := &model.LLMConfig{UserID: admin.ID, Name: "admin-default", Provider: "openai",
		BaseURL: "https://api.example.com", APIKeyCipher: cipher, Model: "m1", IsDefault: true}
	if err := common.DB.Create(adminCfg).Error; err != nil {
		t.Fatalf("建管理员配置失败: %v", err)
	}
	t.Cleanup(func() {
		common.DB.Where("1=1").Delete(&model.LLMConfig{})
		common.DB.Delete(admin)
		common.DB.Delete(user)
	})

	// 1) 普通用户无配置 → 回退命中管理员默认配置。
	cfg, key, err := svc.ResolveForUse(user.ID, 0)
	if err != nil {
		t.Fatalf("无配置用户应回退管理员配置: %v", err)
	}
	if cfg.UserID != admin.ID || cfg.ID != adminCfg.ID || key != "admin-key" {
		t.Fatalf("回退应命中管理员默认配置，得到 owner=%d cfg=%d", cfg.UserID, cfg.ID)
	}

	// 2) 内网放行按配置所有者：管理员的配置放行，普通用户自己的不放行。
	if !llmAllowPrivate(false, cfg) {
		t.Fatal("回退到管理员配置时应放行内网（URL 由管理员配置，非用户可控）")
	}

	// 3) 用户有自有配置时优先自己的（即使非默认）。
	uc, _ := common.Encrypt("user-key")
	userCfg := &model.LLMConfig{UserID: user.ID, Name: "mine", Provider: "openai",
		BaseURL: "https://api2.example.com", APIKeyCipher: uc, Model: "m2"}
	if err := common.DB.Create(userCfg).Error; err != nil {
		t.Fatalf("建用户配置失败: %v", err)
	}
	cfg, key, err = svc.ResolveForUse(user.ID, 0)
	if err != nil || cfg.ID != userCfg.ID || key != "user-key" {
		t.Fatalf("有自有配置时应优先自己的: cfg=%v err=%v", cfg, err)
	}
	if llmAllowPrivate(false, cfg) {
		t.Fatal("普通用户自己的配置不应放行内网")
	}

	// 4) 指定 id 仍限本人：用户不能指定管理员的配置 id。
	if _, _, err := svc.ResolveForUse(user.ID, adminCfg.ID); err == nil {
		t.Fatal("指定他人配置 id 必须被拒绝（隔离边界）")
	}

	// 5) 管理员本人无配置 → 报引导错误，不回退到自己死循环。
	common.DB.Delete(adminCfg)
	common.DB.Delete(userCfg)
	if _, _, err := svc.ResolveForUse(admin.ID, 0); err == nil {
		t.Fatal("管理员本人无配置应报错引导配置")
	}

	// 6) 管理员也没有任何配置 → 普通用户回退失败，保持引导报错。
	if _, _, err := svc.ResolveForUse(user.ID, 0); err == nil {
		t.Fatal("管理员未配置时普通用户应收到引导报错")
	}
}
