package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// fakeBoardSource 计数各方法调用次数，可配置单块失败。
type fakeBoardSource struct {
	heatCalls  int
	stockCalls int
	klineCalls int
	flowCalls  int
	heatErr    error
	stockErr   error
	klineErr   error
	flowErr    error
	stocks     []datasource.BoardStock
	flows      []datasource.StockFundFlowBar
}

func (f *fakeBoardSource) GetBoardHeat(ctx context.Context, kind string) ([]datasource.BoardHeat, error) {
	f.heatCalls++
	if f.heatErr != nil {
		return nil, f.heatErr
	}
	return []datasource.BoardHeat{{Code: "BK1036", Name: "半导体", ChangePct: 2.1}}, nil
}

func (f *fakeBoardSource) GetBoardConstituents(ctx context.Context, code string, limit int) ([]datasource.BoardStock, error) {
	f.stockCalls++
	if f.stockErr != nil {
		return nil, f.stockErr
	}
	return f.stocks, nil
}

func (f *fakeBoardSource) GetBoardKline(ctx context.Context, code string, limit int) ([]datasource.Bar, error) {
	f.klineCalls++
	if f.klineErr != nil {
		return nil, f.klineErr
	}
	return []datasource.Bar{{TradeDate: "2026-07-09", Close: 100}}, nil
}

func (f *fakeBoardSource) GetBoardFundFlow(ctx context.Context, code string, limit int) ([]datasource.StockFundFlowBar, error) {
	f.flowCalls++
	if f.flowErr != nil {
		return nil, f.flowErr
	}
	return f.flows, nil
}

func newTestBoardService(src boardSource) *BoardService {
	return &BoardService{src: src, heatCache: map[string]boardHeatEntry{}, flowCache: map[string]boardFlowEntry{}}
}

func TestBoardHeatmapCache(t *testing.T) {
	f := &fakeBoardSource{}
	s := newTestBoardService(f)
	for i := 0; i < 3; i++ {
		if _, err := s.Heatmap(context.Background(), "industry"); err != nil {
			t.Fatalf("Heatmap: %v", err)
		}
	}
	if f.heatCalls != 1 {
		t.Fatalf("60s 缓存内应只拉一次, got %d", f.heatCalls)
	}
	// 不同 kind 独立缓存，各拉一次。
	if _, err := s.Heatmap(context.Background(), "concept"); err != nil {
		t.Fatalf("Heatmap concept: %v", err)
	}
	if f.heatCalls != 2 {
		t.Fatalf("concept 应独立拉取, heatCalls=%d", f.heatCalls)
	}
	// 缓存过期后重新拉取。
	s.heatCache["industry"] = boardHeatEntry{rows: nil, exp: time.Now().Add(-time.Second)}
	if _, err := s.Heatmap(context.Background(), "industry"); err != nil {
		t.Fatalf("Heatmap: %v", err)
	}
	if f.heatCalls != 3 {
		t.Fatalf("过期后应重拉, heatCalls=%d", f.heatCalls)
	}
}

func TestBoardHeatmapErrorNotCached(t *testing.T) {
	f := &fakeBoardSource{heatErr: errors.New("限流")}
	s := newTestBoardService(f)
	if _, err := s.Heatmap(context.Background(), "industry"); err == nil {
		t.Fatal("应返回错误")
	}
	// 错误不进缓存：再拉一次仍触源。
	_, _ = s.Heatmap(context.Background(), "industry")
	if f.heatCalls != 2 {
		t.Fatalf("错误不应缓存, heatCalls=%d", f.heatCalls)
	}
}

func TestBoardDetailMarksLeaderAndGainer(t *testing.T) {
	f := &fakeBoardSource{stocks: []datasource.BoardStock{
		{Symbol: "A", Amount: 100, ChangePct: 1.0},
		{Symbol: "B", Amount: 500, ChangePct: 3.0}, // 成交额与涨幅都第一
		{Symbol: "C", Amount: 300, ChangePct: 9.9}, // 涨幅第一
	}}
	// 让 B 成交额最大、C 涨幅最大，验证两个标注互相独立。
	f.stocks[1].ChangePct = 3.0
	s := newTestBoardService(f)
	d := s.Detail(context.Background(), "BK1036")
	if len(d.Errors) != 0 {
		t.Fatalf("不应有错误: %v", d.Errors)
	}
	if len(d.Bars) != 1 {
		t.Fatalf("bars 应有 1 根")
	}
	byId := map[string]datasource.BoardStock{}
	for _, x := range d.Stocks {
		byId[x.Symbol] = x
	}
	if !byId["B"].IsLeader || byId["A"].IsLeader || byId["C"].IsLeader {
		t.Fatalf("龙头应是成交额第一 B: %+v", d.Stocks)
	}
	if !byId["C"].IsTopGainer || byId["B"].IsTopGainer {
		t.Fatalf("领涨应是涨幅第一 C: %+v", d.Stocks)
	}
}

