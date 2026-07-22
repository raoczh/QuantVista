package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// P0-5 capability matrix + P0-9 模块化输出预算测试：
// 能力声明合并/TTL、声明化路由端到端（含 flag 关反例）、provider smoke 探测三态与
// 探针纯净性、预算表表驱动、模块预算截断 fail-closed、repair 回灌截断。

// setCapRoutingFlag 切换声明化路由开关（写 options 表 + 内存变量），退场恢复默认开。
func setCapRoutingFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMCapabilityRouting(v); err != nil {
		t.Fatalf("切换能力路由开关失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMCapabilityRouting(true) })
}

// TestCapabilitiesMerge 内置声明与运行时观察的合并语义：openai 内置 supported、
// 未知 provider 缺省 unknown、观察覆盖声明、TTL 过期观察失效回落声明。
func TestCapabilitiesMerge(t *testing.T) {
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)

	target := llmCapabilityTarget(0, "gw", "https://x.example.com", "m1", "")
	if got := capabilitiesFor("openai", target).JSONObject; got != capSupported {
		t.Fatalf("openai 内置声明应 supported, got %s", got)
	}
	if got := capabilitiesFor("my-gateway", target).JSONObject; got != capUnknown {
		t.Fatalf("未登记 provider 缺省应 unknown, got %s", got)
	}
	// 观察覆盖声明（真实响应比静态假设可信）。
	observeLLMCapability(target, capJSONObject, capUnsupported, "测试观察")
	if got := capabilitiesFor("openai", target).JSONObject; got != capUnsupported {
		t.Fatalf("观察应覆盖内置声明, got %s", got)
	}
	// TTL 过期：观察失效，回落声明值。
	llmCapabilityStore.Store(target+"#"+string(capJSONObject),
		llmCapObservation{State: capUnsupported, ObservedAt: time.Now().Add(-llmCapObservationTTL - time.Minute)})
	if _, ok := lookupLLMCapability(target, capJSONObject); ok {
		t.Fatal("过期观察不应命中")
	}
	if got := capabilitiesFor("openai", target).JSONObject; got != capSupported {
		t.Fatalf("过期后应回落内置声明, got %s", got)
	}
	// configID 参与 key：同 URL 不同配置身份互不污染。
	if llmCapabilityTarget(7, "gw", "https://x.example.com", "m1", "") == target {
		t.Fatal("配置身份应参与观察 key")
	}
	// P0-5 修复批：BaseURL 与 provider 都参与 key——配置只换上游（改 BaseURL 或 provider）
	// 而保持 configID/model/endpoint 时，新上游不得继承旧上游的观察。
	if llmCapabilityTarget(7, "gw", "https://x.example.com", "m1", "") ==
		llmCapabilityTarget(7, "gw", "https://y.example.com", "m1", "") {
		t.Fatal("BaseURL 应参与观察 key（换上游旧观察须失效）")
	}
	if llmCapabilityTarget(7, "gw", "https://x.example.com", "m1", "") ==
		llmCapabilityTarget(7, "other", "https://x.example.com", "m1", "") {
		t.Fatal("provider 应参与观察 key")
	}
	// 规范化：BaseURL 尾斜杠/大小写不产生第二个 key。
	if llmCapabilityTarget(7, "GW", "https://X.example.com/", "m1", "") !=
		llmCapabilityTarget(7, "gw", "https://x.example.com", "m1", "") {
		t.Fatal("provider/BaseURL 应规范化后参与 key")
	}
}

// capRouteFakeUpstream 假上游：带 response_format 的请求回 4xx「不支持」，
// 其余（含 stream 整包形态）回合法 chat JSON。记录每个请求体。
func capRouteFakeUpstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "response_format") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format is not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":1}"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	return srv, &bodies
}

