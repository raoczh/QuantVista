package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// TestLLMRunIDs ID 形态与唯一性：trace/run 前缀区分、128bit hex、连续生成不重复。
func TestLLMRunIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tr, rn := newLLMTraceID(), newLLMRunID()
		if !strings.HasPrefix(tr, "t") || !strings.HasPrefix(rn, "r") || len(tr) != 33 || len(rn) != 33 {
			t.Fatalf("ID 形态不符: trace=%q run=%q", tr, rn)
		}
		if seen[tr] || seen[rn] {
			t.Fatalf("ID 重复: %q %q", tr, rn)
		}
		seen[tr], seen[rn] = true, true
	}
}

// TestLLMContentHash hash 稳定性与前缀；空串不产生假 hash。
func TestLLMContentHash(t *testing.T) {
	h1 := llmContentHash("abc")
	h2 := llmContentHash("abc")
	if h1 != h2 || !strings.HasPrefix(h1, "sha256:") || len(h1) != len("sha256:")+64 {
		t.Fatalf("hash 不稳定或形态不符: %q vs %q", h1, h2)
	}
	if llmContentHash("abcd") == h1 {
		t.Fatal("不同输入 hash 应不同")
	}
	if llmContentHash("") != "" {
		t.Fatal("空输入不应产生 hash")
	}
	// prompt hash：初始消息决定，与 repair 轮追加消息无关（调用方只在初始消息上算一次）。
	msgs := []chatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}
	if llmPromptHash(msgs) == "" || llmPromptHash(msgs) != llmPromptHash(msgs) {
		t.Fatal("prompt hash 应稳定非空")
	}
}

// TestNormalizeLLMFinishState 规范化终态枚举表驱动（成功/错误两侧 + 文案还原截断语义）。
func TestNormalizeLLMFinishState(t *testing.T) {
	cases := []struct {
		raw  string
		err  error
		want string
	}{
		// 成功侧。
		{"stop", nil, "stop"},
		{"Tool_Calls", nil, "tool_calls"},
		{"completed", nil, "completed"},
		{"", nil, ""},                    // 兼容网关未报告
		{"length", nil, "length"},        // 关契约旧路径异常终态仍算成功：如实记录
		{"whatever_new", nil, "unknown"}, // 未知枚举不粉饰
		// 错误侧：按拒答码归类。
		{"", refusalErr(RefusalLLMContentFiltered, "拦截"), "content_filter"},
		{"length", refusalErr(RefusalLLMResponseIncomplete, "截断"), "length"},
		{"", refusalErr(RefusalLLMResponseIncomplete, "输出因 token 上限被截断（finish_reason=length）"), "length"},
		{"", refusalErr(RefusalLLMResponseIncomplete, "Responses 响应未完成（status=incomplete/max_output_tokens）"), "max_tokens"},
		{"", refusalErr(RefusalLLMResponseIncomplete, "流式响应结束但未收到终止标记（eof_without_marker）"), "eof_without_marker"},
		{"", refusalErr(RefusalLLMResponseIncomplete, "LLM 返回空内容"), "error"},
		{"failed", refusalErr(RefusalLLMCallFailed, "失败"), "failed"},
		{"", refusalErr(RefusalLLMCallFailed, "HTTP 401"), "error"},
		{"", errors.New("裸网络错误"), "error"},
	}
	for i, c := range cases {
		if got := normalizeLLMFinishState(c.raw, c.err); got != c.want {
			t.Errorf("#%d raw=%q err=%v: got %q want %q", i, c.raw, c.err, got, c.want)
		}
	}
}

// TestLLMRunManifestInvariant attempt_count = 1 + repair_count 不变式与 marshal 过滤。
func TestLLMRunManifestInvariant(t *testing.T) {
	run := newLLMRun("t-x", "r-parent", "analysis", "analysis.v1", "p17")
	cfg := &model.LLMConfig{ID: 3, Provider: "openai", Model: "m", EndpointType: model.LLMEndpointChat}
	_ = run.chatMeta(1, cfg, 1)
	_ = run.chatMeta(1, cfg, 2)
	_ = run.chatMeta(1, cfg, 3)
	m := run.manifest(cfg, true)
	if m.AttemptCount != 3 || m.RepairCount != 2 || m.AttemptCount != 1+m.RepairCount {
		t.Fatalf("attempt 不变式破坏: %+v", m)
	}
	if m.ParentRunID != "r-parent" || m.Module != "analysis" || m.StructuredMethod != model.LLMStructuredJSONObject ||
		m.Provider != "openai" || m.LLMConfigID != 3 {
		t.Fatalf("manifest 字段不符: %+v", m)
	}
	// 未发起调用的 run 不进 manifest 数组；全部未发起返回空串。
	idle := newLLMRun("t-x", "", "trade_plan", "trade_plan.v1", "p17")
	if s := marshalLLMRunManifests(cfg, runEntry(idle, true), runEntry(nil, true)); s != "" {
		t.Fatalf("零调用 run 不应输出 manifest: %q", s)
	}
	s := marshalLLMRunManifests(cfg, runEntry(run, true), runEntry(idle, true))
	if !strings.Contains(s, `"attempt_count":3`) || strings.Contains(s, "trade_plan") {
		t.Fatalf("manifest 序列化不符: %s", s)
	}
}

