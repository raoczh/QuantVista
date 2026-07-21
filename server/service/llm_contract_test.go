package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// P0-1/P0-7 契约层测试：ac1 注入、低温钳制、repair 温度归零、流式完整性门禁
//（半截流 / finish_reason=length / responses incomplete 三类反例）、机读拒答码、
// news 注入窗口。flag 切换依赖 options 表（setupTestDB），Cleanup 恢复默认开。

// setContractFlag 切换准确性契约开关（写 options 表 + 内存变量），退场恢复默认开。
func setContractFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMAccuracyContract(v); err != nil {
		t.Fatalf("切换契约开关失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMAccuracyContract(true) })
}

// TestApplyAccuracyContract 契约注入与温度语义：开=ac1 与业务 system 合成单一信封 + JSONMode 钳 0.2 +
// repair 归 0（自由文本温度不动）；关=原样直通。
func TestApplyAccuracyContract(t *testing.T) {
	setContractFlag(t, true)
	base := chatParams{
		Temperature: 0.9,
		Messages:    []chatMessage{{Role: "system", Content: "模块提示"}, {Role: "developer", Content: "开发者提示"}, {Role: "user", Content: "问题"}},
	}

	jsonP := base
	jsonP.JSONMode = true
	got := applyAccuracyContract(jsonP)
	if got.Temperature != llmStructuredTempCap {
		t.Fatalf("结构化调用温度应钳到 %.1f, got %v", llmStructuredTempCap, got.Temperature)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" ||
		!strings.Contains(got.Messages[0].Content, "准确性契约 "+llmAccuracyContractVersion) {
		t.Fatalf("应生成 ac1 单一 system 信封: %+v", got.Messages)
	}
	if !strings.Contains(got.Messages[0].Content, llmAccuracyTaskSectionHeader) ||
		!strings.Contains(got.Messages[0].Content, "模块提示") || !strings.Contains(got.Messages[0].Content, "开发者提示") ||
		got.Messages[1].Content != "问题" {
		t.Fatalf("业务 system/developer 应收进受限任务段，非 system 消息保持顺序: %+v", got.Messages)
	}

	repairP := jsonP
	repairP.Repair = true
	if got := applyAccuracyContract(repairP); got.Temperature != 0 {
		t.Fatalf("repair 轮温度应固定 0, got %v", got.Temperature)
	}

	freeP := base // JSONMode=false 的自由文本（qa/compare）不钳温
	if got := applyAccuracyContract(freeP); got.Temperature != 0.9 {
		t.Fatalf("自由文本温度不应被钳, got %v", got.Temperature)
	}

	lowP := jsonP
	lowP.Temperature = 0.1 // 已低于上限：保持用户值
	if got := applyAccuracyContract(lowP); got.Temperature != 0.1 {
		t.Fatalf("低于上限的温度应保持原值, got %v", got.Temperature)
	}

	setContractFlag(t, false)
	if got := applyAccuracyContract(jsonP); got.Temperature != 0.9 || len(got.Messages) != 3 {
		t.Fatalf("开关关闭应原样直通: temp=%v msgs=%d", got.Temperature, len(got.Messages))
	}
}

// sseServer 构造一个按行发送 SSE 的假上游。
func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
			fl.Flush()
		}
	}))
}

// TestChatStreamEOFWithoutMarkerRejected 反例：SSE 发了内容后直接关连接（无 [DONE]、无
// finish_reason）——网关在上游超时后干净关流的典型形态。契约开=拒收；关=旧行为当成功。
func TestChatStreamEOFWithoutMarkerRejected(t *testing.T) {
	setContractFlag(t, true)
	lines := []string{
		`data: {"choices":[{"delta":{"content":"半截"}}]}`,
		`data: {"choices":[{"delta":{"content":"内容"}}]}`,
		// 直接 EOF，无终止标记
	}
	srv := sseServer(t, lines)
	defer srv.Close()
	p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}

	if _, err := chatCompletionStream(context.Background(), p, nil); err == nil {
		t.Fatal("无终止标记的流应被拒收")
	} else if !strings.Contains(err.Error(), "eof_without_marker") {
		t.Fatalf("错误应含 eof_without_marker: %v", err)
	}

	setContractFlag(t, false)
	res, err := chatCompletionStream(context.Background(), p, nil)
	if err != nil || res.Content != "半截内容" {
		t.Fatalf("开关关闭应保持旧行为成功: res=%+v err=%v", res, err)
	}
}

