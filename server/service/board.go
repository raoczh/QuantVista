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
	// flowCache 按板块代码缓存资金流历史（P3b：纯透传不落库，上游自带历史；
	// 短缓存挡住详情页反复进出——push2his 无备用域，能省一次是一次）。
	flowCache map[string]boardFlowEntry
	// fetchMu 缓存未命中时串行化取数（简版 singleflight），防并发首访重复打上游。
	fetchMu sync.Mutex
}

type boardHeatEntry struct {
	rows []datasource.BoardHeat
	exp  time.Time
}

type boardFlowEntry struct {
	bars []datasource.StockFundFlowBar
	exp  time.Time
}

// boardSource 板块链路依赖的数据源能力子集（东财实现；单测注入假实现）。
type boardSource interface {
	GetBoardHeat(ctx context.Context, kind string) ([]datasource.BoardHeat, error)
	GetBoardConstituents(ctx context.Context, code string, limit int) ([]datasource.BoardStock, error)
	GetBoardKline(ctx context.Context, code string, limit int) ([]datasource.Bar, error)
	GetBoardFundFlow(ctx context.Context, code string, limit int) ([]datasource.StockFundFlowBar, error)
}

func NewBoardService() *BoardService {
	return &BoardService{src: datasource.NewEastMoneyAdapter(), heatCache: map[string]boardHeatEntry{}, flowCache: map[string]boardFlowEntry{}}
}

const (
	boardHeatCacheTTL = 60 * time.Second
	boardFlowCacheTTL = 5 * time.Minute // 逐日历史盘中只有当日一行在变，5min 足够新鲜
	boardFlowBarLimit = 250             // 与个股资金流缓存窗口对齐
)

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

// BoardDetail 板块详情：指数日线 + 成分股 + 估值（各自可缺，errors 记录哪块失败）。
type BoardDetail struct {
	Code     string                  `json:"code"`
	Bars     []datasource.Bar        `json:"bars"`   // 板块指数日线（120 根）
	Stocks   []datasource.BoardStock `json:"stocks"` // 成分股（成交额降序 50 只）
	// Valuation 板块估值（P3b 聚合表最新行；概念板块无 f100 覆盖自然缺席，前端 v-if 不渲染）。
	Valuation *BoardValuationView `json:"valuation,omitempty"`
	Errors    map[string]string   `json:"errors"` // 哪些块取数失败
	DataTime  time.Time           `json:"data_time"`
}

// Detail 并行拉板块指数日线 + 成分股 + 查库估值，单块失败降级（同 GetOverview 的 WaitGroup+setErr 模式）。
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
	wg.Add(3)
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
	go func() {
		defer wg.Done()
		// 查库不打上游：无数据（概念板块/聚合未跑过）不算错误，Valuation 缺席即可。
		d.Valuation = boardValuationFor(code)
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

// BoardFundFlowView 板块资金流响应（P3b）：逐日序列（近 days 根）+ 汇总。
// 结构对齐个股 StockFundFlowView（前端复用同一张图的画法），Close 为板块指数点位。
type BoardFundFlowView struct {
	Code string             `json:"code"`
	Days []StockFundFlowDay `json:"days"`
	// 汇总（亿元）：今日/5日/10日/20日主力净额与连续净流入天数。
	MainNet1dYi  float64 `json:"main_net_1d_yi"`
	MainNet5dYi  float64 `json:"main_net_5d_yi"`
	MainNet10dYi float64 `json:"main_net_10d_yi"`
	MainNet20dYi float64 `json:"main_net_20d_yi"`
	StreakDays   int     `json:"streak_days"` // 正=连续净流入天数，负=连续净流出
	LastDate     string  `json:"last_date,omitempty"`
}

// FundFlow 板块资金流历史（纯透传：上游 fflow/daykline 自带全历史，不落库；
// per-code 短缓存 + fetchMu 串行化，同 Heatmap 模式）。days 控制返回序列长度。
func (s *BoardService) FundFlow(ctx context.Context, code string, days int) (*BoardFundFlowView, error) {
	if days <= 0 || days > boardFlowBarLimit {
		days = 90
	}
	bars, ok := s.cachedFlow(code)
	if !ok {
		s.fetchMu.Lock()
		bars, ok = s.cachedFlow(code) // 双检：排队期间可能已被首个请求填好
		if !ok {
			var err error
			bars, err = s.src.GetBoardFundFlow(ctx, code, boardFlowBarLimit)
			if err != nil {
				s.fetchMu.Unlock()
				return nil, err
			}
			s.mu.Lock()
			s.flowCache[code] = boardFlowEntry{bars: bars, exp: time.Now().Add(boardFlowCacheTTL)}
			s.mu.Unlock()
		}
		s.fetchMu.Unlock()
	}
	view := &BoardFundFlowView{Code: code, Days: []StockFundFlowDay{}}
	if len(bars) == 0 {
		return view, nil
	}
	view.LastDate = bars[len(bars)-1].TradeDate
	view.MainNet1dYi = round2(boardMainNetSum(bars, 1) / 1e8)
	view.MainNet5dYi = round2(boardMainNetSum(bars, 5) / 1e8)
	view.MainNet10dYi = round2(boardMainNetSum(bars, 10) / 1e8)
	view.MainNet20dYi = round2(boardMainNetSum(bars, 20) / 1e8)
	view.StreakDays = boardNetStreakDays(bars)
	tail := bars
	if len(tail) > days {
		tail = tail[len(tail)-days:]
	}
	for _, b := range tail {
		view.Days = append(view.Days, StockFundFlowDay{
			Date: b.TradeDate, MainNetYi: round2(b.MainNet / 1e8),
			MainPct: b.MainPct, Close: b.Close, ChangePct: b.ChangePct,
		})
	}
	return view, nil
}

func (s *BoardService) cachedFlow(code string) ([]datasource.StockFundFlowBar, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.flowCache[code]; ok && time.Now().Before(e.exp) {
		return e.bars, true
	}
	return nil, false
}

// ---------- 板块版汇总纯函数（单测锚点） ----------
// 与 fundflow.go 的 mainNetSum/mainNetStreakDays 同语义，但入参是数据源透传的
// []datasource.StockFundFlowBar（板块不落库，没有 model.FundFlowDaily 形态）——
// 既有纯函数签名不动是 P3b 施工纪律，别为板块改它们。

// boardMainNetSum 末端 n 日主力净额合计（元）。
func boardMainNetSum(bars []datasource.StockFundFlowBar, n int) float64 {
	if n > len(bars) {
		n = len(bars)
	}
	var sum float64
	for _, b := range bars[len(bars)-n:] {
		sum += b.MainNet
	}
	return sum
}

// boardNetStreakDays 末端连续同号主力净额天数：正=连续净流入、负=连续净流出、
// 0=无数据或末日净额恰为 0。零值行终止计数（休市日占位行不跨越延续「连续」语义）。
func boardNetStreakDays(bars []datasource.StockFundFlowBar) int {
	n := len(bars)
	if n == 0 {
		return 0
	}
	last := bars[n-1].MainNet
	if last == 0 {
		return 0
	}
	streak := 0
	for i := n - 1; i >= 0; i-- {
		v := bars[i].MainNet
		if v == 0 || (v > 0) != (last > 0) {
			break
		}
		streak++
	}
	if last < 0 {
		return -streak
	}
	return streak
}
