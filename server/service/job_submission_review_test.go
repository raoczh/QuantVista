package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestJobSubmissionReplyFailureDoesNotLeaveAcceptedTask(t *testing.T) {
	resetDurableJobs(t)
	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	handler := durableJobHandler{timeout: time.Minute, run: func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		return DurableJobResult{Value: map[string]bool{"local": true}, Status: model.JobStatusSuccess}, nil
	}}
	const callback = "review_llm_submission_reply"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "llm_tasks" {
			tx.AddError(errors.New("injected receipt read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	if _, err := runtime.start(741, "review_task_receipt", map[string]int{"version": 1}, false, nil, &handler); err == nil {
		t.Fatal("回执故障未触发")
	}
	var count int64
	if err := common.DB.Model(&model.JobRun{}).Where("user_id = ?", 741).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("提交回复报错后仍存在 %d 个已接受的任务", count)
	}
}