// TestMalformedStreamEventsRejected 反例：中间坏事件/错误事件后即使再收到可靠终态，
// 也不能把已丢 delta 的半截内容当成功。Responses 缺 type 同样属于不可验证协议形态。
func TestMalformedStreamEventsRejected(t *testing.T) {
	setContractFlag(t, true)
	tests := []struct {
		name     string
		endpoint string
		lines    []string
		wantCode string
	}{
		{
			name: "chat_malformed_json_then_done",
			lines: []string{
				`data: {"choices":[{"delta":{"content":"半截"}}]}`,
				`data: {"choices":[`,
				`data: [DONE]`,
			},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name: "chat_error_object_then_done",
			lines: []string{
				`data: {"choices":[{"delta":{"content":"半截"}}]}`,
				`data: {"error":{"message":"upstream failed"}}`,
				`data: [DONE]`,
			},
			wantCode: RefusalLLMCallFailed,
		},
		{
			name: "chat_missing_choices_then_done",
			lines: []string{
				`data: {"choices":[{"delta":{"content":"半截"}}]}`,
				`data: {}`,
				`data: [DONE]`,
			},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name:     "responses_malformed_json_then_completed",
			endpoint: model.LLMEndpointResponses,
			lines: []string{
				`data: {"type":"response.output_text.delta","delta":"半截"}`,
				`data: {"type":`,
				`data: {"type":"response.completed","response":{"status":"completed"}}`,
			},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name:     "responses_missing_type_then_completed",
			endpoint: model.LLMEndpointResponses,
			lines: []string{
				`data: {"type":"response.output_text.delta","delta":"半截"}`,
				`data: {}`,
				`data: {"type":"response.completed","response":{"status":"completed"}}`,
			},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name:     "responses_content_filter_error",
			endpoint: model.LLMEndpointResponses,
			lines: []string{
				`data: {"type":"response.output_text.delta","delta":"半截"}`,
				`data: {"type":"error","error":{"code":"content_filter","message":"blocked"}}`,
			},
			wantCode: RefusalLLMContentFiltered,
		},
		{
			name:     "responses_top_level_content_filter_error",
			endpoint: model.LLMEndpointResponses,
			lines: []string{
				`data: {"type":"response.output_text.delta","delta":"半截"}`,
				`data: {"type":"error","code":"content_filter","message":"blocked"}`,
			},
			wantCode: RefusalLLMContentFiltered,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseServer(t, tc.lines)
			defer srv.Close()
			_, err := chatCompletionStream(context.Background(), chatParams{
				BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
			}, nil)
			if err == nil || RefusalCodeOf(err) != tc.wantCode {
				t.Fatalf("坏流事件应拒收并返回 %s: %v (code=%q)", tc.wantCode, err, RefusalCodeOf(err))
			}
		})
	}
}

func TestWholeResponseBodyIntegrityRejected(t *testing.T) {
	setContractFlag(t, true)
	tests := []struct {
		name     string
		endpoint string
		body     string
	}{
		{
			name: "chat_content_length_not_fulfilled",
			body: `{"choices":[{"message":{"content":"看似完整"},"finish_reason":"stop"}]}`,
		},
		{
			name:     "responses_content_length_not_fulfilled",
			endpoint: model.LLMEndpointResponses,
			body:     `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"看似完整"}]}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+32))
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := chatCompletion(context.Background(), chatParams{
				BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
			})
			if err == nil || RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
				t.Fatalf("整包读取中断应拒收为 %s: %v (code=%q)", RefusalLLMResponseIncomplete, err, RefusalCodeOf(err))
			}
		})
	}

	oversized := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}` + strings.Repeat(" ", aiResponseBodyLimit)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oversized))
	}))
	defer srv.Close()
	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
	})
	if err == nil || RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
		t.Fatalf("超过整包上限必须拒收，不能本地截断后继续解析: %v (code=%q)", err, RefusalCodeOf(err))
	}

	setContractFlag(t, false)
	legacyBody := `{"choices":[{"message":{"content":"旧路径"},"finish_reason":"stop"}]}`
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(legacyBody)+16))
		_, _ = w.Write([]byte(legacyBody))
	}))
	defer legacy.Close()
	res, legacyErr := chatCompletion(context.Background(), chatParams{
		BaseURL: legacy.URL, APIKey: "k", Model: "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
	})
	if legacyErr != nil || res == nil || res.Content != "旧路径" {
		t.Fatalf("契约关闭应保留旧整包读错兼容路径: res=%+v err=%v", res, legacyErr)
	}
}

