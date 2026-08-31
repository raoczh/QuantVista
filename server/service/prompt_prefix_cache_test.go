package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// 前缀缓存分工测试（qa q14 / analysis p21）。
//
// 核心不变量：**system 段不含任何随本次输入变化的内容**。上游自动 prompt 缓存要求前缀
// 逐字节相同（且 ≥1024 token 才生效），system 之前/之中一旦出现随输入变化的文本，
// 后面全部作废——所以这里不测「文案在不在」，而是构造两次输入不同的调用断言 system
// 逐字节相同。

// TestQaSystemPrefixStableAcrossTurns 同一会话不同轮：问题不同、时效重判状态不同，
// system 必须逐字节相同；随轮次变化的分层历史段与时效声明段都落在最后一条 user 消息。
func TestQaSystemPrefixStableAcrossTurns(t *testing.T) {
	setLayeredContextFlag(t, true)
	// 日历钉到上一个交易日：会话快照的行情时刻（远旧）按当前时刻重判为 stale。
	pinCalendarTo(t, prevOpenTradeDate(time.Now().Format("2006-01-02")))

	conv := model.AiConversation{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		DataSnapshot: `{"symbol":"600000","quote":{"price":10},"quote_as_of":"2026-07-08 15:00:00","freshness_status":"fresh"}`}
	var history []model.AiConversationMessage
	for i := 1; i <= 8; i++ { // 16 条 > qaHistoryLimit → 有窗口外历史，Tier2/Tier3 段会注入
		history = append(history,
			qaMsg(model.QaRoleUser, fmt.Sprintf("历史问题 %d：均线与量能怎么看", i)),
			qaMsg(model.QaRoleAssistant, fmt.Sprintf("历史回答 %d：站上 MA20，量能温和", i)))
	}
	pr := loadPromptRuntime(1, model.PromptModuleQa)
	staleSvc := &QaService{market: NewMarketService(nil)} // 时效可判 → stale 段注入
	freshSvc := &QaService{}                              // 无 market → 不判时效，无该段

	msgsA, _ := staleSvc.buildMessagesFrom(pr, conv, history, "均线怎么样")
	msgsB, _ := staleSvc.buildMessagesFrom(pr, conv, history, "换个完全不同的问题：这家公司的估值水位如何")
	msgsC, _ := freshSvc.buildMessagesFrom(pr, conv, history, "均线怎么样")

	sysA, sysB, sysC := msgsA[0].Content, msgsB[0].Content, msgsC[0].Content
	if sysA != sysB {
		t.Fatalf("不同问题的 system 必须逐字节相同（前缀缓存前提）\nA=%q\nB=%q", sysA, sysB)
	}
	if sysA != sysC {
		t.Fatalf("时效重判状态不同也不得改变 system\n有时效段=%q\n无时效段=%q", sysA, sysC)
	}
	// system 只留会话内稳定内容：快照在、随轮次变化的两段都不在。
	if !strings.Contains(sysA, "个股数据快照") || !strings.Contains(sysA, "600000") {
		t.Fatalf("system 应含固定快照: %q", sysA)
	}
	for _, bad := range []string{"【行情时效", "【历史会话分层上下文】", "均线怎么样", "估值水位"} {
		if strings.Contains(sysA, bad) {
			t.Fatalf("system 不得含随本轮输入变化的内容「%s」: %q", bad, sysA)
		}
	}
	// 两段随轮次变化的内容落在最后一条 user 消息，且本轮问题在末尾。
	turnA := msgsA[len(msgsA)-1].Content
	if !strings.Contains(turnA, "【行情时效（按本轮提问时刻重新核验，优先级高于快照内 freshness_status）】") {
		t.Fatalf("时效重判段应在本轮 user 段: %q", turnA)
	}
	if !strings.Contains(turnA, "历史数据解释") {
		t.Fatalf("时效段措辞（含「历史数据解释」）不得改动: %q", turnA)
	}
	if !strings.Contains(turnA, "【历史会话分层上下文】") || !strings.HasSuffix(turnA, "【本轮问题】均线怎么样") {
		t.Fatalf("本轮 user 段应为 分层段+时效段+本轮问题: %q", turnA)
	}
	if strings.Contains(msgsC[len(msgsC)-1].Content, "【行情时效") {
		t.Fatalf("无法判定时效时不应注入该段")
	}
}

