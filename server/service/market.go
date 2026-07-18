package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MarketService 行情业务：缓存 → 数据源适配层 → 落库快照。
type MarketService struct {
	mgr *datasource.Manager
	// wide 全市场日线链路（M1）专用源：clist 快照只有东财有；历史初始化/除权重锚的
	// kline 必须与增量同为东财前复权口径，不走 mgr 路由（防新浪不复权兜底毒化基准）。
	// 独立实例（自带断路器）：长任务的限流状态不与在线路径互相影响。测试可注入假实现。
	wide wideSource
}

// wideSource 全市场日线链路依赖的数据源能力子集（东财实现；单测注入假实现）。
type wideSource interface {
	GetCNSpotSnapshot(ctx context.Context) ([]datasource.SpotRow, error)
	GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error)
}

func NewMarketService(mgr *datasource.Manager) *MarketService {
	return &MarketService{mgr: mgr, wide: datasource.NewEastMoneyAdapter()}
}

const quoteCacheTTL = 10 * time.Second

// DataSourceHealth 各 (数据源,能力) 健康滑窗快照（管理端 GET /api/admin/datasources）。
func (s *MarketService) DataSourceHealth() []datasource.HealthStat {
	return s.mgr.HealthSnapshot()
}

// GetQuote 取实时行情：先查缓存，miss 则走数据源，成功后落库并回种缓存。
func (s *MarketService) GetQuote(ctx context.Context, market, symbol string) (*datasource.Quote, error) {
	cacheKey := "quote:" + market + ":" + symbol

	if cached, ok := common.RedisGet(cacheKey); ok {
		var q datasource.Quote
		if json.Unmarshal([]byte(cached), &q) == nil {
			return &q, nil
		}
	}

	q, err := s.mgr.GetQuote(ctx, market, symbol)
	if err != nil {
		return nil, err
	}

	// 落库：股票基础信息 + 最新行情快照（按 symbol+market 覆盖更新）。
	s.persist(q)

	if b, err := json.Marshal(q); err == nil {
		common.RedisSet(cacheKey, string(b), quoteCacheTTL)
	}
	return q, nil
}

const dailyBarsCacheTTL = 10 * time.Minute

// freshMissTTL 全源过期候选的短标记 TTL：停牌股每次新建会话都会判 stale 触发全源遍历，
// 用此标记在 60s 内复用候选，避免同标的重复扫源打满 15s 预算。
const freshMissTTL = 60 * time.Second

// GetFreshQuote 取「新鲜行情」：读缓存/上游结果后按交易日历与交易时段校验 DataTime，
// 交易时段内过期则继续尝试下一数据源（不把旧行情当成功），全部源都过期则返回最新候选并
// 标注 stale。返回 行情、新鲜度信息、错误。仅个股快照采集（buildStockSnapshot）走此链路。
// 非 cn 市场无交易日历判定，降级为普通 GetQuote 并标 unknown。
func (s *MarketService) GetFreshQuote(ctx context.Context, market, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
	if market != "cn" {
		q, err := s.GetQuote(ctx, market, symbol)
		return q, quoteFreshInfo{Status: freshStatusUnknown, MarketState: marketStateClosed}, err
	}

	now := time.Now().In(time.Local)
	isTrading := isTradingDayToday(now)
	state := cnMarketState(now, isTrading)
	expected := expectedQuoteDate(now, isTrading, prevOpenTradeDate(now.Format("2006-01-02")))
	accept := func(q *datasource.Quote) bool {
		return quoteFreshness(q.DataTime, now, state, expected).Status == freshStatusFresh
	}
	info := func(q *datasource.Quote) quoteFreshInfo {
		if q == nil {
			return quoteFreshInfo{Status: freshStatusStale, MarketState: state, ExpectedDate: expected}
		}
		return quoteFreshness(q.DataTime, now, state, expected)
	}

	cacheKey := "quote:" + market + ":" + symbol
	if cached, ok := common.RedisGet(cacheKey); ok {
		var q datasource.Quote
		if json.Unmarshal([]byte(cached), &q) == nil && accept(&q) {
			return &q, info(&q), nil
		}
	}
	// 全源过期短标记：命中即复用候选，跳过重复扫源。
	missKey := "quote:freshmiss:" + market + ":" + symbol
	if cached, ok := common.RedisGet(missKey); ok {
		var q datasource.Quote
		if json.Unmarshal([]byte(cached), &q) == nil {
			return &q, info(&q), nil
		}
	}

	q, fresh, err := s.mgr.GetQuoteFresh(ctx, market, symbol, accept)
	if err != nil {
		if errors.Is(err, datasource.ErrSymbolInvalid) {
			return nil, quoteFreshInfo{Status: freshStatusStale, MarketState: state, ExpectedDate: expected}, err
		}
		common.SysWarn("取新鲜行情失败 symbol=%s: %v", symbol, err)
		return nil, quoteFreshInfo{Status: freshStatusStale, MarketState: state, ExpectedDate: expected}, err
	}
	s.persist(q)
	if b, err := json.Marshal(q); err == nil {
		common.RedisSet(cacheKey, string(b), quoteCacheTTL)
		if !fresh {
			common.RedisSet(missKey, string(b), freshMissTTL)
		}
	}
	return q, info(q), nil
}