// TestCapabilityRoutingDeclarative 声明化路由端到端：首次调用隐式回落并落观察；
// 第二次同目标调用不再发注定失败的结构化请求（直接 free_text），审计记录实际生效形态。
func TestCapabilityRoutingDeclarative(t *testing.T) {
	setCapRoutingFlag(t, true)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	srv, bodies := capRouteFakeUpstream(t)
	defer srv.Close()

	params := chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", JSONMode: true, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     chatMeta{CallerUserID: 1, Module: "captest"},
	}
	// 第一次：结构化请求被 4xx 拒 → 隐式回落成功，观察落库。
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("首次调用应回落成功: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("首次调用应 2 个请求（结构化被拒+回落）, got %d", len(*bodies))
	}
	obs, ok := lookupLLMCapability(capabilityTargetOf(params), capJSONObject)
	if !ok || obs.State != capUnsupported {
		t.Fatalf("回落点应写入 unsupported 观察: %+v ok=%v", obs, ok)
	}
	// 第二次：声明化路由直接 free_text——只发 1 个请求且不含 response_format。
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("路由后调用应成功: %v", err)
	}
	if len(*bodies) != 3 {
		t.Fatalf("路由后应只发 1 个请求（累计 3）, got %d", len(*bodies))
	}
	if strings.Contains((*bodies)[2], "response_format") {
		t.Fatal("声明化路由后不得再发 response_format")
	}
	// 审计：两次调用的 structured_method 都应记实际生效的 free_text。
	var logs []model.LLMCallLog
	if err := common.DB.Where("module = ?", "captest").Order("id asc").Find(&logs).Error; err != nil || len(logs) != 2 {
		t.Fatalf("审计行数不符: %v n=%d", err, len(logs))
	}
	for i, row := range logs {
		if row.StructuredMethod != model.LLMStructuredFreeText {
			t.Fatalf("第 %d 行 structured_method 应 free_text, got %s", i, row.StructuredMethod)
		}
	}
}

// TestCapabilityRoutingFlagOff 反例：flag 关闭回退隐式回落旧路径——即使已有 unsupported
// 观察，每次调用仍先发结构化请求在线试错（观察记录本身不受 flag 控制）。
func TestCapabilityRoutingFlagOff(t *testing.T) {
	setCapRoutingFlag(t, false)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	srv, bodies := capRouteFakeUpstream(t)
	defer srv.Close()

	params := chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", JSONMode: true, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     chatMeta{CallerUserID: 1, Module: "captest_off"},
	}
	for i := 0; i < 2; i++ {
		if _, err := chatCompletion(context.Background(), params); err != nil {
			t.Fatalf("第 %d 次调用应回落成功: %v", i+1, err)
		}
	}
	if len(*bodies) != 4 {
		t.Fatalf("flag 关闭时每次调用都应在线试错（2 次×2 请求=4）, got %d", len(*bodies))
	}
	if obs, ok := lookupLLMCapability(capabilityTargetOf(params), capJSONObject); !ok || obs.State != capUnsupported {
		t.Fatalf("flag 关闭不影响观察记录: %+v ok=%v", obs, ok)
	}
}

// TestResponsesFallbackObserved responses 端点的回落点同样写观察（四回落点对齐）。
func TestResponsesFallbackObserved(t *testing.T) {
	setCapRoutingFlag(t, true)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "format") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"text.format is not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()

	params := chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses,
		JSONMode: true, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     chatMeta{CallerUserID: 1, Module: "captest_resp"},
	}
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("responses 回落应成功: %v", err)
	}
	if obs, ok := lookupLLMCapability(capabilityTargetOf(params), capJSONObject); !ok || obs.State != capUnsupported {
		t.Fatalf("responses 回落点应写观察: %+v ok=%v", obs, ok)
	}
	// 路由后再调：请求数只 +1。
	before := n
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("路由后调用应成功: %v", err)
	}
	if n != before+1 {
		t.Fatalf("responses 路由后应只发 1 个请求, got +%d", n-before)
	}
}

