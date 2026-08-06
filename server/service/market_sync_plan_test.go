package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type planAdapter struct {
	bars map[string][]datasource.Bar
}

func (p *planAdapter) Name() string { return "eastmoney" }
func (p *planAdapter) GetQuote(context.Context, string, string) (*datasource.Quote, error) {
	return nil, datasource.ErrNotSupported
}
func (p *planAdapter) GetDailyBars(_ context.Context, _, symbol string, _ int) ([]datasource.Bar, error) {
	rows := p.bars[symbol]
	if len(rows) == 0 {
		return nil, datasource.ErrNoData
	}
	return rows, nil
}

func setupPlanDB(t *testing.T) {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Stock{}, &model.TradingCalendar{}, &model.DailyBar{}, &model.StockUniverseDaily{}, &model.MarketSyncState{}, &model.DataSyncLog{}); err != nil {
		t.Fatal(err)
	}
	common.DB = db
}

func seedPlanFacts(t *testing.T) []string {
	t.Helper()
	dates := []string{"2026-08-03", "2026-08-04", "2026-08-05"}
	cal := []model.TradingCalendar{
		{Market: "cn", TradeDate: dates[0], IsOpen: true},
		{Market: "cn", TradeDate: dates[1], IsOpen: true},
		{Market: "cn", TradeDate: dates[2], IsOpen: true},
		{Market: "cn", TradeDate: "2026-08-02", IsOpen: false},
	}
	common.DB.Create(&cal)
	common.DB.Create(&[]model.Stock{{Symbol: "600001", Market: "cn", Name: "甲"}, {Symbol: "600002", Market: "cn", Name: "乙"}})
	for _, date := range dates {
		common.DB.Create(&[]model.StockUniverseDaily{
			{TradeDate: date, Symbol: "600001", Market: "cn", Name: "甲"},
			{TradeDate: date, Symbol: "600002", Market: "cn", Name: "乙", Suspended: date == dates[1]},
		})
	}
	common.DB.Create(&[]model.DailyBar{
		{Symbol: "600001", Market: "cn", TradeDate: dates[0], Close: 10},
		{Symbol: "600002", Market: "cn", TradeDate: dates[0], Close: 20},
		{Symbol: "600001", Market: "cn", TradeDate: dates[1], Close: 11},
	})
	return dates
}

