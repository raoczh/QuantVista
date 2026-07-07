package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
