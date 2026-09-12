package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

type reviewDailyAdapter struct {
	name  string
	bars  []datasource.Bar
	calls map[string]int
}

func (a *reviewDailyAdapter) Name() string { return a.name }
func (a *reviewDailyAdapter) GetQuote(context.Context, string, string) (*datasource.Quote, error) {
	return nil, datasource.ErrNotSupported
}
func (a *reviewDailyAdapter) GetDailyBars(_ context.Context, _, symbol string, _ int) ([]datasource.Bar, error) {
	a.calls[symbol]++
	return append([]datasource.Bar(nil), a.bars...), nil
}

func TestUnadjustedDailyBarsCannotOverwriteSharedHistory(t *testing.T) {
	setupTestDB(t)
	date := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	original := model.DailyBar{Symbol: "600081", Market: "cn", TradeDate: date,
		Open: 10, High: 11, Low: 9, Close: 10, Volume: 100, Source: "eastmoney"}
	if err := common.DB.Create(&original).Error; err != nil {
		t.Fatal(err)
	}
	raw := datasource.Bar{TradeDate: date, Open: 20, High: 22, Low: 18, Close: 20, Volume: 100, Source: "sina"}
	svc := &MarketService{}
	if err := svc.persistDailyBars(context.Background(), "cn", original.Symbol, []datasource.Bar{raw}); err == nil {
		t.Error("不复权历史不得被当作成功补采写入前复权共用表")
	}
	var after model.DailyBar
	if err := common.DB.First(&after, original.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Close != original.Close || after.Source != original.Source {
		t.Fatalf("前复权历史被不复权回退覆盖：%+v", after)
	}
	if err := svc.persistDailyBars(context.Background(), "cn", "600082", []datasource.Bar{raw}); err == nil {
		t.Error("新标的也不能用不复权序列初始化共用历史")
	}
	if barCount(t, "600082") != 0 {
		t.Error("写入了无法保证复权口径的新标的")
	}
}

func TestTrackedSyncVisitsEachStockOnceAndChecksPersistence(t *testing.T) {
	setupTestDB(t)
	oldCursor := syncCursor.Load()
	syncCursor.Store(0)
	t.Cleanup(func() { syncCursor.Store(oldCursor) })
	stocks := []model.Stock{{Market: "cn", Symbol: "600083"}, {Market: "cn", Symbol: "600084"}}
	if err := common.DB.Create(&stocks).Error; err != nil {
		t.Fatal(err)
	}
	adapter := &reviewDailyAdapter{name: "eastmoney", calls: map[string]int{}, bars: []datasource.Bar{{
		TradeDate: time.Now().AddDate(0, 0, -2).Format("2006-01-02"), Open: 10, High: 10, Low: 10, Close: 10,
	}}}
	svc := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	const callback = "review:reject_daily_bar_write"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(db *gorm.DB) {
		if db.Statement.Schema != nil && db.Statement.Schema.Table == "daily_bars" {
			db.AddError(errors.New("模拟日线写入失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Create().Remove(callback) })
	log, err := svc.SyncTrackedDailyBars(context.Background(), "cn", 120)
	if err != nil {
		t.Fatal(err)
	}
	if log.Total != 2 || log.Succeeded != 0 || log.Failed != 2 || log.Status != "failed" {
		t.Fatalf("同步必须按不重复标的的实际落库结果统计：%+v", log)
	}
	for _, stock := range stocks {
		if adapter.calls[stock.Symbol] != 1 {
			t.Errorf("%s 本轮请求次数应为 1，实际 %d", stock.Symbol, adapter.calls[stock.Symbol])
		}
	}
}

func TestFailedRebaseCannotOverwriteOnlyRecentWindow(t *testing.T) {
	setupTestDB(t)
	const symbol = "600087"
	rows := []model.DailyBar{
		{Symbol: symbol, Market: "cn", TradeDate: "2026-07-01", Open: 20, High: 20, Low: 20, Close: 20, Source: "eastmoney"},
		{Symbol: symbol, Market: "cn", TradeDate: "2026-07-02", Open: 20, High: 20, Low: 20, Close: 20, Source: "eastmoney"},
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.MarketSyncState{Symbol: symbol, Market: "cn", InitStatus: "done", BarsCount: 2, LastBarDate: "2026-07-02"}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &MarketService{wide: &fakeWideSource{barsErr: map[string]error{symbol: errors.New("全量复权历史不可用")}}}
	fresh := []datasource.Bar{{TradeDate: "2026-07-02", Open: 10, High: 10, Low: 10, Close: 10, Source: "eastmoney"}}
	if err := svc.persistDailyBars(context.Background(), "cn", symbol, fresh); err == nil {
		t.Error("发现复权基准变化但全量重锚失败时，不能报告写入成功")
	}
	var after []model.DailyBar
	if err := common.DB.Where("symbol = ?", symbol).Order("trade_date").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].Close != 20 || after[1].Close != 20 {
		t.Fatalf("重锚失败不应留下 20→10 的半新半旧历史：%+v", after)
	}
}

func TestRebaseInProgressDoesNotReportAnotherWriteCompleted(t *testing.T) {
	setupTestDB(t)
	const symbol = "600088"
	if err := common.DB.Create(&model.DailyBar{Symbol: symbol, Market: "cn", TradeDate: "2026-07-01", Close: 20, Source: "eastmoney"}).Error; err != nil {
		t.Fatal(err)
	}
	fresh := []datasource.Bar{{TradeDate: "2026-07-01", Open: 10, High: 10, Low: 10, Close: 10, Source: "eastmoney"}}
	svc := &MarketService{}
	var overlappingErr error
	called := false
	svc.wide = &fakeWideSource{bars: map[string][]datasource.Bar{symbol: fresh}, onBars: func(string, int) {
		called = true
		overlappingErr = svc.persistDailyBars(context.Background(), "cn", symbol, fresh)
	}}
	if err := svc.persistDailyBars(context.Background(), "cn", symbol, fresh); err != nil {
		t.Fatal(err)
	}
	if !called || overlappingErr == nil {
		t.Fatalf("重锚仍在取数据时，另一个请求不能称为完成：called=%v err=%v", called, overlappingErr)
	}
}

func TestOnlineDailyBarsDoNotFeedUnadjustedSeriesToAnalysis(t *testing.T) {
	setupTestDB(t)
	adapter := &reviewDailyAdapter{name: "sina", calls: map[string]int{}, bars: []datasource.Bar{{TradeDate: "2026-07-01", Open: 20, High: 20, Low: 20, Close: 20}}}
	svc := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	if bars, err := svc.GetDailyBars(context.Background(), "cn", "600089", 250); err == nil || len(bars) != 0 {
		t.Fatalf("在线分析/评分也不能把不复权序列当作前复权数据：bars=%+v err=%v", bars, err)
	}
}
