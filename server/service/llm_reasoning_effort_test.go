package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// 思考档位（reasoning_effort）配置项测试：
//   - payload 形态：chat 平铺 reasoning_effort、responses 嵌套 reasoning.effort，**未配档位
//     时两者都不出现**（这是零回归的核心断言：存量配置的请求字节不得变化）；
//   - 两类上游拒绝都必须去参重试成功、业务照常拿到结果、能力观察落 unsupported：
//     ①参数本身不认（非推理模型）②所配档位不在取值集合内（max/ultra 非 OpenAI 官方档位）；
//   - 去参重试仍失败时不得落观察（一次误判会污染该目标 12h）；
//   - 声明化路由在已观察 unsupported 时直接不发该参数。

// setupLLMConfigTest 准备 LLM 配置 CRUD 测试环境（照 llm_test.go 既有范式：清空表 +
// 固定加密密钥，退场恢复）。返回 service 与本用例专用的 userID。
func setupLLMConfigTest(t *testing.T) (*LLMService, int64) {
	t.Helper()
	setupTestDB(t)
	if err := common.DB.Exec("DELETE FROM llm_configs").Error; err != nil {
		t.Fatalf("清理 LLM 配置失败: %v", err)
	}
	oldKey := common.EncryptionKey
	common.EncryptionKey = "llm-effort-test-key"
	t.Cleanup(func() {
		common.EncryptionKey = oldKey
		common.DB.Exec("DELETE FROM llm_configs")
	})
	return &LLMService{}, 4201
}

