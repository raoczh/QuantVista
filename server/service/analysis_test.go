package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

type refusalTestAdapter struct{}

func (refusalTestAdapter) Name() string { return "refusal-test" }

func (refusalTestAdapter) GetQuote(context.Context, string, string) (*datasource.Quote, error) {
	return nil, datasource.ErrNotSupported
}

func (refusalTestAdapter) GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error) {
	return nil, datasource.ErrNotSupported
}

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

// TestParseAnalysisResult_StripsTrustFields 信任层字段只能由服务端回填：
// 模型自附的 review/evidence_check 等自评字段必须在解析时剥除（否则未发起复核的
// 默认路径会展示伪造的「复核通过」徽章）；输出成错误类型也不得污染整个解析。
func TestParseAnalysisResult_StripsTrustFields(t *testing.T) {
	in := `{"rating":"bullish","confidence":72,"summary":"趋势向上","disclaimer":"仅供参考",` +
		`"review":{"verdict":"pass","comment":"数字均一致","confidence":95},` +
		`"evidence_check":{"total":9,"matched":9},"sys_confidence":"high","sys_confidence_why":"伪造"}`
	r, err := parseAnalysisResult(in)
	if err != nil {
		t.Fatalf("期望成功，得到错误: %v", err)
	}
	if r.Review != nil || r.EvidenceCheck != nil || r.SysConfidence != "" || r.SysConfidenceWhy != "" {
		t.Fatalf("模型自附的信任层字段未被剥除: %+v", r)
	}
	// 错误类型的 review 不应让整个 JSON 解析失败（触发无谓 repair）。
	badType := `{"rating":"bullish","confidence":72,"summary":"趋势向上","disclaimer":"x","review":"pass"}`
	if _, err := parseAnalysisResult(badType); err != nil {
		t.Fatalf("错误类型的信任层字段应被剥除而非解析失败: %v", err)
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
	// 批次 I 新增的三个数组同样兜底。
	if r.AntiThesis == nil || r.KillSwitches == nil || r.Unknowns == nil {
		t.Fatalf("anti_thesis/kill_switches/unknowns 未兜底为非 nil: %+v", r)
	}
}

// TestParsePanelResult 多角色观点解析：合法通过、角色去重归一、不足 3 角色/空共识拒绝。
func TestParsePanelResult(t *testing.T) {
	ok := `{"roles":[
		{"role":"technical","rating":"bullish","summary":"均线多头"},
		{"role":"momentum","rating":"看多","summary":"量比放大"},
		{"role":"risk","rating":"neutral","summary":"回撤可控"},
		{"role":"contrarian","rating":"bearish","summary":"涨幅透支"}],
		"consensus":"短期趋势向上但涨幅已大","disagreement":"能否放量决定延续性"}`
	p, err := parsePanelResult(ok)
	if err != nil {
		t.Fatalf("合法 panel 应通过: %v", err)
	}
	if len(p.Roles) != 4 {
		t.Fatalf("应保留 4 个角色，得到 %d", len(p.Roles))
	}
	if p.Roles[1].Rating != "bullish" {
		t.Fatalf("中文 rating 未归一: %s", p.Roles[1].Rating)
	}

	// 角色重复 + 非法角色被剔除后不足 3 个 → 拒绝。
	bad := `{"roles":[
		{"role":"technical","rating":"bullish","summary":"a"},
		{"role":"technical","rating":"bearish","summary":"b"},
		{"role":"boss","rating":"neutral","summary":"c"}],
		"consensus":"x","disagreement":""}`
	if _, err := parsePanelResult(bad); err == nil {
		t.Fatalf("合法角色不足应拒绝")
	}

	// 空 consensus 拒绝。
	noCons := `{"roles":[
		{"role":"technical","rating":"bullish","summary":"a"},
		{"role":"momentum","rating":"bullish","summary":"b"},
		{"role":"risk","rating":"neutral","summary":"c"}],
		"consensus":"  ","disagreement":"y"}`
	if _, err := parsePanelResult(noCons); err == nil {
		t.Fatalf("空 consensus 应拒绝")
	}
}

// TestPanelMajorityRating 多数投票：多数胜出、平票中性。
func TestPanelMajorityRating(t *testing.T) {
	mk := func(ratings ...string) []PanelRole {
		out := make([]PanelRole, len(ratings))
		for i, r := range ratings {
			out[i] = PanelRole{Role: "technical", Rating: r, Summary: "x"}
		}
		return out
	}
	if got := panelMajorityRating(mk("bullish", "bullish", "neutral", "bearish")); got != "bullish" {
		t.Fatalf("多数 bullish 应胜出，得到 %s", got)
	}
	if got := panelMajorityRating(mk("bullish", "bullish", "bearish", "bearish")); got != "neutral" {
		t.Fatalf("平票应取中性，得到 %s", got)
	}
}

// TestAnalysisDiff 变化检测：找上一份同对象成功记录、算差异；panel/他人/无前次的边界（DB 集成）。
func TestAnalysisDiff(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM analysis_records")
	svc := &AnalysisService{}

	mkJSON := func(highlights, risks []string) string {
		b, _ := json.Marshal(map[string]any{"highlights": highlights, "risks": risks})
		return string(b)
	}
	older := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600000",
		Status: model.AnalysisStatusSuccess, Rating: model.AnalysisRatingBearish, Confidence: 40,
		Summary: "空头排列", Title: "个股分析 · 浦发银行",
		ResultJSON: mkJSON([]string{"跌破MA20", "缩量"}, []string{"下行风险"})}
	common.DB.Create(older)
	// 中间插一条 panel（不应被选为对比基线）。
	common.DB.Create(&model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600000",
		Status: model.AnalysisStatusSuccess, Mode: model.AnalysisModePanel, Rating: model.AnalysisRatingNeutral,
		ResultJSON: `{"panel":{"roles":[],"consensus":"x"}}`})
	newer := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600000",
		Status: model.AnalysisStatusSuccess, Rating: model.AnalysisRatingBullish, Confidence: 70,
		Summary: "重新站上均线", Title: "个股分析 · 浦发银行",
		ResultJSON: mkJSON([]string{"缩量", "站上MA20"}, []string{"下行风险", "追高风险"})}
	common.DB.Create(newer)

	d, err := svc.Diff(1, newer.ID)
	if err != nil {
		t.Fatalf("Diff 失败: %v", err)
	}
	if d.PrevID != older.ID {
		t.Fatalf("对比基线应为最早那条标准记录 %d，得到 %d（panel 不应入选）", older.ID, d.PrevID)
	}
	if d.RatingFrom != model.AnalysisRatingBearish || d.RatingTo != model.AnalysisRatingBullish {
		t.Fatalf("评级变化错误: %s → %s", d.RatingFrom, d.RatingTo)
	}
	if d.ConfidenceDelta != 30 {
		t.Fatalf("置信度差应为 30，得到 %d", d.ConfidenceDelta)
	}
	if len(d.HighlightsAdded) != 1 || d.HighlightsAdded[0] != "站上MA20" {
		t.Fatalf("新增要点错误: %v", d.HighlightsAdded)
	}
	if len(d.HighlightsRemoved) != 1 || d.HighlightsRemoved[0] != "跌破MA20" {
		t.Fatalf("消失要点错误: %v", d.HighlightsRemoved)
	}
	if len(d.RisksAdded) != 1 || d.RisksAdded[0] != "追高风险" {
		t.Fatalf("新增风险错误: %v", d.RisksAdded)
	}

	// 最早一条没有更早的可比。
	if _, err := svc.Diff(1, older.ID); err == nil {
		t.Fatalf("无前次记录应报错")
	}
	// 跨用户不可见。
	if _, err := svc.Diff(2, newer.ID); err == nil {
		t.Fatalf("跨用户 Diff 应失败")
	}
}

