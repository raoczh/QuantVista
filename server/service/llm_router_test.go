package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// ---------- P2-4 模型路由与成本优化（llm_router.go） ----------

func setModelRoutingFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMModelRouting(v); err != nil {
		t.Fatalf("切换模型路由开关失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMModelRouting(false) }) // 缺省关
}

func cleanRouteTables(t *testing.T) {
	t.Helper()
	clean := func() {
		common.DB.Where("1=1").Delete(&model.LLMModuleRoute{})
		common.DB.Where("1=1").Delete(&model.LLMCallLog{})
		common.DB.Where("name LIKE ?", "route-%").Delete(&model.LLMConfig{})
		common.DB.Where("username LIKE ?", "route-%").Delete(&model.User{})
		invalidateLLMRouteCache()
		resetLLMCapabilityStore()
	}
	clean()
	t.Cleanup(clean)
}

// seedRouteAdminConfig 路由目标配置（属启用管理员——AllowPrivate 语义要求）。
func seedRouteAdminConfig(t *testing.T, baseURL string) *model.LLMConfig {
	t.Helper()
	common.EncryptionKey = "unit-test-key"
	admin := &model.User{Username: "route-admin", Role: model.RoleAdmin, Status: model.StatusEnabled}
	if err := common.DB.Create(admin).Error; err != nil {
		t.Fatalf("seed 管理员失败: %v", err)
	}
	cipher, _ := common.Encrypt("sk-routed")
	cfg := &model.LLMConfig{UserID: admin.ID, Name: "route-target", Provider: "routed-prov",
		BaseURL: baseURL, APIKeyCipher: cipher, Model: "routed-model", Temperature: 0.3, MaxTokens: 1200}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("seed 路由配置失败: %v", err)
	}
	return cfg
}

// routeTestParams 一次待路由调用的基础参数（原配置 id=999 provider=orig）。
func routeTestParams(module string) chatParams {
	return chatParams{
		BaseURL: "http://orig.example", APIKey: "k-orig", Model: "orig-model",
		Temperature: 0.7, MaxTokens: 2500,
		Meta: chatMeta{CallerUserID: 1, Module: module, ConfigID: 999, Provider: "orig-prov"},
	}
}

// TestApplyModelRoutingFlagAndScope 反例组：flag 关/无路由/module 空/探针恒零改写。
func TestApplyModelRoutingFlagAndScope(t *testing.T) {
	setModelRoutingFlag(t, false)
	cleanRouteTables(t)
	cfg := seedRouteAdminConfig(t, "http://routed.example")
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建路由: %v", err)
	}

	// flag 关：有路由也零改写。
	p := applyModelRouting(routeTestParams("qa"))
	if p.BaseURL != "http://orig.example" || p.Model != "orig-model" || p.Meta.ConfigID != 999 {
		t.Fatalf("flag 关不得路由: %+v", p)
	}

	_ = setting.SetLLMModelRouting(true)
	// module 空 / 探针：不路由。
	for _, m := range []string{"", "test"} {
		p := applyModelRouting(routeTestParams(m))
		if p.BaseURL != "http://orig.example" {
			t.Fatalf("module=%q 不得路由", m)
		}
	}
	// 无路由的模块：不路由。
	if p := applyModelRouting(routeTestParams("analysis")); p.Model != "orig-model" {
		t.Fatalf("无路由模块不得改写: %+v", p)
	}
	// 路由 Enabled=false：不路由。
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: false}); err != nil {
		t.Fatalf("停用路由: %v", err)
	}
	if p := applyModelRouting(routeTestParams("qa")); p.Model != "orig-model" {
		t.Fatalf("停用路由不得改写: %+v", p)
	}
}

