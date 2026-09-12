package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func setupQaReview(t *testing.T) (*QaService, model.AiConversation, model.LLMConfig) {
	t.Helper()
	setupTestDB(t)
	user := model.User{Username: "qa-review-" + strings.ReplaceAll(t.Name(), "/", "-"), Role: model.RoleAdmin, Status: model.StatusEnabled}
	if err := common.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	oldKey := common.EncryptionKey
	common.EncryptionKey = "qa-review-key"
	t.Cleanup(func() { common.EncryptionKey = oldKey })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"审查回答"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`))
	}))
	t.Cleanup(upstream.Close)
	cipher, err := common.Encrypt("local-test-key")
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.LLMConfig{UserID: user.ID, Name: "qa-review", Provider: "openai", Model: "current-model",
		BaseURL: upstream.URL, APIKeyCipher: cipher, IsDefault: true}
	if err := common.DB.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}
	conv := model.AiConversation{UserID: user.ID, Symbol: "600000", Market: "cn", Name: "测试股票",
		LLMConfigID: cfg.ID, Provider: cfg.Provider, Model: "previous-model", DataSnapshot: `{"quote":{"price":10},"freshness_status":"fresh"}`}
	if err := common.DB.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	return NewQaService(nil, NewLLMService()), conv, cfg
}

func afterQaAudit(t *testing.T, action func()) {
	t.Helper()
	const name = "review_qa_after_audit_commit"
	if err := common.DB.Callback().Create().After("gorm:commit_or_rollback_transaction").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "llm_call_logs" && tx.Error == nil {
			action()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(name) })
}

func TestQaReviewRejectsCancellationAfterModelResponse(t *testing.T) {
	for _, mode := range []string{"context", "stream", "job"} {
		t.Run(mode, func(t *testing.T) {
			svc, conv, _ := setupQaReview(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "job" {
				job := model.JobRun{Kind: JobKindQA, UserID: conv.UserID, Status: model.JobStatusRunning}
				if err := common.DB.Create(&job).Error; err != nil {
					t.Fatal(err)
				}
				ctx = withJobExecution(ctx, jobExecution{jobID: job.ID})
				afterQaAudit(t, func() {
					if err := common.DB.Model(&job).Update("cancel_requested", true).Error; err != nil {
						t.Error(err)
					}
				})
			} else {
				afterQaAudit(t, cancel)
			}
			var err error
			req := QaAskRequest{ConversationID: conv.ID, Question: "问题"}
			if mode == "stream" {
				_, err = svc.AskStream(ctx, conv.UserID, true, req, nil)
			} else {
				_, err = svc.Ask(ctx, conv.UserID, true, req)
			}
			var count int64
			if dbErr := common.DB.Model(&model.AiConversationMessage{}).Where("conversation_id = ?", conv.ID).Count(&count).Error; dbErr != nil {
				t.Fatal(dbErr)
			}
			if !errors.Is(err, context.Canceled) || count != 0 {
				t.Fatalf("取消已先于结果提交，不得追加消息：err=%v messages=%d", err, count)
			}
		})
	}
}

func TestQaReviewRejectsDeletedConversationBeforeCommit(t *testing.T) {
	svc, conv, _ := setupQaReview(t)
	afterQaAudit(t, func() {
		if err := common.DB.Delete(&model.AiConversation{}, conv.ID).Error; err != nil {
			t.Error(err)
		}
	})
	_, err := svc.Ask(context.Background(), conv.UserID, true, QaAskRequest{ConversationID: conv.ID, Question: "问题"})
	var count int64
	if dbErr := common.DB.Model(&model.AiConversationMessage{}).Where("conversation_id = ?", conv.ID).Count(&count).Error; dbErr != nil {
		t.Fatal(dbErr)
	}
	if err == nil || count != 0 {
		t.Fatalf("提交时会话已不存在，不得留下孤儿消息：err=%v messages=%d", err, count)
	}
}

func TestQaReviewCleansFailedNewConversation(t *testing.T) {
	for _, stage := range []string{"history", "commit"} {
		t.Run(stage, func(t *testing.T) {
			svc, conv, _ := setupQaReview(t)
			rec := model.AnalysisRecord{UserID: conv.UserID, Module: model.AnalysisModuleStock, Symbol: conv.Symbol, Market: conv.Market, DataSnapshot: conv.DataSnapshot}
			if err := common.DB.Create(&rec).Error; err != nil {
				t.Fatal(err)
			}
			const callback = "review_qa_failed_new_conversation"
			failure := errors.New("injected qa write/read failure")
			inject := func(tx *gorm.DB) {
				if tx.Statement.Table == "ai_conversation_messages" {
					tx.AddError(failure)
				}
			}
			if stage == "history" {
				if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, inject); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
			} else {
				if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, inject); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
			}
			_, err := svc.Ask(context.Background(), conv.UserID, true, QaAskRequest{AnalysisRecordID: rec.ID, Question: "问题"})
			var count int64
			if dbErr := common.DB.Model(&model.AiConversation{}).Where("user_id = ? AND id <> ?", conv.UserID, conv.ID).Count(&count).Error; dbErr != nil {
				t.Fatal(dbErr)
			}
			if !errors.Is(err, failure) || count != 0 {
				t.Fatalf("新提问失败应清理自己的空会话：err=%v empty_conversations=%d", err, count)
			}
		})
	}
}

func TestQaReviewUsesLatestAnswerModel(t *testing.T) {
	svc, conv, cfg := setupQaReview(t)
	view, err := svc.Ask(context.Background(), conv.UserID, true, QaAskRequest{ConversationID: conv.ID, Question: "问题"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Model != cfg.Model || view.Provider != cfg.Provider || view.LLMConfigID != cfg.ID {
		t.Fatalf("会话摘要应随本轮实际使用模型更新：got=%s/%s/%d", view.Provider, view.Model, view.LLMConfigID)
	}
}

func TestQaReviewUsesAcceptedRouteModel(t *testing.T) {
	svc, conv, cfg := setupQaReview(t)
	setModelRoutingFlag(t, true)
	invalidateLLMRouteCache()
	t.Cleanup(invalidateLLMRouteCache)
	routed := cfg
	routed.ID, routed.IsDefault = 0, false
	routed.Name, routed.Provider, routed.Model = "qa-routed", "routed-provider", "routed-model"
	if err := common.DB.Create(&routed).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: routed.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	view, err := svc.Ask(context.Background(), conv.UserID, true, QaAskRequest{ConversationID: conv.ID, LLMConfigID: cfg.ID, Question: "问题"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Model != routed.Model || view.Provider != routed.Provider || view.LLMConfigID != cfg.ID {
		t.Fatalf("实际路由模型与用户所选配置需分别保留：got=%s/%s/%d", view.Provider, view.Model, view.LLMConfigID)
	}
	var log model.LLMCallLog
	if err := common.DB.Where("run_id = ?", view.Messages[1].RunID).First(&log).Error; err != nil || log.Model != view.Model || log.Provider != view.Provider {
		t.Fatalf("回答归因须与实际请求审计一致：log=%+v err=%v", log, err)
	}
}

func TestQaReviewUnknownFreshnessDoesNotReuseHistoricalFresh(t *testing.T) {
	svc, conv, _ := setupQaReview(t)
	view, err := svc.Get(conv.UserID, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.SnapshotMeta == nil || view.SnapshotMeta.FreshnessStatus != freshStatusFresh || view.SnapshotMeta.CurrentStatus != freshStatusUnknown {
		t.Fatalf("保留创建时的 fresh 事实，但无法核验的当前状态必须为 unknown：%+v", view.SnapshotMeta)
	}
	for _, meta := range []*qaSnapshotMeta{nil, {}, {QuoteAsOf: "invalid"}} {
		if status, note := (&QaService{market: NewMarketService(nil)}).qaCurrentFreshness("cn", meta); status != freshStatusUnknown || note == "" {
			t.Errorf("无法核验须返回未知及原因：status=%q note=%q", status, note)
		}
	}
	messages := svc.buildMessages(conv, nil, "现在可以买吗")
	if !strings.Contains(messages[len(messages)-1].Content, "历史数据解释") {
		t.Fatal("缺少当前行情时间时，模型上下文必须限制为历史数据解释")
	}
}

func TestQaReviewCommittedAnswerWinsLateCancellation(t *testing.T) {
	svc, conv, _ := setupQaReview(t)
	runtime := newJobRuntime(1, 2)
	oldRuntime := defaultJobRuntime
	defaultJobRuntime = runtime
	t.Cleanup(func() { runtime.close(); defaultJobRuntime = oldRuntime })
	svc.registerDurableJobHandler()
	handler, ok := runtime.handlerFor(0, JobKindQA)
	if !ok {
		t.Fatal("缺少 QA 处理器")
	}
	committed, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	runtime.registerWithBinding(JobKindQA, handler.timeout, func(ctx context.Context, userID int64, allowPrivate bool, raw json.RawMessage) (DurableJobResult, error) {
		result, err := handler.run(ctx, userID, allowPrivate, raw)
		close(committed)
		<-release
		return result, err
	}, handler.binding, true)
	task, err := StartDurableLLMTask(conv.UserID, JobKindQA, QaAskRequest{ConversationID: conv.ID, Question: "问题"}, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("回答未及时完成")
	}
	var job model.JobRun
	if err := common.DB.Where("result_id = ? AND result_type = ?", task.ID, JobResultLLMTask).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := CancelJobRun(conv.UserID, job.ID); err != nil {
		t.Fatal(err)
	}
	unblock()
	done := waitJobRun(t, conv.UserID, job.ID)
	if done.Status != model.JobStatusSuccess || done.CancelRequested {
		t.Fatalf("回答已提交，迟到取消不得改成失败：status=%s canceled=%v", done.Status, done.CancelRequested)
	}
	view, err := svc.Get(conv.UserID, conv.ID)
	if err != nil || len(view.Messages) != 2 {
		t.Fatalf("任务成功须对应一轮已提交回答：view=%+v err=%v", view, err)
	}
}

func TestQaReviewNoReadFailureAfterSuccessfulCommit(t *testing.T) {
	svc, conv, _ := setupQaReview(t)
	updated := false
	const callback = "review_qa_post_commit_read_failure"
	if err := common.DB.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "ai_conversations" && tx.Error == nil {
			if values, ok := tx.Statement.Dest.(map[string]any); ok {
				_, updated = values["message_count"]
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Update().Remove(callback) })
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if updated && tx.Statement.Table == "ai_conversations" {
			tx.AddError(errors.New("injected post-commit read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	view, err := svc.Ask(context.Background(), conv.UserID, true, QaAskRequest{ConversationID: conv.ID, Question: "问题"})
	if err != nil || view == nil || len(view.Messages) != 2 || view.MessageCount != 2 || view.TotalTokens != 12 {
		t.Fatalf("已提交的一轮问答必须直接返回完整结果：view=%+v err=%v", view, err)
	}
}

func TestQaReviewMessageLimitAndConcurrentVersion(t *testing.T) {
	for _, count := range []int{qaMaxMessages - 1, 2} {
		t.Run(map[int]string{qaMaxMessages - 1: "limit", 2: "version"}[count], func(t *testing.T) {
			svc, conv, _ := setupQaReview(t)
			afterQaAudit(t, func() {
				if err := common.DB.Model(&conv).Update("message_count", count).Error; err != nil {
					t.Error(err)
				}
			})
			_, err := svc.Ask(context.Background(), conv.UserID, true, QaAskRequest{ConversationID: conv.ID, Question: "问题"})
			var stored int64
			if dbErr := common.DB.Model(&model.AiConversationMessage{}).Where("conversation_id = ?", conv.ID).Count(&stored).Error; dbErr != nil {
				t.Fatal(dbErr)
			}
			if err == nil || stored != 0 {
				t.Fatalf("提交时须复验消息上限与准备阶段版本：count=%d err=%v appended=%d", count, err, stored)
			}
		})
	}
}