func TestBoardDetailSingleBlockDegrade(t *testing.T) {
	// kline 失败但成分股成功：整体不崩，errors 记录 bars，stocks 仍在。
	f := &fakeBoardSource{
		klineErr: errors.New("push2his 限流"),
		stocks:   []datasource.BoardStock{{Symbol: "A", Amount: 100, ChangePct: 1}},
	}
	s := newTestBoardService(f)
	d := s.Detail(context.Background(), "BK1036")
	if len(d.Bars) != 0 {
		t.Fatalf("bars 应为空")
	}
	if _, ok := d.Errors["bars"]; !ok {
		t.Fatalf("errors 应含 bars: %v", d.Errors)
	}
	if len(d.Stocks) != 1 {
		t.Fatalf("stocks 应仍在: %+v", d.Stocks)
	}
	if _, ok := d.Errors["stocks"]; ok {
		t.Fatalf("stocks 未失败不应进 errors")
	}
}

// P3b：板块资金流透传——缓存内只打一次上游、汇总手工验算、days 截尾。
func TestBoardFundFlowCacheAndSummary(t *testing.T) {
	f := &fakeBoardSource{flows: []datasource.StockFundFlowBar{
		{TradeDate: "2026-07-08", MainNet: -2e8, MainPct: -1.5, Close: 1650.0, ChangePct: -0.8},
		{TradeDate: "2026-07-09", MainNet: 1e8, MainPct: 0.9, Close: 1660.5, ChangePct: 0.64},
		{TradeDate: "2026-07-10", MainNet: 3e8, MainPct: 2.1, Close: 1687.45, ChangePct: 1.62},
	}}
	s := newTestBoardService(f)
	var view *BoardFundFlowView
	var err error
	for i := 0; i < 3; i++ {
		view, err = s.FundFlow(context.Background(), "BK1036", 90)
		if err != nil {
			t.Fatalf("FundFlow: %v", err)
		}
	}
	if f.flowCalls != 1 {
		t.Fatalf("缓存内应只拉一次, got %d", f.flowCalls)
	}
	// 手工验算：1d=3 亿、5d 覆盖全部 3 根 = -2+1+3 = 2 亿；末端连续净流入 2 天。
	if view.MainNet1dYi != 3 || view.MainNet5dYi != 2 || view.MainNet10dYi != 2 || view.StreakDays != 2 {
		t.Fatalf("汇总验算失败: %+v", view)
	}
	if view.LastDate != "2026-07-10" || len(view.Days) != 3 || view.Days[2].Close != 1687.45 {
		t.Fatalf("逐日序列错误: %+v", view)
	}
	// days 截尾取末端。
	v2, _ := s.FundFlow(context.Background(), "BK1036", 2)
	if len(v2.Days) != 2 || v2.Days[0].Date != "2026-07-09" {
		t.Fatalf("days 截尾错误: %+v", v2.Days)
	}
	// 不同板块独立缓存。
	if _, err := s.FundFlow(context.Background(), "BK0447", 90); err != nil {
		t.Fatalf("FundFlow BK0447: %v", err)
	}
	if f.flowCalls != 2 {
		t.Fatalf("不同 code 应独立拉取, flowCalls=%d", f.flowCalls)
	}
	// 错误不进缓存。
	f.flowErr = errors.New("限流")
	if _, err := s.FundFlow(context.Background(), "BK0998", 90); err == nil {
		t.Fatal("应返回错误")
	}
	f.flowErr = nil
	if _, err := s.FundFlow(context.Background(), "BK0998", 90); err != nil {
		t.Fatalf("错误不应缓存: %v", err)
	}
}

// boardNetStreakDays 边界：净流出方向、零值行终止、末日为 0。
func TestBoardNetStreakDays(t *testing.T) {
	mk := func(nets ...float64) []datasource.StockFundFlowBar {
		out := make([]datasource.StockFundFlowBar, len(nets))
		for i, n := range nets {
			out[i] = datasource.StockFundFlowBar{MainNet: n}
		}
		return out
	}
	cases := []struct {
		name string
		bars []datasource.StockFundFlowBar
		want int
	}{
		{"空序列", nil, 0},
		{"末日为 0", mk(1, 0), 0},
		{"连续流出", mk(1, -2, -3), -2},
		{"零值行终止", mk(2, 0, 3, 4), 2},
		{"全流入", mk(1, 2, 3), 3},
	}
	for _, c := range cases {
		if got := boardNetStreakDays(c.bars); got != c.want {
			t.Errorf("%s: streak=%d, want %d", c.name, got, c.want)
		}
	}
}

// P3b：Detail 估值第三块——库中有行业板块估值行时随详情返回，无数据自然缺席。
func TestBoardDetailValuationBlock(t *testing.T) {
	setupTestDB(t)
	cleanBoardValTables(t)
	if err := common.DB.Create(&model.BoardValuationDaily{
		Kind: "industry", BoardCode: "BK1036", BoardName: "半导体", TradeDate: "2026-07-14",
		MedianPETTM: 42, MedianPB: 4.1, PosPECount: 99, StockCount: 120, PctRank: 81,
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := newTestBoardService(&fakeBoardSource{stocks: []datasource.BoardStock{{Symbol: "A", Amount: 1, ChangePct: 1}}})
	d := s.Detail(context.Background(), "BK1036")
	if d.Valuation == nil || d.Valuation.MedianPETTM != 42 || d.Valuation.HistDays != 1 {
		t.Fatalf("估值块缺失或错误: %+v", d.Valuation)
	}
	// 概念板块/未聚合板块自然缺席。
	d2 := s.Detail(context.Background(), "BK0817")
	if d2.Valuation != nil {
		t.Fatalf("无估值数据应缺席: %+v", d2.Valuation)
	}
}
