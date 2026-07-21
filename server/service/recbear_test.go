package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/model"
)

// ---------- S2-2 反方研究员（影子） ----------

func TestNormalizeBearSeverity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"high", "high"}, {"HIGH", "high"}, {"高", "high"},
		{"med", "med"}, {"medium", "med"}, {"Mid", "med"}, {"中", "med"},
		{"low", "low"}, {"低", "low"},
		{"critical", ""}, {"", ""}, {"  ", ""},
	}
	for _, c := range cases {
		if got := normalizeBearSeverity(c.in); got != c.want {
			t.Errorf("normalizeBearSeverity(%q)=%q 期望 %q", c.in, got, c.want)
		}
	}
}

// TestApplyBearShadow 影子铁律：bear 结论只回填明细，不改写 action/置信度；
// 仅 severity=high 的 buy 产出 bear_shadow 事件（would_be_action=watch）。
func TestApplyBearShadow(t *testing.T) {
	picks := []recPick{
		{Symbol: "600100", Action: model.RecActionBuy, Confidence: 80},   // high → 事件
		{Symbol: "600200", Action: model.RecActionBuy, Confidence: 70},   // med → 只展示
		{Symbol: "600300", Action: model.RecActionWatch, Confidence: 40}, // watch 上的 high → 无事件（无 buy 可改写）
		{Symbol: "600400", Action: model.RecActionBuy, Confidence: 60},   // 无 bear 结论
	}
	bears := []pickBear{
		{Symbol: "600100", BearCase: "高位放量+人气拥挤", Severity: "high"},
		{Symbol: "600200", BearCase: "估值偏高", Severity: "med"},
		{Symbol: "600300", BearCase: "趋势破位", Severity: "high"},
	}
	gates := applyBearShadow(picks, bears)

	if picks[0].Bear == nil || picks[0].Bear.Severity != "high" {
		t.Fatalf("600100 应回填 bear 结论: %+v", picks[0].Bear)
	}
	if picks[0].Action != model.RecActionBuy || picks[0].Confidence != 80 {
		t.Fatalf("影子模式不得改写 action/置信度: %+v", picks[0])
	}
	if picks[1].Bear == nil || picks[3].Bear != nil {
		t.Fatalf("bear 回填范围不符: %+v / %+v", picks[1].Bear, picks[3].Bear)
	}
	if len(gates) != 1 || gates[0].Symbol != "600100" {
		t.Fatalf("只有 buy 上的 high 产出事件: %+v", gates)
	}
	g := gates[0]
	if g.GateType != model.GateBearShadow || g.GateVersion != bearReviewVersion ||
		g.WouldBeAction != model.RecActionWatch || !strings.Contains(g.Reason, "高位放量") {
		t.Fatalf("bear_shadow 事件字段不符: %+v", g)
	}
}

