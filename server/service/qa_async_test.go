package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestQaAskAsyncRejectsInvalidQuestionBeforeCreatingTask(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM llm_tasks")
	svc := &QaService{}
	for _, req := range []QaAskRequest{
		{Question: "  \r\n "},
		{Question: string(make([]rune, qaMaxQuestionRunes+1))},
	} {
		if _, err := svc.AskAsync(72, false, req); err == nil {
			t.Fatalf("非法问题应在建任务前拒绝: %+v", req)
		}
	}
	var count int64
	if err := common.DB.Model(&model.LLMTask{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("非法问题不应留下后台任务: %d", count)
	}
}

// TestQaAskAsyncReturnsBeforeLLM 验证 HTTP 入口使用的异步壳层：上游仍阻塞时立即返回
// processing，释放上游后后台任务完成，result 只保存会话定位，完整结果从业务表读取。
func TestQaAskAsyncReturnsBeforeLLM(t *testing.T) {
	setupTestDB(t)
	for _, table := range []string{"llm_tasks", "ai_conversation_messages", "ai_conversations", "llm_configs", "user_quota"} {
		common.DB.Exec("DELETE FROM " + table)
	}
	common.EncryptionKey = "unit-test-key"

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"后台回答完成"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`))
	}))
	defer srv.Close()

	cipher, err := common.Encrypt("sk-test")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	cfg := &model.LLMConfig{UserID: 71, Name: "qa-async", Provider: "openai", BaseURL: srv.URL,
		APIKeyCipher: cipher, Model: "m", IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建 LLM 配置失败: %v", err)
	}
	conv := &model.AiConversation{UserID: 71, Symbol: "600000", Market: "cn", Name: "浦发银行",
		LLMConfigID: cfg.ID, Provider: cfg.Provider, Model: cfg.Model,
		DataSnapshot: `{"symbol":"600000","quote":{"price":10}}`}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	svc := NewQaService(nil, NewLLMService())
	started := time.Now()
	task, err := svc.AskAsync(71, true, QaAskRequest{ConversationID: conv.ID, Question: "现在走势如何？"})
	if err != nil {
		t.Fatalf("AskAsync 应创建任务: %v", err)
	}
	if task.ID == 0 || task.Status != model.LLMTaskStatusProcessing {
		t.Fatalf("应立即返回 processing 任务: %+v", task)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("AskAsync 不应等待 LLM，耗时 %v", elapsed)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("后台 runner 未进入 LLM 调用")
	}
	processing, err := GetAsyncLLMTask(71, task.ID)
	if err != nil || processing.Status != model.LLMTaskStatusProcessing {
		t.Fatalf("上游阻塞期间任务应保持 processing: task=%+v err=%v", processing, err)
	}
	releaseUpstream()

	var completed *LLMTaskView
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		completed, err = GetAsyncLLMTask(71, task.ID)
		if err == nil && completed.Status != model.LLMTaskStatusProcessing {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || completed == nil || completed.Status != model.LLMTaskStatusSuccess {
		t.Fatalf("后台问答应完成: task=%+v err=%v", completed, err)
	}
	var result QaTaskResult
	if err := json.Unmarshal(completed.Result, &result); err != nil {
		t.Fatalf("任务 result 应为 QaTaskResult: %v result=%s", err, completed.Result)
	}
	if result.ConversationID != conv.ID {
		t.Fatalf("任务应返回会话定位: %+v", result)
	}
	view, err := svc.Get(71, result.ConversationID)
	if err != nil {
		t.Fatalf("应可从业务表读取最终会话: %v", err)
	}
	if view.ID != conv.ID || len(view.Messages) != 2 || view.Messages[1].Content != "后台回答完成" {
		t.Fatalf("最终会话结果不符: %+v", view)
	}
}

func TestQaDeleteRejectsConversationInFlight(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM llm_tasks")
	common.DB.Exec("DELETE FROM ai_conversation_messages")
	common.DB.Exec("DELETE FROM ai_conversations")

	conv := &model.AiConversation{UserID: 73, Symbol: "600000", Market: "cn", Title: "生成中"}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatal(err)
	}
	mu := qaConvLock(conv.ID)
	mu.Lock()
	if err := (&QaService{}).Delete(73, conv.ID); err == nil {
		mu.Unlock()
		t.Fatal("后台问答持有会话锁时应拒绝删除")
	}
	mu.Unlock()

	task := &model.LLMTask{UserID: 73, Kind: "qa", RequestHash: "delete-guard", Status: model.LLMTaskStatusProcessing}
	if err := common.DB.Create(task).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&QaService{}).Delete(73, conv.ID); err == nil {
		t.Fatal("存在跨实例可见的 processing 任务时应拒绝删除")
	}
	if err := common.DB.Model(task).Update("status", model.LLMTaskStatusFailed).Error; err != nil {
		t.Fatal(err)
	}

	if err := (&QaService{}).Delete(73, conv.ID); err != nil {
		t.Fatalf("任务结束后应允许删除: %v", err)
	}
}
