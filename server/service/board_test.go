package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/datasource"
)

// fakeBoardSource 计数各方法调用次数，可配置单块失败。
type fakeBoardSource struct {
	heatCalls  int
	stockCalls int
	klineCalls int
	heatErr    error
	stockErr   error
	klineErr   error
	stocks     []datasource.BoardStock
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

func newTestBoardService(src boardSource) *BoardService {
	return &BoardService{src: src, heatCache: map[string]boardHeatEntry{}}
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