// TestChatStreamFinishReasonRejected 反例：finish_reason=length（截断）与 content_filter
// （拦截）即使带内容也拒收；finish_reason=stop 正常成功。
func TestChatStreamFinishReasonRejected(t *testing.T) {
	setContractFlag(t, true)
	cases := []struct {
		reason  string
		wantErr string
	}{
		{"length", "length"},
		{"content_filter", "content_filter"},
		{"stop", ""},
	}
	for _, tc := range cases {
		lines := []string{
			`data: {"choices":[{"delta":{"content":"部分输出"}}]}`,
			fmt.Sprintf(`data: {"choices":[{"delta":{},"finish_reason":%q}]}`, tc.reason),
			`data: [DONE]`,
		}
		srv := sseServer(t, lines)
		p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m",
			Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
		res, err := chatCompletionStream(context.Background(), p, nil)
		srv.Close()
		if tc.wantErr == "" {
			if err != nil || res.Content != "部分输出" || res.FinishReason != "stop" {
				t.Fatalf("[%s] 应成功: res=%+v err=%v", tc.reason, res, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("[%s] 应拒收且错误含 %q: %v", tc.reason, tc.wantErr, err)
		}
	}
}

// TestChatFinishReasonWhitelistFailClosed 未知、空值和大小写/空白变体不能绕过完成状态门禁。
// 流式 [DONE] 仅能证明传输收尾；若同时给出 finish_reason，则仍必须是 stop/tool_calls。
func TestChatFinishReasonWhitelistFailClosed(t *testing.T) {
	setContractFlag(t, true)
	cases := []struct {
		reason string
		wantOK bool
	}{
		{" STOP ", true},
		{"tool_calls", true},
		{"", true}, // 只有 [DONE] 时允许缺省；非流式单独覆盖在下方
		{"max_tokens", false},
		{"failed", false},
		{"cancelled", false},
		{"vendor_complete", false},
	}
	for _, tc := range cases {
		lines := []string{`data: {"choices":[{"delta":{"content":"ok"}}]}`}
		if tc.reason != "" {
			lines = append(lines, fmt.Sprintf(`data: {"choices":[{"delta":{},"finish_reason":%q}]}`, tc.reason))
		}
		lines = append(lines, `data: [DONE]`)
		srv := sseServer(t, lines)
		p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
		res, err := chatCompletionStream(context.Background(), p, nil)
		srv.Close()
		if tc.wantOK {
			if err != nil || res.Content != "ok" {
				t.Fatalf("finish_reason=%q 应成功: res=%+v err=%v", tc.reason, res, err)
			}
		} else if err == nil {
			t.Fatalf("finish_reason=%q 应 fail-closed", tc.reason)
		} else {
			wantCode := RefusalLLMResponseIncomplete
			if tc.reason == "failed" || tc.reason == "cancelled" {
				wantCode = RefusalLLMCallFailed
			}
			if got := RefusalCodeOf(err); got != wantCode {
				t.Fatalf("finish_reason=%q code=%q, want %q", tc.reason, got, wantCode)
			}
		}
	}

	// 非流式完整 JSON 允许缺失 finish_reason（兼容网关）；未知枚举仍必须拒收。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"vendor_complete"}]}`))
	}))
	defer srv.Close()
	if _, err := chatCompletion(context.Background(), chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}); err == nil {
		t.Fatal("非流式未知 finish_reason 应拒收")
	}
}

// TestChatPlainFinishLengthRejected 整包 JSON（假流式回落路径）的 finish_reason=length
// 同样拒收——截断的输出即使 JSON 碰巧仍合法也不可信。
func TestChatPlainFinishLengthRejected(t *testing.T) {
	setContractFlag(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"a\":1}"},"finish_reason":"length"}],"usage":{"total_tokens":9}}`))
	}))
	defer srv.Close()
	p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", JSONMode: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
	if _, err := chatCompletion(context.Background(), p); err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("整包 length 应拒收: %v", err)
	}
}

func TestChatPlain200ErrorEnvelopeRejected(t *testing.T) {
	setContractFlag(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"server_error"},"choices":[{"message":{"content":"不得接收"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
	})
	if err == nil || RefusalCodeOf(err) != RefusalLLMCallFailed {
		t.Fatalf("HTTP 200 error 包络必须优先拒绝，不能接收 choices: %v (code=%q)", err, RefusalCodeOf(err))
	}
}

func TestResponsesPlain200ErrorFlagCompatibility(t *testing.T) {
	setContractFlag(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","error":{"code":"vendor_warning"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"旧网关内容"}]}]}`))
	}))
	defer srv.Close()
	p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses,
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
	if _, err := chatCompletion(context.Background(), p); err == nil || RefusalCodeOf(err) != RefusalLLMCallFailed {
		t.Fatalf("契约开启时 code-only error 必须拒收: %v", err)
	}

	setContractFlag(t, false)
	if res, err := chatCompletion(context.Background(), p); err != nil || res.Content != "旧网关内容" {
		t.Fatalf("契约关闭应保留 Responses 旧兼容路径: res=%+v err=%v", res, err)
	}
}

