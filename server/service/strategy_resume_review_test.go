package service

import (
	"context"
	"errors"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestStrategyResearchCanceledSubmissionCreatesNoJob(t *testing.T) {
	setupTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, kind := range []string{JobKindScreenerScan, JobKindStrategyBacktest} {
		var err error
		if kind == JobKindScreenerScan {
			_, err = (&ScreenerService{}).StartScanJob(14131, ScanRequest{StrategyKey: "vol-break-20d"}, ctx)
		} else {
			_, err = (&BacktestService{}).StartBacktestJob(14131, BacktestRequest{StrategyKey: "vol-break-20d"}, ctx)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("已取消提交必须在规范化及创建前结束：kind=%s err=%v", kind, err)
		}
		if _, err := ListStrategyRuns(14131, kind, 10, ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("历史列表也必须传递取消：kind=%s err=%v", kind, err)
		}
	}
	for _, table := range []any{&model.JobRun{}, &model.StrategyRunResult{}} {
		var count int64
		if err := common.DB.Model(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("已取消提交留下了任务或结果占位：count=%d err=%v", count, err)
		}
	}
}
