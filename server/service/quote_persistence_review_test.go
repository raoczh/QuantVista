package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func verifyQuotePersistenceDoesNotMoveBackward(t *testing.T) {
	t.Helper()
	svc := &MarketService{}
	now := time.Date(2026, 9, 9, 14, 0, 0, 0, time.Local)
	fresh := datasource.Quote{Symbol: "600082", Market: "cn", Name: "行情样本", Price: 11, Open: 10,
		High: 11, Low: 10, PrevClose: 10, Volume: 1000, Amount: 1100000, ChangePct: 10, Source: "eastmoney", DataTime: now}
	svc.persist(t.Context(), &fresh)
	for _, staleTime := range []time.Time{now.Add(-time.Minute), {}} {
		stale := fresh
		stale.DataTime, stale.Price, stale.Source, stale.Volume = staleTime, 9, "sina", 500
		svc.persist(t.Context(), &stale)
		var row model.StockQuote
		if err := common.DB.Where("market = ? AND symbol = ?", "cn", fresh.Symbol).First(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Price != fresh.Price || row.Source != fresh.Source || row.Volume != fresh.Volume || !row.DataTime.Equal(now) {
			t.Errorf("迟到旧行情和无时间行情不能覆盖最近已知快照：incoming=%v row=%+v", staleTime, row)
		}
	}
	newer := fresh
	newer.Price, newer.DataTime = 12, now.Add(time.Minute)
	svc.persist(t.Context(), &newer)
	var row model.StockQuote
	if err := common.DB.Where("market = ? AND symbol = ?", "cn", fresh.Symbol).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Price != 12 || !row.DataTime.Equal(newer.DataTime) {
		t.Fatalf("真正更新的行情仍须落库：%+v", row)
	}
}

func TestQuotePersistenceReviewRejectsOlderSnapshot(t *testing.T) {
	setupTestDB(t)
	verifyQuotePersistenceDoesNotMoveBackward(t)
}

func TestMySQLQuotePersistenceReviewRejectsOlderSnapshot(t *testing.T) {
	setupMySQLReviewDB(t, &model.Stock{}, &model.StockQuote{})
	verifyQuotePersistenceDoesNotMoveBackward(t)
}

func TestOnlineDailyWriteReviewAdvancesExistingWatermark(t *testing.T) {
	setupTestDB(t)
	seedFactorBuildReview(t, common.DB, "pending")
	svc := &MarketService{}
	bars := []datasource.Bar{
		{TradeDate: "2026-09-08", Open: 10, High: 10, Low: 10, Close: 10, Source: "eastmoney"},
		{TradeDate: "2026-09-09", Open: 11, High: 11, Low: 11, Close: 11, Source: "eastmoney"},
	}
	if err := svc.persistDailyBars(context.Background(), "cn", "600081", bars); err != nil {
		t.Fatal(err)
	}
	var state model.MarketSyncState
	if err := common.DB.Where("market = ? AND symbol = ?", "cn", "600081").First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.LastBarDate != "2026-09-09" || state.BarsCount != 2 || state.InitStatus != "pending" {
		t.Fatalf("普通日线写入也须同步已有水位，不得把有限窗口冒充初始化完成：%+v", state)
	}
	table, err := buildFactorTable(t.Context())
	if err != nil || table.TradeDate != "2026-09-09" || !table.Fresh(0) {
		t.Fatalf("已落库的新日线应在宽表使用真实数据日期：table=%+v err=%v", table, err)
	}
}
