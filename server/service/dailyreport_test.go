package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestInReportWindow 收盘日报生成窗口（15:35 ~ 20:00）。
func TestInReportWindow(t *testing.T) {
	cases := []struct {
		hhmm string
		want bool
	}{
		{"09:00", false},
		{"15:29", false},
		{"15:34", false},
		{"15:35", true},
		{"16:00", true},
		{"19:59", true},
		{"20:00", false},
		{"23:30", false},
	}
	for _, c := range cases {
		tm, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-03 "+c.hhmm, time.Local)
		if got := inReportWindow(tm); got != c.want {
			t.Errorf("inReportWindow(%s) = %v, 期望 %v", c.hhmm, got, c.want)
		}
	}
}

// TestIsTradingDayToday 优先交易日历，无记录时回退周一~五。
func TestIsTradingDayToday(t *testing.T) {
	setupTestDB(t)

	// 2026-07-03 是周五：无日历记录 → 回退工作日判定 = true。
	fri, _ := time.ParseInLocation("2006-01-02", "2026-07-03", time.Local)
	if !isTradingDayToday(fri) {
		t.Fatalf("无日历时周五应视为交易日")
	}
	// 2026-07-04 是周六 → false。
	sat := fri.AddDate(0, 0, 1)
	if isTradingDayToday(sat) {
		t.Fatalf("无日历时周六不应视为交易日")
	}
	// 日历声明周五休市（如节假日调休）→ 以日历为准。
	common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-03", IsOpen: false})
	if isTradingDayToday(fri) {
		t.Fatalf("日历标记休市应优先于工作日回退")
	}
}

// TestDailyReport_ListGetIsolation 日报按用户隔离；列表排除大字段。
func TestDailyReport_ListGetIsolation(t *testing.T) {
	setupTestDB(t)
	svc := &DailyReportService{rec: &RecommendationService{}}

	common.DB.Create(&model.DailyReport{UserID: 1, TradeDate: "2026-07-02", Market: "cn",
		Status: model.ReportStatusSuccess, ReviewJSON: `{"summary":"ok"}`, SnapshotJSON: `{}`})
	common.DB.Create(&model.DailyReport{UserID: 2, TradeDate: "2026-07-02", Market: "cn",
		Status: model.ReportStatusSuccess})

	rows, err := svc.List(1, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("用户1 应只见自己的 1 条: %v %d", err, len(rows))
	}
	if rows[0].ReviewJSON != "" {
		t.Fatalf("列表应排除大字段 review_json")
	}

	v, err := svc.Get(1, rows[0].ID)
	if err != nil || v.Review == nil || v.Review.Summary != "ok" {
		t.Fatalf("详情应含解析后的复盘: %v %+v", err, v)
	}
	if _, err := svc.Get(2, rows[0].ID); err == nil {
		t.Fatalf("跨用户 Get 应失败（隔离）")
	}

	// Latest：用户1 有、用户3 无（nil 不报错）。
	lv, err := svc.Latest(1)
	if err != nil || lv == nil {
		t.Fatalf("Latest 应返回用户1 的报告: %v", err)
	}
	lv3, err := svc.Latest(3)
	if err != nil || lv3 != nil {
		t.Fatalf("无日报用户 Latest 应返回 nil, nil: %v %+v", err, lv3)
	}
}

// TestChargeAction 手动动作计次辅助：只加 action_used。
func TestChargeAction(t *testing.T) {
	setupTestDB(t)
	if _, err := getUserQuota(9); err != nil {
		t.Fatalf("建配额行失败: %v", err)
	}
	chargeAction(9)
	q, _ := getUserQuota(9)
	if q.ActionUsed != 1 || q.TokenUsed != 0 || q.RequestCount != 0 {
		t.Fatalf("chargeAction 只应加次数: %+v", q)
	}
}