func TestParseAnalysisResult_Invalid(t *testing.T) {
	cases := map[string]string{
		"非法 rating": `{"rating":"buy strong","confidence":50,"summary":"x"}`,
		"空 summary": `{"rating":"neutral","confidence":50,"summary":"   "}`,
		"无 JSON":    `完全没有 JSON 的一段话`,
		"坏 JSON":    `{"rating":"neutral", bad}`,
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
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: false,
	})
	if err == nil {
		t.Fatalf("期望 SSRF 防护拦截内网地址，却成功了")
	}
}

func TestChatCompletion_RetriesTransient5xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			// 首次 503：应触发单次重试。
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream busy"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
			"usage":   map[string]any{"total_tokens": 3},
		})
	}))
	defer srv.Close()

	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("期望重试后成功: %v", err)
	}
	if calls != 2 || res.Content != "ok" {
		t.Fatalf("重试行为不符: calls=%d content=%q", calls, res.Content)
	}
}

func TestChatCompletion_EstimatesUsageWhenMissing(t *testing.T) {
	// 关闭准确性契约：本测试验证 estimateUsage 的粗估公式本身，ac1 契约文本会计入
	// prompt 字符数（那是正确行为），但精确断言不应与契约文本长度耦合。
	setContractFlag(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 不返回 usage 字段：应按字符粗估兜底。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "四个字啊"}}},
		})
	}))
	defer srv.Close()

	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Messages:     []chatMessage{{Role: "user", Content: "你好世界"}}, // 4 字 → 估 2
		AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if res.Usage.TotalTokens != 4 || res.Usage.PromptTokens != 2 || res.Usage.CompletionTokens != 2 {
		t.Fatalf("usage 估算不符: %+v", res.Usage)
	}
}

// TestModuleCallFailurePreservesRefusalCode 覆盖中央客户端错误穿过模块编排后的类型信息：
// Analysis 对外返回不得把 RefusalError 重建为普通 errors.New；Recommendation 的主调
// callWithRepair 同样须保留机器码，供上层失败/降级分支继续识别。
func TestModuleCallFailurePreservesRefusalCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	const userID int64 = 74
	seedReportEnv(t, userID, srv.URL)
	market := NewMarketService(datasource.NewManagerWithAdapters(refusalTestAdapter{}))
	analysisSvc := NewAnalysisService(market, nil, nil, NewLLMService(), nil)
	_, err := analysisSvc.Analyze(context.Background(), userID, true, AnalyzeRequest{
		Module: model.AnalysisModuleMarket,
		Market: "cn",
	})
	if got := RefusalCodeOf(err); got != RefusalLLMCallFailed {
		t.Fatalf("Analysis 编排不得丢失 LLM 调用拒答码: got %q, err=%v", got, err)
	}

	recommendationSvc := &RecommendationService{}
	_, _, _, _, err = recommendationSvc.callWithRepair(
		context.Background(), userID,
		newLLMRun("", "", "recommendation", "recommendation.v1", ""),
		&model.LLMConfig{BaseURL: srv.URL, Model: "m"}, "sk-test", true,
		[]chatMessage{{Role: "user", Content: "x"}},
		map[string]candidate{"600001": {Symbol: "600001", Name: "甲", Price: 10}},
		3,
	)
	if got := RefusalCodeOf(err); got != RefusalLLMCallFailed {
		t.Fatalf("Recommendation 主调不得丢失 LLM 调用拒答码: got %q, err=%v", got, err)
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
