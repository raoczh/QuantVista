package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// 推荐异步任务化（2026-07-14）测试：processing 壳层立即返回与后台回写、幂等防重、
// 量化降级纯函数、repair 输出预算与坏输出截断、capModuleTokens 钳制。

// seedRecEnv 内存库 + 默认 LLM 配置（指向 baseURL），清场注册。
func seedRecEnv(t *testing.T, userID int64, baseURL string, maxTokens int) {
	t.Helper()
	setupTestDB(t)
	common.DB.Exec("DELETE FROM llm_configs")
	common.DB.Exec("DELETE FROM user_quota")
	common.DB.Exec("DELETE FROM recommendation_batches")
	common.DB.Exec("DELETE FROM recommendations")
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM llm_configs")
		common.DB.Exec("DELETE FROM user_quota")
		common.DB.Exec("DELETE FROM recommendation_batches")
		common.DB.Exec("DELETE FROM recommendations")
	})
	common.EncryptionKey = "unit-test-key"
	cipher, err := common.Encrypt("sk-test")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	cfg := &model.LLMConfig{UserID: userID, Name: "t", Provider: "openai", BaseURL: baseURL,
		APIKeyCipher: cipher, Model: "m", MaxTokens: maxTokens, IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建配置失败: %v", err)
	}
}

// waitBatchStatus 轮询等待批次脱离 processing（后台 goroutine 回写），超时 fail。
func waitBatchStatus(t *testing.T, batchID int64) model.RecommendationBatch {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var b model.RecommendationBatch
		if err := common.DB.First(&b, batchID).Error; err == nil && b.Status != model.RecStatusProcessing {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("批次 %d 超时未脱离 processing", batchID)
	return model.RecommendationBatch{}
}

// TestRecommendationAsyncShell 异步壳层：Generate 立即返回 processing 批次（不等待
// 建池/LLM），后台任务失败后回写 failed（us 市场榜单 ErrNotSupported 零网络快速空池，
// 端到端验证「同步段落行→后台回写」链路本身）。
func TestRecommendationAsyncShell(t *testing.T) {
	seedRecEnv(t, 31, "http://127.0.0.1:1", 0) // LLM 不会被调到（池空先失败）

	// us 市场：榜单源全部 ErrNotSupported、strategy_signal 仅 cn——零网络快速空池。
	market := NewMarketService(datasource.DefaultManager())
	svc := NewRecommendationService(market, NewWatchlistService(market), NewLLMService())
	started := time.Now()
	v, err := svc.Generate(context.Background(), 31, true, RecommendRequest{Type: model.RecTypeShortTerm, Market: "us"})
	if err != nil {
		t.Fatalf("Generate 应立即成功返回任务: %v", err)
	}
	if v.Status != model.RecStatusProcessing || v.ID == 0 {
		t.Fatalf("应返回 processing 批次: %+v", v.RecommendationBatch)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("同步段应立即返回, 耗时 %v", elapsed)
	}
	if len(v.Items) != 0 {
		t.Fatalf("processing 视图不应带条目: %d", len(v.Items))
	}
	// 元数据在 processing 行即可见（前端历史列表立刻能展示标题）。
	if v.Title == "" || v.Type != model.RecTypeShortTerm || v.Model != "m" {
		t.Fatalf("processing 行应带元数据: %+v", v.RecommendationBatch)
	}

	b := waitBatchStatus(t, v.ID)
	if b.Status != model.RecStatusFailed || !strings.Contains(b.Error, "该市场暂无行情数据源支持") {
		t.Fatalf("后台应回写 failed 与原因: %+v", b)
	}
}

// TestRecommendationProcessingReuse 幂等防重：fresh processing 批次被复用（不建新任务、
// 不调 LLM）；stale processing（超过 recProcessingStale）被惰性判 failed 并放行新任务。
func TestRecommendationProcessingReuse(t *testing.T) {
	seedRecEnv(t, 32, "http://127.0.0.1:1", 0)

	fresh := &model.RecommendationBatch{UserID: 32, Type: model.RecTypeShortTerm, Market: "cn",
		Strategy: "momentum", Title: "生成中", Status: model.RecStatusProcessing}
	if err := common.DB.Create(fresh).Error; err != nil {
		t.Fatalf("造 processing 批次失败: %v", err)
	}

	market := NewMarketService(datasource.DefaultManager())
	svc := NewRecommendationService(market, NewWatchlistService(market), NewLLMService())
	v, err := svc.Generate(context.Background(), 32, true, RecommendRequest{Type: model.RecTypeShortTerm, Market: "cn"})
	if err != nil {
		t.Fatalf("复用路径不应报错: %v", err)
	}
	if v.ID != fresh.ID || v.Status != model.RecStatusProcessing {
		t.Fatalf("应复用已有 processing 批次 %d: got %d(%s)", fresh.ID, v.ID, v.Status)
	}
	var cnt int64
	common.DB.Model(&model.RecommendationBatch{}).Where("user_id = ?", 32).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("不应创建新批次: %d", cnt)
	}

	// stale：手写 updated_at 到 16 分钟前 → reuse 判 failed 放行。
	common.DB.Model(&model.RecommendationBatch{}).Where("id = ?", fresh.ID).
		Update("updated_at", time.Now().Add(-16*time.Minute))
	if v := svc.reuseProcessingBatch(32); v != nil {
		t.Fatalf("stale processing 不应被复用: %+v", v)
	}
	var b model.RecommendationBatch
	common.DB.First(&b, fresh.ID)
	if b.Status != model.RecStatusFailed || !strings.Contains(b.Error, "任务中断") {
		t.Fatalf("stale processing 应被判 failed: %+v", b)
	}
}

