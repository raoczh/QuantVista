package service

import (
	"encoding/json"
	"fmt"
	"testing"

	"quantvista/model"
)

func TestMySQLDailyReportReviewLargePortfolioSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.DailyReport{})
	snapshot := reportSnapshot{TradeDate: "2026-09-09"}
	for i := 0; i < 300; i++ {
		snapshot.Positions = append(snapshot.Positions, reportPosition{
			Symbol: fmt.Sprintf("%06d", 600000+i), Name: "组合持仓", Type: "long_term", QuoteStale: true,
			QuoteAsOf: "2026-09-08 15:00", QuoteNote: "无当前有效行情（获取失败或已过期）：当日涨跌与累计盈亏未知，不得计入今日汇总或给出操作结论",
		})
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 65535 {
		t.Fatal("回归快照必须覆盖 MySQL TEXT 字节边界")
	}
	report := model.DailyReport{UserID: 19116, TradeDate: snapshot.TradeDate, Market: "cn",
		Status: model.ReportStatusSuccess, SnapshotJSON: string(raw), ReviewJSON: `{"summary":"持仓行情暂不可用"}`}
	if err := db.Create(&report).Error; err != nil {
		t.Fatalf("完整组合快照必须能够保存，bytes=%d err=%v", len(raw), err)
	}
	var stored model.DailyReport
	if err := db.First(&stored, report.ID).Error; err != nil || stored.SnapshotJSON != string(raw) {
		t.Fatalf("保存后必须完整保留复盘依据：err=%v bytes=%d", err, len(stored.SnapshotJSON))
	}
}
