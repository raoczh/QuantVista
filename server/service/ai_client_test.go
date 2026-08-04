package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/model"
)

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
		AllowPrivate: true, ConfigID: 77, Provider: "accepted-provider",
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
		prepared.accuracyContractEnabled() {
		t.Fatalf("accepted attempt 的能力/契约准备态未固定: json=%v omit_temp=%v max_completion=%v ac=%v",
			prepared.JSONMode, prepared.temperatureOmitted(), prepared.usesMaxCompletionTokens(), prepared.accuracyContractEnabled())
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