// TestQaHistoryWindowSegmentedSliding Tier1 窗口分段滑动：起点只在整批边界前移，
// 窗口大小落在 [qaHistoryLimit, qaHistoryLimit+step-2]（只会多带历史，不会更少）；
// 未跨批的相邻两轮，system+全部历史消息逐字节相同（缓存能连历史一起命中），
// 跨批那一轮才在第一条历史消息处断开。
func TestQaHistoryWindowSegmentedSliding(t *testing.T) {
	for _, c := range []struct{ n, wantStart int }{
		{0, 0}, {2, 0}, {12, 0},
		{14, 0}, // 差 2 条不够一批：先不丢，窗口临时 14
		{16, 4}, // 攒满一批：一次性丢 4，窗口回到 12
		{18, 4}, // 同批内不动：窗口 14
		{20, 8}, // 下一批
		{22, 8}, {24, 12},
	} {
		got := qaWindowStart(c.n)
		if got != c.wantStart {
			t.Fatalf("qaWindowStart(%d)=%d, want %d", c.n, got, c.wantStart)
		}
		if win := c.n - got; c.n > qaHistoryLimit &&
			(win < qaHistoryLimit || win > qaHistoryLimit+qaHistoryDropStep-2) {
			t.Fatalf("n=%d 窗口 %d 条超出 [%d,%d]", c.n, win, qaHistoryLimit, qaHistoryLimit+qaHistoryDropStep-2)
		}
	}

	setLayeredContextFlag(t, true)
	conv := model.AiConversation{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		DataSnapshot: `{"symbol":"600000","quote":{"price":10},"freshness_status":"fresh"}`}
	var history []model.AiConversationMessage
	for i := 1; i <= 12; i++ { // 24 条
		history = append(history,
			qaMsg(model.QaRoleUser, fmt.Sprintf("历史问题 %d", i)),
			qaMsg(model.QaRoleAssistant, fmt.Sprintf("历史回答 %d", i)))
	}
	svc := NewQaService(nil, NewLLMService())
	pr := loadPromptRuntime(1, model.PromptModuleQa)
	build := func(n int) []chatMessage {
		msgs, _ := svc.buildMessagesFrom(pr, conv, history[:n], "本轮问题")
		return msgs
	}

	// 同批内（16→18，start 均为 4）：system + 前 12 条历史逐字节相同。
	a, b := build(16), build(18)
	for i := 0; i <= qaHistoryLimit; i++ {
		if a[i] != b[i] {
			t.Fatalf("同批内相邻轮的前缀应逐字节相同，第 %d 条不同: %q vs %q", i, a[i].Content, b[i].Content)
		}
	}
	// 跨批（18→20，start 4→8）：system 仍相同，第一条历史消息处断开（缓存只到 system）。
	c := build(20)
	if b[0] != c[0] {
		t.Fatalf("system 不应因窗口滑动而变化")
	}
	if b[1] == c[1] {
		t.Fatalf("跨批应丢掉最旧一批历史，第一条历史消息应变化")
	}
}

// TestQaCacheScopeKey QA 的会话级缓存分片：llmRun.CacheScope 透传 chatMeta 并拼进
// prompt_cache_key；值只含内部会话 ID。
func TestQaCacheScopeKey(t *testing.T) {
	if got := qaCacheScope(0); got != "" {
		t.Fatalf("未落库会话不应有 scope: %q", got)
	}
	if got := qaCacheScope(42); got != "conv42" {
		t.Fatalf("scope 应为 conv42: %q", got)
	}
	run := newLLMRun("t1", "", "qa", "qa.free_text.v1", qaPromptVersion)
	run.CacheScope = qaCacheScope(42)
	p := chatParams{Meta: run.chatMeta(9, nil, 1)}
	if got, want := p.promptCacheKey(), "qa:"+qaPromptVersion+":conv42"; got != want {
		t.Fatalf("promptCacheKey()=%q, want %q", got, want)
	}
	// 其他模块不填 scope：key 保持 module:promptVersion 粒度（加标的只会打散 key）。
	other := newLLMRun("t1", "", "analysis", "analysis.v1", analysisPromptVersion)
	po := chatParams{Meta: other.chatMeta(9, nil, 1)}
	if got, want := po.promptCacheKey(), "analysis:"+analysisPromptVersion; got != want {
		t.Fatalf("analysis 不应带 scope: %q, want %q", got, want)
	}
}

