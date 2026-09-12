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

func TestJobResultCommitRejectsEarlierCancellation(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "主业务", true: "嵌套业务"}[nested], func(t *testing.T) {
			resetDurableJobs(t)
			const userID int64 = 957
			runtime := newJobRuntime(1, 2)
			oldRuntime := defaultJobRuntime
			defaultJobRuntime = runtime
			t.Cleanup(func() { runtime.close(); defaultJobRuntime = oldRuntime })
			entered, release := make(chan struct{}), make(chan struct{})
			commitCalled := false
			runtime.registerWithBinding("review_cancel_before_commit", time.Minute,
				func(ctx context.Context, _ int64, _ bool, _ json.RawMessage) (DurableJobResult, error) {
					resultID, ok := currentJobResultID(ctx)
					if !ok {
						return DurableJobResult{}, errors.New("missing result id")
					}
					close(entered)
					<-release
					// 故意去掉本地取消信号，验证尚未观测到 ctx.Done 时，数据库的取消事实仍生效。
					guarded := context.WithoutCancel(ctx)
					if nested {
						guarded = withoutJobExecution(guarded)
					}
					err := withJobResultTransaction(guarded, func(tx *gorm.DB) error {
						commitCalled = true
						return tx.Model(&model.AnalysisRecord{}).Where("id = ?", resultID).
							Updates(map[string]any{"status": model.AnalysisStatusSuccess, "summary": "不应写入"}).Error
					})
					return DurableJobResult{Status: model.JobStatusSuccess}, err
				}, testAnalysisBusinessBinding(), false)
			run, err := runtime.startWithBinding(userID, "review_cancel_before_commit", map[string]int{"version": 1}, false, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("测试工作器未开始")
			}
			_, cancelErr := CancelJobRun(userID, run.ID)
			close(release)
			if cancelErr != nil {
				t.Fatal(cancelErr)
			}
			done := waitJobRun(t, userID, run.ID)
			if done.Status != model.JobStatusCanceled || commitCalled {
				t.Fatalf("取消先提交不得执行业务写入：status=%s commitCalled=%v", done.Status, commitCalled)
			}
			var rec model.AnalysisRecord
			if err := common.DB.First(&rec, *run.ResultID).Error; err != nil || rec.Status != model.AnalysisStatusFailed {
				t.Fatalf("取消后业务结果应保持失败：%+v err=%v", rec, err)
			}
		})
	}
}
