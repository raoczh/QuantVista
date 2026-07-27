package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatCompletionStream SSE 逐行剥 data: 前缀、delta 增量回调、[DONE] 终止、usage 捕获。
func TestChatCompletionStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		lines := []string{
			`data: {"choices":[{"delta":{"content":"现价 "}}]}`,
			``,
			`: keep-alive 注释行应被忽略`,
			`data: {"choices":[{"delta":{"content":"12.34"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":null}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			`data: [DONE]`,
		}
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()

	var chunks []string
	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	}, func(c string) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if res.Content != "现价 12.34" {
		t.Fatalf("聚合内容不符: %q", res.Content)
	}
	if len(chunks) != 2 || chunks[0] != "现价 " || chunks[1] != "12.34" {
		t.Fatalf("增量回调不符: %v", chunks)
	}
	if res.Usage.TotalTokens != 15 {
		t.Fatalf("应捕获最后 chunk 的 usage: %+v", res.Usage)
	}
}

// TestChatCompletionStream_FinishReason 无 [DONE] 行、靠 finish_reason 终止；usage 缺失走字符粗估。
func TestChatCompletionStream_FinishReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"答案"}}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n"))
	}))
	defer srv.Close()

	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	}, nil)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if res.Content != "答案" {
		t.Fatalf("内容不符: %q", res.Content)
	}
	if res.Usage.TotalTokens == 0 {
		t.Fatal("usage 缺失应走字符粗估")
	}
}

// TestChatCompletionStream_UsageChunkAfterFinishReason 真实 OpenAI 的 include_usage 排序：
// usage 专属 chunk（choices:[] + usage）排在 finish_reason **之后**、[DONE] 之前。
// 曾在 finish_reason 处直接 break 导致该 chunk 永远读不到、usage 恒回落字符粗估——
// 粗估会污染 llm_router 的 cost_exceeded 自动回退判定（chat 粗估 vs responses 真值混比）。
func TestChatCompletionStream_UsageChunkAfterFinishReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		lines := []string{
			`data: {"choices":[{"delta":{"content":"结论"},"finish_reason":null}],"usage":null}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":null}`,
			`data: {"choices":[],"usage":{"prompt_tokens":800,"completion_tokens":120,"total_tokens":920}}`,
			`data: [DONE]`,
		}
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()

	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	}, nil)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if res.Content != "结论" {
		t.Fatalf("内容不符: %q", res.Content)
	}
	// 关键断言：拿到上游真值 920，而不是 estimateUsage 对 "结论" 的粗估（个位数）。
	if res.Usage.TotalTokens != 920 || res.Usage.PromptTokens != 800 || res.Usage.CompletionTokens != 120 {
		t.Fatalf("应捕获 finish_reason 之后的 usage 专属 chunk，实得: %+v", res.Usage)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("终态应为 stop: %q", res.FinishReason)
	}
}

// TestChatCompletionStream_NoUsageAfterFinishStillTerminates 上游报完 finish_reason 后
// 只发无关事件、既不给 usage 也不发 [DONE]：续读窗口用满即收尾，不挂到 ctx deadline，
// 且已聚合的正文与终态照常返回（usage 回落粗估）。
func TestChatCompletionStream_NoUsageAfterFinishStillTerminates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"答案"}}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n"))
		fl.Flush()
		// 远超续读窗口的无关事件：不给 usage、不发 [DONE]。
		for i := 0; i < 50; i++ {
			_, _ = w.Write([]byte(`data: {"choices":[]}` + "\n"))
			fl.Flush()
		}
		// 不主动收尾：等客户端断开（续读上限生效即断），handler 才返回——
		// 直接 select{} 会让 srv.Close() 永久阻塞。
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := chatCompletionStream(ctx, chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	}, nil)
	if err != nil {
		t.Fatalf("续读用满应正常收尾: %v", err)
	}
	if res.Content != "答案" || res.FinishReason != "stop" {
		t.Fatalf("正文/终态不符: %q %q", res.Content, res.FinishReason)
	}
	if res.Usage.TotalTokens == 0 {
		t.Fatal("拿不到上游 usage 时应回落字符粗估")
	}
}

// TestChatCompletionStream_HTTPError 建流前的 4xx 报带状态提示的错误。
func TestChatCompletionStream_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	_, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("应报 401 错误: %v", err)
	}
}

// TestChatCompletionStream_Empty 全程无内容判失败（不落半截/空回答）。
func TestChatCompletionStream_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	_, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "空内容") {
		t.Fatalf("空流应报错: %v", err)
	}
}
