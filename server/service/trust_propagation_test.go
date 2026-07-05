package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestAnalysisEvidenceBackfill 分析证据核验回填：结论文本引用的数字与快照值域比对，
// Verify=false 时不发起复核（usage 为空）。
func TestAnalysisEvidenceBackfill(t *testing.T) {
	svc := &AnalysisService{}
	snapshot := map[string]any{
		"quote":       map[string]any{"price": 12.34},
		"technicals":  map[string]any{"ma20": 11.98},
		"quant_score": map[string]any{"total": 78.5},
		"recent_bars": []map[string]any{{"c": 999.99}}, // 应整棵排除，不进值域
	}
	result := &AnalysisResult{
		Rating:     model.AnalysisRatingBullish,
		Confidence: 80,
		Summary:    "现价 12.34 站上 MA20=11.98",
		Highlights: []string{"quant_score 78.5 偏强"},
	}
	usage := svc.fillAnalysisTrust(context.Background(), &model.LLMConfig{}, "", false, AnalyzeRequest{Verify: false}, snapshot, result)
	if usage.TotalTokens != 0 {
		t.Fatalf("Verify=false 不应发起复核，usage=%+v", usage)
	}
	if result.EvidenceCheck == nil {
		t.Fatalf("EvidenceCheck 未回填")
	}
	if result.EvidenceCheck.Total != 3 || result.EvidenceCheck.Matched != 3 {
		t.Fatalf("证据核验应 3/3（12.34/11.98/78.5，MA20 的 20 与 recent_bars 999.99 不计），得到 %d/%d 未吻合=%v",
			result.EvidenceCheck.Matched, result.EvidenceCheck.Total, result.EvidenceCheck.Unmatched)
	}
	if result.SysConfidence != "high" {
		t.Fatalf("全吻合+个股技术锚点齐全应为 high，得到 %s（%s）", result.SysConfidence, result.SysConfidenceWhy)
	}
}

