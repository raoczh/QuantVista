package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
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

// TestLLMFallbackSwitchAndPinned 管理后台"LLM 回退"开关与指定回退配置：
// 关闸后无配置用户必须自己配置；开闸且指定配置时优先命中指定（可以不是默认配置）；
// 指定失效（配置被删）静默回落首管默认；新闻的系统默认 LLM 不受该开关控制。
func TestLLMFallbackSwitchAndPinned(t *testing.T) {
	setupTestDB(t)
	common.EncryptionKey = "unit-test-key"
	svc := NewLLMService()

	admin := &model.User{Username: "sw-admin", Role: model.RoleAdmin, Status: model.StatusEnabled}
	user := &model.User{Username: "sw-user", Role: model.RoleUser, Status: model.StatusEnabled}
	common.DB.Create(admin)
	common.DB.Create(user)
	c1, _ := common.Encrypt("k-default")
	c2, _ := common.Encrypt("k-cheap")
	defCfg := &model.LLMConfig{UserID: admin.ID, Name: "default", Provider: "openai",
		BaseURL: "https://a.example.com", APIKeyCipher: c1, Model: "big", IsDefault: true}
	cheapCfg := &model.LLMConfig{UserID: admin.ID, Name: "cheap", Provider: "openai",
		BaseURL: "https://b.example.com", APIKeyCipher: c2, Model: "small"}
	common.DB.Create(defCfg)
	common.DB.Create(cheapCfg)

	t.Cleanup(func() {
		_ = setting.SetLLMFallback(true, 0) // 恢复默认，防污染其他测试
		common.DB.Where("1=1").Delete(&model.LLMConfig{})
		common.DB.Delete(admin)
		common.DB.Delete(user)
		common.DB.Where("`key` IN ?", []string{"llm_fallback_enabled", "llm_fallback_config_id"}).Delete(&model.Option{})
	})

	// 1) 关闸：无配置用户必须自己配置。
	if err := setting.SetLLMFallback(false, 0); err != nil {
		t.Fatalf("关闸失败: %v", err)
	}
	if _, _, err := svc.ResolveForUse(user.ID, 0); err == nil {
		t.Fatal("回退开关关闭时，无配置用户应报错引导自行配置")
	}

	// 2) 关闸不影响新闻的系统默认 LLM。
	if cfg, key, _, err := resolveNewsLLM(); err != nil || cfg.ID != defCfg.ID || key != "k-default" {
		t.Fatalf("关闸不应影响新闻系统默认 LLM: cfg=%v err=%v", cfg, err)
	}

	// 3) 开闸+指定非默认配置：用户回退与新闻都命中指定。
	if err := setting.SetLLMFallback(true, cheapCfg.ID); err != nil {
		t.Fatalf("指定回退配置失败: %v", err)
	}
	cfg, key, err := svc.ResolveForUse(user.ID, 0)
	if err != nil || cfg.ID != cheapCfg.ID || key != "k-cheap" {
		t.Fatalf("应命中指定的回退配置: cfg=%v err=%v", cfg, err)
	}
	if ncfg, _, _, err := resolveNewsLLM(); err != nil || ncfg.ID != cheapCfg.ID {
		t.Fatalf("新闻系统默认 LLM 应同样命中指定配置: cfg=%v err=%v", ncfg, err)
	}

	// 4) 指定失效（配置被删）：静默回落首管默认，不瘫在死引用上。
	common.DB.Delete(cheapCfg)
	cfg, _, err = svc.ResolveForUse(user.ID, 0)
	if err != nil || cfg.ID != defCfg.ID {
		t.Fatalf("指定配置被删后应回落首管默认: cfg=%v err=%v", cfg, err)
	}
}
