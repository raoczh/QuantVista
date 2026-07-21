package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestApplyAccuracyContract 契约注入与温度语义：开=注入 ac1 首条 system + JSONMode 钳 0.2 +
// repair 归 0（自由文本温度不动）；关=原样直通。
func TestApplyAccuracyContract(t *testing.T) {
	setContractFlag(t, true)
	base := chatParams{
		Temperature: 0.9,
		Messages:    []chatMessage{{Role: "system", Content: "模块提示"}, {Role: "user", Content: "问题"}},
	}

	jsonP := base
	jsonP.JSONMode = true
	got := applyAccuracyContract(jsonP)
	if got.Temperature != llmStructuredTempCap {
		t.Fatalf("结构化调用温度应钳到 %.1f, got %v", llmStructuredTempCap, got.Temperature)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" ||
		!strings.Contains(got.Messages[0].Content, "准确性契约 "+llmAccuracyContractVersion) {
		t.Fatalf("应 prepend ac1 契约系统消息: %+v", got.Messages)
	}
	if got.Messages[1].Content != "模块提示" || got.Messages[2].Content != "问题" {
		t.Fatalf("原消息应原序后移: %+v", got.Messages)
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
	if got := applyAccuracyContract(jsonP); got.Temperature != 0.9 || len(got.Messages) != 2 {
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

// TestChatStreamFinishReasonRejected 反例：finish_reason=length（截断）与 content_filter
//（拦截）即使带内容也拒收；finish_reason=stop 正常成功。
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
}

// TestLatestNewsBriefsWindow news 注入窗口（P0-7）：7 天窗口外旧闻不注入、Time 带年份。
func TestLatestNewsBriefsWindow(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(func() { common.DB.Where("1=1").Delete(&model.News{}) })
	common.DB.Where("1=1").Delete(&model.News{})

	now := time.Now()
	seed := []model.News{
		{Title: "两天前的新闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(-2 * 24 * time.Hour), ContentHash: "h1", Sentiment: "positive"},
		{Title: "六天前的新闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(-6 * 24 * time.Hour), ContentHash: "h2"},
		{Title: "十天前的旧闻", RelatedSymbols: `["600000"]`, PublishTime: now.Add(-10 * 24 * time.Hour), ContentHash: "h3"},
	}
	for i := range seed {
		if err := common.DB.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed 新闻失败: %v", err)
		}
	}

	briefs := latestNewsBriefs("600000", 5)
	if len(briefs) != 2 {
		t.Fatalf("窗口外旧闻应被过滤（期望 2 条）, got %d: %+v", len(briefs), briefs)
	}
	for _, b := range briefs {
		if strings.Contains(b.Title, "十天前") {
			t.Fatalf("十天前旧闻不应注入: %+v", briefs)
		}
		if len(b.Time) != len("2006-01-02 15:04") {
			t.Fatalf("Time 应带年份的完整时点: %q", b.Time)
		}
	}
}