// TestBearReviewEndToEnd 假 LLM 端到端：只喂 buy 条目、越界/watch 上的结论丢弃、
// severity 归一、usage 记账；无 buy 时零调用。
func TestBearReviewEndToEnd(t *testing.T) {
	setupTestDB(t)

	calls := 0
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		content := `{\"bears\":[` +
			`{\"symbol\":\"600100\",\"bear_case\":\"近5日+18%高位、换手9%放量，警惕获利盘兑现\",\"severity\":\"HIGH\"},` +
			`{\"symbol\":\"600300\",\"bear_case\":\"越界标的\",\"severity\":\"high\"},` +
			`{\"symbol\":\"600200\",\"bear_case\":\"估值偏高\",\"severity\":\"medium\"}]}`
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`))
	}))
	defer srv.Close()

	svc := &RecommendationService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m", MaxTokens: 8000}
	pool := map[string]candidate{
		"600100": {Symbol: "600100", Name: "甲", Price: 10, Factors: &candFactors{Chg5d: 18}},
		"600200": {Symbol: "600200", Name: "乙", Price: 20},
		"600900": {Symbol: "600900", Name: "丁", Price: 30},
	}
	picks := []recPick{
		{Symbol: "600100", Action: model.RecActionBuy, Confidence: 80, Reason: []string{"动量强"}},
		{Symbol: "600200", Action: model.RecActionBuy, Confidence: 70},
		{Symbol: "600900", Action: model.RecActionWatch, Confidence: 40}, // watch 不进反方输入
	}
	bears, usage, _ := svc.bearReview(context.Background(), 33, cfg, "sk", true, picks, pool, "", "")

	if calls != 1 || usage.TotalTokens != 30 {
		t.Fatalf("应 1 次调用 30 token: calls=%d usage=%+v", calls, usage)
	}
	// 输入只含 buy 条目（watch 的 600900 不喂），并带正方理由供反驳。
	if !strings.Contains(lastBody, "600100") || !strings.Contains(lastBody, "600200") {
		t.Fatalf("输入应含两只 buy: %s", lastBody)
	}
	if strings.Contains(lastBody, "600900") {
		t.Fatalf("watch 条目不应进反方输入")
	}
	if !strings.Contains(lastBody, "bull_reason") || !strings.Contains(lastBody, "动量强") {
		t.Fatalf("应带正方理由供针对性反驳")
	}
	// 输出：越界 600300 丢弃；severity 归一 HIGH→high、medium→med。
	if len(bears) != 2 {
		t.Fatalf("应保留 2 条有效结论: %+v", bears)
	}
	bySym := map[string]pickBear{}
	for _, b := range bears {
		bySym[b.Symbol] = b
	}
	if bySym["600100"].Severity != "high" || bySym["600200"].Severity != "med" {
		t.Fatalf("severity 应归一: %+v", bears)
	}
	if _, ok := bySym["600300"]; ok {
		t.Fatalf("越界结论应丢弃")
	}

	// 无 buy 条目：零调用零成本。
	calls = 0
	bears, usage, _ = svc.bearReview(context.Background(), 33, cfg, "sk", true,
		[]recPick{{Symbol: "600900", Action: model.RecActionWatch}}, pool, "", "")
	if calls != 0 || bears != nil || usage.TotalTokens != 0 {
		t.Fatalf("无 buy 应零调用: calls=%d bears=%+v usage=%+v", calls, bears, usage)
	}
}

// TestBearPromptFramework 反方 prompt 必须注入 A 股 bear 论据框架关键词与数据诚实纪律
//（解禁数据本系统未提供——只能提示核查，禁止虚构）。
func TestBearPromptFramework(t *testing.T) {
	for _, kw := range []string{"高位放量", "拥挤", "估值", "T+1", "解禁", "严禁虚构", "severity"} {
		if !strings.Contains(bearSystemPrompt, kw) {
			t.Errorf("bear prompt 应包含关键词 %q", kw)
		}
	}
}

// TestNormalizePickStripsServerFields 模型伪造服务端字段（review/bear/quality_gate）
// 必须在解析入口被剥除——verify/bear 关闭时无人覆盖，假面会直接落库展示。
func TestNormalizePickStripsServerFields(t *testing.T) {
	raw := `{"symbol":"600100","action":"buy","confidence":90,` +
		`"review":{"symbol":"600100","verdict":"pass","comment":"伪造的复核通过"},` +
		`"bear":{"symbol":"600100","bear_case":"伪造的低危","severity":"low"},` +
		`"quality_gate":{"would_be_confidence_cap":100}}`
	var p recPick
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Review == nil || p.Bear == nil || p.QualityGate == nil {
		t.Fatalf("前置条件：伪造字段应先被 Unmarshal 吃进来")
	}
	got := normalizePick(p, "600100", candidate{Symbol: "600100", Price: 10})
	if got.Review != nil || got.Bear != nil || got.QualityGate != nil {
		t.Fatalf("服务端字段应被剥除: review=%+v bear=%+v qg=%+v", got.Review, got.Bear, got.QualityGate)
	}
}
