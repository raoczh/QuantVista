package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// 提交后编辑并归档策略，排队/重试请求仍须执行提交时的不可变条件。
func TestRecommendationJobKeepsSubmittedStrategyRevision(t *testing.T) {
	const userID int64 = 701
	seedReportEnv(t, userID, "http://127.0.0.1:1")
	screener := NewScreenerService()
	tree := allOf(leafV("close", "<", 20))
	original, err := screener.SaveStrategy(userID, SaveStrategyRequest{Name: "原始低价策略", Tree: &tree})
	if err != nil {
		t.Fatal(err)
	}
	req := RecommendRequest{Type: model.RecTypeShortTerm, Strategy: "screen:u" + strconv.FormatInt(original.ID, 10)}
	svc := &RecommendationService{llm: NewLLMService()}
	plan, err := svc.prepareGeneration(userID, true, req, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(recommendationJobRequestFromPlan(req, plan, true))
	if err != nil {
		t.Fatal(err)
	}
	tree = allOf(leafV("close", ">", 100))
	if _, err := screener.SaveStrategy(userID, SaveStrategyRequest{
		ID: original.ID, BaseRevisionID: original.CurrentRevisionID, Name: "后来高价策略", Tree: &tree,
	}); err != nil {
		t.Fatal(err)
	}
	if err := screener.DeleteStrategy(userID, original.ID); err != nil {
		t.Fatal(err)
	}
	job, err := decodeRecommendationJobRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := svc.prepareGenerationWithSnapshot(userID, true, job.Request, job.Manual, job.PreferenceSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if restored.strat.Name != original.Name {
		t.Fatalf("排队期间策略漂移：原始 %q，执行 %q", original.Name, restored.strat.Name)
	}
	scan, err := screener.resolveStrategy(userID, restored.strat.scanRequest(10))
	if err != nil {
		t.Fatal(err)
	}
	if scan.RevisionID != original.CurrentRevisionID || scan.Name != original.Name {
		t.Fatalf("候选扫描未固定提交版本：revision=%d，name=%q", scan.RevisionID, scan.Name)
	}
	if !strings.Contains(restored.newProcessingBatch().Title, original.Name) {
		t.Fatal("批次标题未保留提交时策略")
	}
	if _, err := svc.prepareGenerationWithSnapshot(userID+1, true, job.Request, job.Manual, job.PreferenceSnapshot); err == nil {
		t.Fatal("其他用户不得执行本人的策略版本")
	}
}

type reviewStrategyBarsAdapter struct {
	recPreheatMarketAdapter
	bars []datasource.Bar
}

func (a *reviewStrategyBarsAdapter) GetDailyBars(_ context.Context, _, _ string, limit int) ([]datasource.Bar, error) {
	bars := a.bars
	if limit > 0 && len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}
	return append([]datasource.Bar(nil), bars...), nil
}

func (a *reviewStrategyBarsAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	return &datasource.Quote{Symbol: symbol, Market: market, Name: "样本" + symbol,
		Price: 10, PrevClose: 10, Amount: 2e8, DataTime: recFreshQuoteTime()}, nil
}

func (a *reviewStrategyBarsAdapter) GetStockRanking(_ context.Context, _, sort string, _ bool, limit int) ([]datasource.StockRank, error) {
	prefix := map[string]string{"changepercent": "601", "turnoverratio": "602", "amount": "603"}[sort]
	rows := make([]datasource.StockRank, limit)
	for i := range rows {
		rows[i] = datasource.StockRank{Symbol: fmt.Sprintf("%s%03d", prefix, i),
			Name: "榜单样本", Price: 10, Amount: 2e8, TurnoverRate: 5}
	}
	return rows, nil
}

// 选股可命中的年线策略，在推荐的实际评分路径中也必须取得完整年线窗口。
func TestRecommendationYearLineMatchesScreener(t *testing.T) {
	setupTestDB(t)
	resetFactorTable()
	t.Cleanup(resetFactorTable)
	bars := make([]datasource.Bar, 250)
	for i := range bars {
		bars[i] = datasource.Bar{
			TradeDate: time.Date(2025, 1, 1+i, 0, 0, 0, 0, time.Local).Format("2006-01-02"),
			Open:      10, High: 10.1, Low: 9.9, Close: 10, Volume: 200000, Amount: 2e8, TurnoverRate: 2,
		}
	}
	seedWideStock(t, "600101", "年线样本", bars)
	scan, err := NewScreenerService().Scan(context.Background(), 1, ScanRequest{StrategyKey: "year-line-stand"})
	if err != nil || scan.Matched != 1 {
		t.Fatalf("选股基准应命中年线样本：scan=%+v err=%v", scan, err)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(&reviewStrategyBarsAdapter{bars: bars}))
	svc := NewRecommendationService(market, nil, nil)
	svc.em.SetFetchForTest(func(context.Context, string, map[string]string) ([]byte, int, error) {
		return nil, 503, datasource.ErrNoData
	})
	strat, err := resolveRecStrategy(1, model.RecTypeShortTerm, "screen:year-line-stand")
	if err != nil {
		t.Fatal(err)
	}
	pool := []candidate{{Symbol: "600101", Market: "cn", Name: "年线样本", Price: 10, Amount: 2e8, TurnoverRate: 2}}
	svc.scorePool(context.Background(), model.RecTypeShortTerm, strat, pool, RecFilters{}, nil)
	if hit := pool[0].StrategyHit; hit == nil || !hit.Full {
		t.Fatalf("同一日线的选股与推荐命中应一致：%+v", hit)
	}
}

func TestRecommendationDeleteRejectsProcessing(t *testing.T) {
	setupTestDB(t)
	batch := model.RecommendationBatch{UserID: 702, Type: model.RecTypeShortTerm, Status: model.RecStatusProcessing}
	if err := common.DB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&RecommendationService{}).Delete(702, batch.ID); err == nil {
		t.Fatal("正在生成的推荐不能删除，否则 worker Save 会重新插入批次")
	}
	var stored model.RecommendationBatch
	if err := common.DB.First(&stored, batch.ID).Error; err != nil {
		t.Fatalf("拒绝删除后批次应仍存在：%v", err)
	}
}