// TestQaPromptCacheKeyEndToEnd 端到端：Ask 实际发出的请求体带会话级 prompt_cache_key。
func TestQaPromptCacheKeyEndToEnd(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM ai_conversations")
	common.DB.Exec("DELETE FROM ai_conversation_messages")
	common.DB.Exec("DELETE FROM llm_configs")
	common.DB.Exec("DELETE FROM user_quota")
	common.EncryptionKey = "unit-test-key"

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "回答正文"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer srv.Close()

	cipher, _ := common.Encrypt("sk-test")
	cfg := &model.LLMConfig{UserID: 21, Name: "t", Provider: "openai", BaseURL: srv.URL,
		APIKeyCipher: cipher, Model: "m", IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建配置失败: %v", err)
	}
	conv := &model.AiConversation{UserID: 21, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Title: "t", LLMConfigID: cfg.ID, Provider: "openai", Model: "m",
		DataSnapshot: `{"symbol":"600000","quote":{"price":10}}`}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	svc := NewQaService(nil, NewLLMService())
	if _, err := svc.Ask(context.Background(), 21, true, QaAskRequest{ConversationID: conv.ID, Question: "如何"}); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	want := fmt.Sprintf(`"prompt_cache_key":"qa:%s:conv%d"`, qaPromptVersion, conv.ID)
	if !strings.Contains(gotBody, want) {
		t.Fatalf("请求体应含会话级缓存 key %s，实际: %s", want, gotBody)
	}
}

// TestAnalysisSystemPrefixStable 分析模块：对象/问题/快照/回溯声明全都不同，system 仍逐字节
// 相同；固定的数据时点与输出收束两段在 system（不再每次随 user 段全价重付）。
func TestAnalysisSystemPrefixStable(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM prompt_templates")
	s := &AnalysisService{}

	req1 := AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600519", Question: "estimate?"}
	req2 := AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "hk", Symbol: "000001", AsOf: "2026-07-01"}
	pr := loadPromptRuntime(41, model.AnalysisModuleStock)
	m1 := s.buildMessages(pr, req1, &analysisContext{Label: "贵州茅台"}, `{"symbol":"600519"}`)
	m2 := s.buildMessages(pr, req2, &analysisContext{Label: "平安银行"}, `{"symbol":"000001"}`)

	if m1[0].Content != m2[0].Content {
		t.Fatalf("不同对象/问题/快照的 system 必须逐字节相同")
	}
	sys := m1[0].Content
	for _, want := range []string{"数据时间：以快照内 data_as_of 为采集时刻", "请严格按上述 JSON schema 输出", "输出要求："} {
		if !strings.Contains(sys, want) {
			t.Fatalf("固定段应在 system: 缺「%s」", want)
		}
	}
	for _, bad := range []string{"600519", "贵州茅台", "estimate?", "2026-07-01", "【回溯声明】"} {
		if strings.Contains(sys, bad) {
			t.Fatalf("system 不得含随本次请求变化的内容「%s」", bad)
		}
	}
	// user 段只留随本次请求变化的内容；固定两段已前移，不得重复出现。
	u1, u2 := m1[1].Content, m2[1].Content
	if !strings.Contains(u1, "【数据】") || !strings.Contains(u1, `{"symbol":"600519"}`) ||
		!strings.Contains(u1, "用户特别关注的问题") {
		t.Fatalf("user 段应含标题/问题/快照: %q", u1)
	}
	for _, bad := range []string{"数据时间：以快照内", "JSON schema 输出"} {
		if strings.Contains(u1, bad) {
			t.Fatalf("固定段不应仍留在 user 段: 「%s」", bad)
		}
	}
	if !strings.Contains(u2, "【回溯声明】") || !strings.Contains(u2, "2026-07-01") {
		t.Fatalf("动态的回溯声明应留在 user 段: %q", u2)
	}
	// panel 固定编排也要拿到固定尾段（此前它同样从 user 段获得这两句）。
	panelReq := AnalyzeRequest{Module: model.AnalysisModuleStock, Mode: model.AnalysisModePanel, Market: "cn", Symbol: "600519"}
	pm := s.buildMessages(pr, panelReq, &analysisContext{Label: "贵州茅台"}, `{"symbol":"600519"}`)
	if !strings.Contains(pm[0].Content, "四位立场不同的研究员") ||
		!strings.Contains(pm[0].Content, "数据时间：以快照内 data_as_of 为采集时刻") ||
		!strings.Contains(pm[0].Content, "请严格按上述 JSON schema 输出") {
		t.Fatalf("panel system 应为多角色编排 + 固定尾段")
	}
}

