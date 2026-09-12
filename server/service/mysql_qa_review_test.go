package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMySQLQaGetSeesOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.AiConversation{}, &model.AiConversationMessage{})
	conv := model.AiConversation{UserID: 1921, Symbol: "600000", Market: "cn", DataSnapshot: `{"quote":{"price":10}}`}
	if err := db.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var readOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_mysql_qa_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "ai_conversations" {
			readOnce.Do(func() {
				close(read)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
					tx.AddError(errors.New("reader was not released"))
				}
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	type outcome struct {
		view *QaConversationView
		err  error
	}
	done := make(chan outcome, 1)
	go func() { view, err := (&QaService{}).Get(conv.UserID, conv.ID); done <- outcome{view, err} }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("详情读取未进入会话查询")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		messages := []model.AiConversationMessage{
			{ConversationID: conv.ID, UserID: conv.UserID, Role: model.QaRoleUser, Content: "问题"},
			{ConversationID: conv.ID, UserID: conv.UserID, Role: model.QaRoleAssistant, Content: "回答"},
		}
		if err := tx.Create(&messages).Error; err != nil {
			return err
		}
		return tx.Model(&conv).Updates(map[string]any{"message_count": 2, "total_tokens": 12}).Error
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil || result.view == nil || result.view.MessageCount != 0 || len(result.view.Messages) != 0 || result.view.TotalTokens != 0 {
			t.Fatalf("同一读取应返回完整旧快照，不能拼接旧计数与新消息：view=%+v err=%v", result.view, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("详情读取未完成")
	}
}

func TestMySQLQaCommitRejectsCancellationWhileWaiting(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.AiConversation{}, &model.AiConversationMessage{}, &model.JobRun{})
	conv := model.AiConversation{UserID: 1922, Symbol: "600000", Market: "cn", DataSnapshot: `{}`}
	job := model.JobRun{UserID: conv.UserID, Kind: JobKindQA, Status: model.JobStatusRunning, QueuedAt: time.Now()}
	if err := db.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	writer := db.Begin()
	if writer.Error != nil {
		t.Fatal(writer.Error)
	}
	defer writer.Rollback()
	var locked model.JobRun
	if err := writer.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	const callback = "review_mysql_qa_cancel_wait"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" {
			once.Do(func() { close(waiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ctx = withJobExecution(ctx, jobExecution{jobID: job.ID})
	ac := &qaAskContext{conv: conv, question: "问题", cfg: &model.LLMConfig{Model: "local"}, run: newLLMRun("qa-review", "", "qa", "qa.free_text.v1", qaPromptVersion)}
	done := make(chan error, 1)
	go func() {
		_, err := (&QaService{}).finalizeAsk(ctx, conv.UserID, ac, &chatResult{Content: "回答"})
		done <- err
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("结果提交未竞争 JobRun 锁")
	}
	if err := writer.Model(&job).Update("cancel_requested", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("必须读取锁等待期间已提交的取消事实：%v", err)
	}
	var count int64
	if err := db.Model(&model.AiConversationMessage{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("取消后不得写入消息：count=%d err=%v", count, err)
	}
}