// TestQuantFallbackPicks 量化降级纯函数：短线 ATR 规则价位满足四价关系（normalizePick
// 不清零）、action=watch、降级标记与免责在场、count 截断、长线不出计划价出 thesis。
func TestQuantFallbackPicks(t *testing.T) {
	cands := []candidate{
		{Symbol: "600001", Market: "cn", Name: "甲", Price: 10.00, ChangePct: 2.1, TurnoverRate: 5.5,
			Rank: 1, Score: 82.5, Bonus: []string{"放量突破（+4）", "水上金叉（+4）", "第三条加分"},
			Factors: &candFactors{ATRPct: 3.2}},
		{Symbol: "600002", Market: "cn", Name: "乙", Price: 5.55, ChangePct: -1.0, Rank: 2, Score: 75},
		{Symbol: "600003", Market: "cn", Name: "丙", Price: 20.00, Rank: 3, Score: 70,
			Factors: &candFactors{ATRPct: 9.9}}, // ATR% 超上限应被钳到 6
		{Symbol: "600004", Market: "cn", Name: "丁", Price: 8.00, Rank: 4, Score: 65},
	}
	picks := buildQuantFallbackPicks(model.RecTypeShortTerm, cands, 3)
	if len(picks) != 3 {
		t.Fatalf("应按 count 截断为 3: %d", len(picks))
	}
	for _, p := range picks {
		if p.Action != model.RecActionWatch {
			t.Fatalf("降级条目应一律 watch: %+v", p)
		}
		if p.DegradedSource != "quant_fallback" {
			t.Fatalf("应带降级标记: %+v", p)
		}
		// ATR 规则价位必须过 normalizePick 的四价关系校验（清零即 bug）。
		if p.BuyZoneLow <= 0 || p.StopLoss <= 0 || p.TakeProfit <= 0 {
			t.Fatalf("短线降级价位被清零（规则不自洽）: %+v", p)
		}
		if !(p.TakeProfit > p.BuyZoneHigh && p.BuyZoneHigh > p.BuyZoneLow && p.BuyZoneLow > p.StopLoss) {
			t.Fatalf("四价关系不成立: %+v", p)
		}
		if !strings.Contains(p.Disclaimer, "量化规则") {
			t.Fatalf("免责应声明规则生成: %q", p.Disclaimer)
		}
	}
	// 手工验算首条（price=10, atrp=3.2）：zone=[9.84,10.16]、止损 9.36、止盈 10.96。
	p0 := picks[0]
	if p0.BuyZoneLow != 9.84 || p0.BuyZoneHigh != 10.16 || p0.StopLoss != 9.36 || p0.TakeProfit != 10.96 {
		t.Fatalf("ATR 规则价位不符: %+v", p0)
	}
	if len(p0.Reason) != 3 { // 排名说明 + Bonus 前 2 条（第三条截断）
		t.Fatalf("Reason 应为排名+至多 2 条加分: %v", p0.Reason)
	}
	// ATR% 钳制上限 6：止损距离 12%。
	p2 := picks[2]
	if p2.StopLoss != round2(20.00*(1-0.12)) {
		t.Fatalf("ATR%% 应钳到 6: %+v", p2)
	}

	longs := buildQuantFallbackPicks(model.RecTypeLongTerm, cands[:1], 3)
	if len(longs) != 1 || longs[0].TakeProfit != 0 || longs[0].Thesis == "" {
		t.Fatalf("长线降级应无计划价、有 thesis 说明: %+v", longs[0])
	}
}

