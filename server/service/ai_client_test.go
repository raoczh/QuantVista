package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/model"
)

// promptCacheRejectCN 国产上游严格字段校验的真实报错形态：中文原文、无任何英文措辞。
// 修复前 matchUnsupportedParam 字段名命中而措辞全落空 → 不降级 → 原样重发再报 400。
const promptCacheRejectCN = `{"error":{"message":"未知请求字段：prompt_cache_key"}}`

// promptCacheUpstream 假上游：reject 非空时对携带 prompt_cache_key 的请求回该错误体，
// 其余请求成功。成功体是整包 JSON（非 SSE）——流式路径按「上游忽略 stream」兼容解析，
// 四条降级路径可共用同一个假上游。
func promptCacheUpstream(t *testing.T, endpoint, reject string) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	okBody := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`
	if endpoint == model.LLMEndpointResponses {
		okBody = `{"status":"completed","output":[{"type":"message","role":"assistant",` +
			`"content":[{"type":"output_text","text":"ok"}]}],"usage":{"total_tokens":5}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if reject != "" && strings.Contains(string(b), "prompt_cache_key") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(reject))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// TestChatCompletionsURL 端点拼接对齐 new-api/OpenAI SDK 惯例：
// 根地址补 /v1/chat/completions、版本段结尾补 /chat/completions、完整端点原样。
func TestChatCompletionsURL(t *testing.T) {
	cases := map[string]string{
		// new-api / one-api 惯例：填根地址
		"https://api.openai.com": "https://api.openai.com/v1/chat/completions",
		"https://my-newapi.com":  "https://my-newapi.com/v1/chat/completions",
		"https://my-newapi.com/": "https://my-newapi.com/v1/chat/completions",
		"http://10.0.0.2:3000":   "http://10.0.0.2:3000/v1/chat/completions",
		" https://x.com/ ":       "https://x.com/v1/chat/completions", // 首尾空白与斜杠
		// OpenAI SDK 惯例：以 /v1 结尾
		"https://api.deepseek.com/v1": "https://api.deepseek.com/v1/chat/completions",
		"https://api.moonshot.cn/v1/": "https://api.moonshot.cn/v1/chat/completions",
		// 火山方舟这类多级版本段
		"https://ark.cn-beijing.volces.com/api/v3": "https://ark.cn-beijing.volces.com/api/v3/chat/completions",
		// 直接填完整端点
		"https://x.com/v1/chat/completions": "https://x.com/v1/chat/completions",
	}
	for in, want := range cases {
		if got := chatCompletionsURL(in); got != want {
			t.Errorf("chatCompletionsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestChatCompletion_RootBaseURLAutoV1 根地址（new-api 惯例填法）应打到 /v1/chat/completions。
func TestChatCompletion_RootBaseURLAutoV1(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()

	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("根地址应自动补 /v1/chat/completions，实际请求 %s", gotPath)
	}
	if res.Content != "ok" {
		t.Fatalf("内容不符: %q", res.Content)
	}
}

// TestChatCompletion_HTMLBodyDiagnostics 200 + HTML（SPA fallback 典型形态）应报可操作的错误，
// 而不是裸的 invalid character '<'。
func TestChatCompletion_HTMLBodyDiagnostics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>New API</title></head><body>app</body></html>"))
	}))
	defer srv.Close()

	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	})
	if err == nil {
		t.Fatalf("HTML 响应应报错")
	}
	if !strings.Contains(err.Error(), "网页") || !strings.Contains(err.Error(), "Base URL") {
		t.Fatalf("报错应指出返回了网页并提示检查 Base URL，实际: %v", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("不应再向用户裸抛 json 解析错误: %v", err)
	}
}

// TestBodySnippet 报错片段：压平空白、rune 安全截断、空体兜底。
func TestBodySnippet(t *testing.T) {
	if got := bodySnippet([]byte("  a\n b\t c  ")); got != "a b c" {
		t.Errorf("空白未压平: %q", got)
	}
	if got := bodySnippet(nil); got != "(空)" {
		t.Errorf("空体应返回 (空): %q", got)
	}
	long := strings.Repeat("汉", 200)
	got := bodySnippet([]byte(long))
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != 121 {
		t.Errorf("超长应按 rune 截断到 120+…，got len=%d", len([]rune(got)))
	}
}

