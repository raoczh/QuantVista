package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestFactorCacheRevalidatesCalendarWithoutNewBars(t *testing.T) {
	setupTestDB(t)
	resetFactorTable()
	t.Cleanup(resetFactorTable)
	now := time.Now()
	oldDate := now.AddDate(0, 0, -7).Format("2006-01-02")
	for i := 7; i >= 0; i-- {
		day := now.AddDate(0, 0, -i)
		if err := common.DB.Create(&model.TradingCalendar{
			Market: "cn", TradeDate: day.Format("2006-01-02"),
			IsOpen: day.Weekday() >= time.Monday && day.Weekday() <= time.Friday,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := common.DB.Create(&model.MarketSyncState{Symbol: "600001", Market: "cn", LastBarDate: oldDate}).Error; err != nil {
		t.Fatal(err)
	}
	old := miniTable()
	old.TradeDate, old.ExpectedDate, old.LagOpenDays = oldDate, oldDate, 0
	old.BuiltAt = now.AddDate(0, 0, -7)
	old.LastDates = []string{oldDate, oldDate}
	factorTableMu.Lock()
	factorTableCur = old
	factorTableMu.Unlock()

	current, err := ensureFactorTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current.ExpectedDate <= oldDate || current.LagOpenDays <= 0 {
		t.Fatalf("无新日线也必须重算日历时效：数据=%s，应有=%s，滞后=%d", current.TradeDate, current.ExpectedDate, current.LagOpenDays)
	}
	if old.ExpectedDate != oldDate || old.LagOpenDays != 0 {
		t.Fatal("时效更新不得修改其他请求正在读取的共享表")
	}
	if current.BuiltAt != old.BuiltAt || &current.Col("close")[0] != &old.Col("close")[0] {
		t.Fatal("只有日历推进时应复用因子列，避免重建全市场数据")
	}
	result, err := NewScreenerService().Scan(context.Background(), 1, ScanRequest{Tree: &CondNode{Factor: "close", Op: ">", Value: fptr(0)}})
	if err != nil {
		t.Fatal(err)
	}
	if result.StaleNote == "" || result.LagOpenDays <= 0 {
		t.Fatalf("选股响应必须说明旧数据：%+v", result)
	}
}
