package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestOrgViewCanceledReadDoesNotFetchOrWrite(t *testing.T) {
	setupTestDB(t)
	cleanOrgView(t)
	t.Cleanup(func() { cleanOrgView(t) })
	oldReports, oldSurveys := fetchOrgReports, fetchOrgSurveys
	t.Cleanup(func() { fetchOrgReports, fetchOrgSurveys = oldReports, oldSurveys })
	calls := 0
	fetchOrgReports = func(context.Context, string, int) ([]datasource.ReportRow, error) {
		calls++
		return []datasource.ReportRow{{InfoCode: "TEST", PublishDate: "2026-09-09", Rating: "买入"}}, nil
	}
	fetchOrgSurveys = func(context.Context, string, int) ([]datasource.SurveyRow, error) {
		calls++
		return []datasource.SurveyRow{{SurveyDate: "2026-09-09", OrgName: "测试机构"}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	NewOrgViewService().Overview(ctx, "cn", "600901", 10)
	if calls != 0 {
		t.Errorf("已取消请求不能触发上游：%d", calls)
	}
	for _, table := range []any{&model.ReportRating{}, &model.OrgSurvey{}} {
		var count int64
		if err := common.DB.Model(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("取消请求仍写入机构事实：%d %v", count, err)
		}
	}
}

func TestOrgRowsExcludeFutureAndOtherMarket(t *testing.T) {
	setupTestDB(t)
	cleanOrgView(t)
	t.Cleanup(func() { cleanOrgView(t) })
	today, future := time.Now().Format("2006-01-02"), time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	for i, data := range []struct{ market, date, notice string }{{"cn", today, today}, {"hk", today, today}, {"cn", future, future}, {"cn", "2026-09-01", future}} {
		if i < 3 {
			if err := common.DB.Create(&model.ReportRating{Symbol: "600901", Market: data.market, InfoCode: "TEST" + itoa(i), ReportDate: data.date, Rating: "买入"}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := common.DB.Create(&model.OrgSurvey{Symbol: "600901", Market: data.market, SurveyDate: data.date, NoticeDate: data.notice, OrgCount: 2}).Error; err != nil {
			t.Fatal(err)
		}
	}
	reports, surveys := loadOrgRows("600901", 0, 0)
	if len(reports) != 1 || len(surveys) != 1 {
		t.Fatalf("机构事实必须同市场且已发生、已公告：reports=%d surveys=%d", len(reports), len(surveys))
	}
}

func TestOrgSurveyAggregationKeepsPublicationBoundary(t *testing.T) {
	rows := aggregateSurveys("600901", []datasource.SurveyRow{
		{SurveyDate: "2026-09-09", NoticeDate: "2026-09-10", OrgName: "同一机构"},
		{SurveyDate: "2026-09-09", NoticeDate: "2026-09-11", OrgName: "同一机构"},
		{SurveyDate: "2026-09-09", NoticeDate: "2026-09-12", OrgName: "另一机构"},
	})
	if len(rows) != 1 || rows[0].OrgCount != 2 || rows[0].NoticeDate != "2026-09-12" {
		t.Fatalf("重复机构不能加家数，聚合不能早于任何组成公告：%+v", rows)
	}
}

func TestOrgSurveySummaryDoesNotTruncateComparisonWindow(t *testing.T) {
	setupTestDB(t)
	cleanOrgView(t)
	t.Cleanup(func() { cleanOrgView(t) })
	for i := 0; i < 150; i++ {
		if err := common.DB.Create(&model.OrgSurvey{Market: "cn", Symbol: "600901", SurveyDate: time.Now().AddDate(0, 0, -i).Format("2006-01-02"), OrgCount: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	reports, surveys := loadOrgRows("600901", 0, 0)
	view := computeOrgView(reports, surveys, 0, time.Now())
	if view == nil {
		t.Fatal("有调研应提供概览")
	}
	survey := view["survey"].(map[string]any)
	if survey["batches_prev_90d"] != 59 {
		t.Fatalf("环比前窗不能被前 100 行截断：%v", survey)
	}
}

func TestMySQLOrgViewUsesOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.ReportRating{}, &model.OrgSurvey{})
	day := time.Now().Format("2006-01-02")
	if err := db.Create(&model.ReportRating{Market: "cn", Symbol: "600901", InfoCode: "TEST", ReportDate: day, Rating: "买入"}).Error; err != nil {
		t.Fatal(err)
	}
	survey := model.OrgSurvey{Market: "cn", Symbol: "600901", SurveyDate: day, NoticeDate: day, OrgCount: 1}
	if err := db.Create(&survey).Error; err != nil {
		t.Fatal(err)
	}
	wrote := false
	const callback = "review_org_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "report_ratings" && !wrote && tx.Error == nil {
			wrote = true
			if err := db.Model(&survey).Update("org_count", 9).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	_, surveys := loadOrgRows("600901", 0, 0)
	if !wrote || len(surveys) != 1 || surveys[0].OrgCount != 1 {
		t.Fatalf("研报和调研必须保持同一个快照：wrote=%v rows=%+v", wrote, surveys)
	}
}
