package datasource

import (
	"context"
	"time"
)

// 内部标准数据结构：上层（缓存、AI、追踪）只依赖这些结构，不感知具体数据源。
// 新增数据源 = 新增一个 Adapter 实现，不改上层（见 docs/ARCHITECTURE.md 5.2）。

// Quote 实时行情快照。
type Quote struct {
	Symbol    string    `json:"symbol"`
	Market    string    `json:"market"`
	Name      string    `json:"name"`
	Price     float64   `json:"price"`
	ChangePct float64   `json:"change_pct"`
	Open      float64   `json:"open"`
	High      float64   `json:"high"`
	Low       float64   `json:"low"`
	PrevClose float64   `json:"prev_close"`
	Volume    int64     `json:"volume"`
	Amount    float64   `json:"amount"`
	Source    string    `json:"source"`    // 实际命中的数据源
	DataTime  time.Time `json:"data_time"` // 数据时间，务必随数据透传
}

// Bar 日线 OHLC。
type Bar struct {
	TradeDate string  `json:"trade_date"` // YYYY-MM-DD
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	Volume    int64   `json:"volume"`
	Amount    float64 `json:"amount"`
}

// Fundamental 基本面摘要（骨架占位，阶段 4/5 扩展）。
type Fundamental struct {
	Symbol   string    `json:"symbol"`
	Market   string    `json:"market"`
	PE       float64   `json:"pe"`
	PB       float64   `json:"pb"`
	MarketCap float64  `json:"market_cap"`
	Source   string    `json:"source"`
	DataTime time.Time `json:"data_time"`
}

// News 新闻条目（骨架占位）。
type News struct {
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Summary  string    `json:"summary"`
	Source   string    `json:"source"`
	PubTime  time.Time `json:"pub_time"`
}

// Index 指数行情（市场首页指数概览用）。
type Index struct {
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	Price     float64   `json:"price"`
	ChangePct float64   `json:"change_pct"`
	Open      float64   `json:"open"`
	High      float64   `json:"high"`
	Low       float64   `json:"low"`
	PrevClose float64   `json:"prev_close"`
	Source    string    `json:"source"`
	DataTime  time.Time `json:"data_time"`
}

// StockRank 榜单条目（涨幅榜/成交额榜等）。
type StockRank struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	ChangePct    float64 `json:"change_pct"`
	Amount       float64 `json:"amount"`
	TurnoverRate float64 `json:"turnover_rate"`
	Source       string  `json:"source"`
}

// SectorRank 板块涨跌榜条目。
type SectorRank struct {
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	ChangePct float64 `json:"change_pct"`
	Leader    string  `json:"leader"` // 领涨股
	Source    string  `json:"source"`
}

// CNIndex A 股主要指数清单（code 用于展示，sina 为新浪批量代码）。
type CNIndex struct {
	Code string
	Sina string
	Name string
}

// CNIndices 市场首页展示的主要指数。
var CNIndices = []CNIndex{
	{"000001", "sh000001", "上证指数"},
	{"399001", "sz399001", "深证成指"},
	{"399006", "sz399006", "创业板指"},
	{"000300", "sh000300", "沪深300"},
	{"000688", "sh000688", "科创50"},
	{"000905", "sh000905", "中证500"},
}

// Adapter 数据源适配器接口。MVP 先实现东财，新浪做备份/校验。
// 未实现的能力返回 ErrNotSupported，由上层决定降级策略。
type Adapter interface {
	// Name 数据源标识，如 "eastmoney"、"sina"。
	Name() string
	// GetQuote 拉取单只股票实时行情快照。
	GetQuote(ctx context.Context, market, symbol string) (*Quote, error)
	// GetDailyBars 拉取日线序列，limit<=0 表示由实现决定默认条数。
	GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error)
}

// 以下为可选能力接口：adapter 按擅长按需实现，manager 按能力路由。
// 不强制每个源都实现全部市场概览能力（新浪擅长指数/榜单，东财擅长板块 clist）。

// IndexProvider 指数批量行情能力。
type IndexProvider interface {
	GetIndices(ctx context.Context, market string) ([]Index, error)
}

// RankingProvider 个股榜单能力（sort: "changepercent" / "amount"）。
type RankingProvider interface {
	GetStockRanking(ctx context.Context, market, sort string, limit int) ([]StockRank, error)
}

// SectorProvider 板块涨跌榜能力。
type SectorProvider interface {
	GetSectorRanking(ctx context.Context, market string, limit int) ([]SectorRank, error)
}
