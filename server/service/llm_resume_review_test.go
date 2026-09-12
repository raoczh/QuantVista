package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestLLMClassificationPreservesErrorCause(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("read failure")} {
		if got := classifyLLMError(fmt.Errorf("请求失败: %w", cause)); !errors.Is(got, cause) {
			t.Errorf("机读错误包装不能丢失取消或存储原因：%v", got)
		}
	}
}

func TestLLMConfigSnapshotFailureRemainsRetryable(t *testing.T) {
	setupTestDB(t)
	oldDB := common.DB
	t.Cleanup(func() { common.DB = oldDB })
	injected := errors.New("temporary transaction start failure")
	common.DB = oldDB.WithContext(t.Context())
	common.DB.AddError(injected)
	_, _, err := NewLLMService().ResolveForUse(11913, 0, t.Context())
	common.DB = oldDB
	var readErr *llmConfigReadError
	if !errors.Is(err, injected) || !errors.As(err, &readErr) {
		t.Errorf("配置快照开启失败须保留可重试读取故障类型：%v", err)
	}
	recordAutoDailyFailure(11913, "2026-09-11", err)
	var count int64
	if err := common.DB.Model(&model.DailyReport{}).Where("user_id = ?", 11913).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("临时故障不能写入当天失败占位并挡住重试：count=%d err=%v", count, err)
	}
}

func TestLLMRouteCacheWaitHonorsCancellation(t *testing.T) {
	if err := llmRouteCacheMu.Lock(); err != nil {
		t.Fatal(err)
	}
	defer llmRouteCacheMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if got := lookupLLMRoute("qa", ctx); got != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("等待其他请求刷新路由时应及时取消：got=%v err=%v", got, ctx.Err())
	}
}

func TestLLMProbeAndModelListRejectIncompleteBody(t *testing.T) {
	setupTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		if r.Method == http.MethodGet {
			body = `{"data":[{"id":"test-model"}]}`
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	svc := NewLLMService()
	in := LLMConfigInput{BaseURL: srv.URL, APIKey: "local-fixture", Model: "test-model"}
	if rows, _, err := svc.FetchModels(1, in, true); err == nil {
		t.Errorf("读取中断不能报告模型列表成功：%v", rows)
	}
	if result, err := svc.TestByInput(1, in, true); err == nil && result.OK {
		t.Errorf("读取中断不能报告连接测试成功：%+v", result)
	}
}

func TestLLMModelListHandlesFullEndpointAndDuplicates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"same"},{"id":"same"},{"id":"other"}]}`))
	}))
	defer srv.Close()
	for _, suffix := range []string{"/v1/chat/completions", "/v1/responses", "/v1/models"} {
		rows, _, err := NewLLMService().FetchModels(1, LLMConfigInput{BaseURL: srv.URL + suffix, APIKey: "local-fixture"}, true)
		if err != nil || len(rows) != 2 {
			t.Errorf("完整端点仍应可拉模型且列表不重复：suffix=%s rows=%v err=%v", suffix, rows, err)
		}
	}
}

func TestLLMRouteHealthReadFailureIsNotHealthy(t *testing.T) {
	setModelRoutingFlag(t, true)
	cleanRouteTables(t)
	cfg := seedRouteAdminConfig(t, "https://route-review.example")
	route, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	const callback = "review_route_stats_failure"
	if err := common.DB.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "llm_call_logs" {
			tx.AddError(errors.New("health storage failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Row().Remove(callback) })
	if routes := loadLLMRoutes(); len(routes) != 0 {
		t.Errorf("健康读取失败不能当零样本继续路由：%d", len(routes))
	}
	if _, _, err := ListLLMRoutes(); err == nil {
		t.Error("管理端不能把统计读取故障展示为零调用")
	}
	var stored model.LLMModuleRoute
	if err := common.DB.First(&stored, route.ID).Error; err != nil || stored.AutoFallbackAt != nil {
		t.Fatalf("瞬时读取失败不能永久停用路由：fallback=%v err=%v", stored.AutoFallbackAt, err)
	}
}

func TestLLMRouteFutureCallsCannotDisableCurrentRoute(t *testing.T) {
	setupTestDB(t)
	cleanRouteTables(t)
	route := model.LLMModuleRoute{Module: "qa", ConfigID: 11121}
	seedRouteCallLogs(t, "qa", route.ConfigID, model.LLMCallStatusSuccess, 5, 100)
	for i := 0; i < 5; i++ {
		if err := common.DB.Create(&model.LLMCallLog{Module: "qa", LLMConfigID: route.ConfigID, Status: model.LLMCallStatusError, CreatedAt: time.Now().Add(time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if reason, ok := evaluateLLMRouteHealth(route, model.LLMConfig{}); !ok {
		t.Fatalf("未来失败记录不能影响当前健康判定：%s", reason)
	}
}

func TestMySQLLLMRouteCostUsesSameSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.LLMCallLog{})
	route := model.LLMModuleRoute{Module: "qa", ConfigID: 1}
	seedRouteCallLogs(t, "qa", 1, model.LLMCallStatusSuccess, 5, 200)
	seedRouteCallLogs(t, "qa", 2, model.LLMCallStatusSuccess, 5, 100)
	wrote := false
	const callback = "review_route_cost_snapshot"
	if err := db.Callback().Row().After("gorm:row").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "llm_call_logs" && !wrote && tx.Error == nil {
			wrote = true
			if err := db.Transaction(func(writeTx *gorm.DB) error {
				if err := writeTx.Model(&model.LLMCallLog{}).Where("llm_config_id = 1").Update("total_tokens", 100).Error; err != nil {
					return err
				}
				return writeTx.Model(&model.LLMCallLog{}).Where("llm_config_id = 2").Update("total_tokens", 400).Error
			}); err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Row().Remove(callback) })
	if reason, ok := evaluateLLMRouteHealth(route, model.LLMConfig{}); !wrote || ok || !strings.HasPrefix(reason, "cost_exceeded:") {
		t.Fatalf("成本分子分母不能来自不同时点：wrote=%v ok=%v reason=%s", wrote, ok, reason)
	}
}
