package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

type maintenanceReviewAdapter struct {
	planAdapter
	days []string
}

func (a *maintenanceReviewAdapter) GetTradingDays(context.Context, string, int) ([]string, error) {
	return append([]string(nil), a.days...), nil
}

func TestMaintenanceReviewCalendarKeepsDaysBeforeSource(t *testing.T) {
	setupTestDB(t)
	row := model.TradingCalendar{Market: "cn", TradeDate: "2026-08-31", IsOpen: true}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewMarketService(datasource.NewManagerWithAdapters(&maintenanceReviewAdapter{days: []string{"2026-09-02"}}))
	req := MaintenanceRequest{Market: "cn", From: row.TradeDate, To: "2026-09-02"}
	plan, err := svc.PlanMaintenance(MaintenanceBackfillCalendar, req)
	if err != nil {
		t.Fatal(err)
	}
	req.PlanHash = plan.PlanHash
	log, err := svc.RunCalendarPlan(t.Context(), req, SyncAudit{})
	if err != nil {
		t.Fatal(err)
	}
	var after model.TradingCalendar
	if err := common.DB.First(&after, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !after.IsOpen || log.Status == "success" {
		t.Fatalf("源窗口之前的未知交易日被改成休市: open=%v log=%+v", after.IsOpen, log)
	}
}

func TestMaintenanceReviewCanceledCalendarWrite(t *testing.T) {
	for _, planned := range []bool{false, true} {
		name := "legacy"
		if planned {
			name = "confirmed_plan"
		}
		t.Run(name, func(t *testing.T) {
			setupTestDB(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			svc := NewMarketService(datasource.NewManagerWithAdapters(&maintenanceReviewAdapter{days: []string{"2026-09-01", "2026-09-02"}}))
			req := MaintenanceRequest{Market: "cn", From: "2026-09-01", To: "2026-09-02"}
			if planned {
				plan, err := svc.PlanMaintenance(MaintenanceBackfillCalendar, req)
				if err != nil {
					t.Fatal(err)
				}
				req.PlanHash = plan.PlanHash
			}
			const callback = "review_calendar_cancel_before_write"
			db := common.DB
			if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "trading_calendars" {
					cancel()
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Create().Remove(callback) })
			var err error
			if planned {
				_, err = svc.RunCalendarPlan(ctx, req, SyncAudit{})
			} else {
				_, err = svc.BackfillCalendar(ctx, "cn")
			}
			var count int64
			if err := db.Model(&model.TradingCalendar{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("请求取消后仍写入 %d 条日历: err=%v", count, err)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("返回错误未保留请求取消原因: %v", err)
			}
		})
	}
}

func TestMaintenanceReviewPartialBarsCannotClaimComplete(t *testing.T) {
	setupTestDB(t)
	for _, date := range []string{"2026-09-01", "2026-09-02"} {
		if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := common.DB.Create(&model.Stock{Market: "cn", Symbol: "600091"}).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewMarketService(datasource.NewManagerWithAdapters(&planAdapter{bars: map[string][]datasource.Bar{
		"600091": {{TradeDate: "2026-09-01", Open: 10, High: 10, Low: 10, Close: 10, Volume: 100, Source: "eastmoney"}},
	}}))
	req := MaintenanceRequest{Market: "cn", From: "2026-09-01", To: "2026-09-02"}
	plan, err := svc.PlanMaintenance(MaintenanceSyncBars, req)
	if err != nil {
		t.Fatal(err)
	}
	req.PlanHash = plan.PlanHash
	log, err := svc.RunSyncBarsPlan(t.Context(), req, SyncAudit{})
	if err != nil {
		t.Fatal(err)
	}
	if log.Status == "success" || log.Succeeded != 0 {
		t.Fatalf("计划缺两天只补一天仍被记为全部成功: %+v", log)
	}
}

func TestMarketWideReviewStatusPropagatesLogFailure(t *testing.T) {
	setupTestDB(t)
	const callback = "review_wide_status_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "data_sync_logs" {
			tx.AddError(errors.New("本地故障：同步日志查询失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	db := common.DB
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	if report, err := (&MarketService{}).MarketWideStatus(); report != nil || err == nil {
		t.Fatalf("全市场状态将日志故障吞为成功: report=%v err=%v", report, err)
	}
}

func TestMarketWideReviewRejectsFutureSnapshotDate(t *testing.T) {
	if date, err := spotTradeDate([]datasource.SpotRow{{DataTime: time.Now().AddDate(0, 0, 1).Unix()}}); err == nil {
		t.Fatalf("未来快照时间不应决定全市场日线与PIT日期: %s", date)
	}
}

func TestMaintenanceReviewWidePlanRequiresKnownCalendar(t *testing.T) {
	setupTestDB(t)
	if plan, err := (&MarketService{}).PlanMaintenance(MaintenanceWideSync, MaintenanceRequest{Market: "cn"}); plan != nil || err == nil {
		t.Fatalf("不能用星期估计替代补采应有日期: plan=%v err=%v", plan, err)
	}
}

func TestMarketWideReviewPublicationFailureIsVisible(t *testing.T) {
	for _, table := range []string{"trading_calendars", "stock_universe_dailies"} {
		t.Run(table, func(t *testing.T) {
			setupTestDB(t)
			snap, _ := wideTestSnapshot()
			const callback = "review_wide_publication_failure"
			if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("本地故障：发布行情辅助事实失败"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			db := common.DB
			t.Cleanup(func() { db.Callback().Create().Remove(callback) })
			log, err := (&MarketService{wide: &fakeWideSource{snapshot: snap}}).SyncMarketWide(t.Context())
			if err == nil || log == nil || log.Status != "failed" {
				t.Fatalf("日历/PIT 写入失败仍报告同步成功: log=%+v err=%v", log, err)
			}
		})
	}
}

func TestMySQLMaintenanceReviewPlanSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.Stock{}, &model.TradingCalendar{}, &model.DailyBar{}, &model.StockUniverseDaily{})
	if err := db.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-09-01", IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Stock{Market: "cn", Symbol: "600001"}).Error; err != nil {
		t.Fatal(err)
	}
	var changed atomic.Bool
	const callback = "review_maintenance_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_bars" && changed.CompareAndSwap(false, true) {
			if err := db.Create(&model.StockUniverseDaily{Market: "cn", Symbol: "600001", TradeDate: "2026-09-01", Suspended: true}).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	plan, err := (&MarketService{}).PlanMaintenance(MaintenanceSyncBars, MaintenanceRequest{Market: "cn", From: "2026-09-01", To: "2026-09-01"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Load() || plan.ExpectedCount != 1 || plan.MissingCount != 1 {
		t.Fatalf("补采计划混入稍后才提交的停牌数据: changed=%v plan=%+v", changed.Load(), plan)
	}
}
