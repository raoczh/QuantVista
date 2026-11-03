package service

import (
	"context"
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
	mgr         *datasource.Manager
	cacheOnce   sync.Once
	cache       *marketResponseCache
	financeOnce sync.Once
	finance     *FinanceService
	// Online reads and historical maintenance share the same adjusted-only routing.
	// Tests can still inject a separate full-market source.
	wide wideSource
	// Quote validation uses an injectable clock without changing scheduler time.
	quoteNow func() time.Time
}

// wideSource 全市场链路依赖的能力子集（生产使用 Manager；测试可注入假源）。
type wideSource interface {
	GetCNSpotSnapshot(ctx context.Context) ([]datasource.SpotRow, error)
	GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error)
}

func NewMarketService(mgr *datasource.Manager) *MarketService {
	return &MarketService{mgr: mgr, wide: mgr}
}

const quoteCacheTTL = 10 * time.Second

// DataSourceHealth 各 (数据源,能力) 健康滑窗快照（管理端 GET /api/admin/datasources）。
func (s *MarketService) DataSourceHealth() []datasource.HealthStat {
	return s.mgr.HealthSnapshot()
}

// GetQuote 取实时行情：先查缓存，miss 则走数据源，成功后落库并回种缓存。
func (s *MarketService) GetQuote(ctx context.Context, market, symbol string) (*datasource.Quote, error) {
	if market == "cn" {
		quote, _, err := s.GetFreshQuote(ctx, market, symbol)
		return quote, err
	}
	return marketCachedJSON(ctx, s.responseCache(), "quote:v2:"+market+":"+symbol, quoteCacheTTL,
		func(fetchContext context.Context) (*datasource.Quote, error) {
			quote, err := s.mgr.GetQuote(fetchContext, market, symbol)
			if err == nil {
				s.persist(fetchContext, quote)
			}
			return quote, err
		})
}

const dailyBarsCacheTTL = 10 * time.Minute

// GetFreshQuote 取「新鲜行情」：读缓存/上游结果后按交易日历与交易时段校验 DataTime，
// 交易时段内过期则继续尝试下一数据源（不把旧行情当成功），全部源都过期则返回最新候选并
// 标注 stale。返回 行情、新鲜度信息、错误。A 股 GetQuote 同样经此链路并共用短缓存。
// 非 cn 市场无交易日历判定，降级为普通 GetQuote 并标 unknown。
func (s *MarketService) GetFreshQuote(ctx context.Context, market, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
	if market != "cn" {
		q, err := s.GetQuote(ctx, market, symbol)
		return q, quoteFreshInfo{Status: freshStatusUnknown, MarketState: marketStateClosed}, err
	}

	judge := s.freshnessJudge(ctx, market)
	accept := func(q *datasource.Quote) bool {
		return q != nil && judge(q.DataTime).Status == freshStatusFresh
	}
	info := func(q *datasource.Quote) quoteFreshInfo {
		if q == nil {
			return judge(time.Time{})
		}
		return judge(q.DataTime)
	}

	quote, err := marketCachedJSON(ctx, s.responseCache(), "quote:v2:"+market+":"+symbol, quoteCacheTTL,
		func(fetchContext context.Context) (*datasource.Quote, error) {
			candidate, _, fetchErr := s.mgr.GetQuoteFresh(fetchContext, market, symbol, accept)
			if fetchErr == nil {
				s.persist(fetchContext, candidate)
			}
			return candidate, fetchErr
		})
	if err != nil {
		return nil, info(nil), err
	}
	return quote, info(quote), nil
}

// QuoteFreshnessOf 对一条已取得的行情按当前时刻做时效判定——提醒/守护/模拟成交/追踪/
// 推荐等「旧价会造成误动作」的消费链路共用此入口（统一新鲜度判定，别再各自比对日期）。
// cn 按交易日历+交易时段判定；其余市场无日历，返回 unknown（调用方 fail-closed 处理）。
func (s *MarketService) QuoteFreshnessOf(market string, dataTime time.Time) quoteFreshInfo {
	return s.FreshnessJudge(market)(dataTime)
}

// FreshnessJudge 返回某市场的批量时效判定函数：日历/时段只查一次，逐条判定为纯计算
// ——列表富化（自选/ETF/持仓等展示行统一过期徽标）对每行打 freshness_status 用它，
// 避免每行重复查交易日历。
func (s *MarketService) FreshnessJudge(market string) func(dataTime time.Time) quoteFreshInfo {
	return s.freshnessJudge(context.Background(), market)
}

// FreshQuoteResult 批量新鲜行情的单只结果（Quote 可为 nil=拉取失败）。
type FreshQuoteResult struct {
	Quote *datasource.Quote
	Fresh quoteFreshInfo
}

