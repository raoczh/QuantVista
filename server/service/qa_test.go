package service

import (
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestQaBuildMessages 系统消息含快照，历史仅带最近 qaHistoryLimit 条，末尾为本轮提问。
func TestQaBuildMessages(t *testing.T) {
	svc := &QaService{}
	conv := model.AiConversation{Symbol: "600000", Name: "浦发银行", DataSnapshot: `{"symbol":"600000","quote":{"price":10}}`}

	// 构造超过上限的历史。
	var history []model.AiConversationMessage
	for i := 0; i < qaHistoryLimit+6; i++ {
		role := model.QaRoleUser
		if i%2 == 1 {
			role = model.QaRoleAssistant
		}
		history = append(history, model.AiConversationMessage{Role: role, Content: "历史消息"})
	}

	msgs := svc.buildMessages(conv, history, "现在的均线怎么样？")
	if msgs[0].Role != "system" {
		t.Fatalf("首条应为 system")
	}
	if !strings.Contains(msgs[0].Content, "600000") || !strings.Contains(msgs[0].Content, "个股数据快照") {
		t.Fatalf("系统消息应含快照与说明")
	}
	// 1 system + qaHistoryLimit 历史 + 1 本轮提问。
	if len(msgs) != 1+qaHistoryLimit+1 {
		t.Fatalf("消息数应为 %d，得到 %d", 1+qaHistoryLimit+1, len(msgs))
	}
	if msgs[len(msgs)-1].Role != "user" || msgs[len(msgs)-1].Content != "现在的均线怎么样？" {
		t.Fatalf("末条应为本轮用户提问")
	}
}

// TestQaListGetDelete DB 集成：列表不含重字段、详情含消息、用户隔离、删除级联。
func TestQaListGetDelete(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM ai_conversations")
	common.DB.Exec("DELETE FROM ai_conversation_messages")
	svc := &QaService{}

	conv := &model.AiConversation{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Title: "均线情况", Model: "gpt-x", Provider: "openai", DataSnapshot: `{"x":1}`, MessageCount: 2, TotalTokens: 100}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatalf("插入会话失败: %v", err)
	}
	common.DB.Create(&model.AiConversationMessage{ConversationID: conv.ID, UserID: 1, Role: model.QaRoleUser, Content: "均线怎么样"})
	common.DB.Create(&model.AiConversationMessage{ConversationID: conv.ID, UserID: 1, Role: model.QaRoleAssistant, Content: "站上 MA20"})
	// 他人会话。
	common.DB.Create(&model.AiConversation{UserID: 2, Symbol: "000001", Market: "cn"})

	// List：仅本人、不含快照。
	rows, err := svc.List(1, 30)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("用户1 应有 1 个会话，得到 %d", len(rows))
	}
	if rows[0].DataSnapshot != "" {
		t.Fatalf("列表不应返回快照")
	}

	// Get：含消息、快照已清空。
	v, err := svc.Get(1, conv.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if len(v.Messages) != 2 || v.DataSnapshot != "" {
		t.Fatalf("详情应含 2 条消息且不回传快照: %+v", v)
	}

	// 跨用户 Get/Delete 隔离。
	if _, err := svc.Get(2, conv.ID); err == nil {
		t.Fatalf("跨用户 Get 应失败")
	}
	if err := svc.Delete(2, conv.ID); err == nil {
		t.Fatalf("跨用户 Delete 应失败")
	}

	// 本人删除级联删消息。
	if err := svc.Delete(1, conv.ID); err != nil {
		t.Fatalf("本人删除失败: %v", err)
	}
	var msgCnt int64
	common.DB.Model(&model.AiConversationMessage{}).Where("conversation_id = ?", conv.ID).Count(&msgCnt)
	if msgCnt != 0 {
		t.Fatalf("删除会话应级联删消息，剩 %d", msgCnt)
	}
}

// TestQaConversationFromAnalysis 从分析记录发起问答：快照复用、归属校验、模块校验（DB 集成）。
func TestQaConversationFromAnalysis(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM analysis_records")
	common.DB.Exec("DELETE FROM ai_conversations")
	cfg := &model.LLMConfig{ID: 9, Provider: "openai", Model: "gpt-x"}

	rec := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600519",
		Target: "贵州茅台", Status: model.AnalysisStatusSuccess, DataSnapshot: `{"quote":{"price":1700}}`}
	common.DB.Create(rec)
	nonStock := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleMarket, Market: "cn",
		Status: model.AnalysisStatusSuccess, DataSnapshot: `{"x":1}`}
	common.DB.Create(nonStock)

	conv, err := qaConversationFromAnalysis(1, rec.ID, cfg, "现在的估值贵吗", qaPromptVersion)
	if err != nil {
		t.Fatalf("从个股分析发起问答失败: %v", err)
	}
	if conv.DataSnapshot != rec.DataSnapshot {
		t.Fatalf("会话快照应复用分析快照")
	}
	if conv.Symbol != "600519" || conv.Name != "贵州茅台" {
		t.Fatalf("标的信息应取自分析记录: %+v", conv)
	}
	if conv.PromptVersion != qaPromptVersion {
		t.Fatalf("会话应固化调用方传入的版本快照: %q", conv.PromptVersion)
	}

	// 非个股模块拒绝。
	if _, err := qaConversationFromAnalysis(1, nonStock.ID, cfg, "q", qaPromptVersion); err == nil {
		t.Fatalf("非个股分析应拒绝")
	}
	// 跨用户拒绝。
	if _, err := qaConversationFromAnalysis(2, rec.ID, cfg, "q", qaPromptVersion); err == nil {
		t.Fatalf("跨用户应拒绝")
	}
}