// TestJSONModeSmokeProbe provider smoke 三态：支持/不支持/非结论性失败，
// 观察写入与探针请求纯净性（无业务 prompt）。
func TestJSONModeSmokeProbe(t *testing.T) {
	setupTestDB(t)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	svc := &LLMService{}

	t.Run("supported", func(t *testing.T) {
		var probeMessages []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var payload struct {
				Messages []chatMessage `json:"messages"`
			}
			_ = json.Unmarshal(b, &payload)
			for _, m := range payload.Messages {
				probeMessages = append(probeMessages, m.Content)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
		}))
		defer srv.Close()

		r := svc.testOpenAICompatibleForUser(1, 0, "gw", "", srv.URL, "k", "m", true)
		if !r.OK || !strings.Contains(r.Message, "JSON 结构化：支持") {
			t.Fatalf("应连接成功且 smoke 判支持: %+v", r)
		}
		target := llmCapabilityTarget(0, "gw", srv.URL, "m", "")
		if obs, ok := lookupLLMCapability(target, capJSONObject); !ok || obs.State != capSupported {
			t.Fatalf("smoke 应写 supported 观察: %+v ok=%v", obs, ok)
		}
		if obs, ok := lookupLLMCapability(target, capEndpointChat); !ok || obs.State != capSupported {
			t.Fatalf("连通探测应写端点观察: %+v ok=%v", obs, ok)
		}
		// 探针纯净性：全部请求消息只有固定探测文本（hi / JSON 探测句），不含业务 prompt。
		for _, m := range probeMessages {
			if m != "hi" && !strings.Contains(m, `{"ok":true}`) {
				t.Fatalf("探针请求不得携带业务 prompt: %q", m)
			}
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "response_format") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"response_format not supported"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
		}))
		defer srv.Close()

		r := svc.testOpenAICompatibleForUser(1, 0, "gw", "", srv.URL, "k", "m", true)
		if !r.OK || !strings.Contains(r.Message, "JSON 结构化：不支持") {
			t.Fatalf("基础连通成功 + smoke 判不支持: %+v", r)
		}
		if obs, ok := lookupLLMCapability(llmCapabilityTarget(0, "gw", srv.URL, "m", ""), capJSONObject); !ok || obs.State != capUnsupported {
			t.Fatalf("smoke 应写 unsupported 观察: %+v ok=%v", obs, ok)
		}
	})

	t.Run("inconclusive_no_observation", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "response_format") {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
		}))
		defer srv.Close()

		r := svc.testOpenAICompatibleForUser(1, 0, "gw", "", srv.URL, "k", "m", true)
		if !r.OK || !strings.Contains(r.Message, "未能确认") {
			t.Fatalf("5xx 属非结论性失败: %+v", r)
		}
		if _, ok := lookupLLMCapability(llmCapabilityTarget(0, "gw", srv.URL, "m", ""), capJSONObject); ok {
			t.Fatal("非结论性失败不得写观察")
		}
	})
}

// TestModuleBudgetTable P0-9 预算表表驱动：token 钳制取小、repair 次数默认 1 与显式覆盖、
// 全部业务模块已登记。
func TestModuleBudgetTable(t *testing.T) {
	cases := []struct {
		module  string
		userMax int
		want    int
	}{
		{"recommendation", 0, 2500},      // 用户未配置 → 模块预算
		{"recommendation", 100000, 2500}, // 用户过大 → 钳到模块预算
		{"recommendation", 800, 800},     // 用户更小 → 用户优先
		{"qa", 100000, 8000},
		{"compare", 0, 2000},
		{"unregistered_module", 1234, 1234}, // 未登记模块不钳（接线遗漏由覆盖测试兜底）
	}
	for _, c := range cases {
		if got := moduleTokenCap(c.module, c.userMax); got != c.want {
			t.Errorf("moduleTokenCap(%s,%d)=%d, want %d", c.module, c.userMax, got, c.want)
		}
	}
	if moduleRepairAttempts("analysis") != 2 || moduleRepairAttempts("trade_plan") != 2 {
		t.Fatal("analysis/trade_plan 显式覆盖 2 次不得丢失")
	}
	if moduleRepairAttempts("recommendation") != 1 || moduleRepairAttempts("screener_parse") != 1 {
		t.Fatal("registered 模块 repair 应为 1")
	}
	if moduleRepairAttempts("qa") != 0 || moduleRepairAttempts("compare") != 0 {
		t.Fatal("自由文本模块无结构化 repair")
	}
	if moduleRepairAttempts("unregistered_module") != llmDefaultRepairAttempts {
		t.Fatal("未登记模块 repair 应回默认 1")
	}
	// 全部业务调用模块必须登记（新增 chatCompletion* 模块先登记预算的纪律锚点）。
	for _, m := range []string{"analysis", "trade_plan", "analysis_review", "recommendation",
		"rec_review", "rec_bear", "daily_report", "qa", "compare", "news", "screener_parse"} {
		b, ok := llmModuleBudgets[m]
		if !ok {
			t.Errorf("业务模块 %s 未登记预算", m)
			continue
		}
		if b.MaxTokens <= 0 {
			t.Errorf("业务模块 %s 预算不得为 0（回退用户全局值）", m)
		}
	}
}