// FreshQuotesFor 并发批量取「新鲜行情」：单只走 GetFreshQuote 的全源换源逻辑（主源
// 旧价不当成功，逐源找当前有效行情），stale 候选照常返回并带状态，由调用方决定
// fail-closed 策略。守护/推荐终选名单等高风险链路用它替代 QuotesFor。
func (s *MarketService) FreshQuotesFor(ctx context.Context, refs []QuoteRef) map[string]FreshQuoteResult {
	out := make(map[string]FreshQuoteResult, len(refs))
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
			q, fi, err := s.GetFreshQuote(ctx, ref.Market, ref.Symbol)
			if err != nil {
				return
			}
			mu.Lock()
			out[QuoteKey(ref.Market, ref.Symbol)] = FreshQuoteResult{Quote: q, Fresh: fi}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// QuoteFreshnessView 行情响应统一携带的新鲜度块（全项目契约字段）：采集时刻、数据源
// 行情时刻、期望交易日、时段、状态与过期原因。前端/AI 消费方据此区分「刚请求成功」
// 与「数据仍然有效」。
type QuoteFreshnessView struct {
	CapturedAt      string `json:"captured_at"`                // 本次采集（请求）时刻
	SourceDataTime  string `json:"source_data_time,omitempty"` // 数据源行情时刻；空=源未返回时间（timestamp_unknown）
	ExpectedAsOf    string `json:"expected_as_of,omitempty"`   // 期望的最近有效行情交易日
	Source          string `json:"source,omitempty"`
	MarketState     string `json:"market_state"`
	FreshnessStatus string `json:"freshness_status"` // fresh | stale | unknown
	StaleReason     string `json:"stale_reason,omitempty"`
}

// FreshnessView 组装一条行情的新鲜度视图（q 为 nil 时按 stale 组装）。
func (s *MarketService) FreshnessView(market string, q *datasource.Quote) *QuoteFreshnessView {
	v := &QuoteFreshnessView{CapturedAt: time.Now().In(datasource.ChinaMarketLocation()).Format("2006-01-02 15:04:05")}
	var dataTime time.Time
	if q != nil {
		dataTime = q.DataTime
		v.Source = q.Source
	}
	fi := s.QuoteFreshnessOf(market, dataTime)
	v.ExpectedAsOf = fi.ExpectedDate
	v.MarketState = fi.MarketState
	v.FreshnessStatus = fi.Status
	if !dataTime.IsZero() {
		v.SourceDataTime = dataTime.In(datasource.ChinaMarketLocation()).Format("2006-01-02 15:04:05")
	}
	if fi.Status != freshStatusFresh {
		if note, _ := stockFreshnessNote(fi, dataTime); note != "" {
			v.StaleReason = note
		}
	}
	return v
}

// GetDailyBars 取日线序列。加 10 分钟缓存：推荐评分/个股分析/对比/评分卡会在短时间内
// 反复拉同一批标的的日线（收盘后日线不变、盘中仅末根在动），缓存显著降低对免费源的压力。
func (s *MarketService) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	if limit <= 0 {
		limit = 120
	}
	limit = min(limit, 1023)
	// Chart, indicators and chips request different tails of the same 250-day history.
	// One canonical window also supplies enough overlap for adjustment validation.
	fetchLimit := max(limit, wideBarLimit)
	policy := currentDailyBarPolicy(market, time.Now())
	cacheKey := fmt.Sprintf("bars:qfq-v2:%s:%s:%d:%s", market, symbol, fetchLimit, policy.cacheScope)
	bars, err := marketCachedJSON(ctx, s.responseCache(), cacheKey, policy.cacheTTL,
		func(fetchContext context.Context) ([]datasource.Bar, error) {
			series, _, fetchErr := s.mgr.GetDailyBarsFresh(fetchContext, market, symbol, fetchLimit, policy.expectedDate)
			if fetchErr != nil {
				return nil, fetchErr
			}
			if validationErr := validateAdjustedBars(market, series); validationErr != nil {
				return nil, validationErr
			}
			if persistenceErr := s.persistDailyBars(fetchContext, market, symbol, series); persistenceErr != nil {
				common.SysWarn("日线缓存持久化失败 %s.%s: %v", market, symbol, persistenceErr)
			}
			return series, nil
		})
	if err != nil {
		return nil, err
	}
	if err := validateAdjustedBars(market, bars); err != nil {
		return nil, err
	}
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}
	return bars, nil
}

const rankingCacheTTL = 60 * time.Second

// GetRanking 取个股榜单（sort: changepercent/amount/turnoverratio/pb；asc=true 升序），60s 缓存。
// 供推荐候选池多源建池使用（首页概览走 GetOverview 的 10 条口径，互不影响）。
func (s *MarketService) GetRanking(ctx context.Context, market, sort string, asc bool, limit int) ([]datasource.StockRank, error) {
	cacheKey := fmt.Sprintf("ranking:%s:%s:%v:%d", market, sort, asc, limit)
	return marketCachedJSON(ctx, s.responseCache(), cacheKey, rankingCacheTTL,
		func(fetchContext context.Context) ([]datasource.StockRank, error) {
			return s.mgr.GetStockRanking(fetchContext, market, sort, asc, limit)
		})
}