// TestAnalysisCustomTemplatePlaceholderSplit 自定义模板含占位符时：含占位符的块进 user 段，
// 其余块留在 system——否则 system 变成「每只标的一份」，前缀缓存必挂。
// 无占位符的模板整段留在 system（与拆分前逐字节一致）。
func TestAnalysisCustomTemplatePlaceholderSplit(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM prompt_templates")
	common.DB.Exec("DELETE FROM prompt_template_revisions")
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM prompt_templates")
		common.DB.Exec("DELETE FROM prompt_template_revisions")
	})
	ps := &PromptService{}
	s := &AnalysisService{}

	// ① 含占位符：稳定块留 system，动态块进 user 段（内容照样生效）。
	content := "只看量价配合，忽略估值。\n\n本次重点分析 {{target}}（{{symbol}}）在 {{market}} 市场的表现。\n\n给出明确的支撑压力位。"
	if _, _, err := ps.Upsert(42, PromptInput{Module: model.AnalysisModuleStock, Content: content, Enabled: true}); err != nil {
		t.Fatalf("建模板失败: %v", err)
	}
	pr := loadPromptRuntime(42, model.AnalysisModuleStock)
	if !pr.Custom {
		t.Fatal("应命中自定义模板")
	}
	a := s.buildMessages(pr, AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600519"},
		&analysisContext{Label: "贵州茅台"}, `{"a":1}`)
	b := s.buildMessages(pr, AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "hk", Symbol: "000001"},
		&analysisContext{Label: "平安银行"}, `{"a":2}`)
	if a[0].Content != b[0].Content {
		t.Fatalf("含占位符的自定义模板不得让 system 随标的变化\nA=%q\nB=%q", a[0].Content, b[0].Content)
	}
	if !strings.Contains(a[0].Content, "只看量价配合，忽略估值。") ||
		!strings.Contains(a[0].Content, "给出明确的支撑压力位。") {
		t.Fatalf("稳定块应留在 system: %q", a[0].Content)
	}
	if strings.Contains(a[0].Content, "本次重点分析") {
		t.Fatalf("含占位符的块不得留在 system")
	}
	if !strings.Contains(a[1].Content, analysisCustomDynamicHeader) ||
		!strings.Contains(a[1].Content, "本次重点分析 贵州茅台（600519）在 cn 市场的表现。") {
		t.Fatalf("动态块应带分界头进 user 段并照常渲染: %q", a[1].Content)
	}

	// ② 无占位符：整段留在 system，与「拆分前」逐字节一致。
	plain := "只看量价配合，忽略估值。\n\n给出明确的支撑压力位。"
	if _, _, err := ps.Upsert(43, PromptInput{Module: model.AnalysisModuleStock, Content: plain, Enabled: true}); err != nil {
		t.Fatalf("建模板失败: %v", err)
	}
	pr2 := loadPromptRuntime(43, model.AnalysisModuleStock)
	c := s.buildMessages(pr2, AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600519"},
		&analysisContext{Label: "贵州茅台"}, `{"a":1}`)
	want := analysisRoleIntro + "\n\n" + plain + "\n\n" + analysisOutputSpec + "\n\n" + analysisFixedTail
	if c[0].Content != want {
		t.Fatalf("无占位符模板应整段留在 system:\ngot=%q\nwant=%q", c[0].Content, want)
	}
	if strings.Contains(c[1].Content, analysisCustomDynamicHeader) {
		t.Fatalf("无动态块时不应出现分界头")
	}
}