// TestLLMRunWiring_AnalysisTraceEndToEnd 分析链端到端：首轮坏 JSON 触发 repair、第二轮成功——
// 记录落 trace_id + manifest（attempt_count=2/repair_count=1）；两条审计行共享 trace/run、
// attempt 1→2 递增、repair 标记只在第二轮、schema/prompt hash 齐全。
func TestLLMRunWiring_AnalysisTraceEndToEnd(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		content := "not-json"
		if calls >= 2 {
			content = `{\"rating\":\"neutral\",\"confidence\":50,\"summary\":\"市场中性\",\"highlights\":[],\"risks\":[],\"opportunities\":[],\"suggestions\":[],\"anti_thesis\":[\"a\"],\"kill_switches\":[\"k\"],\"unknowns\":[\"u\"],\"disclaimer\":\"d\"}`
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	const userID int64 = 75
	seedReportEnv(t, userID, srv.URL)
	resetLLMCallLogs(t)
	common.DB.Exec("DELETE FROM analysis_records")
	t.Cleanup(func() { common.DB.Exec("DELETE FROM analysis_records") })

	market := NewMarketService(datasource.NewManagerWithAdapters(refusalTestAdapter{}))
	analysisSvc := NewAnalysisService(market, nil, nil, NewLLMService(), nil)
	view, err := analysisSvc.Analyze(context.Background(), userID, true, AnalyzeRequest{
		Module: model.AnalysisModuleMarket,
		Market: "cn",
	})
	if err != nil {
		t.Fatalf("分析应成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("应恰好 2 轮（主调+repair）: %d", calls)
	}
	if view.TraceID == "" || !strings.HasPrefix(view.TraceID, "t") {
		t.Fatalf("记录应带 trace_id: %q", view.TraceID)
	}
	var rec model.AnalysisRecord
	if err := common.DB.Order("id desc").First(&rec).Error; err != nil {
		t.Fatal(err)
	}
	if rec.TraceID != view.TraceID {
		t.Fatalf("trace 落库不一致: %q vs %q", rec.TraceID, view.TraceID)
	}
	if !strings.Contains(rec.LlmRunJSON, `"attempt_count":2`) || !strings.Contains(rec.LlmRunJSON, `"repair_count":1`) ||
		!strings.Contains(rec.LlmRunJSON, `"module":"analysis"`) || !strings.Contains(rec.LlmRunJSON, rec.TraceID) {
		t.Fatalf("llm_run_json manifest 不符: %s", rec.LlmRunJSON)
	}

	var logs []model.LLMCallLog
	if err := common.DB.Where("trace_id = ?", rec.TraceID).Order("id asc").Find(&logs).Error; err != nil || len(logs) != 2 {
		t.Fatalf("按 trace 应查到 2 条审计: n=%d err=%v", len(logs), err)
	}
	if logs[0].RunID == "" || logs[0].RunID != logs[1].RunID {
		t.Fatalf("主调与 repair 应同 run: %q vs %q", logs[0].RunID, logs[1].RunID)
	}
	if logs[0].Attempt != 1 || logs[0].Repair || logs[1].Attempt != 2 || !logs[1].Repair {
		t.Fatalf("attempt/repair 标记不符: %+v %+v", logs[0], logs[1])
	}
	for _, lg := range logs {
		if lg.SchemaVersion != "analysis.v1" || lg.StructuredMethod != model.LLMStructuredJSONObject ||
			!strings.HasPrefix(lg.PromptHash, "sha256:") || !strings.HasPrefix(lg.DataHash, "sha256:") ||
			lg.FinishState != "stop" {
			t.Fatalf("审计元数据不符: %+v", lg)
		}
	}
	if logs[0].PromptHash != logs[1].PromptHash {
		t.Fatalf("同 run 的 prompt hash 应一致（标识初始 prompt）: %q vs %q", logs[0].PromptHash, logs[1].PromptHash)
	}
	// 管理端按 trace 追溯：run_id 也可作为筛选值。
	admin := &AdminService{}
	if list, err := admin.ListLLMCalls(0, "", "", rec.TraceID, 1, 10); err != nil || list.Total != 2 {
		t.Fatalf("trace 筛选应命中 2 条: err=%v total=%d", err, list.Total)
	}
	if list, _ := admin.ListLLMCalls(0, "", "", logs[0].RunID, 1, 10); list.Total != 2 {
		t.Fatalf("run 筛选应命中 2 条: %d", list.Total)
	}
}

// TestLLMCallLog_JSONModeFallbackStructuredMethod 端点不支持 response_format 回落后，
// 审计必须记录实际生效形态 free_text（而非入口意图 json_object）。
func TestLLMCallLog_JSONModeFallbackStructuredMethod(t *testing.T) {
	setupTestDB(t)
	resetLLMCallLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 1<<16)
		n, _ := r.Body.Read(body)
		if strings.Contains(string(body[:n]), "response_format") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format is not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"total_tokens":3,"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	if _, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", JSONMode: true,
		Messages:     []chatMessage{{Role: "user", Content: "x"}},
		AllowPrivate: true,
		Meta:         chatMeta{Module: "analysis", Attempt: 1},
	}); err != nil {
		t.Fatalf("回落后应成功: %v", err)
	}
	var row model.LLMCallLog
	if err := common.DB.Order("id desc").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.StructuredMethod != model.LLMStructuredFreeText {
		t.Fatalf("JSON mode 回落应记 free_text: %+v", row)
	}
}

// TestLLMCallLog_FinishStateOnError 错误路径：finish_reason=length 门禁拒收后，
// 审计行 finish_state 应还原为 length（从错误文案），而非笼统 error。
func TestLLMCallLog_FinishStateOnError(t *testing.T) {
	setupTestDB(t)
	resetLLMCallLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"半截"},"finish_reason":"length"}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()

	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "x"}},
		AllowPrivate: true,
		Meta:         chatMeta{Module: "analysis", Attempt: 1},
	})
	if RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
		t.Fatalf("length 应被门禁拒收: %v", err)
	}
	var row model.LLMCallLog
	if err := common.DB.Order("id desc").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != model.LLMCallStatusError || row.FinishState != "length" {
		t.Fatalf("finish_state 应还原 length: %+v", row)
	}
}
