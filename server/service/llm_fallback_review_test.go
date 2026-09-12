package service

import (
	"errors"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"

	"gorm.io/gorm"
)

func TestLLMFallbackReviewReadFailurePreservesCauseAndSelection(t *testing.T) {
	for _, target := range []string{"owned", "pinned_config", "pinned_owner", "auto_admin", "auto_config"} {
		t.Run(target, func(t *testing.T) {
			const adminID, userID int64 = 19113, 19114
			seedReportEnv(t, adminID, "http://127.0.0.1:1")
			user := model.User{ID: userID, Username: "fallback-read-review", Role: model.RoleUser, Status: model.StatusEnabled}
			if err := common.DB.Create(&user).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Delete(&user) })
			var cfg model.LLMConfig
			if err := common.DB.Where("user_id = ?", adminID).First(&cfg).Error; err != nil {
				t.Fatal(err)
			}
			pinned := int64(0)
			if strings.HasPrefix(target, "pinned_") {
				pinned = cfg.ID
			}
			if err := setting.SetLLMFallback(true, pinned); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = setting.SetLLMFallback(true, 0) })
			injected := errors.New("模拟模型回退链路读取故障")
			configQueries, faults := 0, 0
			const hook = "review_llm_fallback_read"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "llm_configs" {
					configQueries++
				}
				matches := target == "owned" && tx.Statement.Table == "llm_configs" ||
					(target == "pinned_config" || target == "auto_config") && tx.Statement.Table == "llm_configs" && configQueries == 2 ||
					(target == "pinned_owner" || target == "auto_admin") && tx.Statement.Table == "users"
				if matches && faults == 0 {
					faults++
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
			caller, id := userID, int64(0)
			if target == "owned" {
				caller, id = adminID, cfg.ID
			}
			selected, _, err := NewLLMService().ResolveForUse(caller, id)
			var readErr *llmConfigReadError
			if faults != 1 || selected != nil || !errors.Is(err, injected) || !errors.As(err, &readErr) || RefusalCodeOf(err) != RefusalLLMUnavailable {
				t.Fatalf("配置读取故障须保留错误链，不能冒充缺少配置或改用其他配置：faults=%d selected=%v err=%v", faults, selected != nil, err)
			}
		})
	}
}