// TestSplitPromptStableBlocks 分块判定：只有「会被实际替换」的占位符算动态；未知占位符与
// 单花括号原样保留、不算动态；两段各自保持模板原序。
func TestSplitPromptStableBlocks(t *testing.T) {
	vars := map[string]string{"symbol": "600519", "target": "贵州茅台", "market": "cn"}
	raw := "块一稳定。\n\n块二 {{symbol}} 动态。\n\n块三 {{unknown}} 与 {symbol} 都不算动态。\n\n块四 {{target}} 动态。"
	stable, dynamic := splitPromptStableBlocks(raw, renderPromptTemplate(raw, vars), vars)
	wantStable := "块一稳定。\n\n块三 {{unknown}} 与 {symbol} 都不算动态。"
	wantDynamic := "块二 600519 动态。\n\n块四 贵州茅台 动态。"
	if stable != wantStable {
		t.Fatalf("稳定段不符:\ngot=%q\nwant=%q", stable, wantStable)
	}
	if dynamic != wantDynamic {
		t.Fatalf("动态段不符:\ngot=%q\nwant=%q", dynamic, wantDynamic)
	}
	// 无有效占位符：直接返回渲染后的整段（短路，保证与拆分前逐字节一致）。
	plain := "只有一段。\n\n\n中间三个换行也原样保留。"
	if st, dy := splitPromptStableBlocks(plain, plain, vars); st != plain || dy != "" {
		t.Fatalf("无占位符应原样返回: st=%q dy=%q", st, dy)
	}
	// 全部块动态：稳定段为空（analysisSystemPromptParts 会跳过空段，不留多余空行）。
	allDyn := "{{symbol}} 一。\n\n{{target}} 二。"
	if st, dy := splitPromptStableBlocks(allDyn, renderPromptTemplate(allDyn, vars), vars); st != "" ||
		dy != "600519 一。\n\n贵州茅台 二。" {
		t.Fatalf("全动态时稳定段应为空: st=%q dy=%q", st, dy)
	}
	sys, dyn := analysisSystemPromptParts(promptRuntime{Module: model.AnalysisModuleStock, Custom: true, Raw: allDyn}, model.AnalysisModuleStock, vars)
	if sys != analysisRoleIntro+"\n\n"+analysisOutputSpec || dyn == "" {
		t.Fatalf("全动态时 system 应只剩角色总纲+输出要求（无多余空行）: %q", sys)
	}

	// CRLF 模板：正文按用户提交原样入库（normalizePromptInput 不规范行尾），空行是
	// "\r\n\r\n" 不含 "\n\n"——不归一行尾就整段分不开，含占位符的模板会全量落到动态段，
	// 稳定块白白离开 system（不影响正确性，但拆分等于没做）。
	crlf := "块一稳定。\r\n\r\n块二 {{symbol}} 动态。\r\n\r\n块三也稳定。"
	if st, dy := splitPromptStableBlocks(crlf, renderPromptTemplate(crlf, vars), vars); st !=
		"块一稳定。\n\n块三也稳定。" || dy != "块二 600519 动态。" {
		t.Fatalf("CRLF 模板应与 LF 同样分块: st=%q dy=%q", st, dy)
	}
	// CRLF 且无有效占位符：走短路原样返回，行尾不变（与拆分前逐字节一致）。
	crlfPlain := "块一。\r\n\r\n块二。"
	if st, dy := splitPromptStableBlocks(crlfPlain, crlfPlain, vars); st != crlfPlain || dy != "" {
		t.Fatalf("无占位符的 CRLF 模板应原样返回: st=%q dy=%q", st, dy)
	}
}
