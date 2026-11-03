package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestOrgRefreshFailureKeepsHistoricalScopeThroughCooldown(test *testing.T) {
	setupTestDB(test)
	cleanOrgView(test)
	test.Cleanup(func() { cleanOrgView(test) })
	historical := time.Now().Add(-10 * 24 * time.Hour)
	date := historical.Format(time.DateOnly)
	report := model.ReportRating{Market: "cn", Symbol: "600911", InfoCode: "refresh-history", ReportDate: date,
		Rating: "buy", TargetPrice: 31.5, UpdatedAt: historical}
	survey := model.OrgSurvey{Market: "cn", Symbol: report.Symbol, SurveyDate: date, OrgCount: 3, UpdatedAt: historical}
	if err := common.DB.Create(&report).Error; err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Create(&survey).Error; err != nil {
		test.Fatal(err)
	}
	upstreamErr := errors.New("Get https://proxy-user:private-proxy-token@proxy.invalid: fixture upstream unavailable")
	reportCalls, surveyCalls := 0, 0
	service := &OrgViewService{
		fetchReports: func(context.Context, string, int) ([]datasource.ReportRow, error) {
			reportCalls++
			return nil, upstreamErr
		},
		fetchSurveys: func(context.Context, string, int) ([]datasource.SurveyRow, error) {
			surveyCalls++
			return nil, upstreamErr
		},
	}
	if err := service.refreshReports(test.Context(), report.Symbol); !errors.Is(err, upstreamErr) {
		test.Fatalf("ordinary report error was replaced with nil ctx.Err(): %v", err)
	}
	if err := service.refreshSurveys(test.Context(), report.Symbol); !errors.Is(err, upstreamErr) {
		test.Fatalf("ordinary survey error was replaced with nil ctx.Err(): %v", err)
	}
	view, err := service.OverviewContext(test.Context(), "cn", report.Symbol, 30)
	if err != nil || len(view["reports"].([]model.ReportRating)) != 1 || len(view["surveys"].([]model.OrgSurvey)) != 1 {
		test.Fatalf("network failure hid readable history: %+v %v", view, err)
	}
	if view["refresh_failed"] != true || len(view["refresh_errors"].(map[string]string)) != 2 {
		test.Fatalf("history was presented as a successful refresh: %+v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		test.Fatal(err)
	}
	for _, forbidden := range []string{"private-proxy-token", "proxy-user", "proxy.invalid", "fixture upstream unavailable"} {
		if strings.Contains(string(encoded), forbidden) {
			test.Fatalf("public institutional data or AI summary leaked raw refresh error: %s", forbidden)
		}
	}
	summary := service.brief(test.Context(), report.Symbol, 30)
	if summary["refresh_failed"] != true || summary["reports_as_of"] != date || summary["surveys_as_of"] != date ||
		!strings.Contains(summary["refresh_note"].(string), "历史库存") {
		test.Fatalf("AI summary lost historical scope during cooldown: %+v", summary)
	}
	if reportCalls != 1 || surveyCalls != 1 {
		test.Fatalf("failure visibility must not bypass cooldown: reports=%d surveys=%d", reportCalls, surveyCalls)
	}
	var after model.ReportRating
	if err := common.DB.First(&after, report.ID).Error; err != nil || !after.UpdatedAt.Equal(historical) {
		test.Fatalf("failed refresh advanced inventory timestamp: %+v %v", after, err)
	}
}

func TestOrgRefreshFailureWithoutHistoryStillDeclaresUnknown(test *testing.T) {
	setupTestDB(test)
	cleanOrgView(test)
	test.Cleanup(func() { cleanOrgView(test) })
	service := &OrgViewService{
		fetchReports: func(context.Context, string, int) ([]datasource.ReportRow, error) {
			return nil, errors.New("report fixture unavailable")
		},
		fetchSurveys: func(context.Context, string, int) ([]datasource.SurveyRow, error) {
			return nil, errors.New("survey fixture unavailable")
		},
	}
	summary := service.brief(test.Context(), "600912", 30)
	if summary == nil || summary["refresh_failed"] != true || summary["refresh_note"] == nil {
		test.Fatalf("missing inventory became 'no institutional activity': %+v", summary)
	}
	if summary["target_price"] != nil || summary["survey"] != nil || summary["rating_dist_90d"] != nil {
		test.Fatalf("failure-only summary fabricated numerical facts: %+v", summary)
	}
}

func TestOrgRefreshCancellationStopsSecondSource(test *testing.T) {
	setupTestDB(test)
	cleanOrgView(test)
	test.Cleanup(func() { cleanOrgView(test) })
	ctx, cancel := context.WithCancel(test.Context())
	defer cancel()
	surveyCalls := 0
	service := &OrgViewService{
		fetchReports: func(context.Context, string, int) ([]datasource.ReportRow, error) {
			cancel()
			return nil, context.Canceled
		},
		fetchSurveys: func(context.Context, string, int) ([]datasource.SurveyRow, error) {
			surveyCalls++
			return nil, nil
		},
	}
	if _, err := service.OverviewContext(ctx, "cn", "600913", 30); !errors.Is(err, context.Canceled) || surveyCalls != 0 {
		test.Fatalf("caller cancellation was downgraded to historical fallback: err=%v calls=%d", err, surveyCalls)
	}
}
