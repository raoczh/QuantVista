package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestResultDeleteWaitsForJobFinalization(t *testing.T) {
	for index, kind := range []string{JobKindAnalysis, JobKindDailyReport} {
		t.Run(kind, func(t *testing.T) {
			resetDurableJobs(t)
			userID := int64(18002 + index)
			runtime := newJobRuntime(1, 1)
			previous := defaultJobRuntime
			defaultJobRuntime = runtime
			t.Cleanup(func() { runtime.close(); defaultJobRuntime = previous })
			var resultID int64
			var remove func(int64, int64) error
			var resultType string
			if kind == JobKindAnalysis {
				svc := &AnalysisService{}
				svc.registerDurableJobHandler()
				rec := model.AnalysisRecord{UserID: userID, Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600000",
					Status: model.AnalysisStatusSuccess, ResultJSON: `{"rating":"neutral","summary":"业务已经提交"}`}
				if err := common.DB.Create(&rec).Error; err != nil {
					t.Fatal(err)
				}
				resultID, resultType, remove = rec.ID, JobResultAnalysis, svc.Delete
			} else {
				svc := &DailyReportService{}
				svc.registerDurableJobHandler()
				rec := model.DailyReport{UserID: userID, TradeDate: "2026-06-01", Market: "cn", Status: model.ReportStatusSuccess}
				if err := common.DB.Create(&rec).Error; err != nil {
					t.Fatal(err)
				}
				resultID, resultType, remove = rec.ID, JobResultDailyReport, svc.Delete
			}
			// withJobResultTransaction 提交后、运行时保存工件前，业务结果已成功而 JobRun 仍在运行。
			run := model.JobRun{UserID: userID, OwnerType: model.JobOwnerUser, Kind: kind, Status: model.JobStatusRunning,
				ResultType: resultType, ResultID: &resultID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			if err := common.DB.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
			if err := remove(userID, resultID); err == nil {
				t.Error("后台尚未完成工件和作业终态保存时不能删除业务结果")
			}
			handler, ok := runtime.handler(kind)
			if !ok {
				t.Fatal("必须使用生产业务绑定验证后续保存")
			}
			if err := runtime.persistSuccess(run, handler.binding, DurableJobResult{Status: model.JobStatusSuccess}, nil, 1); err != nil {
				failureErr := runtime.finishFailed(run, AsyncLLMTaskErrorFailed, "模拟运行时收尾失败")
				var state model.JobRun
				_ = common.DB.First(&state, run.ID).Error
				t.Fatalf("删除破坏生产收尾：persist=%v failure=%v remaining_status=%s", err, failureErr, state.Status)
			}
			if err := remove(userID, resultID); err != nil {
				t.Fatalf("作业终态确认后应可正常删除：%v", err)
			}
		})
	}
}
