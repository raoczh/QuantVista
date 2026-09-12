package service

import (
	"testing"

	"quantvista/model"
)

func TestMySQLFinanceReviewPublicationProof(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.FinanceIndicator{}, &model.FinanceStatement{}, &model.DisclosureSchedule{})
	for _, row := range []any{
		&model.FinanceIndicator{Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", NoticeDate: "2026-03-20", ROE: 12},
		&model.FinanceIndicator{Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", NoticeDate: "", ROE: 20},
		&model.FinanceIndicator{Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", NoticeDate: "2026-09-10", ROE: 99},
		&model.FinanceIndicator{Symbol: "600519", Market: "hk", ReportDate: "2026-03-31", NoticeDate: "2026-04-20", ROE: 88},
		&model.DisclosureSchedule{Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", ActualDate: "2026-04-20", IsPublished: true},
		&model.FinanceStatement{Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", TotalAssets: 100},
		&model.FinanceStatement{Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", TotalAssets: 999},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	rows, err := readFinanceIndicatorsAsOf(t.Context(), "600519", "2026-09-09", 8)
	if err != nil || len(rows) != 2 || rows[0].ROE != 20 {
		t.Fatalf("披露校验须在 MySQL 返回同市场且已发布的报告：rows=%v err=%v", rows, err)
	}
	statements, err := readFinanceStatementsForIndicators(t.Context(), "600519", rows)
	if err != nil || len(statements) != 1 || statements[0].TotalAssets != 100 {
		t.Fatalf("三表须对应已发布报告：rows=%v err=%v", statements, err)
	}
}
