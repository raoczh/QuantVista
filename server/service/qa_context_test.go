package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// P2-3 QA 多层上下文测试（qa_context.go）：轮次切分/Tier2 索引预算/Tier3 相关性检索
// 纯函数 + flag 关回退等价 + ContextJSON 落库端到端。

func setLayeredContextFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMLayeredContext(v); err != nil {
		t.Fatalf("切换多层上下文开关失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMLayeredContext(true) })
}

func qaMsg(role, content string) model.AiConversationMessage {
	return model.AiConversationMessage{Role: role, Content: content}
}

func TestQaSplitRounds(t *testing.T) {
	rounds := qaSplitRounds([]model.AiConversationMessage{
		qaMsg(model.QaRoleUser, "第一问"),
		qaMsg(model.QaRoleAssistant, "第一答"),
		qaMsg(model.QaRoleAssistant, "补充段"), // 连续 assistant 并入同轮
		qaMsg(model.QaRoleUser, "第二问"),      // 无回答的尾轮
	})
	if len(rounds) != 2 {
		t.Fatalf("应 2 轮: %+v", rounds)
	}
	if rounds[0].Idx != 1 || rounds[0].User != "第一问" || rounds[0].Assistant != "第一答\n补充段" {
		t.Fatalf("第一轮不符: %+v", rounds[0])
	}
	if rounds[1].Idx != 2 || rounds[1].User != "第二问" || rounds[1].Assistant != "" {
		t.Fatalf("第二轮不符: %+v", rounds[1])
	}
	// 开头孤儿 assistant 并入虚拟首轮（防御路径）。
	rounds = qaSplitRounds([]model.AiConversationMessage{qaMsg(model.QaRoleAssistant, "孤儿")})
	if len(rounds) != 1 || rounds[0].Assistant != "孤儿" {
		t.Fatalf("孤儿 assistant 应并入首轮: %+v", rounds)
	}
}

func TestQaLayeredContextNoOlder(t *testing.T) {
	// 无被裁剪历史：零注入零噪声，但分层快照仍统计 Tier1（观测一致性）。
	recent := []model.AiConversationMessage{
		qaMsg(model.QaRoleUser, "问一"), qaMsg(model.QaRoleAssistant, "答一"),
	}
	lc := buildQaLayeredContext(nil, recent, "新问题")
	if lc.Segment != "" {
		t.Fatalf("无 older 不应注入: %q", lc.Segment)
	}
	if lc.Layers == nil || lc.Layers.Tier1Msgs != 2 || lc.Layers.HistoryMsgs != 2 ||
		lc.Layers.Tier2Rounds != 0 || lc.Layers.Tier3Rounds != 0 || lc.Layers.InvisibleRounds != 0 {
		t.Fatalf("Tier1 统计不符: %+v", lc.Layers)
	}
}

func TestQaLayeredContextTiers(t *testing.T) {
	// older 三轮：轮 1 与本轮问题高度相关（Tier3 命中）、轮 2/3 无关（Tier2 索引）。
	older := []model.AiConversationMessage{
		qaMsg(model.QaRoleUser, "公司的半导体设备订单情况怎么样"),
		qaMsg(model.QaRoleAssistant, "快照显示半导体设备订单同比增长，景气度较高。"),
		qaMsg(model.QaRoleUser, "分红率高吗"),
		qaMsg(model.QaRoleAssistant, "股息率约 2%。"),
		qaMsg(model.QaRoleUser, "换手率怎么看"),
		qaMsg(model.QaRoleAssistant, "换手率 3% 属于正常水平。"),
	}
	recent := []model.AiConversationMessage{qaMsg(model.QaRoleUser, "近况"), qaMsg(model.QaRoleAssistant, "略")}
	lc := buildQaLayeredContext(older, recent, "半导体设备订单还在增长吗")
	l := lc.Layers
	if l.Tier3Rounds != 1 || len(l.Tier3Matched) != 1 || l.Tier3Matched[0].Round != 1 {
		t.Fatalf("轮 1 应被 Tier3 命中: %+v", l)
	}
	if l.Tier3Matched[0].Score < qaTier3MinScore {
		t.Fatalf("命中分应达门槛: %+v", l.Tier3Matched)
	}
	if l.Tier2Rounds != 2 || l.Tier2DroppedRounds != 0 || l.InvisibleRounds != 0 {
		t.Fatalf("轮 2/3 应进 Tier2 索引: %+v", l)
	}
	if !strings.Contains(lc.Segment, "【历史会话分层上下文】") ||
		!strings.Contains(lc.Segment, "第 1 轮（相关度") ||
		!strings.Contains(lc.Segment, "第 2 轮｜问：分红率高吗") {
		t.Fatalf("注入段不符:\n%s", lc.Segment)
	}
	// Tier3 摘录含旧回答文本（截断），Tier2 只有要点行。
	if !strings.Contains(lc.Segment, "景气度较高") {
		t.Fatalf("Tier3 应含旧回答摘录:\n%s", lc.Segment)
	}

	// 不相关问题：Tier3 零命中，三轮全部进 Tier2。
	lc = buildQaLayeredContext(older, recent, "今天天气如何呢")
	if lc.Layers.Tier3Rounds != 0 || lc.Layers.Tier2Rounds != 3 {
		t.Fatalf("不相关问题不应命中 Tier3: %+v", lc.Layers)
	}
}

func TestQaLayeredContextBudgetDrop(t *testing.T) {
	// 构造超预算：24 轮，每轮索引行约 78 rune（Q 截 qaTier2QMax=60）→ 总量 ~1870 超
	// qaTier2CharBudget=1200；最早的轮被丢弃且 invisible 可见。
	var older []model.AiConversationMessage
	for i := 1; i <= 24; i++ {
		q := fmt.Sprintf("第%02d个问题·", i) + strings.Repeat("很长的背景描述", 30)
		older = append(older, qaMsg(model.QaRoleUser, q), qaMsg(model.QaRoleAssistant, "简短回答"))
	}
	lc := buildQaLayeredContext(older, nil, "与历史完全无关的新话题")
	l := lc.Layers
	if l.Tier2DroppedRounds == 0 || l.InvisibleRounds != l.Tier2DroppedRounds {
		t.Fatalf("超预算应有不可见轮: %+v", l)
	}
	if l.Tier2Rounds+l.Tier2DroppedRounds != 24 {
		t.Fatalf("索引+丢弃应覆盖全部 24 轮: %+v", l)
	}
	if l.Tier2Chars > qaTier2CharBudget {
		t.Fatalf("索引层字符超预算: %d > %d", l.Tier2Chars, qaTier2CharBudget)
	}
	// 索引保最近：被丢弃的是最早的轮（段里不含「第01个问题」、含最后一轮）。
	if strings.Contains(lc.Segment, "第01个问题") || !strings.Contains(lc.Segment, "第24个问题") {
		t.Fatalf("预算应优先保留最近轮:\n%s", lc.Segment)
	}
	if !strings.Contains(lc.Segment, fmt.Sprintf("最早 %d 轮因篇幅未列出", l.Tier2DroppedRounds)) {
		t.Fatalf("被丢弃轮数应在段内声明:\n%s", lc.Segment)
	}
}

// TestQaBuildMessagesLayeredFlagOff flag 关=回退旧静默截断：system 不含分层段且与
// flag 开时「去掉分层段后的 system」逐字节一致；消息条数/顺序不变；分层快照仍产出
// （观测不受 flag 控）且如实反映「未注入」。
func TestQaBuildMessagesLayeredFlagOff(t *testing.T) {
	setLayeredContextFlag(t, true)
	conv := model.AiConversation{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		DataSnapshot: `{"symbol":"600000","quote":{"price":10},"freshness_status":"fresh"}`}
	var history []model.AiConversationMessage
	for i := 1; i <= 8; i++ { // 16 条 > qaHistoryLimit=12 → older 4 条（2 轮）
		history = append(history,
			qaMsg(model.QaRoleUser, fmt.Sprintf("历史问题 %d", i)),
			qaMsg(model.QaRoleAssistant, fmt.Sprintf("历史回答 %d", i)))
	}
	svc := NewQaService(nil, NewLLMService())
	pr := loadPromptRuntime(1, model.PromptModuleQa)

	msgsOn, layersOn := svc.buildMessagesFrom(pr, conv, history, "新问题")
	if layersOn == nil || layersOn.Tier2Rounds == 0 {
		t.Fatalf("flag 开应有 Tier2 索引: %+v", layersOn)
	}
	sysOn := msgsOn[0].Content
	if !strings.Contains(sysOn, "【历史会话分层上下文】") {
		t.Fatalf("flag 开 system 应含分层段")
	}

	if err := setting.SetLLMLayeredContext(false); err != nil {
		t.Fatalf("关 flag 失败: %v", err)
	}
	msgsOff, layersOff := svc.buildMessagesFrom(pr, conv, history, "新问题")
	sysOff := msgsOff[0].Content
	if strings.Contains(sysOff, "【历史会话分层上下文】") {
		t.Fatalf("flag 关 system 不得含分层段")
	}
	// 逐字节等价锁定：sysOn 去掉分层段（\n\n+segment）后 == sysOff。
	start := strings.Index(sysOn, "\n\n【历史会话分层上下文】")
	end := strings.Index(sysOn, "\n\n对象：")
	if start < 0 || end <= start {
		t.Fatalf("分层段定位失败")
	}
	if sysOn[:start]+sysOn[end:] != sysOff {
		t.Fatalf("flag 关的 system 应与旧版逐字节一致")
	}
	if len(msgsOn) != len(msgsOff) || len(msgsOff) != 1+qaHistoryLimit+1 {
		t.Fatalf("消息条数不符: on=%d off=%d", len(msgsOn), len(msgsOff))
	}
	for i := 1; i < len(msgsOff); i++ {
		if msgsOff[i] != msgsOn[i] {
			t.Fatalf("历史窗口消息应一致: idx=%d", i)
		}
	}
	// flag 关的分层快照如实反映「未注入」：Tier2/3 清零、全部 older 轮不可见。
	if layersOff == nil || layersOff.Tier2Rounds != 0 || layersOff.Tier3Rounds != 0 ||
		layersOff.InvisibleRounds != 2 {
		t.Fatalf("flag 关快照应如实记未注入: %+v", layersOff)
	}
}

// TestQaAskContextJSONEndToEnd 端到端：ask 后 assistant 消息带 context_json（短会话
// 也落 Tier1 统计）；核验值域不受分层注入影响（历史 user 消息全量进值域的既有语义不变）。
func TestQaAskContextJSONEndToEnd(t *testing.T) {
	setLayeredContextFlag(t, true)
	common.DB.Exec("DELETE FROM ai_conversations")
	common.DB.Exec("DELETE FROM ai_conversation_messages")
	common.DB.Exec("DELETE FROM llm_configs")
	common.DB.Exec("DELETE FROM user_quota")
	common.EncryptionKey = "unit-test-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "回答正文"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer srv.Close()

	cipher, _ := common.Encrypt("sk-test")
	cfg := &model.LLMConfig{UserID: 8, Name: "t", Provider: "openai", BaseURL: srv.URL,
		APIKeyCipher: cipher, Model: "m", IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建配置失败: %v", err)
	}
	conv := &model.AiConversation{UserID: 8, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Title: "t", LLMConfigID: cfg.ID, Provider: "openai", Model: "m",
		DataSnapshot: `{"symbol":"600000","quote":{"price":10}}`}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	svc := NewQaService(nil, NewLLMService())
	view, err := svc.Ask(context.Background(), 8, true, QaAskRequest{ConversationID: conv.ID, Question: "如何"})
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	ans := view.Messages[1]
	if ans.ContextJSON == "" {
		t.Fatalf("assistant 消息应带 context_json")
	}
	var layers QaContextLayers
	if err := json.Unmarshal([]byte(ans.ContextJSON), &layers); err != nil {
		t.Fatalf("context_json 解析失败: %v", err)
	}
	if layers.Version != qaCtxVersion || layers.HistoryMsgs != 0 || layers.ApproxTokens <= 0 {
		t.Fatalf("短会话分层快照不符: %+v", layers)
	}
	// user 消息不带 context_json（只有回答落）。
	if view.Messages[0].ContextJSON != "" {
		t.Fatalf("user 消息不应带 context_json")
	}
}
