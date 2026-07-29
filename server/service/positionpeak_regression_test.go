package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// TestPositionPeakHistoricalTradeRebuild 历史建仓、历史加仓与首笔买入修正都要
// 补齐新起算日后的本地日线，同时排除起算日整日 High。
func TestPositionPeakHistoricalTradeRebuild(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now().In(time.Local)
	date := func(days int) string { return now.AddDate(0, 0, days).Format("2006-01-02") }
	buyDate, firstPeakDate := date(-8), date(-7)
	resetDate, resetPeakDate := date(-5), date(-4)
	market := NewMarketService(datasource.NewManagerWithAdapters())
	svc := NewPositionService(market)

	common.DB.Create(&model.DailyBar{Symbol: "600001", Market: "cn", TradeDate: buyDate, High: 90, Close: 10})
	common.DB.Create(&model.DailyBar{Symbol: "600001", Market: "cn", TradeDate: firstPeakDate, High: 18, Close: 17})
	p, err := svc.Create(context.Background(), 1, PositionInput{
		Symbol: "600001", Market: "cn", Name: "历史建仓", PositionType: model.PositionTypeLongTerm,
		BuyPrice: 10, BuyDate: buyDate, Quantity: 100,
	})
	if err != nil {
		t.Fatalf("历史建仓失败: %v", err)
	}
	if p.PeakPrice != 18 || p.PeakDate != firstPeakDate || !p.PeakBackfilled {
		t.Fatalf("历史建仓应回填起算日后的峰值，且排除起算日 High: %+v", p)
	}

	common.DB.Create(&model.DailyBar{Symbol: p.Symbol, Market: p.Market, TradeDate: resetDate, High: 99, Close: 12})
	common.DB.Create(&model.DailyBar{Symbol: p.Symbol, Market: p.Market, TradeDate: resetPeakDate, High: 25, Close: 24})
	p, err = svc.AddTrade(1, p.ID, PositionTradeInput{
		Side: model.PositionTradeBuy, Price: 12, Quantity: 100, TradeDate: resetDate,
	})
	if err != nil {
		t.Fatalf("历史加仓失败: %v", err)
	}
	if p.PeakPrice != 25 || p.PeakDate != resetPeakDate || p.PeakFrom != resetDate || !p.PeakBackfilled {
		t.Fatalf("历史加仓应按新起算日重建峰值，且排除加仓日 High: %+v", p)
	}

	common.DB.Create(&model.DailyBar{Symbol: "600002", Market: "cn", TradeDate: buyDate, High: 80, Close: 10})
	common.DB.Create(&model.DailyBar{Symbol: "600002", Market: "cn", TradeDate: firstPeakDate, High: 16, Close: 15})
	p2, err := svc.Create(context.Background(), 1, PositionInput{
		Symbol: "600002", Market: "cn", Name: "待修正建仓", PositionType: model.PositionTypeLongTerm,
		BuyPrice: 10, BuyDate: buyDate, Quantity: 100,
	})
	if err != nil {
		t.Fatalf("建待修正持仓失败: %v", err)
	}
	common.DB.Create(&model.DailyBar{Symbol: p2.Symbol, Market: p2.Market, TradeDate: resetDate, High: 88, Close: 12})
	common.DB.Create(&model.DailyBar{Symbol: p2.Symbol, Market: p2.Market, TradeDate: resetPeakDate, High: 20, Close: 19})
	p2, err = svc.Update(1, p2.ID, PositionInput{
		PositionType: model.PositionTypeLongTerm, Currency: "CNY",
		BuyPrice: 12, BuyDate: resetDate, Quantity: 100,
	})
	if err != nil {
		t.Fatalf("修正首笔买入失败: %v", err)
	}
	if p2.PeakPrice != 20 || p2.PeakDate != resetPeakDate || p2.PeakFrom != resetDate || !p2.PeakBackfilled {
		t.Fatalf("首笔买入价/日期修正后应同步重建峰值: %+v", p2)
	}
}