// 大自选和其他来源超过入池/评分上限时，所选策略的独立命中仍应进入评分。
func TestRecommendationSelectedStrategyHasPoolAndScoreCapacity(t *testing.T) {
	setupTestDB(t)
	resetFactorTable()
	t.Cleanup(resetFactorTable)
	today := time.Now().Format("2006-01-02")
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: today, IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	group := model.Watchlist{UserID: 703, Name: "自选"}
	if err := common.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		symbol := fmt.Sprintf("600%03d", i)
		if err := common.DB.Create(&model.WatchlistItem{UserID: 703, WatchlistID: group.ID, Symbol: symbol, Market: "cn", Name: "自选样本"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := common.DB.Create(&model.DailyBar{Symbol: symbol, Market: "cn", TradeDate: today, Close: 10, Amount: 2e8}).Error; err != nil {
			t.Fatal(err)
		}
	}
	bar := datasource.Bar{TradeDate: today, Open: 10, High: 11, Low: 9, Close: 10, Amount: 2e8, TurnoverRate: 5}
	table := singleRowFactorTable(computeWideRow("600999", wideStockMeta{Name: "指定策略命中"}, []datasource.Bar{bar}))
	table.Symbols, table.Names, table.LastDates = []string{"600999"}, []string{"指定策略命中"}, []string{today}
	table.TradeDate, table.ExpectedDate = today, today
	factorTableMu.Lock()
	factorTableCur = table
	factorTableMu.Unlock()
	factorFreshMu.Lock()
	factorFreshVal, factorFreshAt = today, time.Now()
	factorFreshMu.Unlock()
	tree := allOf(leafV("close", "<", 20))
	strategy, err := NewScreenerService().SaveStrategy(703, SaveStrategyRequest{Name: "指定低价", Period: "short", Tree: &tree})
	if err != nil {
		t.Fatal(err)
	}
	strat, err := resolveRecStrategy(703, model.RecTypeShortTerm, "screen:u"+strconv.FormatInt(strategy.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(&reviewStrategyBarsAdapter{}))
	svc := NewRecommendationService(market, NewWatchlistService(market), nil)
	pool, _, err := svc.buildPool(context.Background(), 703, "cn", model.RecTypeShortTerm, strat, RecFilters{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pool {
		if c.Symbol == "600999" {
			if c.Excluded != "" || !hasSource(c.Sources, "strategy_signal") {
				t.Fatalf("所选策略命中须获得评分名额：excluded=%q sources=%v", c.Excluded, c.Sources)
			}
			return
		}
	}
	t.Fatal("所选策略的候选被其他来源挤出候选池")
}
