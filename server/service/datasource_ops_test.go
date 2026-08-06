package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

type opsProbeAdapter struct {
	quote func(context.Context) (*datasource.Quote, error)
}

func (a *opsProbeAdapter) Name() string { return "eastmoney" }
func (a *opsProbeAdapter) GetQuote(ctx context.Context, _, _ string) (*datasource.Quote, error) {
	return a.quote(ctx)
}
func (a *opsProbeAdapter) GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error) {
	return nil, datasource.ErrNotSupported
}

func TestDataSourceProbeWritesSafeAudit(t *testing.T) {
	setupTestDB(t)
	datasource.ResetProbeLimiterForTest()
	common.DB.Where("task IN ?", []string{dataSourceProbeTask, dataSourceUncoolTask}).Delete(&model.DataSyncLog{})
	mgr := datasource.NewManagerWithAdapters(&opsProbeAdapter{quote: func(context.Context) (*datasource.Quote, error) {
		return &datasource.Quote{Symbol: "600000"}, nil
	}})
	svc := NewMarketService(mgr)
	op, err := svc.ProbeDataSource(context.Background(), 42, DataSourceProbeRequest{Provider: "eastmoney", Capability: "quote", Market: "cn"})
	if err != nil || op.Result.Outcome != datasource.ProbeSuccess || op.AuditID == 0 {
		t.Fatalf("probe operation mismatch: %+v err=%v", op, err)
	}
	var log model.DataSyncLog
	if err := common.DB.First(&log, op.AuditID).Error; err != nil {
		t.Fatal(err)
	}
	if log.Task != dataSourceProbeTask || log.Status != "success" || log.UserID != 42 || log.Succeeded != 1 {
		t.Fatalf("audit fields mismatch: %+v", log)
	}
	for _, forbidden := range []string{"600000", "response", "authorization", "cookie"} {
		if strings.Contains(strings.ToLower(log.Message+log.ParameterSummary), forbidden) {
			t.Fatalf("audit leaked forbidden text %q: %+v", forbidden, log)
		}
	}

	datasource.ResetProbeLimiterForTest()
	emptySvc := NewMarketService(datasource.NewManagerWithAdapters(&opsProbeAdapter{quote: func(context.Context) (*datasource.Quote, error) {
		return nil, datasource.ErrNoData
	}}))
	empty, err := emptySvc.ProbeDataSource(context.Background(), 42, DataSourceProbeRequest{Provider: "eastmoney", Capability: "quote", Market: "cn"})
	if err != nil || empty.Result.Outcome != datasource.ProbeEmpty {
		t.Fatalf("empty probe mismatch: %+v err=%v", empty, err)
	}
	var emptyLog model.DataSyncLog
	if err := common.DB.First(&emptyLog, empty.AuditID).Error; err != nil || emptyLog.Status != "partial" {
		t.Fatalf("empty audit mismatch: %+v err=%v", emptyLog, err)
	}

	datasource.ResetProbeLimiterForTest()
	errorSvc := NewMarketService(datasource.NewManagerWithAdapters(&opsProbeAdapter{quote: func(context.Context) (*datasource.Quote, error) {
		return nil, errors.New("raw upstream response")
	}}))
	failed, err := errorSvc.ProbeDataSource(context.Background(), 42, DataSourceProbeRequest{Provider: "eastmoney", Capability: "quote", Market: "cn"})
	if err == nil || failed.Result.Code != "UPSTREAM_ERROR" || failed.AuditID == 0 {
		t.Fatalf("error probe mismatch: %+v err=%v", failed, err)
	}
	var failedLog model.DataSyncLog
	if err := common.DB.First(&failedLog, failed.AuditID).Error; err != nil || failedLog.Status != "failed" || failedLog.Message != "probe failed" {
		t.Fatalf("failed audit must be normalized: %+v err=%v", failedLog, err)
	}
	if strings.Contains(failedLog.ParameterSummary+failedLog.Message, "raw upstream response") {
		t.Fatalf("failed audit leaked upstream response: %+v", failedLog)
	}

	var before int64
	common.DB.Model(&model.DataSyncLog{}).Where("task = ?", dataSourceProbeTask).Count(&before)
	_, err = svc.ProbeDataSource(context.Background(), 42, DataSourceProbeRequest{
		Provider: "authorization-bearer-secret", Capability: "quote", Market: "cn",
	})
	if !errors.Is(err, datasource.ErrProbeNotAllowed) {
		t.Fatalf("invalid tuple must be rejected before probe/audit: %v", err)
	}
	var after int64
	common.DB.Model(&model.DataSyncLog{}).Where("task = ?", dataSourceProbeTask).Count(&after)
	if after != before {
		t.Fatalf("unregistered raw tuple must not enter whitelist audit: before=%d after=%d", before, after)
	}
}

func TestDataSourceUncoolReasonValidation(t *testing.T) {
	if _, err := validateDataSourceReason(""); err == nil {
		t.Fatal("empty reason must be rejected")
	}
	if _, err := validateDataSourceReason("authorization: Bearer token"); err == nil {
		t.Fatal("credential-like reason must be rejected")
	}
	if _, err := validateDataSourceReason("上游 token 已轮换"); err == nil {
		t.Fatal("generic token field must be rejected")
	}
	if _, err := validateDataSourceReason(strings.Repeat("x", dataSourceReasonMax+1)); err == nil {
		t.Fatal("overlong reason must be rejected")
	}
	if reason, err := validateDataSourceReason(strings.Repeat("恢", dataSourceReasonMax)); err != nil || len(reason) == 0 {
		t.Fatalf("max-length UTF-8 reason must remain auditable: len=%d err=%v", len([]rune(reason)), err)
	}
	reason, err := validateDataSourceReason("上游限流已恢复")
	if err != nil || reason == "" {
		t.Fatalf("valid reason rejected: %q %v", reason, err)
	}
}

func TestDataSourceUncoolWritesReasonAndResultAudit(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("task = ?", dataSourceUncoolTask).Delete(&model.DataSyncLog{})
	mgr := datasource.NewManagerWithAdapters(&opsProbeAdapter{quote: func(context.Context) (*datasource.Quote, error) {
		return &datasource.Quote{Symbol: "600000"}, nil
	}})
	svc := NewMarketService(mgr)
	op, err := svc.UncoolDataSource(context.Background(), 77, DataSourceUncoolRequest{
		Provider: "EASTMONEY", Capability: "QUOTE", Market: "CN", Reason: "上游限流已恢复",
	})
	if err != nil || op.AuditID == 0 || op.Cleared || op.CooldownBeforeSec != 0 {
		t.Fatalf("uncool operation mismatch: %+v err=%v", op, err)
	}
	var log model.DataSyncLog
	if err := common.DB.First(&log, op.AuditID).Error; err != nil {
		t.Fatal(err)
	}
	if log.Task != dataSourceUncoolTask || log.UserID != 77 || log.Status != "success" || log.Succeeded != 1 ||
		!strings.Contains(log.ParameterSummary, "上游限流已恢复") ||
		!strings.Contains(log.ParameterSummary, `"cleared":false`) {
		t.Fatalf("uncool audit mismatch: %+v", log)
	}
}
