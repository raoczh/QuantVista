package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"quantvista/model"
)

// TestResponsesURL /responses 端点拼接与 chatCompletionsURL 同一套归一化：
// 根地址补 /v1/responses、版本段结尾补 /responses、完整端点原样。
func TestResponsesURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.openai.com", "https://api.openai.com/v1/responses"},
		{"https://api.example.com/", "https://api.example.com/v1/responses"},
		{"https://api.example.com/v1", "https://api.example.com/v1/responses"},
		{"https://ark.cn.volces.com/api/v3", "https://ark.cn.volces.com/api/v3/responses"},
		{"https://api.example.com/v1/responses", "https://api.example.com/v1/responses"},
	}
	for _, c := range cases {
		if got := responsesURL(c.in); got != c.want {
			t.Fatalf("responsesURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResponsesCompletion 非流式 /responses：请求体映射（system→instructions、
// messages→input、max_tokens→max_output_tokens、JSON mode→text.format）与
// 响应解析（output 提取、usage input/output_tokens→prompt/completion 映射）。
func TestResponsesCompletion(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": "completed",
			"output": [
				{"type":"reasoning","content":[{"type":"reasoning_text","text":"思考中"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"你好"},{"type":"output_text","text":"世界"}]}
			],
			"usage": {"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}
		}`))
	}))
	defer srv.Close()

	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses, AllowPrivate: true,
		Temperature: 0.5, MaxTokens: 1234, JSONMode: true,
		Messages: []chatMessage{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "打个招呼"},
		},
	})
	if err != nil {
		t.Fatalf("responses 调用失败: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("应请求 /v1/responses，实际 %s", gotPath)
	}
	if res.Content != "你好世界" {
		t.Fatalf("output_text 应拼接为「你好世界」（且跳过 reasoning 项），得到 %q", res.Content)
	}
	if res.Usage.PromptTokens != 11 || res.Usage.CompletionTokens != 7 || res.Usage.TotalTokens != 18 {
		t.Fatalf("usage 应按 input/output_tokens 映射: %+v", res.Usage)
	}
	// 请求体映射断言。ac1 契约（P0-1）作为首条 system 一并合入 instructions，模块 system 随后。
	instr, _ := gotBody["instructions"].(string)
	if !strings.HasPrefix(instr, "【系统准确性契约 "+llmAccuracyContractVersion) || !strings.Contains(instr, "你是助手") {
		t.Fatalf("instructions 应为 ac1 契约+模块 system 合并: %v", instr)
	}
	if gotBody["max_output_tokens"] != float64(1234) {
		t.Fatalf("max_tokens 应映射为 max_output_tokens: %v", gotBody["max_output_tokens"])
	}
	if _, has := gotBody["max_tokens"]; has {
		t.Fatal("responses 请求不应带 max_tokens 字段")
	}
	input, _ := gotBody["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("system 不应进 input，input 应只剩 1 条 user: %v", gotBody["input"])
	}
	text, _ := gotBody["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("JSON mode 应映射为 text.format.type=json_object: %v", gotBody["text"])
	}
}

// TestResponsesCompletionError 上游 4xx 错误对象应抽出 message；200+error 对象同样按错误处理。
func TestResponsesCompletionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"auth"}}`))
	}))
	defer srv.Close()
	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "bad", Model: "m", EndpointType: model.LLMEndpointResponses, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("应带出上游错误 message: %v", err)
	}
}

// TestResponsesCompletionStream 流式 /responses：output_text.delta 累积吐增量、
// completed 事件取 usage、event: 行被忽略只解析 data 行。
func TestResponsesCompletionStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n" +
			`data: {"type":"response.created"}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"今天"}` + "\n\n" +
			`data: {"type":"response.output_text.delta","delta":"天气不错"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9}}}` + "\n\n"))
	}))
	defer srv.Close()

	var deltas []string
	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("responses 流式失败: %v", err)
	}
	if res.Content != "今天天气不错" || len(deltas) != 2 {
		t.Fatalf("delta 累积错误: content=%q deltas=%v", res.Content, deltas)
	}
	if res.Usage.PromptTokens != 5 || res.Usage.CompletionTokens != 4 {
		t.Fatalf("completed 事件的 usage 应被采用: %+v", res.Usage)
	}
}

// TestResponsesCompletionStreamFailed 流中 response.failed 事件应判失败（不落半截）。
func TestResponsesCompletionStreamFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"半截"}` + "\n\n" +
			`data: {"type":"response.failed","response":{"status":"failed","error":{"message":"model overloaded"}}}` + "\n\n"))
	}))
	defer srv.Close()
	_, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "model overloaded") || RefusalCodeOf(err) != RefusalLLMCallFailed {
		t.Fatalf("failed 事件应判失败并带出 message: %v", err)
	}
}

// TestChatStreamOptionsFallback chat 流式带 stream_options.include_usage；
// 不认识该字段的 4xx 上游去掉后重试一次仍能成功。
func TestChatStreamOptionsFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if atomic.AddInt32(&calls, 1) == 1 {
			if _, has := body["stream_options"]; !has {
				t.Error("首次请求应携带 stream_options.include_usage")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field: stream_options"}}`))
			return
		}
		if _, has := body["stream_options"]; has {
			t.Error("重试请求不应再带 stream_options")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n" + "data: [DONE]\n\n"))
	}))
	defer srv.Close()

	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("stream_options fallback 应成功: %v", err)
	}
	if res.Content != "ok" || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("应重试一次并成功: content=%q calls=%d", res.Content, calls)
	}
}

// TestExtractErrTolerant 错误体宽容解析：error 对象/裸字符串、顶层 message/msg/error_msg/detail。
func TestExtractErrTolerant(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"error":{"message":"标准错误"}}`, "标准错误"},
		{`{"error":"裸字符串错误"}`, "裸字符串错误"},
		{`{"message":"顶层message"}`, "顶层message"},
		{`{"msg":"顶层msg"}`, "顶层msg"},
		{`{"error_msg":"百度式"}`, "百度式"},
		{`{"detail":"fastapi式"}`, "fastapi式"},
		{`纯文本错误`, "纯文本错误"},
	}
	for _, c := range cases {
		if got := extractErr([]byte(c.raw)); got != c.want {
			t.Fatalf("extractErr(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}