// GetDailyBars 取日线序列。加 10 分钟缓存：推荐评分/个股分析/对比/评分卡会在短时间内
// 反复拉同一批标的的日线（收盘后日线不变、盘中仅末根在动），缓存显著降低对免费源的压力。
func (s *MarketService) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	cacheKey := fmt.Sprintf("bars:%s:%s:%d", market, symbol, limit)
	if cached, ok := common.RedisGet(cacheKey); ok {
		var bars []datasource.Bar
		if json.Unmarshal([]byte(cached), &bars) == nil && len(bars) > 0 {
			return bars, nil
		}
	}
	bars, err := s.mgr.GetDailyBars(ctx, market, symbol, limit)
	if err != nil {
		return nil, err
	}
	// 在线读路径 best-effort 落库：拉取成功即向调用方返回行情，落库失败只告警
	//（persistDailyBars 内部已 SysWarn），不因缓存回种失败阻断读。
	_ = s.persistDailyBars(market, symbol, bars)
	if b, err := json.Marshal(bars); err == nil {
		common.RedisSet(cacheKey, string(b), dailyBarsCacheTTL)
	}
	return bars, nil
}

const rankingCacheTTL = 60 * time.Second

// GetRanking 取个股榜单（sort: changepercent/amount/turnoverratio/pb；asc=true 升序），60s 缓存。
// 供推荐候选池多源建池使用（首页概览走 GetOverview 的 10 条口径，互不影响）。
func (s *MarketService) GetRanking(ctx context.Context, market, sort string, asc bool, limit int) ([]datasource.StockRank, error) {
	cacheKey := fmt.Sprintf("ranking:%s:%s:%v:%d", market, sort, asc, limit)
	if cached, ok := common.RedisGet(cacheKey); ok {
		var rows []datasource.StockRank
		if json.Unmarshal([]byte(cached), &rows) == nil && len(rows) > 0 {
			return rows, nil
		}
	}
	rows, err := s.mgr.GetStockRanking(ctx, market, sort, asc, limit)
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(rows); err == nil {
		common.RedisSet(cacheKey, string(b), rankingCacheTTL)
	}
	return rows, nil
}

const valuationCacheTTL = 60 * time.Second

// GetValuation 估值/盘面扩展快照（PE/PB/市值/换手/涨跌停等，腾讯免费源）。
// 变化低频，缓存 60s；读时取用不落库。
func (s *MarketService) GetValuation(ctx context.Context, market, symbol string) (*datasource.Valuation, error) {
	cacheKey := "valuation:" + market + ":" + symbol
	if cached, ok := common.RedisGet(cacheKey); ok {
		var v datasource.Valuation
		if json.Unmarshal([]byte(cached), &v) == nil {
			return &v, nil
		}
	}
	v, err := s.mgr.GetValuation(ctx, market, symbol)
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(v); err == nil {
		common.RedisSet(cacheKey, string(b), valuationCacheTTL)
	}
	return v, nil
}