const valuationCacheTTL = 60 * time.Second

// GetValuation 估值/盘面扩展快照（PE/PB/市值/换手/涨跌停等，腾讯免费源）。
// 变化低频，缓存 60s；读时取用不落库。
func (s *MarketService) GetValuation(ctx context.Context, market, symbol string) (*datasource.Valuation, error) {
	cacheKey := "valuation:" + market + ":" + symbol
	return marketCachedJSON(ctx, s.responseCache(), cacheKey, valuationCacheTTL,
		func(fetchContext context.Context) (*datasource.Valuation, error) {
			return s.mgr.GetValuation(fetchContext, market, symbol)
		})
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
	name, bars, err := s.mgr.GetBenchmarkBars(ctx, market, limit)
	if err == nil {
		persistCompletedBenchmark(ctx, market, bars, time.Now())
	}
	return name, bars, err
}

// QuoteRef 批量取行情的标的引用。
type QuoteRef struct {
	Market string
	Symbol string
}

// QuoteKey 批量结果的 map 键。
func QuoteKey(market, symbol string) string { return market + ":" + symbol }

// QuotesFor 并发批量取行情（复用单只 GetQuote 的缓存），单只失败只是缺席不影响其余。
// 用于自选/持仓列表富化现价等**展示场景**（行携带 data_time 供用户自判）；提醒/守护/
// 模拟成交/推荐等旧价会造成误动作的链路必须改用 FreshQuotesFor + 新鲜度判定 fail-closed。
// 并发上限避免瞬时打爆数据源。
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
	overview, err := marketCachedJSON(ctx, s.responseCache(), cacheKey, overviewCacheTTL,
		func(fetchContext context.Context) (*Overview, error) {
			result := s.fetchOverview(fetchContext, market)
			return result, fetchContext.Err()
		})
	if err != nil {
		return &Overview{Errors: map[string]string{"market": err.Error()}, DataTime: time.Now()}
	}
	return overview
}

