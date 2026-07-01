package service

import (
	"context"
	"encoding/json"
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
}

func NewMarketService(mgr *datasource.Manager) *MarketService {
	return &MarketService{mgr: mgr}
}

const quoteCacheTTL = 10 * time.Second

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

// GetDailyBars 取日线序列（暂不缓存，数据量大且变动低频，后续可加）。
func (s *MarketService) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	bars, err := s.mgr.GetDailyBars(ctx, market, symbol, limit)
	if err != nil {
		return nil, err
	}
	s.persistDailyBars(market, symbol, bars)
	return bars, nil
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
		if r, err := s.mgr.GetStockRanking(ctx, market, "changepercent", 10); err == nil {
			ov.Gainers = r
		} else {
			setErr("gainers", err)
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := s.mgr.GetStockRanking(ctx, market, "amount", 10); err == nil {
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

	if b, err := json.Marshal(ov); err == nil {
		common.RedisSet(cacheKey, string(b), overviewCacheTTL)
	}
	return ov
}

func (s *MarketService) persistDailyBars(market, symbol string, bars []datasource.Bar) {
	if common.DB == nil || len(bars) == 0 {
		return
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
			Symbol:    symbol,
			Market:    market,
			TradeDate: b.TradeDate,
			Open:      b.Open,
			High:      b.High,
			Low:       b.Low,
			Close:     b.Close,
			Volume:    b.Volume,
			Amount:    b.Amount,
			Source:    source,
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
		if err := common.DB.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"open", "high", "low", "close", "volume", "amount", "source",
			}),
		}).CreateInBatches(dailyRows, 200).Error; err != nil && err != gorm.ErrEmptySlice {
			common.SysWarn("落库 daily_bars 失败 %s: %v", symbol, err)
		}
	}

	if len(calendarRows) > 0 {
		if err := common.DB.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{"is_open"}),
		}).CreateInBatches(calendarRows, 200).Error; err != nil && err != gorm.ErrEmptySlice {
			common.SysWarn("落库 trading_calendar 失败 %s: %v", market, err)
		}
	}
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