func TestStructuredRefusalClassifiedAsContentFiltered(t *testing.T) {
	setContractFlag(t, true)
	tests := []struct {
		name     string
		endpoint string
		ctype    string
		body     string
	}{
		{
			name:  "chat_plain",
			ctype: "application/json",
			body:  `{"choices":[{"message":{"content":null,"refusal":"policy denied"},"finish_reason":"stop"}]}`,
		},
		{
			name:  "chat_stream",
			ctype: "text/event-stream",
			body:  "data: {\"choices\":[{\"delta\":{\"refusal\":\"policy denied\"}}]}\n\n",
		},
		{
			name:     "responses_plain",
			endpoint: model.LLMEndpointResponses,
			ctype:    "application/json",
			body:     `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"policy denied"}]}]}`,
		},
		{
			name:     "responses_stream",
			endpoint: model.LLMEndpointResponses,
			ctype:    "text/event-stream",
			body:     "data: {\"type\":\"response.refusal.delta\",\"delta\":\"policy denied\"}\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := chatCompletion(context.Background(), chatParams{
				BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
			})
			if err == nil || RefusalCodeOf(err) != RefusalLLMContentFiltered {
				t.Fatalf("结构化 refusal 应归类为 %s: %v (code=%q)", RefusalLLMContentFiltered, err, RefusalCodeOf(err))
			}
		})
	}
}

// TestResponsesStatusGate Responses 端点：status=incomplete 带内容也拒收、completed 成功；
// 流式 incomplete 完成事件拒收、EOF 无完成事件拒收。
func TestResponsesStatusGate(t *testing.T) {
	setContractFlag(t, true)

	// 非流式整包（流式请求被假上游以整包 JSON 应答 → 假流式回落路径 → parseResponsesBody）。
	mk := func(status string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			body := map[string]any{
				"status": status,
				"output": []map[string]any{{"type": "message", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": "有内容"}}}},
				"usage": map[string]int{"input_tokens": 3, "output_tokens": 4, "total_tokens": 7},
			}
			if status == "incomplete" {
				body["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
			}
			b, _ := json.Marshal(body)
			_, _ = w.Write(b)
		}))
	}
	srv := mk("incomplete")
	p := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses,
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
	if _, err := chatCompletion(context.Background(), p); err == nil ||
		!strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("responses incomplete 带内容也应拒收: %v", err)
	}
	srv.Close()

	srv = mk("completed")
	p.BaseURL = srv.URL
	if res, err := chatCompletion(context.Background(), p); err != nil || res.Content != "有内容" {
		t.Fatalf("responses completed 应成功: res=%+v err=%v", res, err)
	}
	srv.Close()

	srv = mk("failed")
	p.BaseURL = srv.URL
	if _, err := chatCompletion(context.Background(), p); err == nil || RefusalCodeOf(err) != RefusalLLMCallFailed {
		t.Fatalf("responses failed 应归类为调用失败: %v (code=%q)", err, RefusalCodeOf(err))
	}
	srv.Close()

	filtered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"半截"}]}]}`))
	}))
	p.BaseURL = filtered.URL
	if _, err := chatCompletion(context.Background(), p); err == nil || RefusalCodeOf(err) != RefusalLLMContentFiltered {
		t.Fatalf("Responses 非流式 content_filter 应独立分类: %v (code=%q)", err, RefusalCodeOf(err))
	}
	filtered.Close()

	// 流式：incomplete 完成事件拒收。
	srv = sseServer(t, []string{
		`data: {"type":"response.output_text.delta","delta":"半截"}`,
		`data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	})
	p.BaseURL = srv.URL
	if _, err := chatCompletionStream(context.Background(), p, nil); err == nil ||
		!strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("responses 流式 incomplete 应拒收: %v", err)
	}
	srv.Close()

	// 流式：EOF 无完成事件拒收。
	srv = sseServer(t, []string{`data: {"type":"response.output_text.delta","delta":"半截"}`})
	p.BaseURL = srv.URL
	if _, err := chatCompletionStream(context.Background(), p, nil); err == nil ||
		!strings.Contains(err.Error(), "eof_without_marker") {
		t.Fatalf("responses 流式 EOF 无完成事件应拒收: %v", err)
	}
	srv.Close()
}

