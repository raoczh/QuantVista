package service

import (
	"context"
	"encoding/json"
	"io"
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
// 业务预算不变/AllowPrivate 重判；experiment 恒跟随 recommendation 路由（单变量）。
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
	if p.MaxTokens != 2500 {
		t.Fatalf("路由不得改写已计算的业务预算（目标配置默认值为 1200）: %d", p.MaxTokens)
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
	// 中央客户端只产出本 attempt 的候选归因；业务接受输出后才允许进 manifest。
	run.acceptRouteAttribution()
	m := run.manifest(origCfg, false)
	if m.Routed == nil || m.Routed.ConfigID != cfg.ID || m.Routed.FromModel != "orig-model" {
		t.Fatalf("manifest 应带 routed 声明: %+v", m.Routed)
	}

	// 业务预算小于目标配置默认值时同样逐值保留。
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

// TestModelRoutingPreservesLengthRepairBudget 端到端验证路由只换目标：业务首轮预算
// 2000 必须原样发送，length repair 的 1.5 倍扩容必须实际发送为 3000，均不得被
// 路由目标配置自身的 MaxTokens=1200 二次压低。
func TestModelRoutingPreservesLengthRepairBudget(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	if err := setting.SetLLMAccuracyContract(true); err != nil {
		t.Fatalf("开启准确性契约: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMAccuracyContract(true) })

	var routedCalls, origCalls atomic.Int64
	var firstBudget, repairBudget atomic.Int64
	routedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := routedCalls.Add(1)
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析路由请求体: %v", err)
		}
		if call == 1 {
			firstBudget.Store(int64(body.MaxTokens))
		} else {
			repairBudget.Store(int64(body.MaxTokens))
		}
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"partial\":true}"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}}`))
	}))
	defer routedSrv.Close()
	origSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		origCalls.Add(1)
		http.Error(w, "原目标不应收到请求", http.StatusInternalServerError)
	}))
	defer origSrv.Close()

	targetCfg := seedRouteAdminConfig(t, routedSrv.URL) // 目标配置 MaxTokens=1200
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "analysis", ConfigID: targetCfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建 analysis 路由: %v", err)
	}
	origCfg := &model.LLMConfig{ID: 999, Provider: "orig-prov", BaseURL: origSrv.URL,
		Model: "orig-model", MaxTokens: 2000}
	run := newLLMRun("t-route-length-repair", "", "analysis", "analysis.v1", "p1")
	parse := func(raw string) error {
		var value map[string]any
		return json.Unmarshal([]byte(raw), &value)
	}
	_, _, _, err := (&AnalysisService{}).callWithRepair(context.Background(), 1, run, origCfg,
		"sk-orig", true, []chatMessage{{Role: "user", Content: "分析"}}, parse, analysisRepairHint)
	if err != nil {
		t.Fatalf("length repair 应成功: %v", err)
	}
	if routedCalls.Load() != 2 || origCalls.Load() != 0 {
		t.Fatalf("两轮都应只请求路由目标: routed=%d orig=%d", routedCalls.Load(), origCalls.Load())
	}
	if firstBudget.Load() != 2000 || repairBudget.Load() != 3000 {
		t.Fatalf("业务预算/repair 扩容不得被目标配置压低: first=%d repair=%d",
			firstBudget.Load(), repairBudget.Load())
	}
}

// TestApplyBatchRouteAttribution 审查修复批：推荐批次归因列回写路由后真实目标——
// 批次 Provider/Model 是校准分层 provider·model、联合评估对照与路由 Brier 回退的
// 数据源，路由流量不得归入原始模型层；未路由零改写，LLMConfigID 恒保持原配置指针。
func TestApplyBatchRouteAttribution(t *testing.T) {
	batch := &model.RecommendationBatch{LLMConfigID: 999, Provider: "orig-prov", Model: "orig-model"}
	run := newLLMRun("t-attr", "", "recommendation", "recommendation.v2", "p13")

	// 未路由：零改写。
	applyBatchRouteAttribution(batch, run)
	if batch.Provider != "orig-prov" || batch.Model != "orig-model" {
		t.Fatalf("未路由不得改写归因: %+v", batch)
	}

	run.routeApplied = LLMRouteApplied{Applied: true, RouteID: 1, ConfigID: 7,
		Provider: "routed-prov", Model: "routed-model", FromConfigID: 999, FromModel: "orig-model"}
	run.acceptRouteAttribution()
	applyBatchRouteAttribution(batch, run)
	if batch.Provider != "routed-prov" || batch.Model != "routed-model" {
		t.Fatalf("路由后归因应记真实目标: %+v", batch)
	}
	if batch.LLMConfigID != 999 {
		t.Fatalf("LLMConfigID 应保持原配置指针（回显/重试语义）: %d", batch.LLMConfigID)
	}
}

// TestSecondaryRunRejectedAttemptsDoNotPublishRoute 验证次级 rec_bear run 即使最后一次
// HTTP 调用命中路由，只要业务 JSON 未通过解析，manifest 就不能记录该失败目标。
func TestSecondaryRunRejectedAttemptsDoNotPublishRoute(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)

	var calls atomic.Int64
	routedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not-json"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	defer routedSrv.Close()

	targetCfg := seedRouteAdminConfig(t, routedSrv.URL)
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "rec_bear", ConfigID: targetCfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建 rec_bear 路由: %v", err)
	}
	origCfg := &model.LLMConfig{ID: 999, Provider: "orig-prov", BaseURL: routedSrv.URL,
		Model: "orig-model", MaxTokens: 1500}
	pool := map[string]candidate{"600000": {Symbol: "600000", Name: "浦发银行", Price: 10}}
	bears, _, run := (&RecommendationService{}).bearReview(context.Background(), 1, origCfg, "sk-orig", true,
		[]recPick{{Symbol: "600000", Action: model.RecActionBuy, Confidence: 60}}, pool, "t-bear-route", "r-parent")
	if bears != nil || run == nil || calls.Load() != int64(1+moduleRepairAttempts("rec_bear")) {
		t.Fatalf("应打满 repair 且拒收: calls=%d bears=%+v run=%v", calls.Load(), bears, run != nil)
	}
	if !run.routeApplied.Applied {
		t.Fatalf("前置条件：最后一次调用应命中路由: %+v", run.routeApplied)
	}
	if run.acceptedRouteApplied.Applied {
		t.Fatalf("解析未通过不得提交路由归因: %+v", run.acceptedRouteApplied)
	}
	if got := run.manifest(origCfg, true); got.Routed != nil {
		t.Fatalf("失败的次级 run 不得在 manifest 中声明 routed: %+v", got.Routed)
	}
}

// TestApplyBatchRouteAttributionUsesAcceptedRepair 首轮路由目标回落 free-text 后输出无效，
// repair 因能力观察改走原模型并成功时，批次只能归因最终被接受的原模型。
func TestApplyBatchRouteAttributionUsesAcceptedRepair(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	if err := setting.SetLLMCapabilityRouting(true); err != nil {
		t.Fatalf("开启能力路由: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMCapabilityRouting(true) })

	var routedCalls, origCalls atomic.Int64
	routedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routedCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "response_format") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format is not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not-json"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	defer routedSrv.Close()
	origSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origCalls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[{\"symbol\":\"600000\",\"action\":\"watch\",\"confidence\":55,\"reason\":[\"r\"],\"risks\":[\"k\"],\"evidence\":[\"e\"]}],\"rejected\":[]}"},"finish_reason":"stop"}],"usage":{"total_tokens":7}}`))
	}))
	defer origSrv.Close()

	targetCfg := seedRouteAdminConfig(t, routedSrv.URL)
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "recommendation", ConfigID: targetCfg.ID, Enabled: true}); err != nil {
		t.Fatalf("建路由: %v", err)
	}
	origCfg := &model.LLMConfig{ID: 999, Provider: "orig-prov", BaseURL: origSrv.URL,
		Model: "orig-model", MaxTokens: 2500}
	run := newLLMRun("t-repair-route", "", "recommendation", "recommendation.v2", "p13")
	pool := map[string]candidate{"600000": {Symbol: "600000", Name: "浦发银行", Price: 10}}
	picks, _, _, _, err := (&RecommendationService{}).callWithRepair(context.Background(), 1, run,
		origCfg, "sk-orig", true, []chatMessage{{Role: "user", Content: "pick"}}, pool, 3)
	if err != nil || len(picks) != 1 {
		t.Fatalf("repair 应由原模型产出可接受结果: err=%v picks=%+v", err, picks)
	}
	if routedCalls.Load() != 2 || origCalls.Load() != 1 {
		t.Fatalf("请求路径应为 routed(结构化拒绝+free-text 无效)→orig repair: routed=%d orig=%d",
			routedCalls.Load(), origCalls.Load())
	}
	if run.routeApplied.Applied {
		t.Fatalf("最终 accepted attempt 未路由，不得继承首轮归因: %+v", run.routeApplied)
	}
	batch := &model.RecommendationBatch{LLMConfigID: origCfg.ID, Provider: origCfg.Provider, Model: origCfg.Model}
	applyBatchRouteAttribution(batch, run)
	if batch.Provider != origCfg.Provider || batch.Model != origCfg.Model {
		t.Fatalf("批次应归因最终原模型: %+v", batch)
	}
}
