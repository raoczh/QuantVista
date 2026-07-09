package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTestOpenAICompatible_HTMLNotOK 测试连接的核心防回归：200 + HTML（SPA fallback / 网关拦截页）
// 不算连接成功——否则会"测试通过、实际分析失败（invalid character '<'）"。
func TestTestOpenAICompatible_HTMLNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>console</body></html>"))
	}))
	defer srv.Close()

	r := (&LLMService{}).testOpenAICompatible("", srv.URL, "sk-x", "m", true)
	if r.OK {
		t.Fatalf("200+HTML 不应判为连接成功: %+v", r)
	}
	if !strings.Contains(r.Message, "网页") {
		t.Fatalf("应提示返回了网页: %s", r.Message)
	}
}

// TestTestOpenAICompatible_OKAndEndpoint 合法 chat completion 响应判成功，
// 且请求路径与真实分析调用同口径（根地址自动补 /v1/chat/completions）。
func TestTestOpenAICompatible_OKAndEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer srv.Close()

	r := (&LLMService{}).testOpenAICompatible("", srv.URL, "sk-x", "m", true)
	if !r.OK {
		t.Fatalf("合法响应应判成功: %+v", r)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("测试连接应与真实调用同端点，实际 %s", gotPath)
	}
}

// TestTestOpenAICompatible_JSONWithoutChoices 200 + JSON 但无 choices（伪装 200 的错误体等）不算成功。
func TestTestOpenAICompatible_JSONWithoutChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer srv.Close()

	r := (&LLMService{}).testOpenAICompatible("", srv.URL, "sk-x", "m", true)
	if r.OK {
		t.Fatalf("无 choices 不应判成功: %+v", r)
	}
	if !strings.Contains(r.Message, "quota exceeded") {
		t.Fatalf("应带出上游错误信息: %s", r.Message)
	}
}

// TestExtractErr_HTML 错误体是 HTML 时给出归类提示而非倾倒标签原文。
func TestExtractErr_HTML(t *testing.T) {
	got := extractErr([]byte("<!DOCTYPE html><html><body>404</body></html>"))
	if !strings.Contains(got, "HTML") {
		t.Fatalf("HTML 错误体应归类提示: %s", got)
	}
}
