package service

import (
	"context"
	"sync"
	"time"

	"quantvista/datasource"
)

// BoardService 板块业务：行业/概念热力图 + 板块详情（指数日线 + 成分股）。
// 板块 clist 三方法只在 EastMoneyAdapter 上（非 Manager 能力路由），持自有实例——
// 同 MarketService.wide 模式，长任务/在线路径互不影响限流状态。单测可注入假实现。
type BoardService struct {
	src boardSource
	mu  sync.Mutex
	// heatCache 按 kind 缓存热力图 60s（板块热度低频变化，热力图会被频繁刷新/切换）。
	heatCache map[string]boardHeatEntry
	// fetchMu 缓存未命中时串行化取数（简版 singleflight），防并发首访重复打上游。
	fetchMu sync.Mutex
}

type boardHeatEntry struct {
	rows []datasource.BoardHeat
	exp  time.Time
}

// boardSource 板块链路依赖的数据源能力子集（东财实现；单测注入假实现）。
type boardSource interface {
	GetBoardHeat(ctx context.Context, kind string) ([]datasource.BoardHeat, error)
	GetBoardConstituents(ctx context.Context, code string, limit int) ([]datasource.BoardStock, error)
	GetBoardKline(ctx context.Context, code string, limit int) ([]datasource.Bar, error)
}

func NewBoardService() *BoardService {
	return &BoardService{src: datasource.NewEastMoneyAdapter(), heatCache: map[string]boardHeatEntry{}}
}

const boardHeatCacheTTL = 60 * time.Second

// Heatmap 取板块热力图（kind=industry/concept），60s 进程内缓存 per kind。
// 缓存未命中时经 fetchMu 串行化：并发首访/缓存刚过期时只有一个请求真正打上游，
// 其余排队后二次查缓存直接命中，防瞬时重复请求触发东财限流。
func (s *BoardService) Heatmap(ctx context.Context, kind string) ([]datasource.BoardHeat, error) {
	if rows, ok := s.cachedHeat(kind); ok {
		return rows, nil
	}
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if rows, ok := s.cachedHeat(kind); ok { // 双检：排队期间可能已被首个请求填好
		return rows, nil
	}
	rows, err := s.src.GetBoardHeat(ctx, kind)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.heatCache[kind] = boardHeatEntry{rows: rows, exp: time.Now().Add(boardHeatCacheTTL)}
	s.mu.Unlock()
	return rows, nil
}

func (s *BoardService) cachedHeat(kind string) ([]datasource.BoardHeat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.heatCache[kind]; ok && time.Now().Before(e.exp) {
		return e.rows, true
	}
	return nil, false
}

// BoardDetail 板块详情：指数日线 + 成分股（各自可缺，errors 记录哪块失败）。
type BoardDetail struct {
	Code     string                  `json:"code"`
	Bars     []datasource.Bar        `json:"bars"`   // 板块指数日线（120 根）
	Stocks   []datasource.BoardStock `json:"stocks"` // 成分股（成交额降序 50 只）
	Errors   map[string]string       `json:"errors"` // 哪些块取数失败
	DataTime time.Time               `json:"data_time"`
}

// Detail 并行拉板块指数日线 + 成分股，单块失败降级（同 GetOverview 的 WaitGroup+setErr 模式）。
// 成分股里成交额第一名标 is_leader、涨幅第一名标 is_top_gainer。
func (s *BoardService) Detail(ctx context.Context, code string) *BoardDetail {
	d := &BoardDetail{Code: code, Errors: map[string]string{}, DataTime: time.Now()}
	var mu sync.Mutex
	setErr := func(k string, err error) {
		mu.Lock()
		d.Errors[k] = err.Error()
		mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if bars, err := s.src.GetBoardKline(ctx, code, 120); err == nil {
			d.Bars = bars
		} else {
			setErr("bars", err)
		}
	}()
	go func() {
		defer wg.Done()
		if stocks, err := s.src.GetBoardConstituents(ctx, code, 50); err == nil {
			markBoardStocks(stocks)
			d.Stocks = stocks
		} else {
			setErr("stocks", err)
		}
	}()
	wg.Wait()
	return d
}

// markBoardStocks 标注成交额第一名（龙头）与涨幅第一名（领涨）。原地修改切片。
// 数据守卫：上游 "-" 容错为 0 的退化行不硬标——龙头须成交额>0，领涨须涨幅>0
//（全盘下跌的板块没有「领涨」是诚实展示，不是漏标）。
func markBoardStocks(stocks []datasource.BoardStock) {
	leaderIdx, gainerIdx := -1, -1
	for i := range stocks {
		if stocks[i].Amount > 0 && (leaderIdx < 0 || stocks[i].Amount > stocks[leaderIdx].Amount) {
			leaderIdx = i
		}
		if stocks[i].ChangePct > 0 && (gainerIdx < 0 || stocks[i].ChangePct > stocks[gainerIdx].ChangePct) {
			gainerIdx = i
		}
	}
	if leaderIdx >= 0 {
		stocks[leaderIdx].IsLeader = true
	}
	if gainerIdx >= 0 {
		stocks[gainerIdx].IsTopGainer = true
	}
}