// TestQuantFallbackEligible 降级触发判定：超时/网络/5xx/流中断降级，鉴权/路径/配额不降级。
func TestQuantFallbackEligible(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"请求失败: context deadline exceeded", true},
		{"LLM 返回 HTTP 504（上游服务异常）：gateway timeout", true},
		{"流式响应中断: unexpected EOF", true},
		{"LLM 返回空内容", true},
		{"LLM 返回 HTTP 500（上游服务异常）：boom", true},
		{"LLM 返回 HTTP 401（API Key 无效或无权限）：bad key", false},
		{"LLM 返回 HTTP 404（接口路径或模型名不存在，请检查 Base URL 与模型）：no", false},
		{"Base URL 非法（仅支持 http/https）", false},
		{"AI 次数配额已用尽，请联系管理员调整额度", false},
	}
	for _, c := range cases {
		if got := quantFallbackEligible(newErr(c.msg)); got != c.want {
			t.Errorf("%q: 期望 %v 得到 %v", c.msg, c.want, got)
		}
	}
	if quantFallbackEligible(nil) {
		t.Error("nil 错误不应降级")
	}
}

func newErr(msg string) error { return &strErr{msg} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

// TestCallWithRepairBudgetAndTruncate 主调用输出预算钳制（用户 8000 → 模块 2500）、
// repair 只跑 1 轮、坏输出回灌截断（≤600 字）。callWithRepair 直接吃 cfg 参数不走 DB。
func TestCallWithRepairBudgetAndTruncate(t *testing.T) {
	setupTestDB(t)

	var bodies []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		content := strings.Repeat("坏输出", 500) // 首轮：1500 字非 JSON，触发 repair 与截断
		if calls >= 2 {
			content = `{\"picks\":[{\"symbol\":\"600001\",\"action\":\"buy\",\"confidence\":70,\"reason\":[\"r\"],\"risks\":[\"k\"],\"evidence\":[\"e\"]}],\"rejected\":[]}`
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	svc := &RecommendationService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 8000}
	pool := map[string]candidate{"600001": {Symbol: "600001", Name: "甲", Price: 10}}
	picks, _, usage, _, err := svc.callWithRepair(context.Background(), 33, newLLMRun("", "", "recommendation", "recommendation.v1", ""), cfg, "sk", true,
		[]chatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}, pool, 3)
	if err != nil {
		t.Fatalf("repair 后应成功: %v", err)
	}
	if calls != 2 || len(picks) != 1 || usage.TotalTokens != 30 {
		t.Fatalf("应恰好 2 轮（主调+1 次 repair）: calls=%d picks=%d tokens=%d", calls, len(picks), usage.TotalTokens)
	}

	// 输出预算：用户配 8000，实际请求应被钳到模块上限 2500。
	var payload struct {
		MaxTokens int `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	if payload.MaxTokens != recMaxTokensCap {
		t.Fatalf("max_tokens 应钳到 %d: %d", recMaxTokensCap, payload.MaxTokens)
	}
	// repair 轮：坏输出回灌须截断（完整 1500 字回灌会拖慢下一轮）。
	if err := json.Unmarshal([]byte(bodies[1]), &payload); err != nil {
		t.Fatalf("解析 repair 请求体失败: %v", err)
	}
	var assistantLen int
	repairHintSeen := false
	for _, m := range payload.Messages {
		if m.Role == "assistant" {
			assistantLen = len([]rune(m.Content))
		}
		if m.Role == "user" && strings.Contains(m.Content, "上一条输出不合格") {
			repairHintSeen = true
		}
	}
	if assistantLen == 0 || assistantLen > 600 {
		t.Fatalf("坏输出回灌应截断到 ≤600 字: %d", assistantLen)
	}
	if !repairHintSeen {
		t.Fatal("repair 提示应在场")
	}
}

// TestCapModuleTokens 模块级输出预算钳制表驱动。
func TestCapModuleTokens(t *testing.T) {
	cases := []struct {
		user, cap, want int
	}{
		{0, 2500, 2500},    // 用户未配 → 模块上限
		{8000, 2500, 2500}, // 用户过大 → 钳到模块上限
		{1200, 2500, 1200}, // 用户更小 → 尊重用户
		{1200, 0, 1200},    // 模块无上限 → 原样
	}
	for _, c := range cases {
		if got := capModuleTokens(c.user, c.cap); got != c.want {
			t.Errorf("capModuleTokens(%d,%d)=%d 期望 %d", c.user, c.cap, got, c.want)
		}
	}
}