// effortProbeUpstream 记录每个请求体的假上游。reject 决定对带档位的请求返回什么错误；
// 空 reject 表示一律成功。
func effortProbeUpstream(t *testing.T, endpoint, reject string) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	okBody := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	if endpoint == model.LLMEndpointResponses {
		okBody = `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if reject != "" && strings.Contains(string(b), "reasoning") {
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

// TestReasoningEffortPayloadShape 两端点的档位字段形态，以及未配档位时必须完全不携带。
func TestReasoningEffortPayloadShape(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		effort   string
		stream   bool
	}{
		{name: "chat_plain", effort: "max"},
		{name: "chat_plain_empty", effort: ""},
		{name: "chat_stream", effort: "high", stream: true},
		{name: "chat_stream_empty", effort: "", stream: true},
		{name: "responses_plain", endpoint: model.LLMEndpointResponses, effort: "xhigh"},
		{name: "responses_plain_empty", endpoint: model.LLMEndpointResponses, effort: ""},
		{name: "responses_stream", endpoint: model.LLMEndpointResponses, effort: "low", stream: true},
		{name: "responses_stream_empty", endpoint: model.LLMEndpointResponses, effort: "", stream: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, bodies := effortProbeUpstream(t, tc.endpoint, "")

			p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				ReasoningEffort: tc.effort, MaxTokens: 256,
				Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
			var err error
			if tc.stream {
				_, err = chatCompletionStream(context.Background(), p, nil)
			} else {
				_, err = chatCompletion(context.Background(), p)
			}
			if err != nil {
				t.Fatalf("调用失败: %v", err)
			}
			if len(*bodies) == 0 {
				t.Fatal("未收到请求")
			}
			body := (*bodies)[0]

			var parsed struct {
				ReasoningEffort string `json:"reasoning_effort"`
				Reasoning       *struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if jerr := json.Unmarshal([]byte(body), &parsed); jerr != nil {
				t.Fatalf("请求体不是合法 JSON: %v（%s）", jerr, body)
			}

			if tc.effort == "" {
				// 零回归底线：不配档位时两种字段都不得出现。
				if strings.Contains(body, "reasoning_effort") || strings.Contains(body, `"reasoning"`) {
					t.Fatalf("未配档位不得携带任何思考参数: %s", body)
				}
				return
			}
			if tc.endpoint == model.LLMEndpointResponses {
				if parsed.Reasoning == nil || parsed.Reasoning.Effort != tc.effort {
					t.Fatalf("responses 端应为 reasoning.effort=%s: %s", tc.effort, body)
				}
				if parsed.ReasoningEffort != "" {
					t.Fatalf("responses 端不得平铺 reasoning_effort: %s", body)
				}
				return
			}
			if parsed.ReasoningEffort != tc.effort {
				t.Fatalf("chat 端应为 reasoning_effort=%s: %s", tc.effort, body)
			}
			if parsed.Reasoning != nil {
				t.Fatalf("chat 端不得嵌套 reasoning: %s", body)
			}
		})
	}
}

// TestReasoningEffortRejectFallback 两类上游拒绝都要去参重试成功、落 unsupported 观察。
// 「值不被接受」是本项目的常见路径——默认档位 max 不在 OpenAI 官方取值集合内。
func TestReasoningEffortRejectFallback(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		stream   bool
		reject   string
	}{
		{
			name:   "chat_plain_param_unknown",
			reject: `{"error":{"message":"unknown parameter: 'reasoning_effort'"}}`,
		},
		{
			name:   "chat_plain_value_invalid",
			reject: `{"error":{"message":"Invalid value: 'max'. Supported values are: 'low', 'medium' and 'high'."}}`,
		},
		{
			name:   "chat_stream_value_invalid",
			stream: true,
			reject: `{"error":{"message":"Invalid value: 'max'. Supported values are: 'low', 'medium' and 'high'."}}`,
		},
		{
			name:     "responses_plain_value_invalid",
			endpoint: model.LLMEndpointResponses,
			reject:   `{"error":{"message":"Invalid value: 'max'. Supported values are: 'low', 'medium' and 'high'."}}`,
		},
		{
			name:     "responses_stream_param_unknown",
			endpoint: model.LLMEndpointResponses,
			stream:   true,
			reject:   `{"error":{"message":"unsupported parameter: reasoning.effort"}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetLLMCapabilityStore()
			t.Cleanup(resetLLMCapabilityStore)
			srv, bodies := effortProbeUpstream(t, tc.endpoint, tc.reject)

			p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				ReasoningEffort: "max", MaxTokens: 256,
				Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
			var res *chatResult
			var err error
			if tc.stream {
				res, err = chatCompletionStream(context.Background(), p, nil)
			} else {
				res, err = chatCompletion(context.Background(), p)
			}
			// 无害降级：业务必须照常拿到结果，不能因档位被拒而失败。
			if err != nil {
				t.Fatalf("档位被拒应去参重试成功，实际失败: %v", err)
			}
			if res == nil || res.Content != "ok" {
				t.Fatalf("应拿到上游正文: %+v", res)
			}
			if len(*bodies) < 2 {
				t.Fatalf("应有去参重试（首轮带档位、次轮不带）: %d 次请求", len(*bodies))
			}
			last := (*bodies)[len(*bodies)-1]
			if strings.Contains(last, "reasoning") {
				t.Fatalf("重试请求不得再带思考参数: %s", last)
			}
			target := llmCapabilityTarget(0, "", srv.URL, "m", tc.endpoint)
			obs, ok := lookupLLMCapability(target, capReasoningEffort)
			if !ok || obs.State != capUnsupported {
				t.Fatalf("去参重试成功后应落 unsupported 观察: %+v ok=%v", obs, ok)
			}
		})
	}
}

// TestReasoningEffortRejectNoObservationOnRetryFailure 去参重试仍失败时不得落观察
// ——错误另有原因（如模型名不存在），一次误判会让该目标 12h 内都不发档位。
func TestReasoningEffortRejectNoObservationOnRetryFailure(t *testing.T) {
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		if strings.Contains(string(b), "reasoning_effort") {
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid value: 'max'. Supported values are: 'low'."}}`))
			return
		}
		// 去参后仍失败：真正的原因是模型名不存在。
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer srv.Close()

	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", ReasoningEffort: "max", MaxTokens: 256,
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
	})
	if err == nil {
		t.Fatal("去参后仍 4xx 应报错")
	}
	if _, ok := lookupLLMCapability(llmCapabilityTarget(0, "", srv.URL, "m", ""), capReasoningEffort); ok {
		t.Fatal("重试仍失败不得落能力观察")
	}
}

