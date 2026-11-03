package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/datasource"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type controllerProbeAdapter struct {
	quoteCalls int
	quoteError error
}

func (adapter *controllerProbeAdapter) Name() string { return "eastmoney" }

func (adapter *controllerProbeAdapter) GetQuote(context.Context, string, string) (*datasource.Quote, error) {
	adapter.quoteCalls++
	if adapter.quoteError != nil {
		return nil, adapter.quoteError
	}
	return &datasource.Quote{Symbol: "600000"}, nil
}

func (adapter *controllerProbeAdapter) GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error) {
	return nil, datasource.ErrNotSupported
}

func TestDataSourceProbeControllerPreservesCompletedOutcomes(test *testing.T) {
	for _, scenario := range []struct {
		name       string
		probeError error
		outcome    datasource.ProbeOutcome
		code       string
		auditState string
	}{
		{name: "success", outcome: datasource.ProbeSuccess, code: "OK", auditState: "success"},
		{name: "empty", probeError: datasource.ErrNoData, outcome: datasource.ProbeEmpty, code: "EMPTY", auditState: "partial"},
		{name: "upstream_error", probeError: errors.New("upstream body: Bearer private-credential"), outcome: datasource.ProbeError, code: "UPSTREAM_ERROR", auditState: "failed"},
		{name: "timeout", probeError: context.DeadlineExceeded, outcome: datasource.ProbeError, code: "UPSTREAM_TIMEOUT", auditState: "failed"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			database := reviewControllerDB(test, &model.DataSyncLog{})
			datasource.ResetProbeLimiterForTest()
			test.Cleanup(datasource.ResetProbeLimiterForTest)
			adapter := &controllerProbeAdapter{quoteError: scenario.probeError}
			controller := &MarketController{svc: service.NewMarketService(datasource.NewManagerWithAdapters(adapter))}
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest("POST", "/api/admin/datasources/probe", strings.NewReader(`{"provider":"eastmoney","capability":"quote","market":"cn"}`))
			requestContext.Set("uid", int64(7))
			controller.ProbeDataSource(requestContext)

			var envelope struct {
				Success bool                             `json:"success"`
				Data    service.DataSourceProbeOperation `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				test.Fatal(err)
			}
			if !envelope.Success || envelope.Data.AuditID <= 0 || envelope.Data.Result.Outcome != scenario.outcome || envelope.Data.Result.Code != scenario.code {
				test.Fatalf("completed probe lost its safe result: %s", response.Body.String())
			}
			if adapter.quoteCalls != 1 || envelope.Data.Result.SampleCount != 1 {
				test.Fatalf("probe must perform exactly one sample: calls=%d result=%+v", adapter.quoteCalls, envelope.Data.Result)
			}
			var audit model.DataSyncLog
			if err := database.First(&audit, envelope.Data.AuditID).Error; err != nil {
				test.Fatal(err)
			}
			if audit.Status != scenario.auditState || audit.UserID != 7 || !strings.Contains(audit.ParameterSummary, scenario.code) {
				test.Fatalf("probe receipt does not identify the completed audit: %+v", audit)
			}
			for _, forbidden := range []string{"upstream body", "Bearer", "private-credential"} {
				if strings.Contains(response.Body.String()+audit.Message+audit.ParameterSummary, forbidden) {
					test.Fatalf("probe response or audit exposed %q", forbidden)
				}
			}
		})
	}
}

func TestDataSourceProbeControllerRejectsAdmissionAndAuditFailures(test *testing.T) {
	for _, scenario := range []struct {
		name        string
		body        string
		auditPhase  string
		rateLimited bool
		wantCalls   int
	}{
		{name: "invalid_json", body: `{"provider":"eastmoney","unexpected":true}`},
		{name: "unregistered_tuple", body: `{"provider":"private-credential","capability":"quote","market":"cn"}`},
		{name: "disabled_probe", body: `{"provider":"eastmoney","capability":"spot_snapshot","market":"cn"}`},
		{name: "rate_limited", rateLimited: true, wantCalls: 1},
		{name: "audit_create_failure", auditPhase: "create"},
		{name: "audit_finish_failure_with_probe_failure", auditPhase: "update", wantCalls: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			database := reviewControllerDB(test, &model.DataSyncLog{})
			datasource.ResetProbeLimiterForTest()
			test.Cleanup(datasource.ResetProbeLimiterForTest)
			adapter := &controllerProbeAdapter{quoteError: errors.New("upstream body: Bearer private-credential")}
			marketService := service.NewMarketService(datasource.NewManagerWithAdapters(adapter))
			if scenario.rateLimited {
				if _, err := marketService.ProbeDataSource(test.Context(), 7, service.DataSourceProbeRequest{Provider: "eastmoney", Capability: "quote", Market: "cn"}); err == nil {
					test.Fatal("setup probe unexpectedly succeeded")
				}
			}
			const callbackName = "review_probe_audit_failure"
			failAudit := func(transaction *gorm.DB) {
				if transaction.Statement.Table == "data_sync_logs" {
					transaction.AddError(errors.New("synthetic audit storage failure"))
				}
			}
			if scenario.auditPhase == "create" {
				if err := database.Callback().Create().Before("gorm:create").Register(callbackName, failAudit); err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() { database.Callback().Create().Remove(callbackName) })
			} else if scenario.auditPhase == "update" {
				if err := database.Callback().Update().Before("gorm:update").Register(callbackName, failAudit); err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() { database.Callback().Update().Remove(callbackName) })
			}
			body := scenario.body
			if body == "" {
				body = `{"provider":"eastmoney","capability":"quote","market":"cn"}`
			}
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest("POST", "/api/admin/datasources/probe", strings.NewReader(body))
			requestContext.Set("uid", int64(7))
			(&MarketController{svc: marketService}).ProbeDataSource(requestContext)
			var envelope struct {
				Success bool            `json:"success"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				test.Fatal(err)
			}
			if envelope.Success || (len(envelope.Data) > 0 && string(envelope.Data) != "null") || adapter.quoteCalls != scenario.wantCalls {
				test.Fatalf("rejected operation was reported as completed: calls=%d response=%s", adapter.quoteCalls, response.Body.String())
			}
			for _, forbidden := range []string{"upstream body", "Bearer", "private-credential", "synthetic audit storage failure"} {
				if strings.Contains(response.Body.String(), forbidden) {
					test.Fatalf("rejected probe exposed %q: %s", forbidden, response.Body.String())
				}
			}
		})
	}
}