// TestApplyModelRoutingSwapsTarget 命中换目标：连接参数/审计 meta/RouteApplied 观测/
// 预算取严/AllowPrivate 重判；experiment 恒跟随 recommendation 路由（单变量）。
func TestApplyModelRoutingSwapsTarget(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	cfg := seedRouteAdminConfig(t, "http://routed.example")
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建路由: %v", err)
	}

	run := newLLMRun("t-route", "", "qa", "qa.free_text.v1", "q13")
	origCfg := &model.LLMConfig{ID: 999, Provider: "orig-prov", Model: "orig-model"}
	p := routeTestParams("qa")
	p.Meta = run.chatMeta(1, origCfg, 1)
	p = applyModelRouting(p)

	if p.BaseURL != "http://routed.example" || p.APIKey != "sk-routed" || p.Model != "routed-model" {
		t.Fatalf("目标未切换: %+v", p)
	}
	if p.Temperature != 0.3 {
		t.Fatalf("温度应取路由配置: %v", p.Temperature)
	}
	if p.MaxTokens != 1200 {
		t.Fatalf("预算应取严（路由配置 1200 < 原 2500）: %d", p.MaxTokens)
	}
	if !p.AllowPrivate {
		t.Fatal("路由目标属管理员应放行内网")
	}
	if p.Meta.ConfigID != cfg.ID || p.Meta.Provider != "routed-prov" {
		t.Fatalf("审计 meta 应随目标走: %+v", p.Meta)
	}
	ra := run.routeApplied
	if !ra.Applied || ra.ConfigID != cfg.ID || ra.Model != "routed-model" ||
		ra.FromConfigID != 999 || ra.FromModel != "orig-model" {
		t.Fatalf("RouteApplied 观测不符: %+v", ra)
	}
	m := run.manifest(origCfg, false)
	if m.Routed == nil || m.Routed.ConfigID != cfg.ID || m.Routed.FromModel != "orig-model" {
		t.Fatalf("manifest 应带 routed 声明: %+v", m.Routed)
	}

	// 预算不放大：原 MaxTokens 更小时保留原值。
	p2 := routeTestParams("qa")
	p2.MaxTokens = 800
	if p2 = applyModelRouting(p2); p2.MaxTokens != 800 {
		t.Fatalf("路由不得放大预算: %d", p2.MaxTokens)
	}

	// experiment 别名：跟随 recommendation 路由；无 recommendation 路由时不路由。
	if p3 := applyModelRouting(routeTestParams("experiment")); p3.Model != "orig-model" {
		t.Fatalf("无 recommendation 路由时 experiment 不得改写: %+v", p3)
	}
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "recommendation", ConfigID: cfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建 recommendation 路由: %v", err)
	}
	if p4 := applyModelRouting(routeTestParams("experiment")); p4.Model != "routed-model" {
		t.Fatalf("experiment 应跟随 recommendation 路由（单变量）: %+v", p4)
	}
	// experiment 不可单独建路由。
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "experiment", ConfigID: cfg.ID, Enabled: true}); err == nil ||
		!strings.Contains(err.Error(), "单变量") {
		t.Fatalf("experiment 单独建路由应拒绝: %v", err)
	}
}

// TestApplyModelRoutingStructuredSkip P0-5 能力矩阵消费：结构化任务不路由到已知
// 不支持 json_object 的目标；free_text 任务不受此限；观察过期后恢复路由。
func TestApplyModelRoutingStructuredSkip(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	cfg := seedRouteAdminConfig(t, "http://routed.example")
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "recommendation", ConfigID: cfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建路由: %v", err)
	}
	target := llmCapabilityTarget(cfg.ID, cfg.Provider, cfg.BaseURL, cfg.Model, cfg.EndpointType)
	observeLLMCapability(target, capJSONObject, capUnsupported, "unit")

	pJSON := routeTestParams("recommendation")
	pJSON.JSONMode = true
	if pJSON = applyModelRouting(pJSON); pJSON.Model != "orig-model" {
		t.Fatalf("结构化任务不得路由到不支持 json_object 的目标: %+v", pJSON)
	}
	pFree := routeTestParams("recommendation")
	if pFree = applyModelRouting(pFree); pFree.Model != "routed-model" {
		t.Fatalf("free_text 任务不受结构化能力限制: %+v", pFree)
	}
	resetLLMCapabilityStore()
	pJSON2 := routeTestParams("recommendation")
	pJSON2.JSONMode = true
	if pJSON2 = applyModelRouting(pJSON2); pJSON2.Model != "routed-model" {
		t.Fatalf("观察清除后应恢复路由: %+v", pJSON2)
	}
}

// seedRouteCallLogs 健康统计用的审计行。
func seedRouteCallLogs(t *testing.T, module string, configID int64, status string, n, tokens int) {
	t.Helper()
	for i := 0; i < n; i++ {
		common.DB.Create(&model.LLMCallLog{UserID: 1, Module: module, LLMConfigID: configID,
			Status: status, TotalTokens: tokens, CreatedAt: time.Now()})
	}
}