// TestModuleCapTruncationRejected 预算超限不得静默当成功：模块钳制后的请求携带预算
// max_tokens，上游 finish_reason=length 截断被完整性门禁拒收（llm_response_incomplete）。
func TestModuleCapTruncationRejected(t *testing.T) {
	setContractFlag(t, true)
	var gotMaxTokens float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		if v, ok := payload["max_tokens"].(float64); ok {
			gotMaxTokens = v
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"半截"},"finish_reason":"length"}],"usage":{"total_tokens":9}}`))
	}))
	defer srv.Close()

	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", AllowPrivate: true,
		MaxTokens: moduleTokenCap("compare", 999999),
		Messages:  []chatMessage{{Role: "user", Content: "hi"}},
	})
	if gotMaxTokens != float64(moduleBudget("compare").MaxTokens) {
		t.Fatalf("请求应携带模块预算 max_tokens=%d, got %v", moduleBudget("compare").MaxTokens, gotMaxTokens)
	}
	if err == nil || RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
		t.Fatalf("预算截断必须拒收: %v (code=%q)", err, RefusalCodeOf(err))
	}
}

// TestCallWithRepairFeedTruncated repair 回灌坏输出按模块字符上限截断（P0-9）。
func TestCallWithRepairFeedTruncated(t *testing.T) {
	setContractFlag(t, true)
	longBad := strings.Repeat("废", 3000)
	var secondReqAssistant string
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 2 {
			b, _ := io.ReadAll(r.Body)
			var payload struct {
				Messages []chatMessage `json:"messages"`
			}
			_ = json.Unmarshal(b, &payload)
			for _, m := range payload.Messages {
				if m.Role == "assistant" {
					secondReqAssistant = m.Content
				}
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + longBad + `"},"finish_reason":"stop"}],"usage":{"total_tokens":9}}`))
	}))
	defer srv.Close()

	svc := &AnalysisService{}
	_, _, _, err := svc.callWithRepair(
		context.Background(), 1, newLLMRun("", "", "analysis", "analysis.v1", ""),
		&model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 64},
		"k", true,
		[]chatMessage{{Role: "user", Content: "x"}},
		func(string) error { return errors.New("恒失败") },
		analysisRepairHint,
	)
	if err != nil {
		t.Fatalf("打满 repair 应降级 nil 错误: %v", err)
	}
	wantMax := moduleBudget("analysis").RepairFeedChars
	if secondReqAssistant == "" {
		t.Fatal("repair 轮应回灌坏输出")
	}
	if got := len([]rune(secondReqAssistant)); got > wantMax+1 { // truncateRunes 带省略号容差
		t.Fatalf("回灌应截断到 ≤%d rune, got %d", wantMax, got)
	}
}

// TestLooksLikeUnsupportedParamRecognition P0-5 修复批：参数识别收紧的表驱动正反例——
// JSON mode 判定必须明确指向结构化字段；「model/stream/temperature not supported」是反例；
// temperature/max_tokens 各自只认指向自身字段且带「不被接受」措辞的错误，值超限不算。
func TestLooksLikeUnsupportedParamRecognition(t *testing.T) {
	cases := []struct {
		name                   string
		body                   string
		json, temperature, tok bool
	}{
		{"response_format 不支持", `{"error":{"message":"response_format is not supported"}}`, true, false, false},
		{"text.format 非法", `{"error":{"message":"Invalid parameter: text.format"}}`, true, false, false},
		{"json_object 拒绝", `{"error":{"message":"json_object unsupported by this model"}}`, true, false, false},
		{"model 不支持（反例）", `{"error":{"message":"model not supported"}}`, false, false, false},
		{"stream 不支持（反例）", `{"error":{"message":"stream not supported"}}`, false, false, false},
		{"temperature 不支持", `{"error":{"message":"temperature is not supported with this model"}}`, false, true, false},
		{"temperature 值不受支持", `{"error":{"message":"Unsupported value: 'temperature' does not support 0.2 with this model."}}`, false, true, false},
		{"max_tokens 参数不支持", `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`, false, false, true},
		{"max_tokens 值超限（反例）", `{"error":{"message":"max_tokens is too large: 999999. This model supports at most 4096."}}`, false, false, false},
		{"泛 400 无字段指向（反例）", `{"error":{"message":"bad request"}}`, false, false, false},
	}
	for _, c := range cases {
		if got := looksLikeUnsupportedJSONMode(400, []byte(c.body)); got != c.json {
			t.Errorf("%s: json=%v want %v", c.name, got, c.json)
		}
		if got := looksLikeUnsupportedTemperature(400, []byte(c.body)); got != c.temperature {
			t.Errorf("%s: temperature=%v want %v", c.name, got, c.temperature)
		}
		if got := looksLikeUnsupportedMaxTokens(400, []byte(c.body)); got != c.tok {
			t.Errorf("%s: max_tokens=%v want %v", c.name, got, c.tok)
		}
	}
	// 5xx 一律不判参数不支持。
	if looksLikeUnsupportedJSONMode(500, []byte("response_format")) ||
		looksLikeUnsupportedTemperature(500, []byte("temperature not supported")) {
		t.Fatal("5xx 不得判参数不支持")
	}
}