func TestSyncBarsPlanExcludesClosedAndSuspendedThenExpires(t *testing.T) {
	setupPlanDB(t)
	dates := seedPlanFacts(t)
	adapter := &planAdapter{bars: map[string][]datasource.Bar{
		"600001": {{TradeDate: dates[2], Open: 12, High: 12, Low: 12, Close: 12, Volume: 100, Source: "eastmoney"}},
		"600002": {{TradeDate: dates[2], Open: 22, High: 22, Low: 22, Close: 22, Volume: 100, Source: "eastmoney"}},
	}}
	svc := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	req := MaintenanceRequest{Market: "cn", From: "2026-08-02", To: dates[2], DryRun: true}
	plan, err := svc.PlanMaintenance(MaintenanceSyncBars, req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.WindowDays != 3 || plan.ExpectedCount != 5 || plan.ExistingCount != 3 || plan.MissingCount != 2 || plan.SuspendedCount != 1 {
		t.Fatalf("计划分母/差异错误: %+v", plan)
	}
	if len(plan.PlanHash) != 64 {
		t.Fatalf("plan hash 格式错误: %q", plan.PlanHash)
	}
	// dry-run 后本地事实变化，旧 hash 必须失效。
	common.DB.Create(&model.DailyBar{Symbol: "600001", Market: "cn", TradeDate: dates[2], Close: 12})
	req.DryRun = false
	req.PlanHash = plan.PlanHash
	if err := svc.ValidateMaintenancePlan(MaintenanceSyncBars, req); !errors.Is(err, ErrMaintenancePlanExpired) {
		t.Fatalf("旧计划应失效，got %v", err)
	}

	newPlan, err := svc.PlanMaintenance(MaintenanceSyncBars, req)
	if err != nil {
		t.Fatal(err)
	}
	req.PlanHash = newPlan.PlanHash
	audit := SyncAudit{TriggerSource: "admin", UserID: 42}
	log, err := svc.RunSyncBarsPlan(context.Background(), req, audit)
	if err != nil {
		t.Fatal(err)
	}
	if log.UserID != 42 || log.TriggerSource != "admin" || log.PlanHash != req.PlanHash || log.RangeSummary != req.From+".."+req.To {
		t.Fatalf("管理员审计字段错误: %+v", log)
	}
	if strings.Contains(log.ParameterSummary, "secret") || len(log.ParameterSummary) > 512 {
		t.Fatalf("参数摘要越界: %q", log.ParameterSummary)
	}
	var persisted model.DataSyncLog
	if err := common.DB.Order("id DESC").First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.UserID != 42 || persisted.PlanHash == "" {
		t.Fatalf("审计未落库: %+v", persisted)
	}
}

func TestMaintenanceRangeHardLimitsAndCalendarPlan(t *testing.T) {
	setupPlanDB(t)
	seedPlanFacts(t)
	svc := NewMarketService(datasource.NewManagerWithAdapters(&planAdapter{}))
	tooWide := MaintenanceRequest{Market: "cn", From: "2026-05-01", To: "2026-08-05", DryRun: true}
	if _, err := svc.PlanMaintenance(MaintenanceSyncBars, tooWide); err == nil {
		t.Fatal("超过 92 个自然日必须拒绝")
	}
	calReq := MaintenanceRequest{Market: "cn", From: "2026-08-01", To: "2026-08-05", DryRun: true}
	plan, err := svc.PlanMaintenance(MaintenanceBackfillCalendar, calReq)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TargetCount != 5 || plan.ExpectedCount != 5 || plan.ExistingCount != 4 || plan.MissingCount != 1 || plan.EstimatedRequests != 1 {
		t.Fatalf("日历计划差异错误: %+v", plan)
	}
	if len(plan.SampleTargets) != 5 || plan.SampleTargets[0] != "2026-08-01" || plan.SampleTargets[4] != "2026-08-05" {
		t.Fatalf("日历计划样本必须展示实际订正范围，而非只展示缺失行: %+v", plan.SampleTargets)
	}
	if err := svc.ValidateMaintenancePlan(MaintenanceBackfillCalendar, MaintenanceRequest{Market: "cn", From: calReq.From, To: calReq.To}); err == nil {
		t.Fatal("有 body 的执行没有 plan_hash 必须拒绝")
	}
	legacy := AdminSyncAudit(7, MaintenanceRequest{Market: "cn"}, true)
	if legacy.TriggerSource != "admin_legacy" || legacy.UserID != 7 {
		t.Fatalf("旧请求审计来源错误: %+v", legacy)
	}
	log := &model.DataSyncLog{Task: MaintenanceSyncBars, Market: "cn", Status: "success"}
	if err := common.DB.Create(log).Error; err != nil {
		t.Fatal(err)
	}
	if log.TriggerSource != "scheduler" {
		t.Fatalf("系统同步日志缺省触发来源应为 scheduler，got %q", log.TriggerSource)
	}
}

func TestWidePlanIgnoresRowsOutsideCurrentUniverse(t *testing.T) {
	setupPlanDB(t)
	expected := wideExpectedDate(time.Now())
	if err := common.DB.Create(&model.MarketSyncState{Symbol: "600001", Market: "cn", Name: "计划内"}).Error; err != nil {
		t.Fatal(err)
	}
	common.DB.Create(&model.DailyBar{Symbol: "600099", Market: "cn", TradeDate: expected, Close: 9})
	common.DB.Create(&model.StockUniverseDaily{Symbol: "600099", Market: "cn", TradeDate: expected, Suspended: true})
	svc := NewMarketService(datasource.NewManagerWithAdapters(&planAdapter{}))
	plan, err := svc.PlanMaintenance(MaintenanceWideSync, MaintenanceRequest{Market: "cn", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TargetCount != 1 || plan.ExpectedCount != 1 || plan.ExistingCount != 0 || plan.SuspendedCount != 0 || plan.MissingCount != 1 {
		t.Fatalf("宇宙外残留行不得污染计划数量: %+v", plan)
	}
}
