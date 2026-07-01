package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseAnalysisResult_Valid(t *testing.T) {
	in := `{"rating":"bullish","confidence":72,"summary":"趋势向上","highlights":["站上MA20"],"risks":["量能不足"],"opportunities":["回踩支撑"],"suggestions":["观察成交量"],"disclaimer":"仅供参考"}`
	r, err := parseAnalysisResult(in)
	if err != nil {
		t.Fatalf("期望成功，得到错误: %v", err)
	}
	if r.Rating != "bullish" || r.Confidence != 72 || r.Summary == "" {
		t.Fatalf("字段解析错误: %+v", r)
	}
}

func TestParseAnalysisResult_CodeFenceAndChineseRating(t *testing.T) {
	in := "这是分析结果：\n```json\n{\"rating\":\"看多\",\"confidence\":150,\"summary\":\"多头\",\"disclaimer\":\"\"}\n```\n以上。"
	r, err := parseAnalysisResult(in)
	if err != nil {
		t.Fatalf("期望容忍代码块与中文枚举，得到错误: %v", err)
	}
	if r.Rating != "bullish" {
		t.Fatalf("中文 rating 未归一: %s", r.Rating)
	}
	if r.Confidence != 100 {
		t.Fatalf("confidence 未被钳制到 100: %d", r.Confidence)
	}
	if r.Disclaimer == "" {
		t.Fatalf("空 disclaimer 未回退默认值")
	}
	// nil 数组应兜底为非 nil，前端无需判空。
	if r.Highlights == nil || r.Risks == nil || r.Opportunities == nil || r.Suggestions == nil {
		t.Fatalf("数组字段未兜底为非 nil: %+v", r)
	}
}

func TestParseAnalysisResult_Invalid(t *testing.T) {
	cases := map[string]string{
		"非法 rating": `{"rating":"buy strong","confidence":50,"summary":"x"}`,
		"空 summary":  `{"rating":"neutral","confidence":50,"summary":"   "}`,
		"无 JSON":     `完全没有 JSON 的一段话`,
		"坏 JSON":     `{"rating":"neutral", bad}`,
	}
	for name, in := range cases {
		if _, err := parseAnalysisResult(in); err == nil {
			t.Errorf("[%s] 期望校验失败，却通过了", name)
		}
	}
}

func TestExtractJSONObject_NestedAndStringBraces(t *testing.T) {
	// 含嵌套对象与字符串内的花括号，必须取到完整平衡对象。
	in := `前缀 {"a":{"b":1},"note":"含 } 花括号的字符串","c":2} 后缀`
	got := extractJSONObject(in)
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("抽取结果非合法 JSON: %q err=%v", got, err)
	}
	if _, ok := m["c"]; !ok {
		t.Fatalf("未取到完整对象（缺 c）: %q", got)
	}
}

func TestChatCompletion_SuccessWithUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("端点路径不对: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("鉴权头缺失: %s", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "{\"ok\":1}"}}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
		})
	}))
	defer srv.Close()

	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "gpt-x", MaxTokens: 100,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		JSONMode: true, AllowPrivate: true, // httptest 是 127.0.0.1，需放行内网
	})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if res.Usage.TotalTokens != 18 || res.Content != `{"ok":1}` {
		t.Fatalf("结果不符: %+v", res)
	}
}

func TestChatCompletion_JSONModeFallback(t *testing.T) {
	var sawJSONMode, sawFallback bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["response_format"]; ok {
			// 第一次带 response_format：模拟不支持，返回 400。
			sawJSONMode = true
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format is not supported"}}`))
			return
		}
		// 回退请求（无 response_format）：成功。
		sawFallback = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
			"usage":   map[string]any{"total_tokens": 5},
		})
	}))
	defer srv.Close()

	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		JSONMode: true, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("期望回退后成功: %v", err)
	}
	if !sawJSONMode || !sawFallback {
		t.Fatalf("未按预期先试 JSON mode 再回退: json=%v fallback=%v", sawJSONMode, sawFallback)
	}
	if res.Content != "ok" {
		t.Fatalf("回退结果不符: %q", res.Content)
	}
}

func TestChatCompletion_BlocksPrivateWhenNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer srv.Close()
	// allowPrivate=false 时，SafeHTTPClient 应拦截 127.0.0.1，返回错误（防 SSRF）。
	_, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: false,
	})
	if err == nil {
		t.Fatalf("期望 SSRF 防护拦截内网地址，却成功了")
	}
}

func TestFitBudget_TrimsWhenOversize(t *testing.T) {
	// 构造超预算快照：带 recent_bars 和大列表。
	bars := make([]map[string]any, 40)
	for i := range bars {
		bars[i] = map[string]any{"d": "2025-01-01", "c": 10.0}
	}
	big := strings.Repeat("填充数据", 3000) // 远超预算
	snap := map[string]any{
		"recent_bars": bars,
		"filler":      big,
	}
	out := fitBudget(snap)
	if _, ok := out["recent_bars"]; ok {
		t.Fatalf("超预算时应先丢弃 recent_bars")
	}
	if _, ok := out["bars_note"]; !ok {
		t.Fatalf("丢弃明细后应留下 bars_note 说明")
	}
}