// ValuationsFor 并发批量取估值（复用单只缓存，best-effort：单只失败缺席不影响其余）。
func (s *MarketService) ValuationsFor(ctx context.Context, refs []QuoteRef) map[string]*datasource.Valuation {
	out := make(map[string]*datasource.Valuation, len(refs))
	if len(refs) == 0 {
		return out
	}
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, ref := range refs {
		ref := ref
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := s.GetValuation(ctx, ref.Market, ref.Symbol)
			if err != nil {
				return
			}
			mu.Lock()
			out[QuoteKey(ref.Market, ref.Symbol)] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// GetBenchmarkBars 取基准指数日线（cn=上证指数），供推荐追踪的超额收益/alpha 计算。
// 指数非 stocks 表标的，不落库。基准不可得（us/hk 无源）时返回错误，由调用方降级。
func (s *MarketService) GetBenchmarkBars(ctx context.Context, market string, limit int) (string, []datasource.Bar, error) {
	return s.mgr.GetBenchmarkBars(ctx, market, limit)
}

// QuoteRef 批量取行情的标的引用。
type QuoteRef struct {
	Market string
	Symbol string
}

// QuoteKey 批量结果的 map 键。
func QuoteKey(market, symbol string) string { return market + ":" + symbol }

// QuotesFor 并发批量取行情（复用单只 GetQuote 的缓存），单只失败只是缺席不影响其余。
// 用于自选/持仓列表富化现价；并发上限避免瞬时打爆数据源。
func (s *MarketService) QuotesFor(ctx context.Context, refs []QuoteRef) map[string]*datasource.Quote {
	out := make(map[string]*datasource.Quote, len(refs))
	if len(refs) == 0 {
		return out
	}
	var mu sync.Mutex
	sem := make(chan struct{}, 8) // 并发上限 8
	var wg sync.WaitGroup
	for _, ref := range refs {
		ref := ref
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			q, err := s.GetQuote(ctx, ref.Market, ref.Symbol)
			if err != nil {
				return
			}
			mu.Lock()
			out[QuoteKey(ref.Market, ref.Symbol)] = q
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// Overview 市场首页概览：各块独立获取，部分失败不影响整体（失败记入 Errors）。
type Overview struct {
	Indices  []datasource.Index         `json:"indices"`
	Gainers  []datasource.StockRank     `json:"gainers"`   // 涨幅榜
	Actives  []datasource.StockRank     `json:"actives"`   // 成交额/热门榜
	Sectors  []datasource.SectorRank    `json:"sectors"`   // 板块涨跌榜（best-effort）
	Breadth  *datasource.Breadth        `json:"breadth"`   // 涨跌家数/涨跌停（市场情绪）
	FundFlow *datasource.MarketFundFlow `json:"fund_flow"` // 两市资金流
	Errors   map[string]string          `json:"errors"`    // 哪些块取数失败
	DataTime time.Time                  `json:"data_time"`
}

const overviewCacheTTL = 15 * time.Second

// GetOverview 并行拉取首页各块，单块失败降级（前端对应卡片显示"暂不可用"）。
func (s *MarketService) GetOverview(ctx context.Context, market string) *Overview {
	cacheKey := "overview:" + market
	if cached, ok := common.RedisGet(cacheKey); ok {
		var ov Overview
		if json.Unmarshal([]byte(cached), &ov) == nil {
			return &ov
		}
	}

	ov := &Overview{Errors: map[string]string{}, DataTime: time.Now()}
	var mu sync.Mutex
	setErr := func(k string, err error) {
		mu.Lock()
		ov.Errors[k] = err.Error()
		mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(6)
	go func() {
		defer wg.Done()
		if r, err := s.mgr.GetIndices(ctx, market); err == nil {
			ov.Indices = r
		} else {
			setErr("indices", err)
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := s.mgr.GetStockRanking(ctx, market, "changepercent", false, 10); err == nil {
			ov.Gainers = r
		} else {
			setErr("gainers", err)
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := s.mgr.GetStockRanking(ctx, market, "amount", false, 10); err == nil {
			ov.Actives = r
		} else {
			setErr("actives", err)
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := s.mgr.GetSectorRanking(ctx, market, 10); err == nil {
			ov.Sectors = r
		} else {
			setErr("sectors", err)
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := s.mgr.GetBreadth(ctx, market); err == nil {
			ov.Breadth = r
		} else {
			setErr("breadth", err)
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := s.mgr.GetMarketFundFlow(ctx, market); err == nil {
			ov.FundFlow = r
		} else {
			setErr("fund_flow", err)
		}
	}()
	wg.Wait()

	// ctx 已取消（客户端断开/超时）时各块几乎必然带着失败结果返回，
	// 此时回写会把残缺概览毒化缓存 15s，直接跳过。
	if ctx.Err() == nil {
		if b, err := json.Marshal(ov); err == nil {
			common.RedisSet(cacheKey, string(b), overviewCacheTTL)
		}
	}
	return ov
}

func (s *MarketService) persistDailyBars(market, symbol string, bars []datasource.Bar) error {
	if common.DB == nil || len(bars) == 0 {
		return nil
	}

	// M1 除权检测（防"部分窗口重写"漏检）：本次拉取若与 DB 内已有窗口的 close 多点
	// 比对出现偏差，说明发生了除权/送转（东财前复权序列整体重锚）——此时只 upsert
	// 本窗会留下"窗口内新基准、窗口外旧基准"的断层。检测命中即全量重锚（250 根删+插）
	// 后直接返回。仅东财源检测：新浪日线不复权，与前复权基准比对必然偏差，会误判。
	if market == "cn" && bars[0].Source == "eastmoney" && s.detectAndRebase(market, symbol, bars) {
		return nil
	}

	dailyRows := make([]model.DailyBar, 0, len(bars))
	calendarRows := make([]model.TradingCalendar, 0, len(bars))
	seenTradeDates := make(map[string]struct{}, len(bars))

	for _, b := range bars {
		if b.TradeDate == "" {
			continue
		}
		source := b.Source
		if source == "" {
			source = "unknown"
		}
		dailyRows = append(dailyRows, model.DailyBar{
			Symbol:       symbol,
			Market:       market,
			TradeDate:    b.TradeDate,
			Open:         b.Open,
			High:         b.High,
			Low:          b.Low,
			Close:        b.Close,
			Volume:       b.Volume,
			Amount:       b.Amount,
			TurnoverRate: b.TurnoverRate,
			Source:       source,
		})
		if _, ok := seenTradeDates[b.TradeDate]; !ok {
			calendarRows = append(calendarRows, model.TradingCalendar{
				Market:    market,
				TradeDate: b.TradeDate,
				IsOpen:    true,
			})
			seenTradeDates[b.TradeDate] = struct{}{}
		}
	}

	if len(dailyRows) > 0 {
		// 无成交额的源（新浪日线 Amount 恒 0）作兜底回退时，不得把东财已写入的
		// 真实 amount 覆盖成 0——本批全为 0 时更新列表不含 amount；换手率（新浪
		// 日线同样没有）照此办理。
		updateCols := []string{"open", "high", "low", "close", "volume", "source"}
		hasAmount, hasTurnover := false, false
		for _, r := range dailyRows {
			if r.Amount > 0 {
				hasAmount = true
			}
			if r.TurnoverRate > 0 {
				hasTurnover = true
			}
			if hasAmount && hasTurnover {
				break
			}
		}
		if hasAmount {
			updateCols = append(updateCols, "amount")
		}
		if hasTurnover {
			updateCols = append(updateCols, "turnover_rate")
		}
		if err := common.DB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns(updateCols),
		}).CreateInBatches(dailyRows, 200).Error; err != nil && err != gorm.ErrEmptySlice {
			// 关键失败：daily_bars 落库失败必须上抛，否则历史初始化调用点会误把
			// 这只标 done、留下永久缺口（宽表/因子/回测都读不到它）。
			common.SysWarn("落库 daily_bars 失败 %s: %v", symbol, err)
			return fmt.Errorf("落库 daily_bars 失败 %s: %w", symbol, err)
		}
	}

	if len(calendarRows) > 0 {
		if err := common.DB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{"is_open"}),
		}).CreateInBatches(calendarRows, 200).Error; err != nil && err != gorm.ErrEmptySlice {
			// 日历是派生副产品（快照日历另有权威补写），失败不阻断标的落库判定，仅告警。
			common.SysWarn("落库 trading_calendar 失败 %s: %v", market, err)
		}
	}
	return nil
}
func (s *MarketService) persist(q *datasource.Quote) {
	if common.DB == nil {
		return
	}
	// upsert stock
	stock := model.Stock{Symbol: q.Symbol, Market: q.Market, Name: q.Name}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "updated_at"}),
	}).Create(&stock).Error; err != nil {
		common.SysWarn("落库 stock 失败 %s: %v", q.Symbol, err)
	}

	// upsert quote 快照
	quote := model.StockQuote{
		Symbol: q.Symbol, Market: q.Market, Price: q.Price, ChangePct: q.ChangePct,
		Open: q.Open, High: q.High, Low: q.Low, PrevClose: q.PrevClose,
		Volume: q.Volume, Amount: q.Amount, Source: q.Source, DataTime: q.DataTime,
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"price", "change_pct", "open", "high", "low", "prev_close",
			"volume", "amount", "source", "data_time", "updated_at",
		}),
	}).Create(&quote).Error; err != nil && err != gorm.ErrEmptySlice {
		common.SysWarn("落库 quote 失败 %s: %v", q.Symbol, err)
	}
}
