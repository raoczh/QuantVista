package service

import (
	"context"
	"errors"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestDataSourceOperationsCanceledBeforeAudit(t *testing.T) {
	for _, operation := range []string{"probe", "uncool"} {
		t.Run(operation, func(t *testing.T) {
			setupTestDB(t)
			datasource.ResetProbeLimiterForTest()
			svc := NewMarketService(datasource.NewManagerWithAdapters(&opsProbeAdapter{quote: func(context.Context) (*datasource.Quote, error) {
				t.Error("已取消的运维请求不能继续访问上游")
				return nil, datasource.ErrNoData
			}}))
			const userID int64 = 8991
			var before, after int64
			if err := common.DB.Model(&model.DataSyncLog{}).Where("user_id = ?", userID).Count(&before).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var err error
			if operation == "probe" {
				_, err = svc.ProbeDataSource(ctx, userID, DataSourceProbeRequest{Provider: "eastmoney", Capability: "quote", Market: "cn"})
			} else {
				_, err = svc.UncoolDataSource(ctx, userID, DataSourceUncoolRequest{Provider: "eastmoney", Capability: "quote", Market: "cn", Reason: "本地测试"})
			}
			if queryErr := common.DB.Model(&model.DataSyncLog{}).Where("user_id = ?", userID).Count(&after).Error; queryErr != nil {
				t.Fatal(queryErr)
			}
			if !errors.Is(err, context.Canceled) || before != after {
				t.Fatalf("已取消的运维操作仍执行或写审计: before=%d after=%d err=%v", before, after, err)
			}
		})
	}
}