// TestResponsesStreamTerminalStateFailClosed 反例覆盖 Responses 的终态边界：裸 [DONE]、
// response.done/incomplete、completed 但状态缺失/失败均不得把半截内容当成功。
func TestResponsesStreamTerminalStateFailClosed(t *testing.T) {
	setContractFlag(t, true)
	cases := []struct {
		name     string
		lines    []string
		ok       bool
		wantCode string
	}{
		{
			name:     "delta_then_done_marker_only",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: [DONE]`},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name:     "response_done_incomplete",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: {"type":"response.done","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name:     "response_completed_missing_status",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: {"type":"response.completed","response":{}}`},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name:     "response_completed_failed_status",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: {"type":"response.completed","response":{"status":"failed"}}`},
			wantCode: RefusalLLMCallFailed,
		},
		{
			name:     "response_completed_with_error",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: {"type":"response.completed","response":{"status":"completed","error":{"code":"server_error"}}}`},
			wantCode: RefusalLLMCallFailed,
		},
		{
			name:     "response_completed_with_incomplete_details",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: {"type":"response.completed","response":{"status":"completed","incomplete_details":{"reason":"max_output_tokens"}}}`},
			wantCode: RefusalLLMResponseIncomplete,
		},
		{
			name:     "response_incomplete_with_content_filter_error",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: {"type":"response.incomplete","response":{"status":"incomplete","error":{"code":"content_filter"}}}`},
			wantCode: RefusalLLMContentFiltered,
		},
		{
			name:     "response_incomplete_with_server_error",
			lines:    []string{`data: {"type":"response.output_text.delta","delta":"半截"}`, `data: {"type":"response.incomplete","response":{"status":"incomplete","error":{"code":"server_error"}}}`},
			wantCode: RefusalLLMCallFailed,
		},
		{
			name: "response_incomplete_with_refusal",
			lines: []string{`data: {"type":"response.output_text.delta","delta":"半截"}`,
				`data: {"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"policy denied"}]}]}}`},
			wantCode: RefusalLLMContentFiltered,
		},
		{
			name:  "response_completed_ok",
			lines: []string{`data: {"type":"response.output_text.delta","delta":"完整"}`, `data: {"type":"response.completed","response":{"status":"completed"}}`},
			ok:    true,
		},
		{
			name:  "response_done_ok",
			lines: []string{`data: {"type":"response.output_text.delta","delta":"完整"}`, `data: {"type":"response.done","response":{"status":"completed"}}`},
			ok:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseServer(t, tc.lines)
			defer srv.Close()
			res, err := chatCompletionStream(context.Background(), chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses, Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}, nil)
			if tc.ok {
				if err != nil || res.Content != "完整" || res.FinishReason != "completed" {
					t.Fatalf("completed 应成功并保留状态: res=%+v err=%v", res, err)
				}
			} else if err == nil {
				t.Fatalf("%s 应被拒收，res=%+v", tc.name, res)
			} else if got := RefusalCodeOf(err); got != tc.wantCode {
				t.Fatalf("%s code=%q, want %q: %v", tc.name, got, tc.wantCode, err)
			}
		})
	}

	// 开关关闭时保持旧 completed 兼容路径：incomplete_details 不得造成 (nil,nil) 或 panic。
	setContractFlag(t, false)
	srv := sseServer(t, []string{
		`data: {"type":"response.output_text.delta","delta":"旧路径"}`,
		`data: {"type":"response.completed","response":{"status":"completed","incomplete_details":{"reason":"max_output_tokens"}}}`,
	})
	defer srv.Close()
	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses,
		Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
	}, nil)
	if err != nil || res == nil || res.Content != "旧路径" {
		t.Fatalf("契约关闭应保持旧 completed 路径且不得返回 nil,nil: res=%+v err=%v", res, err)
	}
}