func (s *MarketService) fetchOverview(ctx context.Context, market string) *Overview {
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
		if r, err := s.GetRanking(ctx, market, "changepercent", false, 10); err == nil {
			ov.Gainers = r
		} else {
			setErr("gainers", err)
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := s.GetRanking(ctx, market, "amount", false, 10); err == nil {
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
	return ov
}

var errUnadjustedBars = errors.New("日线包含新浪不复权历史，前复权序列需完整重建后才能用于分析")

func validateAdjustedBars(market string, bars []datasource.Bar) error {
	if market == "cn" {
		for _, bar := range bars {
			if bar.Source == "sina" {
				return errUnadjustedBars
			}
		}
	}
	return nil
}

func validateLocalAdjustedBars(market string, bars []model.DailyBar) error {
	if market == "cn" {
		for _, bar := range bars {
			if bar.Source == "sina" {
				return errUnadjustedBars
			}
		}
	}
	return nil
}

func (s *MarketService) persistDailyBars(ctx context.Context, market, symbol string, bars []datasource.Bar) error {
	return s.persistDailyBarsWithState(ctx, market, symbol, bars, false)
}

func (s *MarketService) persistDailyBarsWithState(ctx context.Context, market, symbol string, bars []datasource.Bar, initialized bool) error {
	return s.persistDailyBarsChecked(ctx, market, symbol, bars, bars, initialized)
}

// 写入计划可以只包含缺口，但复权校验必须保留上游返回的完整窗口作为重叠锚点。
func (s *MarketService) persistDailyBarsChecked(ctx context.Context, market, symbol string, bars, reference []datasource.Bar, initialized bool) error {
	if common.DB == nil || len(bars) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// daily_bars 是选股、回测和追踪共用的前复权地基。新浪日线无复权参数，
	// 既不能覆盖已有窗口，也不能补缺口后与东财窗口拼接。在线读取也使用相同校验。
	if err := validateAdjustedBars(market, bars); err != nil {
		return err
	}
	if err := validateAdjustedBars(market, reference); err != nil {
		return err
	}
	err := withDailyBarWriteTx(ctx, market, []string{symbol}, func(tx *gorm.DB) error {
		if err := s.persistDailyBarsTx(ctx, tx, market, symbol, bars, reference); err != nil {
			return err
		}
		var state struct {
			Count int
			Last  string
		}
		if err := tx.Model(&model.DailyBar{}).Select("COUNT(*) AS count, COALESCE(MAX(trade_date), '') AS last").
			Where("market = ? AND symbol = ?", market, symbol).Scan(&state).Error; err != nil {
			return err
		}
		// 在线读取/有限窗口补采也会推进实际日线，已有宇宙水位必须与之原子更新。
		// 只有明确完成全历史初始化的调用才能升级 init_status。
		updates := map[string]any{"bars_count": state.Count, "last_bar_date": state.Last}
		if initialized {
			updates["init_status"], updates["fail_count"], updates["last_error"] = "done", 0, ""
		}
		return tx.Model(&model.MarketSyncState{}).Where("market = ? AND symbol = ?", market, symbol).Updates(updates).Error
	})
	if errors.Is(err, errBarBasisUnverified) && ctx.Err() == nil && !errors.Is(err, errRebaseInProgress) {
		err = errors.Join(err, s.markStatePending(ctx, market, symbol))
	}
	if err == nil && initialized {
		s.invalidateDailyBars(market, symbol)
	}
	return err
}

// 调用方已持有同一标的的事务锁；历史校验与窗口写入必须共用 db。
func (s *MarketService) persistDailyBarsTx(ctx context.Context, db *gorm.DB, market, symbol string, bars, reference []datasource.Bar) error {
	// M1 除权检测（防"部分窗口重写"漏检）：本次拉取若与 DB 内已有窗口的 close 多点
	// 比对出现偏差，说明发生了除权/送转（东财前复权序列整体重锚）——此时只 upsert
	// 本窗会留下"窗口内新基准、窗口外旧基准"的断层。检测命中即全量重锚（250 根删+插）
	// 后直接返回。腾讯备用前复权历史也必须核验，不能借换源绕开重锚。
	if market == "cn" && len(reference) > 0 && (reference[0].Source == "eastmoney" || reference[0].Source == "tencent") {
		rebased, err := s.detectAndRebase(ctx, db, market, symbol, reference)
		if err != nil {
			return err
		}
		if rebased {
			return nil
		}
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
		// 腾讯前复权日线未提供成交额/换手率，本批全缺失时不覆盖已存真实值。
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
		if err := db.Clauses(clause.OnConflict{
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
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{"is_open"}),
		}).CreateInBatches(calendarRows, 200).Error; err != nil && err != gorm.ErrEmptySlice {
			// 日历是派生副产品（快照日历另有权威补写），失败不阻断标的落库判定，仅告警。
			common.SysWarn("落库 trading_calendar 失败 %s: %v", market, err)
		}
	}
	return nil
}
func (s *MarketService) persist(ctx context.Context, q *datasource.Quote) {
	if common.DB == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	db := common.DB.WithContext(ctx)
	// upsert stock
	stock := model.Stock{Symbol: q.Symbol, Market: q.Market, Name: q.Name}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "updated_at"}),
	}).Create(&stock).Error; err != nil {
		common.SysWarn("落库 stock 失败 %s: %v", q.Symbol, err)
	}

	// upsert quote 快照。DataTime 零值（三源都未返回行情时间，timestamp_unknown）时以
	// epoch 哨兵落库：列非空且 MySQL 严格模式拒绝零日期；1970 显然非真实行情时间，任何
	// 日期比对都会按过期处理，不会像回填「现在」那样被伪装成实时价。
	dataTime := q.DataTime
	if dataTime.IsZero() {
		dataTime = time.Unix(0, 0)
	}
	quote := model.StockQuote{
		Symbol: q.Symbol, Market: q.Market, Price: q.Price, ChangePct: q.ChangePct,
		Open: q.Open, High: q.High, Low: q.Low, PrevClose: q.PrevClose,
		Volume: q.Volume, Amount: q.Amount, Source: q.Source, DataTime: dataTime, UpdatedAt: time.Now(),
	}
	updates := clause.Assignments(map[string]any{
		"price": quote.Price, "change_pct": quote.ChangePct, "open": quote.Open, "high": quote.High,
		"low": quote.Low, "prev_close": quote.PrevClose, "volume": quote.Volume, "amount": quote.Amount,
		"source": quote.Source, "data_time": quote.DataTime, "updated_at": quote.UpdatedAt,
	})
	// 原子比较数据源时间。慢请求或另一来源的旧值不能把“最近已知行情”向后覆盖；
	// 零时间的 epoch 哨兵仍可用于首次落库，但不能替代已有有效时间。
	for i := range updates {
		updates[i].Value = gorm.Expr("CASE WHEN data_time IS NULL OR data_time <= ? THEN ? ELSE ? END",
			dataTime, updates[i].Value, clause.Column{Name: updates[i].Column.Name})
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}},
		DoUpdates: updates,
	}).Create(&quote).Error; err != nil && err != gorm.ErrEmptySlice {
		common.SysWarn("落库 quote 失败 %s: %v", q.Symbol, err)
	}
}
