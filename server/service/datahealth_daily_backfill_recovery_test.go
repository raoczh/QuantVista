package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestDataHealthDailyBackfillRecoveryUsesPersistedDateAndTaskTime(test *testing.T) {
	now, dates := setupDataHealthReview(test)
	olderDate, latestDate := dates[len(dates)-5], dates[len(dates)-1]
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: "600001", LastBarDate: olderDate}).Error; err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Create(&model.DailyBar{Market: "cn", Symbol: "600001", TradeDate: latestDate, Close: 10}).Error; err != nil {
		test.Fatal(err)
	}
	logs := []model.DataSyncLog{
		{Task: MaintenanceWideSync, Market: "cn", Status: "success", CreatedAt: now.Add(-2 * time.Hour)},
		{Task: MaintenanceSyncBars, Market: "cn", Status: "partial", Message: "one date still missing", CreatedAt: now.Add(-time.Hour)},
	}
	if err := common.DB.Create(&logs).Error; err != nil {
		test.Fatal(err)
	}
	report, err := buildDataHealthReportContext(test.Context(), now, DataHealthMinDays)
	if err != nil {
		test.Fatal(err)
	}
	item := healthItemFor(test, report, "marketwide")
	if item.ObservedDate != latestDate || item.LastRun == nil || item.LastRun.Task != MaintenanceSyncBars || !item.LastRun.CreatedAt.Equal(logs[1].CreatedAt) {
		test.Fatalf("data date and actual last maintenance task were conflated: %+v", item)
	}
	if item.RecentFailure == nil || item.RecentFailure.Task != MaintenanceSyncBars || item.RecentFailure.Message != logs[1].Message {
		test.Fatalf("daily repair failure was omitted: %+v", item.RecentFailure)
	}
	if err := common.DB.Where("market = ?", "cn").Delete(&model.DailyBar{}).Error; err != nil {
		test.Fatal(err)
	}
	report, err = buildDataHealthReportContext(test.Context(), now, DataHealthMinDays)
	if err != nil {
		test.Fatal(err)
	}
	if item := healthItemFor(test, report, "marketwide"); item.ObservedDate != "" || item.Status != "empty" {
		test.Fatalf("state-only date fabricated persisted coverage: %+v", item)
	}
}

func TestDataHealthDailyBackfillRecoveryNewerStateCannotHideOldBars(test *testing.T) {
	now, dates := setupDataHealthReview(test)
	olderDate, latestDate := dates[len(dates)-5], dates[len(dates)-1]
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: "600001", LastBarDate: latestDate}).Error; err != nil {
		test.Fatal(err)
	}
	if err := common.DB.Create(&model.DailyBar{Market: "cn", Symbol: "600001", TradeDate: olderDate, Close: 10}).Error; err != nil {
		test.Fatal(err)
	}
	report, err := buildDataHealthReportContext(test.Context(), now, DataHealthMinDays)
	if err != nil {
		test.Fatal(err)
	}
	item := healthItemFor(test, report, "marketwide")
	if item.ObservedDate != olderDate || item.Status != "behind" || item.LagOpenDays != 4 {
		test.Fatalf("ahead-of-data cursor hid missing persisted days: %+v", item)
	}
}