// TestLLMRouteAutoFallback 自动回退三反例：失败率/成本比触发持久化回退且不再路由；
// 显式恢复（reset/重新保存）后重新生效；配置被删触发 config_unavailable。
func TestLLMRouteAutoFallback(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	cfg := seedRouteAdminConfig(t, "http://routed.example")
	rt, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true})
	if err != nil {
		t.Fatalf("建路由: %v", err)
	}

	// 失败率 3/6=50% ≥30% → 回退。
	seedRouteCallLogs(t, "qa", cfg.ID, model.LLMCallStatusError, 3, 0)
	seedRouteCallLogs(t, "qa", cfg.ID, model.LLMCallStatusSuccess, 3, 500)
	invalidateLLMRouteCache()
	if p := applyModelRouting(routeTestParams("qa")); p.Model != "orig-model" {
		t.Fatalf("失败率超阈值应回退: %+v", p)
	}
	var after model.LLMModuleRoute
	common.DB.First(&after, rt.ID)
	if after.AutoFallbackAt == nil || !strings.Contains(after.AutoFallbackReason, "error_rate") {
		t.Fatalf("回退状态应持久化: %+v", after)
	}
	// 回退状态下即使统计恢复也不自动重启（显式恢复才生效）。
	common.DB.Where("module = ?", "qa").Delete(&model.LLMCallLog{})
	invalidateLLMRouteCache()
	if p := applyModelRouting(routeTestParams("qa")); p.Model != "orig-model" {
		t.Fatal("自动回退不得自动恢复")
	}
	if _, err := ResetLLMRouteFallback(rt.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if p := applyModelRouting(routeTestParams("qa")); p.Model != "routed-model" {
		t.Fatalf("显式恢复后应重新路由: %+v", p)
	}

	// 成本比：路由 2000 vs 基线 1000 = 2.0 > 1.35 → 回退；阈值放宽到 3.0 则健康。
	seedRouteCallLogs(t, "qa", cfg.ID, model.LLMCallStatusSuccess, 5, 2000)
	seedRouteCallLogs(t, "qa", 888, model.LLMCallStatusSuccess, 5, 1000)
	invalidateLLMRouteCache()
	if p := applyModelRouting(routeTestParams("qa")); p.Model != "orig-model" {
		t.Fatal("成本超阈值应回退")
	}
	common.DB.First(&after, rt.ID)
	if !strings.Contains(after.AutoFallbackReason, "cost_exceeded") {
		t.Fatalf("回退原因应为成本: %s", after.AutoFallbackReason)
	}
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true, MaxCostRatio: 3.0}); err != nil {
		t.Fatalf("放宽阈值: %v", err)
	}
	if p := applyModelRouting(routeTestParams("qa")); p.Model != "routed-model" {
		t.Fatalf("阈值内应继续路由: %+v", p)
	}

	// 配置被删：config_unavailable 回退。
	common.DB.Delete(&model.LLMConfig{}, cfg.ID)
	invalidateLLMRouteCache()
	if p := applyModelRouting(routeTestParams("qa")); p.Model != "orig-model" {
		t.Fatal("配置不可用应回退")
	}
	common.DB.First(&after, rt.ID)
	if !strings.Contains(after.AutoFallbackReason, "config_unavailable") {
		t.Fatalf("回退原因应为配置不可用: %s", after.AutoFallbackReason)
	}
}

// TestRouteCalibBrierFallback 校准分层 provider·model 数据的路由消费：路由目标 Brier
// 比最优层恶化超 10% 触发回退；边界内/无可比数据不下结论。
func TestRouteCalibBrierFallback(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	f := func(v float64) *float64 { return &v }
	setCalib := func(rows []CalibSliceRow) {
		calibCacheMu.Lock()
		calibCache = &LLMCalibrationReport{Recommendation: []*RecCalibReport{{
			Slices: []CalibSliceGroup{{Dim: "provider_model", Rows: rows}},
		}}}
		calibCacheMu.Unlock()
	}
	t.Cleanup(func() {
		calibCacheMu.Lock()
		calibCache = nil
		calibCacheMu.Unlock()
	})

	cfg := model.LLMConfig{Provider: "routed-prov", Model: "routed-model"}
	rt := model.LLMModuleRoute{Module: "recommendation", ConfigID: 1}

	// 恶化超 10%：0.30 > 0.20×1.10 → 回退。
	setCalib([]CalibSliceRow{
		{Key: "routed-prov/routed-model", Brier: f(0.30)},
		{Key: "other/m2", Brier: f(0.20)},
	})
	if reason, ok := evaluateLLMRouteHealth(rt, cfg); ok || !strings.Contains(reason, "brier_degraded") {
		t.Fatalf("Brier 恶化应回退: %q ok=%v", reason, ok)
	}
	// 边界内：0.21 ≤ 0.20×1.10 → 健康。
	setCalib([]CalibSliceRow{
		{Key: "routed-prov/routed-model", Brier: f(0.21)},
		{Key: "other/m2", Brier: f(0.20)},
	})
	if _, ok := evaluateLLMRouteHealth(rt, cfg); !ok {
		t.Fatal("阈值内不应回退")
	}
	// 路由目标层未评估（无 Brier 行）：不下结论。
	setCalib([]CalibSliceRow{{Key: "other/m2", Brier: f(0.20)}})
	if _, ok := evaluateLLMRouteHealth(rt, cfg); !ok {
		t.Fatal("无可比数据不得回退")
	}
	// 非 recommendation 模块不吃校准信号。
	rtQA := model.LLMModuleRoute{Module: "qa", ConfigID: 1}
	setCalib([]CalibSliceRow{
		{Key: "routed-prov/routed-model", Brier: f(0.90)},
		{Key: "other/m2", Brier: f(0.10)},
	})
	if _, ok := evaluateLLMRouteHealth(rtQA, cfg); !ok {
		t.Fatal("非 recommendation 模块不应吃校准信号")
	}
}