func TestPrepareChatCompletionForAcceptedTargetPinsCentralState(t *testing.T) {
	setContractFlag(t, true)
	snapshot := llmCallTarget{}
	target := llmCallTarget{
		BaseURL: "https://accepted.example/v1", APIKey: "accepted-key", Model: "accepted-model",
		EndpointType: model.LLMEndpointResponses, Temperature: 0.37, MaxTokens: 4321,
		AccuracyContract: false, JSONMode: false, TemperatureOmitted: true, MaxCompletionTokens: true,
		PromptCacheKeyOmitted: true,
		AllowPrivate:          true, ConfigID: 77, Provider: "accepted-provider",
	}
	prepared := prepareChatCompletionForAcceptedTarget(chatParams{
		BaseURL: "https://wrong.example", APIKey: "wrong", Model: "wrong", JSONMode: true,
		Temperature: 0.99, MaxTokens: 1,
		Messages: []chatMessage{{Role: "system", Content: "业务提示"}, {Role: "user", Content: "问题"}},
		Meta:     chatMeta{ConfigID: 1, Provider: "wrong", TargetSnapshot: &snapshot},
	}, target)

	if prepared.BaseURL != target.BaseURL || prepared.APIKey != target.APIKey || prepared.Model != target.Model ||
		prepared.EndpointType != target.EndpointType || prepared.Temperature != target.Temperature ||
		prepared.MaxTokens != target.MaxTokens || prepared.Meta.ConfigID != target.ConfigID ||
		prepared.Meta.Provider != target.Provider || prepared.AllowPrivate != target.AllowPrivate {
		t.Fatalf("accepted target 应覆盖影子请求全部调用目标字段: %+v", prepared)
	}
	if prepared.JSONMode || !prepared.temperatureOmitted() || !prepared.usesMaxCompletionTokens() ||
		!prepared.promptCacheKeyOmitted() || prepared.accuracyContractEnabled() {
		t.Fatalf("accepted attempt 的能力/契约准备态未固定: json=%v omit_temp=%v max_completion=%v omit_cache=%v ac=%v",
			prepared.JSONMode, prepared.temperatureOmitted(), prepared.usesMaxCompletionTokens(),
			prepared.promptCacheKeyOmitted(), prepared.accuracyContractEnabled())
	}
	if len(prepared.Messages) != 2 || strings.Contains(prepared.Messages[0].Content, llmAccuracyContractVersion) {
		t.Fatalf("全局契约开启不得覆盖 champion 已接受的关闭快照: %+v", prepared.Messages)
	}
	if snapshot != target {
		t.Fatalf("最终准备态快照必须逐值等于 accepted target: got=%+v want=%+v", snapshot, target)
	}
}

func TestPrepareChatCompletionForAcceptedTargetKeepsEnabledContract(t *testing.T) {
	setContractFlag(t, false)
	snapshot := llmCallTarget{}
	target := llmCallTarget{
		BaseURL: "https://accepted.example/v1", APIKey: "accepted-key", Model: "accepted-model",
		Temperature: llmStructuredTempCap, MaxTokens: 6000,
		AccuracyContract: true, JSONMode: true, ConfigID: 88, Provider: "accepted-provider",
	}
	prepared := prepareChatCompletionForAcceptedTarget(chatParams{
		Messages: []chatMessage{{Role: "system", Content: "业务提示"}, {Role: "user", Content: "问题"}},
		Meta:     chatMeta{TargetSnapshot: &snapshot},
	}, target)
	if !prepared.accuracyContractEnabled() || len(prepared.Messages) != 2 ||
		!strings.Contains(prepared.Messages[0].Content, llmAccuracyContractVersion) {
		t.Fatalf("全局契约关闭不得覆盖 champion 已接受的开启快照: %+v", prepared.Messages)
	}
	if snapshot != target {
		t.Fatalf("开启契约的准备态快照漂移: got=%+v want=%+v", snapshot, target)
	}
}