// TestReasoningEffortCapabilityRouting 已观察 unsupported 时声明化路由直接不发该参数，
// 省掉一次注定失败的请求；观察过期后恢复乐观尝试。
func TestReasoningEffortCapabilityRouting(t *testing.T) {
	setCapRoutingFlag(t, true)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)

	srv, bodies := effortProbeUpstream(t, "", "")
	target := llmCapabilityTarget(0, "", srv.URL, "m", "")
	observeLLMCapability(target, capReasoningEffort, capUnsupported, "测试观察")

	p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", ReasoningEffort: "max", MaxTokens: 256,
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
	if _, err := chatCompletion(context.Background(), p); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("声明化路由应一次成功、无重试: %d 次请求", len(*bodies))
	}
	if strings.Contains((*bodies)[0], "reasoning") {
		t.Fatalf("已声明不支持时不得携带思考参数: %s", (*bodies)[0])
	}
}

// TestLooksLikeUnsupportedReasoningEffort 判定边界：两类真实拒绝命中，无关 4xx 不误判。
func TestLooksLikeUnsupportedReasoningEffort(t *testing.T) {
	hit := []string{
		`{"error":{"message":"unknown parameter: 'reasoning_effort'"}}`,
		`{"error":{"message":"reasoning_effort is not supported for this model"}}`,
		`{"error":{"message":"Invalid value: 'max'. Supported values are: 'low', 'medium' and 'high'."}}`,
		`{"error":{"message":"reasoning.effort must be one of low, medium, high"}}`,
	}
	for _, raw := range hit {
		if !looksLikeUnsupportedReasoningEffort(http.StatusBadRequest, []byte(raw)) {
			t.Errorf("应判定为档位被拒: %s", raw)
		}
	}
	miss := []string{
		// 明确指向结构化参数取值：那条归 JSON mode 回落处理，不算档位问题。
		`{"error":{"message":"Invalid value: 'yaml'. Supported values are: 'json_object' for response_format"}}`,
		`{"error":{"message":"model not found"}}`,
		`{"error":{"message":"temperature is not supported"}}`,
		// 5xx 不属于参数问题。
		`{"error":{"message":"unknown parameter: reasoning_effort"}}`,
	}
	for i, raw := range miss {
		status := http.StatusBadRequest
		if i == len(miss)-1 {
			status = http.StatusInternalServerError
		}
		if looksLikeUnsupportedReasoningEffort(status, []byte(raw)) {
			t.Errorf("不应判定为档位被拒（HTTP %d）: %s", status, raw)
		}
	}

	// 「值非法但未指明字段」必须命中——这是配了 max/ultra 时最常见的上游报错形态，
	// 漏判会让业务调用直接失败而非降级（判定函数只在请求确实携带档位时被调用）。
	unnamed := `{"error":{"message":"Invalid value: 'max'. Supported values are: 'low', 'medium' and 'high'."}}`
	if !looksLikeUnsupportedReasoningEffort(http.StatusBadRequest, []byte(unnamed)) {
		t.Errorf("未指明字段的值非法应归因到档位: %s", unnamed)
	}
}

// TestLLMConfigValidateReasoningEffort 校验只管格式不管枚举：新档位不必改代码就能用，
// 但明显非法的输入（大写/空格/超长/前导符号）要拦住。
func TestLLMConfigValidateReasoningEffort(t *testing.T) {
	svc := &LLMService{}
	for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "gpt5-max"} {
		in := validLLMConfigInput()
		in.ReasoningEffort = effort
		if err := svc.validate(in); err != nil {
			t.Errorf("档位 %q 应通过校验: %v", effort, err)
		}
	}
	for _, effort := range []string{"MAX", "very high", "-low", "_x", "这是中文", strings.Repeat("a", 17)} {
		in := validLLMConfigInput()
		in.ReasoningEffort = effort
		if err := svc.validate(in); err == nil {
			t.Errorf("档位 %q 应被拒绝", effort)
		}
	}
}