// TestUpsertLLMRouteValidation 路由 CRUD 校验：未知模块/非管理员配置拒绝；
// 同模块 upsert 复用同一行；保存清除自动回退状态。
func TestUpsertLLMRouteValidation(t *testing.T) {
	setModelRoutingFlag(t, false)
	cleanRouteTables(t)
	cfg := seedRouteAdminConfig(t, "http://routed.example")

	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "no_such_module", ConfigID: cfg.ID, Enabled: true}); err == nil {
		t.Fatal("未知模块应拒绝")
	}
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: 12345, Enabled: true}); err == nil {
		t.Fatal("配置不存在应拒绝")
	}
	// 非管理员所有的配置拒绝。
	u := &model.User{Username: "route-user", Role: model.RoleUser, Status: model.StatusEnabled}
	common.DB.Create(u)
	cipher, _ := common.Encrypt("sk-u")
	ucfg := &model.LLMConfig{UserID: u.ID, Name: "route-user-cfg", BaseURL: "http://x", APIKeyCipher: cipher, Model: "m"}
	common.DB.Create(ucfg)
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: ucfg.ID, Enabled: true}); err == nil ||
		!strings.Contains(err.Error(), "管理员") {
		t.Fatalf("非管理员配置应拒绝: %v", err)
	}

	rt, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true, Note: "省成本"})
	if err != nil {
		t.Fatalf("建路由: %v", err)
	}
	// 人工置回退态后重新保存=显式恢复。
	now := time.Now()
	common.DB.Model(&model.LLMModuleRoute{}).Where("id = ?", rt.ID).
		Updates(map[string]any{"auto_fallback_at": now, "auto_fallback_reason": "error_rate: x"})
	rt2, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true})
	if err != nil || rt2.ID != rt.ID {
		t.Fatalf("同模块应复用同一行: %v %+v", err, rt2)
	}
	var after model.LLMModuleRoute
	common.DB.First(&after, rt.ID)
	if after.AutoFallbackAt != nil || after.AutoFallbackReason != "" {
		t.Fatalf("重新保存应清除回退状态: %+v", after)
	}
	var cnt int64
	common.DB.Model(&model.LLMModuleRoute{}).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("不应产生第二行: %d", cnt)
	}
	if err := DeleteLLMRoute(rt.ID); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if err := DeleteLLMRoute(rt.ID); err == nil {
		t.Fatal("重复删除应报不存在")
	}
}

// TestModelRoutingEndToEndAudit 端到端（假上游）：路由后请求打到路由目标，
// llm_call_logs 逐请求记路由后的真实 config/model，原上游零请求。
func TestModelRoutingEndToEndAudit(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)

	var routedCalls, origCalls atomic.Int64
	routedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routedCalls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer routedSrv.Close()
	origSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origCalls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"orig"},"finish_reason":"stop"}]}`))
	}))
	defer origSrv.Close()

	cfg := seedRouteAdminConfig(t, routedSrv.URL)
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "compare", ConfigID: cfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建路由: %v", err)
	}

	run := newLLMRun("t-route-e2e", "", "compare", "compare.free_text.v1", "c1")
	origCfg := &model.LLMConfig{ID: 999, Provider: "orig-prov", Model: "orig-model"}
	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: origSrv.URL, APIKey: "k-orig", Model: "orig-model",
		MaxTokens: 500, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     run.chatMeta(1, origCfg, 1),
	})
	if err != nil || res.Content != "ok" {
		t.Fatalf("路由调用失败: %v %+v", err, res)
	}
	if routedCalls.Load() != 1 || origCalls.Load() != 0 {
		t.Fatalf("请求应只打路由目标: routed=%d orig=%d", routedCalls.Load(), origCalls.Load())
	}
	var lg model.LLMCallLog
	if err := common.DB.Where("trace_id = ?", "t-route-e2e").First(&lg).Error; err != nil {
		t.Fatalf("审计行: %v", err)
	}
	if lg.LLMConfigID != cfg.ID || lg.Model != "routed-model" || lg.Provider != "routed-prov" {
		t.Fatalf("审计应记路由后真实目标: %+v", lg)
	}
	if !run.routeApplied.Applied || run.routeApplied.FromConfigID != 999 {
		t.Fatalf("run 级路由观测不符: %+v", run.routeApplied)
	}
}