// TestPromptCacheKeyValue 缓存亲和 key 的取值规则：全部业务模块都发（此前只有
// recommendation/analysis 两个）、探针与空 module 排除、PromptVersion 缺失时降级用 module
// 单独做 key（不放弃发送）、CacheScope 非空时拼到尾部。
func TestPromptCacheKeyValue(t *testing.T) {
	cases := []struct {
		name          string
		module        string
		promptVersion string
		scope         string
		want          string
	}{
		// 放开前就带 key 的两个模块：值格式不变。
		{name: "recommendation", module: "recommendation", promptVersion: "p13", want: "recommendation:p13"},
		{name: "analysis", module: "analysis", promptVersion: "p19", want: "analysis:p19"},
		// 放开前恒空的模块（newLLMRun 调用点全集），现在都必须带 key。
		{name: "analysis_review", module: "analysis_review", promptVersion: "r2", want: "analysis_review:r2"},
		{name: "debate_bull", module: "debate_bull", promptVersion: "d4", want: "debate_bull:d4"},
		{name: "debate_bear", module: "debate_bear", promptVersion: "d4", want: "debate_bear:d4"},
		{name: "debate_rebuttal", module: "debate_rebuttal", promptVersion: "d4", want: "debate_rebuttal:d4"},
		{name: "debate_judge", module: "debate_judge", promptVersion: "d4", want: "debate_judge:d4"},
		{name: "trade_plan", module: "trade_plan", promptVersion: "p19", want: "trade_plan:p19"},
		{name: "rec_review", module: "rec_review", promptVersion: "p13", want: "rec_review:p13"},
		{name: "rec_bear", module: "rec_bear", promptVersion: "b2", want: "rec_bear:b2"},
		{name: "reflection", module: "reflection", promptVersion: "f1", want: "reflection:f1"},
		{name: "qa", module: "qa", promptVersion: "q13", want: "qa:q13"},
		{name: "news", module: "news", promptVersion: "n3", want: "news:n3"},
		{name: "daily_report", module: "daily_report", promptVersion: "dr2", want: "daily_report:dr2"},
		{name: "compare", module: "compare", promptVersion: "c1", want: "compare:c1"},
		{name: "position_advice", module: "position_advice", promptVersion: "sp3", want: "position_advice:sp3"},
		{name: "screener_parse", module: "screener_parse", promptVersion: "s1", want: "screener_parse:s1"},
		{name: "experiment", module: "experiment", promptVersion: "p13", want: "experiment:p13"},
		{name: "release_audit", module: "release_audit", promptVersion: "ra1", want: "release_audit:ra1"},
		// PromptVersion 缺失（自定义 prompt/未登记版本）：粗粒度亲和仍优于完全不发。
		{name: "empty_prompt_version", module: "qa", want: "qa"},
		{name: "blank_prompt_version", module: "qa", promptVersion: "   ", want: "qa"},
		// 连接测试探针不注入任何业务语义；未接线路径无可归因维度。
		{name: "probe_module_excluded", module: "test", promptVersion: "v1", want: ""},
		{name: "empty_module", module: "", promptVersion: "v1", want: ""},
		{name: "blank_module", module: "   ", promptVersion: "v1", want: ""},
		// CacheScope（本批无调用点填充，接口先留出）。
		{name: "scope_appended", module: "qa", promptVersion: "q13", scope: "conv-42", want: "qa:q13:conv-42"},
		{name: "scope_without_version", module: "qa", scope: "conv-42", want: "qa:conv-42"},
		{name: "blank_scope_ignored", module: "qa", promptVersion: "q13", scope: "  ", want: "qa:q13"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := chatParams{Meta: chatMeta{
				Module: c.module, PromptVersion: c.promptVersion, CacheScope: c.scope}}
			if got := p.promptCacheKey(); got != c.want {
				t.Fatalf("promptCacheKey()=%q, want %q", got, c.want)
			}
			if got, want := p.sendsPromptCacheKey(), c.want != ""; got != want {
				t.Fatalf("sendsPromptCacheKey()=%v, want %v", got, want)
			}
			payload := map[string]any{}
			p.addPromptCacheField(payload)
			if got, has := payload["prompt_cache_key"]; (c.want == "") == has || (has && got != c.want) {
				t.Fatalf("payload 注入不符: got=%v has=%v want=%q", got, has, c.want)
			}
		})
	}

	// 省略状态置位后不再发送；key 本身不变（审计能看出「本可发但已降级」）。
	omitted := false
	p := chatParams{Meta: chatMeta{Module: "qa", PromptVersion: "q13"}, omitPromptCacheKey: &omitted}
	p.markPromptCacheKeyOmitted()
	if !p.promptCacheKeyOmitted() || p.sendsPromptCacheKey() {
		t.Fatal("置位省略后不得再发送 prompt_cache_key")
	}
	if p.promptCacheKey() != "qa:q13" {
		t.Fatalf("省略状态不应改写 key 本身: %q", p.promptCacheKey())
	}
	payload := map[string]any{}
	p.addPromptCacheField(payload)
	if _, has := payload["prompt_cache_key"]; has {
		t.Fatalf("省略状态不得注入字段: %v", payload)
	}
	// nil 安全（绕过公开出口、无观测指针的路径）。
	bare := chatParams{Meta: chatMeta{Module: "qa", PromptVersion: "q13"}}
	bare.markPromptCacheKeyOmitted()
	if bare.promptCacheKeyOmitted() || !bare.sendsPromptCacheKey() {
		t.Fatal("无观测指针时 mark/读取都必须 nil 安全")
	}
}

