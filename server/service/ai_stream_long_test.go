package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"quantvista/model"
)

// TestLLMStreamSurvivesProxyHeaderWindow 验证真正的 SSE 请求能在代理首响应头窗口之外
// 持续接收分片，并且总耗时超过客户端整体 timeout 仍成功。它锁定请求体/请求头契约和
// 流式 client 的无整体 timeout；真实中转站是否对响应 body 做缓冲仍需部署环境验收。
// 这里用毫秒级窗口代替线上常见的 60 秒，保证回归测试可快速运行。
func TestLLMStreamSurvivesProxyHeaderWindow(t *testing.T) {
	const (
		proxyHeaderWindow = 40 * time.Millisecond
		chunkInterval     = 15 * time.Millisecond
		chunkCount        = 8
	)

	for _, tc := range []struct {
		name         string
		endpointType string
		wantPath     string
	}{
		{name: "chat_completions", endpointType: model.LLMEndpointChat, wantPath: "/v1/chat/completions"},
		{name: "responses", endpointType: model.LLMEndpointResponses, wantPath: "/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.wantPath {
					t.Errorf("上游路径不符: got=%q want=%q", r.URL.Path, tc.wantPath)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("解析请求体失败: %v", err)
				}
				isSSE := body["stream"] == true &&
					strings.Contains(r.Header.Get("Accept"), "text/event-stream") &&
					strings.Contains(r.Header.Get("Cache-Control"), "no-cache") &&
					r.Header.Get("Accept-Encoding") == "identity"
				if !isSSE {
					// 非 SSE 契约故意拖过代理首响应头窗口，由代理返回 504。
					time.Sleep(3 * proxyHeaderWindow)
					return
				}

				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Error("httptest ResponseWriter 应支持 Flush")
					return
				}
				flusher.Flush()
				for i := 0; i < chunkCount; i++ {
					if tc.endpointType == model.LLMEndpointResponses {
						_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"x"}` + "\n\n"))
					} else {
						_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n"))
					}
					flusher.Flush()
					time.Sleep(chunkInterval)
				}
				if tc.endpointType == model.LLMEndpointResponses {
					_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"))
				} else {
					_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
				}
				flusher.Flush()
			}))
			defer upstream.Close()

			upstreamURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
			proxy.Transport = &http.Transport{ResponseHeaderTimeout: proxyHeaderWindow}
			proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			}
			gateway := httptest.NewServer(proxy)
			defer gateway.Close()

			started := time.Now()
			res, err := chatCompletion(context.Background(), chatParams{
				BaseURL: gateway.URL, APIKey: "k", Model: "m", EndpointType: tc.endpointType,
				Messages: []chatMessage{{Role: "user", Content: "hi"}}, AllowPrivate: true,
			})
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("持续分片的长 SSE 不应被代理空闲窗口切断: %v", err)
			}
			if res.Content != strings.Repeat("x", chunkCount) {
				t.Fatalf("聚合内容不符: %q", res.Content)
			}
			if elapsed <= 2*proxyHeaderWindow {
				t.Fatalf("测试未覆盖总时长超过代理窗口: elapsed=%s window=%s", elapsed, proxyHeaderWindow)
			}
		})
	}
}

func TestAIStreamClientHasNoOverallTimeout(t *testing.T) {
	client := newAIStreamClient(true)
	if client.Timeout != 0 {
		t.Fatalf("流式 client 不应设置整体超时: %s", client.Timeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("流式 client Transport 类型异常: %T", client.Transport)
	}
	if !tr.DisableCompression {
		t.Fatal("流式 Transport 应禁用自动压缩，避免 SSE 分片被缓冲")
	}
}