// TestLLMConfigReasoningEffortPersists 档位全链路持久化，且**空值原样保留为空**
// ——GORM 零值不得回落成某个默认档位（那会让用户明确选的「不发送」被悄悄改写）。
func TestLLMConfigReasoningEffortPersists(t *testing.T) {
	svc, userID := setupLLMConfigTest(t)

	in := validLLMConfigInput()
	in.APIKey = "sk-test"
	in.ReasoningEffort = "max"
	created, err := svc.Create(userID, in)
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	if created.ReasoningEffort != "max" {
		t.Fatalf("创建应保留档位: %q", created.ReasoningEffort)
	}

	// 清空 = 不发送该参数，必须原样落库为空。
	in.ReasoningEffort = ""
	updated, err := svc.Update(userID, created.ID, in)
	if err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if updated.ReasoningEffort != "" {
		t.Fatalf("清空档位必须原样保留为空，实际 %q", updated.ReasoningEffort)
	}
	var row model.LLMConfig
	if err := common.DB.First(&row, created.ID).Error; err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if row.ReasoningEffort != "" {
		t.Fatalf("落库值应为空（不得回落 DB 默认值）: %q", row.ReasoningEffort)
	}
}

// TestLLMConfigUpdateKeepsProvider provider 已从表单撤除：入参空时必须沿用原值，
// 否则历史记录的审计标签、能力观察 key 与校准分层键会漂移。
func TestLLMConfigUpdateKeepsProvider(t *testing.T) {
	svc, userID := setupLLMConfigTest(t)

	in := validLLMConfigInput()
	in.APIKey = "sk-test"
	in.Provider = "my-gateway"
	created, err := svc.Create(userID, in)
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	if created.Provider != "my-gateway" {
		t.Fatalf("创建应保留显式 provider: %q", created.Provider)
	}

	in.Provider = "" // 前端不再提交该字段
	updated, err := svc.Update(userID, created.ID, in)
	if err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if updated.Provider != "my-gateway" {
		t.Fatalf("入参空应沿用原 provider，实际 %q", updated.Provider)
	}

	// 新建不带 provider 时按兼容口径填 openai（能力矩阵有该内置声明）。
	fresh := validLLMConfigInput()
	fresh.APIKey, fresh.Provider, fresh.Name = "sk-test", "", "no-provider"
	created2, err := svc.Create(userID, fresh)
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	if created2.Provider != "openai" {
		t.Fatalf("新建缺省 provider 应为 openai，实际 %q", created2.Provider)
	}
}

// TestLLMConfigSetDefault 列表页一键设默认：目标置位且其余全部清掉（单默认不变量）。
func TestLLMConfigSetDefault(t *testing.T) {
	svc, userID := setupLLMConfigTest(t)

	mk := func(name string, isDefault bool) *LLMConfigView {
		in := validLLMConfigInput()
		in.APIKey, in.Name, in.IsDefault = "sk-test", name, isDefault
		v, err := svc.Create(userID, in)
		if err != nil {
			t.Fatalf("创建 %s 失败: %v", name, err)
		}
		return v
	}
	first := mk("first", true)
	second := mk("second", false)
	third := mk("third", false)

	if _, err := svc.SetDefault(userID, second.ID); err != nil {
		t.Fatalf("设为默认失败: %v", err)
	}
	rows, err := svc.List(userID)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	defaults := make([]int64, 0, 1)
	for _, r := range rows {
		if r.IsDefault {
			defaults = append(defaults, r.ID)
		}
	}
	if len(defaults) != 1 || defaults[0] != second.ID {
		t.Fatalf("应只有 second 为默认，实际 %v（first=%d third=%d）", defaults, first.ID, third.ID)
	}

	// 越权：不是本人的配置不得设默认。
	if _, err := svc.SetDefault(userID+1, second.ID); err == nil {
		t.Fatal("跨用户设默认应报错")
	}
}

// TestModelsURL 与 chatCompletionsURL 同族的端点惯例。
func TestModelsURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":           "https://api.example.com/v1/models",
		"https://api.example.com/":          "https://api.example.com/v1/models",
		"https://api.example.com/v1":        "https://api.example.com/v1/models",
		"https://ark.example.com/api/v3":    "https://ark.example.com/api/v3/models",
		"https://api.example.com/v1/models": "https://api.example.com/v1/models",
	}
	for in, want := range cases {
		if got := modelsURL(in); got != want {
			t.Errorf("modelsURL(%q)=%q, want %q", in, got, want)
		}
	}
}