// TestFallbackFailureNoObservation P0-5 修复批：fallback 失败不得提交能力观察——
// 首个 4xx 文案虽含 response_format 字样，但去参重试仍 4xx（错误另有原因），
// 观察存储必须保持干净，后续能力状态不被单次误判污染。
func TestFallbackFailureNoObservation(t *testing.T) {
	setCapRoutingFlag(t, true)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "response_format") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format is not supported"}}`))
			return
		}
		// 去参后仍 4xx：真正的错误另有原因（如模型名不存在）。
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model m not found"}}`))
	}))
	defer srv.Close()

	params := chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", JSONMode: true, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     chatMeta{CallerUserID: 1, Module: "captest_fbfail"},
	}
	if _, err := chatCompletion(context.Background(), params); err == nil {
		t.Fatal("fallback 仍失败应返回错误")
	}
	if _, ok := lookupLLMCapability(capabilityTargetOf(params), capJSONObject); ok {
		t.Fatal("fallback 失败不得提交 unsupported 观察")
	}
}

// TestTemperatureCapabilityFallback temperature 参数能力贯通端到端：首次调用被
// 「temperature not supported」4xx 拒 → 去参重试成功 → 提交观察；第二次调用声明化路由
// 直接省略 temperature（最终 payload 不含该字段），审计 RequestBody 记录最终形态。
func TestTemperatureCapabilityFallback(t *testing.T) {
	setCapRoutingFlag(t, true)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "temperature") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"temperature is not supported with this model"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	defer srv.Close()

	params := chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", Temperature: 0.7, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     chatMeta{CallerUserID: 1, Module: "captest_temp"},
	}
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("温度 fallback 应成功: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("应 2 个请求（带温度被拒+去参成功）, got %d", len(bodies))
	}
	if strings.Contains(bodies[1], "temperature") {
		t.Fatal("fallback 请求不得再带 temperature")
	}
	if obs, ok := lookupLLMCapability(capabilityTargetOf(params), capTemperature); !ok || obs.State != capUnsupported {
		t.Fatalf("fallback 成功后应提交 temperature 观察: %+v ok=%v", obs, ok)
	}
	// 第二次调用：声明化路由直接省略 temperature，只发 1 个请求。
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("路由后调用应成功: %v", err)
	}
	if len(bodies) != 3 || strings.Contains(bodies[2], "temperature") {
		t.Fatalf("路由后应 1 个不带 temperature 的请求: n=%d", len(bodies))
	}
	// 审计 RequestBody=最终实际 payload（不含 temperature，含 messages/model）。
	var lastLog model.LLMCallLog
	if err := common.DB.Where("module = ?", "captest_temp").Order("id desc").First(&lastLog).Error; err != nil {
		t.Fatalf("查审计失败: %v", err)
	}
	if strings.Contains(lastLog.RequestBody, `"temperature"`) || !strings.Contains(lastLog.RequestBody, `"model"`) {
		t.Fatalf("审计 RequestBody 应为最终省略 temperature 的 payload: %.120s", lastLog.RequestBody)
	}
}

// TestMaxTokensCapabilityFallback max_tokens 参数能力贯通：被「Unsupported parameter:
// max_tokens」4xx 拒 → 去参重试成功 → 提交观察；路由后直接省略（预算失效可观察不静默——
// 观察存储/审计 payload 双向可见）。
func TestMaxTokensCapabilityFallback(t *testing.T) {
	setCapRoutingFlag(t, true)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), "max_tokens") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	defer srv.Close()

	params := chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", MaxTokens: 2000, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     chatMeta{CallerUserID: 1, Module: "captest_tok"},
	}
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("max_tokens fallback 应成功: %v", err)
	}
	if len(bodies) != 2 || strings.Contains(bodies[1], "max_tokens") {
		t.Fatalf("fallback 请求不得再带 max_tokens: n=%d", len(bodies))
	}
	if obs, ok := lookupLLMCapability(capabilityTargetOf(params), capMaxTokens); !ok || obs.State != capUnsupported {
		t.Fatalf("fallback 成功后应提交 max_tokens 观察: %+v ok=%v", obs, ok)
	}
	if _, err := chatCompletion(context.Background(), params); err != nil {
		t.Fatalf("路由后调用应成功: %v", err)
	}
	if len(bodies) != 3 || strings.Contains(bodies[2], "max_tokens") {
		t.Fatalf("路由后应 1 个不带 max_tokens 的请求: n=%d", len(bodies))
	}
}