// TestRepairTemperatureZero 端到端：repair 轮上游收到的 temperature 必须是 0，
// 非 repair 结构化轮收到的是钳制后的 0.2。
func TestRepairTemperatureZero(t *testing.T) {
	setContractFlag(t, true)
	var gotTemps []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Temperature float64 `json:"temperature"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotTemps = append(gotTemps, body.Temperature)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	defer srv.Close()

	base := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", JSONMode: true,
		Temperature: 0.9, Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true}
	if _, err := chatCompletion(context.Background(), base); err != nil {
		t.Fatalf("首轮应成功: %v", err)
	}
	repairP := base
	repairP.Repair = true
	if _, err := chatCompletion(context.Background(), repairP); err != nil {
		t.Fatalf("repair 轮应成功: %v", err)
	}
	if len(gotTemps) != 2 || gotTemps[0] != llmStructuredTempCap || gotTemps[1] != 0 {
		t.Fatalf("上游收到的温度应为 [%v 0], got %v", llmStructuredTempCap, gotTemps)
	}
}

// TestRefusalErrorCode 机读拒答码：直接与 wrap 后均可经 errors.As / RefusalCodeOf 提取。
func TestRefusalErrorCode(t *testing.T) {
	err := refusalErrf(RefusalStaleQuote, "行情已过期（仅更新至 %s）", "09-30 15:00")
	var re *RefusalError
	if !errors.As(err, &re) || re.Code != RefusalStaleQuote {
		t.Fatalf("errors.As 应提取到 code: %v", err)
	}
	if RefusalCodeOf(err) != RefusalStaleQuote {
		t.Fatalf("RefusalCodeOf 不符: %q", RefusalCodeOf(err))
	}
	wrapped := fmt.Errorf("外层: %w", err)
	if RefusalCodeOf(wrapped) != RefusalStaleQuote {
		t.Fatalf("wrap 后仍应可提取: %q", RefusalCodeOf(wrapped))
	}
	if RefusalCodeOf(errors.New("普通错误")) != "" {
		t.Fatal("普通错误应返回空码")
	}
	// 配额用尽与 LLM 配置不可用走同一机读码体系（checkQuota / ResolveForUse）。
	if RefusalCodeOf(errQuotaExhausted) != RefusalQuotaExhausted {
		t.Fatalf("errQuotaExhausted 应挂 %s", RefusalQuotaExhausted)
	}
	if RefusalCodeOf(asLLMUnavailable(errors.New("尚未配置任何 LLM，请先在设置中添加"))) != RefusalLLMUnavailable {
		t.Fatal("asLLMUnavailable 应挂 llm_unavailable")
	}
	if RefusalCodeOf(asLLMUnavailable(errQuotaExhausted)) != RefusalQuotaExhausted {
		t.Fatal("asLLMUnavailable 不得覆盖已有 RefusalError 码")
	}
	if RefusalCodeOf(refusalErr(RefusalMarketCalendarUnknown, "日历未知")) != RefusalMarketCalendarUnknown {
		t.Fatal("日报日历未知应保留独立机读码")
	}
	if got := RefusalCodeOf(responsesStatusReject(true, "incomplete", "content_filter")); got != RefusalLLMContentFiltered {
		t.Fatalf("Responses content_filter 应独立分类为 %s，got %q", RefusalLLMContentFiltered, got)
	}
}

func TestClassifyLLMErrorCodes(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{errors.New("内容被上游安全策略拦截（finish_reason=content_filter）"), RefusalLLMContentFiltered},
		{errors.New("流式响应结束但未收到终止标记（eof_without_marker）"), RefusalLLMResponseIncomplete},
		{errors.New("LLM 返回 HTTP 502：bad gateway"), RefusalLLMCallFailed},
		{errQuotaExhausted, RefusalQuotaExhausted},
	}
	for _, tc := range cases {
		if got := RefusalCodeOf(classifyLLMError(tc.err)); got != tc.code {
			t.Fatalf("classifyLLMError(%v) code=%q, want %q", tc.err, got, tc.code)
		}
	}
}

func TestCompareAIRefusalCodePreservesIncompleteResponse(t *testing.T) {
	setContractFlag(t, true)
	srv := sseServer(t, []string{
		`data: {"choices":[{"delta":{"content":"半截点评"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
	})
	defer srv.Close()
	common.EncryptionKey = "unit-test-key"
	cipher, err := common.Encrypt("sk-test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &model.LLMConfig{UserID: 9911, Name: "compare-code", Provider: "openai", BaseURL: srv.URL,
		APIKeyCipher: cipher, Model: "m", IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatal(err)
	}
	svc := &CompareService{llm: NewLLMService()}
	rows := []CompareRow{
		{Symbol: "600001", Market: "cn", Name: "甲", QuoteOK: true, Price: 10},
		{Symbol: "600002", Market: "cn", Name: "乙", QuoteOK: true, Price: 12},
	}
	comment, _, code, _, _ := svc.aiComment(context.Background(), 9911, true, cfg.ID, rows)
	if comment != "" || code != RefusalLLMResponseIncomplete {
		t.Fatalf("Compare 必须把中央不完整响应码写入 ai_refusal_code: comment=%q code=%q", comment, code)
	}
}

func TestCompareFreshnessRefusalPrecedesLLMResolution(t *testing.T) {
	svc := &CompareService{}
	rows := []CompareRow{{Symbol: "600001", Market: "cn", QuoteOK: true, Price: 10}}
	comment, _, code, _, _ := svc.aiComment(context.Background(), 9912, false, 0, rows)
	if comment != "" || code != RefusalFreshQuotesInsufficient {
		t.Fatalf("fresh 行情不足应先于 LLM 配置/配额拒答: comment=%q code=%q", comment, code)
	}
}

func TestP0SevenFloorsRemainEnabledWhenContractFlagOff(t *testing.T) {
	setContractFlag(t, false)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	common.DB.Where("content_hash LIKE ?", "p0-seven-flag-off-%").Delete(&model.News{})

	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.Local)
	daily := &DailyReportService{nowFn: func() time.Time { return now }}
	if _, err := daily.GenerateFor(context.Background(), 9920, true); RefusalCodeOf(err) != RefusalMarketCalendarUnknown {
		t.Fatalf("flag 关闭不得放松日报日历门: %v (code=%q)", err, RefusalCodeOf(err))
	}

	news := []model.News{
		{Title: "窗口内", RelatedSymbols: `["600888"]`, PublishTime: now.Add(-time.Hour), ContentHash: "p0-seven-flag-off-valid"},
		{Title: "未来", RelatedSymbols: `["600888"]`, PublishTime: now.Add(time.Hour), ContentHash: "p0-seven-flag-off-future"},
		{Title: "过旧", RelatedSymbols: `["600888"]`, PublishTime: now.Add(-8 * 24 * time.Hour), ContentHash: "p0-seven-flag-off-old"},
	}
	if err := common.DB.Create(&news).Error; err != nil {
		t.Fatal(err)
	}
	briefs := latestNewsBriefsAt("600888", 10, now)
	if len(briefs) != 1 || briefs[0].Title != "窗口内" {
		t.Fatalf("flag 关闭不得放松新闻双边窗口: %+v", briefs)
	}

	compare := &CompareService{}
	_, _, code, _, _ := compare.aiComment(context.Background(), 9920, false, 0,
		[]CompareRow{{Symbol: "600888", Market: "cn", QuoteOK: true, Price: 10}})
	if code != RefusalFreshQuotesInsufficient {
		t.Fatalf("flag 关闭不得放松 Compare fresh 门: code=%q", code)
	}
}

func TestReviewRepairTemperatureAndAttemptLimit(t *testing.T) {
	setContractFlag(t, true)
	var temperatures []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Temperature float64 `json:"temperature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}
		temperatures = append(temperatures, body.Temperature)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not-json"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", Temperature: 0.9, MaxTokens: 64}

	analysis := &AnalysisService{}
	review, _, _ := analysis.reviewAnalysis(context.Background(), 9921, cfg, "k", true, "stock",
		map[string]any{"price": 10}, &AnalysisResult{Rating: model.AnalysisRatingNeutral, Summary: "x"}, "", "")
	if review != nil {
		t.Fatalf("两轮无效输出后 analysis_review 应确定性放弃: %+v", review)
	}

	recommendation := &RecommendationService{}
	picks := []recPick{{Symbol: "600001", Action: model.RecActionWatch}}
	pool := map[string]candidate{"600001": {Symbol: "600001", Name: "甲", Price: 10}}
	reviews, _, _, _ := recommendation.reviewPicks(context.Background(), 9921, cfg, "k", true,
		model.RecTypeShortTerm, picks, pool, "", "")
	if reviews != nil {
		t.Fatalf("两轮无效输出后 rec_review 应确定性放弃: %+v", reviews)
	}

	want := []float64{llmStructuredTempCap, 0, llmStructuredTempCap, 0}
	if len(temperatures) != len(want) {
		t.Fatalf("两个 review 各只能首轮+1 次 repair，got %d 次: %v", len(temperatures), temperatures)
	}
	for i := range want {
		if temperatures[i] != want[i] {
			t.Fatalf("review 第 %d 轮温度=%v, want %v；全部=%v", i+1, temperatures[i], want[i], temperatures)
		}
	}
}

func TestAccuracyContractFlagSnapshottedPerCall(t *testing.T) {
	t.Run("enabled_call_stays_strict_after_global_disable", func(t *testing.T) {
		setContractFlag(t, true)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"半截\"}}]}\n\n"))
			flusher.Flush()
			if err := setting.SetLLMAccuracyContract(false); err != nil {
				t.Errorf("切换 flag 失败: %v", err)
			}
			_, _ = w.Write([]byte("data: {\"choices\":[\n\ndata: [DONE]\n\n"))
			flusher.Flush()
		}))
		defer srv.Close()
		_, err := chatCompletionStream(context.Background(), chatParams{
			BaseURL: srv.URL, APIKey: "k", Model: "m", AllowPrivate: true,
			Messages: []chatMessage{{Role: "user", Content: "hi"}},
		}, nil)
		if err == nil || RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
			t.Fatalf("调用开始时为严格模式，中途关 flag 也必须拒收坏事件: %v (code=%q)", err, RefusalCodeOf(err))
		}
	})

	t.Run("legacy_call_stays_legacy_after_global_enable", func(t *testing.T) {
		setContractFlag(t, false)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"旧路径\"}}]}\n\n"))
			flusher.Flush()
			if err := setting.SetLLMAccuracyContract(true); err != nil {
				t.Errorf("切换 flag 失败: %v", err)
			}
			_, _ = w.Write([]byte("data: {\"choices\":[\n\ndata: [DONE]\n\n"))
			flusher.Flush()
		}))
		defer srv.Close()
		res, err := chatCompletionStream(context.Background(), chatParams{
			BaseURL: srv.URL, APIKey: "k", Model: "m", AllowPrivate: true,
			Messages: []chatMessage{{Role: "user", Content: "hi"}},
		}, nil)
		if err != nil || res.Content != "旧路径" {
			t.Fatalf("调用开始时为兼容模式，中途开 flag 只能影响下一次调用: res=%+v err=%v", res, err)
		}
	})
}

// TestRepairOverLimitStops 反例：模块 repair 上限到达后不得再隐式多试一轮。
// analysis 主链路 maxRepairAttempts=2 → 总调用 = 1 首轮 + 2 repair = 3；超过则停止。
// 模拟 parse 恒失败：上游被调用次数必须恰好等于上限，不得多打。
func TestRepairOverLimitStops(t *testing.T) {
	setContractFlag(t, true)
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// 故意返回无法通过 parse 的内容，迫使走满 repair 上限。
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not-json"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	svc := &AnalysisService{}
	parseAlwaysFail := func(string) error { return errors.New("故意校验失败") }
	_, _, _, err := svc.callWithRepair(
		context.Background(), 1, newLLMRun("", "", "analysis", "analysis.v1", ""),
		&model.LLMConfig{BaseURL: srv.URL, Model: "m", Temperature: 0.5, MaxTokens: 64},
		"k", true,
		[]chatMessage{{Role: "user", Content: "x"}},
		parseAlwaysFail,
		analysisRepairHint,
	)
	// callWithRepair 在用尽 repair 后返回 callErr=nil + 最后原文（调用方走 degraded）；
	// 关键断言是「不多打一轮」——总次数 = 1 + maxRepairAttempts。
	if err != nil {
		t.Fatalf("用尽 repair 应降级返回 nil callErr, got %v", err)
	}
	wantCalls := 1 + maxRepairAttempts
	if callCount != wantCalls {
		t.Fatalf("repair 超限后不得隐式再试：期望 %d 次上游调用, got %d", wantCalls, callCount)
	}
}

// TestLatestNewsBriefsWindow news 注入窗口（P0-7）：7 天窗口外旧闻不注入、Time 带年份。
func TestLatestNewsBriefsWindow(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(func() { common.DB.Where("1=1").Delete(&model.News{}) })
	common.DB.Where("1=1").Delete(&model.News{})

	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.Local)
	seed := []model.News{
		{Title: "两天前的新闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(-2 * 24 * time.Hour), ContentHash: "h1", Sentiment: "positive"},
		{Title: "六天前的新闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(-6 * 24 * time.Hour), ContentHash: "h2"},
		{Title: "恰好七天边界新闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(-7 * 24 * time.Hour), ContentHash: "h4"},
		{Title: "十天前的旧闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(-10 * 24 * time.Hour), ContentHash: "h3"},
		{Title: "未来时间新闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(time.Hour), ContentHash: "h5"},
		{Title: "跨年新闻", RelatedSymbols: `["600000"]`, PublishTime: time.Date(2025, 12, 31, 23, 0, 0, 0, time.Local), ContentHash: "h6"},
	}
	for i := range seed {
		if err := common.DB.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed 新闻失败: %v", err)
		}
	}

	briefs := latestNewsBriefsAt("600000", 10, now)
	if len(briefs) != 4 {
		t.Fatalf("窗口应包含 2/6/恰好7天/跨年四条，排除旧闻和未来（期望 4 条）, got %d: %+v", len(briefs), briefs)
	}
	for _, b := range briefs {
		if strings.Contains(b.Title, "十天前") || strings.Contains(b.Title, "未来") {
			t.Fatalf("窗口外或未来新闻不应注入: %+v", briefs)
		}
		if len(b.Time) != len("2006-01-02 15:04") {
			t.Fatalf("Time 应带年份的完整时点: %q", b.Time)
		}
	}
}