// TestFetchModels 拉取模型：成功排序、密钥三态、非 JSON 与空列表的归因文案。
func TestFetchModels(t *testing.T) {
	svc, userID := setupLLMConfigTest(t)

	t.Run("ok_sorted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer sk-live" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"zeta"},{"id":"alpha","owned_by":"acme"}]}`))
		}))
		defer srv.Close()

		models, truncated, err := svc.FetchModels(userID,
			LLMConfigInput{BaseURL: srv.URL, APIKey: "sk-live"}, true)
		if err != nil {
			t.Fatalf("拉取失败: %v", err)
		}
		if truncated {
			t.Fatal("两个模型不应触发截断")
		}
		if len(models) != 2 || models[0].ID != "alpha" || models[0].OwnedBy != "acme" || models[1].ID != "zeta" {
			t.Fatalf("应按 id 排序并保留 owned_by: %+v", models)
		}
	})

	t.Run("reuses_saved_key", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		}))
		defer srv.Close()

		in := validLLMConfigInput()
		in.APIKey, in.Name, in.BaseURL = "sk-saved", "saved-key", srv.URL
		cfg, err := svc.Create(userID, in)
		if err != nil {
			t.Fatalf("创建配置失败: %v", err)
		}
		// api_key 留空 + config_id：复用已存密钥（编辑时改 URL 不必重填 key）。
		models, _, err := svc.FetchModels(userID,
			LLMConfigInput{BaseURL: srv.URL, ConfigID: cfg.ID}, true)
		if err != nil {
			t.Fatalf("复用已存密钥拉取失败: %v", err)
		}
		if len(models) != 1 || gotAuth != "Bearer sk-saved" {
			t.Fatalf("应使用已存密钥: auth=%q models=%+v", gotAuth, models)
		}
	})

	t.Run("missing_key", func(t *testing.T) {
		_, _, err := svc.FetchModels(userID, LLMConfigInput{BaseURL: "https://api.example.com"}, true)
		if err == nil || !strings.Contains(err.Error(), "API Key") {
			t.Fatalf("无密钥应报缺 API Key: %v", err)
		}
	})

	t.Run("missing_base_url", func(t *testing.T) {
		_, _, err := svc.FetchModels(userID, LLMConfigInput{APIKey: "sk"}, true)
		if err == nil || !strings.Contains(err.Error(), "Base URL") {
			t.Fatalf("无 Base URL 应报错: %v", err)
		}
	})

	t.Run("html_response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><html><body>SPA</body></html>"))
		}))
		defer srv.Close()

		_, _, err := svc.FetchModels(userID, LLMConfigInput{BaseURL: srv.URL, APIKey: "sk"}, true)
		if err == nil || !strings.Contains(err.Error(), "网页") {
			t.Fatalf("200+HTML 应给出「返回网页而非 API」的归因: %v", err)
		}
	})

	t.Run("empty_list", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()

		_, _, err := svc.FetchModels(userID, LLMConfigInput{BaseURL: srv.URL, APIKey: "sk"}, true)
		if err == nil || !strings.Contains(err.Error(), "自定义模型") {
			t.Fatalf("空列表应引导勾选自定义模型: %v", err)
		}
	})

	t.Run("upstream_4xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
		}))
		defer srv.Close()

		_, _, err := svc.FetchModels(userID, LLMConfigInput{BaseURL: srv.URL, APIKey: "sk"}, true)
		if err == nil || !strings.Contains(err.Error(), "bad key") {
			t.Fatalf("应透出上游错误文案: %v", err)
		}
	})
}

// TestScoreBlindSnapshotOmitsEmptyEffort 未配档位时 score-blind 精确输入快照的序列化字节
// 不得变化——否则 InputHash 变了，sb2 历史样本就不能与新样本混算（须递增 schema 版本）。
// 两个新字段带 omitempty 正是为此。
func TestScoreBlindSnapshotOmitsEmptyEffort(t *testing.T) {
	base := scoreBlindInputSnapshot{
		ExperimentType: "score_blind", InputSchemaVersion: scoreBlindInputSchemaVersion,
		Seed: 42, CandidateOrder: []string{"600000"}, SchemaVersion: "recommendation.v2",
		ConfigID: 7, Provider: "openai", Model: "m", Temperature: 0.7, MaxTokens: 8192,
		JSONMode: true, Messages: []chatMessage{{Role: "user", Content: "x"}},
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), "reasoning_effort") {
		t.Fatalf("未配档位的快照不得出现档位字段（会改变 InputHash）: %s", raw)
	}

	// 配了档位则必须出现——那是真实的单变量变更，本就该体现在快照与 hash 里。
	withEffort := base
	withEffort.ReasoningEffort = "max"
	raw2, err := json.Marshal(withEffort)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(raw2), `"reasoning_effort":"max"`) {
		t.Fatalf("配了档位应进入快照: %s", raw2)
	}
	if llmContentHash(string(raw)) == llmContentHash(string(raw2)) {
		t.Fatal("配档位与不配档位的 InputHash 必须不同")
	}
}

// TestTestConnectionEffortNote 连接测试必须如实报告档位是否被上游接受——这是用户判断
// 「所配档位到底生效了没有」的主入口（业务侧被拒时只会静默去参降级）。
func TestTestConnectionEffortNote(t *testing.T) {
	setupTestDB(t)
	svc := &LLMService{}

	t.Run("accepted", func(t *testing.T) {
		resetLLMCapabilityStore()
		t.Cleanup(resetLLMCapabilityStore)
		srv, _ := effortProbeUpstream(t, "", "")

		r := svc.testOpenAICompatibleForUser(llmProbeTarget{
			Provider: "gw", BaseURL: srv.URL, APIKey: "k", Model: "m",
			ReasoningEffort: "max", AllowPrivate: true,
		})
		if !r.OK || !strings.Contains(r.Message, "推理档位 max：已接受") {
			t.Fatalf("档位被接受应如实说明: %+v", r)
		}
	})

	t.Run("rejected_still_ok", func(t *testing.T) {
		resetLLMCapabilityStore()
		t.Cleanup(resetLLMCapabilityStore)
		srv, bodies := effortProbeUpstream(t, "",
			`{"error":{"message":"Invalid value: 'max'. Supported values are: 'low', 'medium' and 'high'."}}`)

		r := svc.testOpenAICompatibleForUser(llmProbeTarget{
			Provider: "gw", BaseURL: srv.URL, APIKey: "k", Model: "m",
			ReasoningEffort: "max", AllowPrivate: true,
		})
		// 档位被拒不等于配置不可用：必须仍判通过，否则用户会误以为 URL/密钥配错了。
		if !r.OK {
			t.Fatalf("档位被拒不应让测试失败: %+v", r)
		}
		if !strings.Contains(r.Message, "上游不接受") {
			t.Fatalf("应如实说明档位未生效: %+v", r)
		}
		if len(*bodies) < 2 || strings.Contains((*bodies)[1], "reasoning") {
			t.Fatalf("应去参重试: %v", *bodies)
		}
		obs, ok := lookupLLMCapability(llmCapabilityTarget(0, "gw", srv.URL, "m", ""), capReasoningEffort)
		if !ok || obs.State != capUnsupported {
			t.Fatalf("去参成功后应落观察: %+v ok=%v", obs, ok)
		}
	})

	t.Run("no_effort_no_note", func(t *testing.T) {
		resetLLMCapabilityStore()
		t.Cleanup(resetLLMCapabilityStore)
		srv, _ := effortProbeUpstream(t, "", "")

		r := svc.testOpenAICompatibleForUser(llmProbeTarget{
			Provider: "gw", BaseURL: srv.URL, APIKey: "k", Model: "m", AllowPrivate: true,
		})
		if strings.Contains(r.Message, "推理档位") {
			t.Fatalf("未配档位不应有该附注: %+v", r)
		}
	})
}
