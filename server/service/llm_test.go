package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
)

func validLLMConfigInput() LLMConfigInput {
	return LLMConfigInput{
		Name: "test", Provider: "openai", BaseURL: "https://api.example.com",
		Model: "model", MaxTokens: 2048, Stream: true,
	}
}

// TestLLMConfigValidateUnboundedMaxTokens max_tokens 无业务上限（对标 new-api：仅请求层
// 保留整型溢出护栏），大值配置必须能通过校验；0/负数仍拒绝。
func TestLLMConfigValidateUnboundedMaxTokens(t *testing.T) {
	svc := &LLMService{}
	for _, tokens := range []int{1, 2048, 200_001, 1_000_000} {
		in := validLLMConfigInput()
		in.MaxTokens = tokens
		if err := svc.validate(in); err != nil {
			t.Errorf("合法配置 max_tokens=%d 不应被拒绝: %v", tokens, err)
		}
	}
	for _, tokens := range []int{0, -1} {
		in := validLLMConfigInput()
		in.MaxTokens = tokens
		if err := svc.validate(in); err == nil {
			t.Errorf("max_tokens=%d 应被拒绝", tokens)
		}
	}
}

// TestLLMConfigCRUDPersistsLargeMaxTokens 大 max_tokens 全链路持久化：创建/更新/读取
// 不得被任何历史上限（如 200000）截断。
func TestLLMConfigCRUDPersistsLargeMaxTokens(t *testing.T) {
	setupTestDB(t)
	if err := common.DB.Exec("DELETE FROM llm_configs").Error; err != nil {
		t.Fatalf("清理 LLM 配置失败: %v", err)
	}
	oldKey := common.EncryptionKey
	common.EncryptionKey = "llm-config-test-key"
	t.Cleanup(func() {
		common.EncryptionKey = oldKey
		common.DB.Exec("DELETE FROM llm_configs")
	})

	svc := &LLMService{}
	in := validLLMConfigInput()
	in.APIKey = "sk-test"
	in.MaxTokens = 1_000_000
	created, err := svc.Create(101, in)
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	if created.MaxTokens != 1_000_000 {
		t.Fatalf("创建未完整回填 max_tokens: %+v", created.LLMConfig)
	}

	in.APIKey = ""
	in.MaxTokens = 2_000_000
	updated, err := svc.Update(101, created.ID, in)
	if err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if updated.MaxTokens != 2_000_000 {
		t.Fatalf("更新未完整回填 max_tokens: %+v", updated.LLMConfig)
	}

	rows, err := svc.List(101)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取配置失败: rows=%d err=%v", len(rows), err)
	}
	if rows[0].MaxTokens != 2_000_000 {
		t.Fatalf("数据库未持久化完整配置: %+v", rows[0].LLMConfig)
	}
}

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
