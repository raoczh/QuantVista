package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestQaAskStreamEndToEnd DB 集成：已有会话上流式追问——假 SSE LLM 服务器逐 delta 吐出，
// 验证 增量回调顺序、回答与 CheckJSON 落库（核验后置）、会话计数/token 更新。
func TestQaAskStreamEndToEnd(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM ai_conversations")
	common.DB.Exec("DELETE FROM ai_conversation_messages")
	common.DB.Exec("DELETE FROM llm_configs")
	common.DB.Exec("DELETE FROM user_quota")
	common.EncryptionKey = "unit-test-key" // 内存库测试环境无 env，注入包级密钥供 Encrypt/Decrypt

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, l := range []string{
			`data: {"choices":[{"delta":{"content":"现价 10 高于 "}}]}`,
			`data: {"choices":[{"delta":{"content":"MA20=9.5，短线偏强。"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`,
			`data: [DONE]`,
		} {
			_, _ = w.Write([]byte(l + "\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()

	// LLM 配置（Stream=true）：APIKeyCipher 用 common.Encrypt 与 ResolveForUse 对拍。
	cipher, err := common.Encrypt("sk-test")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	cfg := &model.LLMConfig{UserID: 7, Name: "t", Provider: "openai", BaseURL: srv.URL,
		APIKeyCipher: cipher, Model: "m", Stream: true, IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建配置失败: %v", err)
	}

	// 已有会话（避免走 buildStockSnapshot 拉真实行情）：快照含 MA20=9.5 供核验命中。
	conv := &model.AiConversation{UserID: 7, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Title: "t", LLMConfigID: cfg.ID, Provider: "openai", Model: "m",
		DataSnapshot: `{"symbol":"600000","quote":{"price":10},"technicals":{"ma20":9.5}}`}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	svc := NewQaService(nil, NewLLMService())
	var chunks []string
	view, err := svc.AskStream(context.Background(), 7, true,
		QaAskRequest{ConversationID: conv.ID, Question: "现在走势如何？"},
		func(c string) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatalf("AskStream 失败: %v", err)
	}
	if len(chunks) != 2 || !strings.HasPrefix(chunks[0], "现价") {
		t.Fatalf("增量回调不符: %v", chunks)
	}
	if len(view.Messages) != 2 {
		t.Fatalf("应落库 2 条消息: %d", len(view.Messages))
	}
	ans := view.Messages[1]
	if ans.Role != model.QaRoleAssistant || ans.Content != "现价 10 高于 MA20=9.5，短线偏强。" {
		t.Fatalf("回答落库不符: %+v", ans)
	}
	// 核验后置：CheckJSON 已随流结束写入，且 9.5 命中快照值域。
	if ans.CheckJSON == "" || !strings.Contains(ans.CheckJSON, `"total"`) {
		t.Fatalf("CheckJSON 应已落库: %q", ans.CheckJSON)
	}
	if strings.Contains(ans.CheckJSON, "9.5") {
		t.Fatalf("9.5 在快照值域内，不应出现在 unmatched: %s", ans.CheckJSON)
	}
	if view.TotalTokens != 30 || view.MessageCount != 2 {
		t.Fatalf("会话计数/token 应更新: tokens=%d count=%d", view.TotalTokens, view.MessageCount)
	}
}

// TestQaAskStreamErrorNoOrphan 流式调用失败（上游 401）时不留孤儿消息，错误透出。
func TestQaAskStreamErrorNoOrphan(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM ai_conversations")
	common.DB.Exec("DELETE FROM ai_conversation_messages")
	common.DB.Exec("DELETE FROM llm_configs")
	common.EncryptionKey = "unit-test-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	cipher, _ := common.Encrypt("sk-test")
	cfg := &model.LLMConfig{UserID: 8, Name: "t", Provider: "openai", BaseURL: srv.URL,
		APIKeyCipher: cipher, Model: "m", Stream: true, IsDefault: true}
	common.DB.Create(cfg)
	conv := &model.AiConversation{UserID: 8, Symbol: "600000", Market: "cn", Name: "x",
		LLMConfigID: cfg.ID, DataSnapshot: `{"quote":{"price":10}}`}
	common.DB.Create(conv)

	svc := NewQaService(nil, NewLLMService())
	_, err := svc.AskStream(context.Background(), 8, true,
		QaAskRequest{ConversationID: conv.ID, Question: "?"}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("应透出 401 错误: %v", err)
	}
	var cnt int64
	common.DB.Model(&model.AiConversationMessage{}).Where("conversation_id = ?", conv.ID).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("失败不应落库消息: %d", cnt)
	}
	// 已有会话失败不应被误删（abortNewConv 仅清理新建会话）。
	var c2 model.AiConversation
	if err := common.DB.First(&c2, conv.ID).Error; err != nil {
		t.Fatal("已有会话不应被删除")
	}
}