// TestAnalysisSystemConfidence 程序合成置信度三档。
func TestAnalysisSystemConfidence(t *testing.T) {
	stock := func(extra map[string]any) map[string]any {
		s := map[string]any{"quote": map[string]any{"price": 10.0}, "technicals": map[string]any{}, "quant_score": map[string]any{}}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	// high：证据高吻合(+1)、锚点齐全、无偏旧 → 2。
	if lvl, _ := analysisSystemConfidence(&evidenceCheck{Total: 10, Matched: 9}, stock(nil)); lvl != "high" {
		t.Fatalf("期望 high，得到 %s", lvl)
	}
	// low：证据低吻合(-1) + 缺技术因子(-1) → -1 clamp 0。
	if lvl, _ := analysisSystemConfidence(&evidenceCheck{Total: 10, Matched: 2},
		map[string]any{"quote": map[string]any{"price": 10.0}}); lvl != "low" {
		t.Fatalf("期望 low，得到 %s", lvl)
	}
	// medium：无证据、非个股快照（不扣锚点）→ 1。
	if lvl, _ := analysisSystemConfidence(nil, map[string]any{"indices": []any{}}); lvl != "medium" {
		t.Fatalf("期望 medium，得到 %s", lvl)
	}
	// 数据偏旧扣一档：high 基线(+1) 遇 freshness_note(-1) → medium。
	if lvl, _ := analysisSystemConfidence(&evidenceCheck{Total: 10, Matched: 9},
		stock(map[string]any{"freshness_note": "偏旧"})); lvl != "medium" {
		t.Fatalf("偏旧应降一档到 medium，得到 %s", lvl)
	}
}

// TestAnalysisReviewRejectCascade AI 复核 reject 级联：SysConfidence 强制 low，
// 复核置信度覆盖原值；FlexInt 容忍字符串数字。
func TestAnalysisReviewRejectCascade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			// confidence 故意用字符串，验证 FlexInt 容忍。
			"choices": []map[string]any{{"message": map[string]any{"content": `{"verdict":"reject","comment":"引用数字与快照明显不符","confidence":"15"}`}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer srv.Close()

	svc := &AnalysisService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "gpt-x", MaxTokens: 100}
	snapshot := map[string]any{"quote": map[string]any{"price": 10.0}, "technicals": map[string]any{}, "quant_score": map[string]any{}}
	result := &AnalysisResult{Rating: model.AnalysisRatingBullish, Confidence: 80, Summary: "现价 10 强势"}

	usage := svc.fillAnalysisTrust(context.Background(), cfg, "k", true, AnalyzeRequest{Verify: true}, snapshot, result)
	if usage.TotalTokens != 15 {
		t.Fatalf("复核 token 应累计 15，得到 %d", usage.TotalTokens)
	}
	if result.Review == nil || result.Review.Verdict != "reject" {
		t.Fatalf("复核结论应为 reject，得到 %+v", result.Review)
	}
	if int(result.Review.Confidence) != 15 {
		t.Fatalf("FlexInt 应把 \"15\" 解析为 15，得到 %d", int(result.Review.Confidence))
	}
	if result.SysConfidence != "low" {
		t.Fatalf("reject 应级联把 SysConfidence 压到 low，得到 %s", result.SysConfidence)
	}
	if int(result.Confidence) != 15 {
		t.Fatalf("复核置信度应覆盖原 confidence 为 15，得到 %d", int(result.Confidence))
	}
}

// TestCompareValueSetContainsRows 对比点评核验值域包含各行指标字段。
func TestCompareValueSetContainsRows(t *testing.T) {
	rows := []CompareRow{
		{Symbol: "600000", QuoteOK: true, Price: 12.34, Score: 78, ChangePct5d: 3.21},
		{Symbol: "000001", QuoteOK: true, Price: 45.67, Score: 52},
	}
	vals := snapshotValueSet(rows)
	has := func(x float64) bool {
		for _, v := range vals {
			if v == x {
				return true
			}
		}
		return false
	}
	for _, want := range []float64{12.34, 78, 3.21, 45.67, 52} {
		if !has(want) {
			t.Fatalf("值域缺少 %v，got %v", want, vals)
		}
	}
}

// TestDailyReviewEvidence 日报复盘证据核验（纯计算）。提醒文案（[]string）里的
// 触发价必须并入值域——模型忠实引用提醒里的价格不是幻觉。
func TestDailyReviewEvidence(t *testing.T) {
	snap := &reportSnapshot{
		Market:    &reportMarket{FundFlow: map[string]any{"main_net_yi": 23.5}},
		Positions: []reportPosition{{Symbol: "600000", ProfitPct: -4.2}},
		Alerts:    []string{"浦发银行(600000) 现价 12.34 站上 MA20（12.10）"},
	}
	rv := &dailyReview{
		Summary:      "主力净流入 23.5 亿",
		WatchReview:  "某持仓今日 -4.2% 需留意；600000 站上 MA20（12.10）",
		RiskWarnings: []string{"目标价 88.88 属推算"}, // 未吻合
	}
	ev := dailyReviewEvidence(rv, snap)
	if ev == nil {
		t.Fatalf("EvidenceCheck 未回填")
	}
	if ev.Total != 4 || ev.Matched != 3 {
		t.Fatalf("应 3/4 吻合（23.5/4.2/12.10 吻合，88.88 未吻合），得到 %d/%d 未吻合=%v", ev.Matched, ev.Total, ev.Unmatched)
	}
}

// TestQaMessageCheckJSON assistant 消息的 CheckJSON 列落库并随详情回传（AutoMigrate 加列）。
func TestQaMessageCheckJSON(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM ai_conversations")
	common.DB.Exec("DELETE FROM ai_conversation_messages")
	svc := &QaService{}

	conv := &model.AiConversation{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行", MessageCount: 2}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	check := &evidenceCheck{Total: 2, Matched: 1, Unmatched: []string{"88.88"}}
	cj, _ := json.Marshal(check)
	common.DB.Create(&model.AiConversationMessage{ConversationID: conv.ID, UserID: 1, Role: model.QaRoleUser, Content: "均线？"})
	common.DB.Create(&model.AiConversationMessage{ConversationID: conv.ID, UserID: 1, Role: model.QaRoleAssistant,
		Content: "站上 MA20", CheckJSON: string(cj)})

	v, err := svc.Get(1, conv.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("应有 2 条消息，得到 %d", len(v.Messages))
	}
	var got *evidenceCheck
	for _, m := range v.Messages {
		if m.Role == model.QaRoleAssistant {
			if m.CheckJSON == "" {
				t.Fatalf("assistant 消息 CheckJSON 未落库")
			}
			var ec evidenceCheck
			if json.Unmarshal([]byte(m.CheckJSON), &ec) != nil {
				t.Fatalf("CheckJSON 非合法 JSON: %s", m.CheckJSON)
			}
			got = &ec
		}
	}
	if got == nil || got.Total != 2 || got.Matched != 1 {
		t.Fatalf("CheckJSON 回读不符: %+v", got)
	}
}