// TestPromptCacheKeyRejectFallbackChat chat 两处降级点（非流式 + 流式）：中文 4xx 拒绝
// prompt_cache_key 时去参重试成功、业务照常拿到结果，并在确认 200 后落 unsupported 观察。
func TestPromptCacheKeyRejectFallbackChat(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "chat_plain"},
		{name: "chat_stream", stream: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetLLMCapabilityStore()
			t.Cleanup(resetLLMCapabilityStore)
			srv, bodies := promptCacheUpstream(t, "", promptCacheRejectCN)

			p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxTokens: 256, AllowPrivate: true,
				Messages: []chatMessage{{Role: "user", Content: "hi"}},
				Meta:     chatMeta{Module: "cache_fb_chat", PromptVersion: "v1"}}
			var res *chatResult
			var err error
			if tc.stream {
				res, err = chatCompletionStream(context.Background(), p, nil)
			} else {
				res, err = chatCompletionPlain(context.Background(), initCallObservers(p))
			}
			// 无害降级：失去缓存亲和不等于调用失败（修复前这里直接把 400 抛给用户）。
			if err != nil {
				t.Fatalf("prompt_cache_key 被拒应去参重试成功，实际失败: %v", err)
			}
			if res == nil || res.Content != "ok" {
				t.Fatalf("应拿到上游正文: %+v", res)
			}
			if len(*bodies) != 2 {
				t.Fatalf("应 2 个请求（带 key 被拒 + 去参成功）: %d", len(*bodies))
			}
			if !strings.Contains((*bodies)[0], `"prompt_cache_key":"cache_fb_chat:v1"`) {
				t.Fatalf("首轮应携带 module:promptVersion 形态的 key: %s", (*bodies)[0])
			}
			if strings.Contains((*bodies)[1], "prompt_cache_key") {
				t.Fatalf("重试请求不得再带该参数: %s", (*bodies)[1])
			}
			// 区分两条降级路径确实各自跑到（流式恒 stream=true，非流式恒 false）。
			wantStream := `"stream":true`
			if !tc.stream {
				wantStream = `"stream":false`
			}
			for i, body := range *bodies {
				if !strings.Contains(body, wantStream) {
					t.Fatalf("第 %d 个请求应为 %s: %s", i+1, wantStream, body)
				}
			}
			obs, ok := lookupLLMCapability(llmCapabilityTarget(0, "", srv.URL, "m", ""), capPromptCacheKey)
			if !ok || obs.State != capUnsupported {
				t.Fatalf("去参重试成功后应落 unsupported 观察: %+v ok=%v", obs, ok)
			}
		})
	}
}
