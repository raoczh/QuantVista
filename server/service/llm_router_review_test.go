package service

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestLLMRouteCacheFollowsConfigurationChanges(t *testing.T) {
	oldKey := common.EncryptionKey
	t.Cleanup(func() { common.EncryptionKey = oldKey })
	for _, action := range []string{"update", "delete", "disable_owner"} {
		t.Run(action, func(t *testing.T) {
			setModelRoutingFlag(t, true)
			cleanRouteTables(t)
			cfg := seedRouteAdminConfig(t, "http://route-review.example")
			if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if p := applyModelRouting(routeTestParams("qa")); p.Meta.ConfigID != cfg.ID {
				t.Fatal("未进入已配置路由")
			}
			var err error
			switch action {
			case "update":
				in := validLLMConfigInput()
				in.Name, in.BaseURL, in.Model, in.APIKey = cfg.Name, cfg.BaseURL, cfg.Model, "rotated-review-key"
				_, err = NewLLMService().Update(cfg.UserID, cfg.ID, in)
			case "delete":
				err = NewLLMService().Delete(cfg.UserID, cfg.ID)
			case "disable_owner":
				err = NewAdminService().SetUserStatus(9999, cfg.UserID, model.StatusDisabled)
			}
			if err != nil {
				t.Fatal(err)
			}
			p := applyModelRouting(routeTestParams("qa"))
			if action == "update" {
				if p.APIKey != "rotated-review-key" {
					t.Fatal("已完成密钥更新后不得继续使用缓存旧密钥")
				}
			} else if p.Meta.ConfigID == cfg.ID {
				t.Fatal("配置删除或所有者停用后不得继续使用缓存目标")
			}
		})
	}
}

func TestLLMRouteBrierComparisonUsesSameHorizon(t *testing.T) {
	setupTestDB(t)
	old := CachedLLMCalibrationReport()
	t.Cleanup(func() { calibCacheMu.Lock(); calibCache = old; calibCacheMu.Unlock() })
	calibCacheMu.Lock()
	calibCache = &LLMCalibrationReport{Recommendation: []*RecCalibReport{
		{Type: "short_term", HorizonDays: 10, Slices: []CalibSliceGroup{{Dim: "provider_model", Rows: []CalibSliceRow{
			{Key: "routed/m", Brier: fptr(.24)}, {Key: "peer/m", Brier: fptr(.30)},
		}}}},
		{Type: "long_term", HorizonDays: 20, Slices: []CalibSliceGroup{{Dim: "provider_model", Rows: []CalibSliceRow{
			{Key: "routed/m", Brier: fptr(.05)}, {Key: "peer/m", Brier: fptr(.08)},
		}}}},
	}}
	calibCacheMu.Unlock()
	route := model.LLMModuleRoute{Module: "recommendation", ConfigID: 100}
	cfg := model.LLMConfig{Provider: "routed", Model: "m"}
	if reason, ok := evaluateLLMRouteHealth(route, cfg); !ok {
		t.Fatalf("路由模型在两种周期内均更好，不得跨周期误判恶化：%s", reason)
	}
}

func TestLLMRouteResetDoesNotReuseOldFailures(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	cfg := seedRouteAdminConfig(t, "http://route-review.example")
	route, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	seedRouteCallLogs(t, "qa", cfg.ID, model.LLMCallStatusError, 5, 0)
	if p := applyModelRouting(routeTestParams("qa")); p.Meta.ConfigID == cfg.ID {
		t.Fatal("故障证据应触发自动回退")
	}
	if _, err := ResetLLMRouteFallback(route.ID); err != nil {
		t.Fatal(err)
	}
	if p := applyModelRouting(routeTestParams("qa")); p.Meta.ConfigID != cfg.ID {
		t.Fatal("管理员恢复后应采集新调用，不能立即被同一批旧失败重新停用")
	}
}

func TestLLMRouteStaleHealthCannotDisableReplacement(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	first := seedRouteAdminConfig(t, "http://route-first.example")
	second := *first
	second.ID, second.Name, second.BaseURL = 0, "route-second", "http://route-second.example"
	if err := common.DB.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	route, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: first.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	seedRouteCallLogs(t, "qa", first.ID, model.LLMCallStatusError, 5, 0)
	var once atomic.Bool
	const callback = "review_route_stale_health"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "llm_module_routes" && once.CompareAndSwap(false, true) {
			_, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: second.ID, Enabled: true})
			if err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	_ = loadLLMRoutes()
	var stored model.LLMModuleRoute
	if err := common.DB.First(&stored, route.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ConfigID != second.ID || stored.AutoFallbackAt != nil {
		t.Fatalf("旧目标健康检查不能停用刚换好的目标：config=%d fallback=%v", stored.ConfigID, stored.AutoFallbackAt)
	}
}

func TestLLMRouteTransientReadFailureDoesNotPersistFallback(t *testing.T) {
	for _, table := range []string{"llm_configs", "users"} {
		t.Run(table, func(t *testing.T) {
			setModelRoutingFlag(t, true)
			cleanRouteTables(t)
			cfg := seedRouteAdminConfig(t, "http://route-review.example")
			route, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			const callback = "review_route_read_failure"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("transient review database error"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			_ = loadLLMRoutes()
			var stored model.LLMModuleRoute
			if err := common.DB.First(&stored, route.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.AutoFallbackAt != nil {
				t.Fatalf("瞬时读库错误不能永久停用配置：%s", stored.AutoFallbackReason)
			}
		})
	}
}

func TestLLMRouteRecentFailuresStillDisableRoute(t *testing.T) {
	setupTestDB(t)
	route := model.LLMModuleRoute{Module: "qa", ConfigID: 1045}
	seedRouteCallLogs(t, "qa", route.ConfigID, model.LLMCallStatusError, 5, 0)
	if reason, ok := evaluateLLMRouteHealth(route, model.LLMConfig{}); ok || !strings.HasPrefix(reason, "error_rate:") {
		t.Fatalf("新故障仍须触发回退：%s ok=%v at=%v", reason, ok, time.Now())
	}
}
