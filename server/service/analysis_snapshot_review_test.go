package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

type analysisSnapshotReviewAdapter struct {
	refusalTestAdapter
	at      time.Time
	missing string
	price   float64
}

func (analysisSnapshotReviewAdapter) Name() string { return "analysis-snapshot-review" }
func (a analysisSnapshotReviewAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	if symbol == a.missing {
		return nil, datasource.ErrNoData
	}
	return &datasource.Quote{Symbol: symbol, Market: market, Name: "快照样本", Price: a.price,
		Open: a.price, High: a.price, Low: a.price, PrevClose: a.price, DataTime: a.at}, nil
}

func analysisSnapshotReviewEnv(t *testing.T, missing string, price float64) (*AnalysisService, *model.PortfolioAccount, string) {
	t.Helper()
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	quoteTime := now
	if now.Hour()*60+now.Minute() < sessionQuoteReadyMin {
		previous := now.AddDate(0, 0, -1)
		quoteTime = time.Date(previous.Year(), previous.Month(), previous.Day(), 15, 0, 0, 0, time.Local)
	}
	const userID int64 = 1190
	if err := common.DB.Create(&model.User{ID: userID, Username: "snapshot-review", Role: model.RoleAdmin, Status: model.StatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	account, err := EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.UserPreference{UserID: userID, TotalCapital: 100000}).Error; err != nil {
		t.Fatal(err)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(analysisSnapshotReviewAdapter{at: quoteTime, missing: missing, price: price}))
	return &AnalysisService{market: market, position: NewPositionService(market)}, account, now.AddDate(0, 0, -1).Format("2006-01-02")
}

func addAnalysisReviewPosition(t *testing.T, account *model.PortfolioAccount, date, symbol, status string) {
	t.Helper()
	p := model.Position{UserID: account.UserID, AccountID: account.ID, Symbol: symbol, Market: "cn", Currency: "CNY",
		Name: "快照样本", Status: status, BuyDate: date, BuyPrice: 10, Quantity: 100, TotalBuyQty: 100,
		TotalBuyCost: 1000, RemainingCost: 1000, PeakFrom: date, PeakDate: date, PeakPrice: 10}
	if status == model.PositionStatusClosed {
		p.Quantity, p.RemainingCost, p.TotalSellNet, p.SellPrice, p.SellDate = 0, 0, 1000, 10, date
	}
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
}

func TestAnalysisSnapshotPrioritizesOpenPositions(t *testing.T) {
	svc, account, date := analysisSnapshotReviewEnv(t, "", 10)
	for i := 0; i < 60; i++ {
		addAnalysisReviewPosition(t, account, date, "510300", model.PositionStatusClosed)
	}
	addAnalysisReviewPosition(t, account, date, "510500", model.PositionStatusHolding)
	ctx, err := svc.buildPositionContext(t.Context(), account.UserID)
	if err != nil {
		t.Fatal(err)
	}
	rows := ctx.Snapshot["positions"].([]map[string]any)
	found := false
	for _, row := range rows {
		if row["status"] == model.PositionStatusHolding {
			found = true
		}
	}
	if !found || ctx.Snapshot["holding_summary"].(map[string]any)["market_value"] != float64(1000) {
		t.Fatalf("历史平仓不能挤掉当前持仓或使汇总归零：found=%v summary=%v rows=%d", found, ctx.Snapshot["holding_summary"], len(rows))
	}
}

func TestAnalysisSnapshotAggregatesAllHoldingsBeforeDetailLimit(t *testing.T) {
	svc, account, date := analysisSnapshotReviewEnv(t, "", 10)
	for i := 0; i < 62; i++ {
		addAnalysisReviewPosition(t, account, date, "510300", model.PositionStatusHolding)
	}
	ctx, err := svc.buildPositionContext(t.Context(), account.UserID)
	if err != nil {
		t.Fatal(err)
	}
	summary := ctx.Snapshot["holding_summary"].(map[string]any)
	if summary["market_value"] != float64(62000) || summary["total_cost"] != float64(62000) {
		t.Fatalf("明细限额不能少算组合汇总：%v", summary)
	}
}

func TestAnalysisSnapshotPartialPricingDoesNotClaimCashRoom(t *testing.T) {
	svc, account, date := analysisSnapshotReviewEnv(t, "510500", 10)
	addAnalysisReviewPosition(t, account, date, "510300", model.PositionStatusHolding)
	addAnalysisReviewPosition(t, account, date, "510500", model.PositionStatusHolding)
	ctx, err := svc.buildPositionContext(t.Context(), account.UserID)
	if err != nil {
		t.Fatal(err)
	}
	capital := ctx.Snapshot["capital_context"].(map[string]any)
	if _, exists := capital["holding_ratio_pct"]; exists {
		t.Fatalf("只定价一半持仓，不能以部分市值宣称完整仓位和补仓余地：%v", capital)
	}
	if ctx.Snapshot["quote_failed_count"] != 1 {
		t.Fatalf("缺行情仍须如实保留：%v", ctx.Snapshot["quote_failed_count"])
	}
}

func TestAnalysisStockSnapshotKeepsETFQuotePrecision(t *testing.T) {
	svc, _, _ := analysisSnapshotReviewEnv(t, "", 4.037)
	_, snapshot, err := buildStockSnapshot(t.Context(), svc.market, "510300", "cn")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"price", "open", "high", "low", "prev_close"} {
		if snapshot["quote"].(map[string]any)[key] != 4.037 {
			t.Errorf("ETF %s 的三位有效价格不得改成 4.04：%v", key, snapshot["quote"].(map[string]any)[key])
		}
	}
}

func TestAnalysisPortfolioSnapshotsKeepPricePrecision(t *testing.T) {
	svc, account, date := analysisSnapshotReviewEnv(t, "", 4.037)
	addAnalysisReviewPosition(t, account, date, "510300", model.PositionStatusHolding)
	group := model.Watchlist{UserID: account.UserID, Name: "观察"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.WatchlistItem{UserID: account.UserID, WatchlistID: group.ID, Symbol: "510300", Market: "cn", Name: "快照样本"}).Error; err != nil {
		t.Fatal(err)
	}
	svc.watchlist = NewWatchlistService(svc.market)
	watch, err := svc.buildWatchlistContext(t.Context(), account.UserID, "cn")
	if err != nil {
		t.Fatal(err)
	}
	if got := watch.Snapshot["items"].([]map[string]any)[0]["price"]; got != 4.037 {
		t.Errorf("自选快照价格被舍入：%v", got)
	}
	positions, err := svc.buildPositionContext(t.Context(), account.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := positions.Snapshot["positions"].([]map[string]any)[0]["current_price"]; got != 4.037 {
		t.Errorf("持仓快照价格被舍入：%v", got)
	}
	staleMarket := NewMarketService(datasource.NewManagerWithAdapters(analysisSnapshotReviewAdapter{at: time.Now().AddDate(0, 0, -4), price: 4.037}))
	svc.position = NewPositionService(staleMarket)
	stale, err := svc.buildPositionContext(t.Context(), account.UserID)
	if err != nil {
		t.Fatal(err)
	}
	row := stale.Snapshot["positions"].([]map[string]any)[0]
	if got := row["last_known_price"]; got != 4.037 {
		t.Errorf("历史已知价格同样应保持精度：%v", got)
	}
	if _, exists := row["current_price"]; exists {
		t.Fatal("保留精度不能使旧价恢复为现价")
	}
}
