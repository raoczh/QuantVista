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
	Volume    int64     `json:"volume"` // 成交量，单位：手（各源统一口径，新浪原始为股、解析时 /100）
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
	Volume    int64   `json:"volume"` // 成交量，单位：手（各源统一口径，新浪原始为股、解析时 /100）
	Amount    float64 `json:"amount"`
	// TurnoverRate 当日换手率 %（筹码分布累积模型的核心输入）。仅东财日线自带（f61），
	// 新浪兜底恒 0；0=缺失，消费方（chip.go）需按流通股本推断兜底。
	TurnoverRate float64 `json:"turnover_rate,omitempty"`
	Source       string  `json:"source"` // 实际命中的数据源
}

// Min5Bar 5 分钟线单根（腾讯 mkline，M3b 盘中因子的数据源）。
// Time 为 bar 结束时刻（一天 48 根：0935 首根含集合竞价、1500 末根含收盘竞价）。
// 上游无成交额列，消费方以 量×典型价((O+H+L+C)/4) 估算。
type Min5Bar struct {
	Time   string  `json:"time"` // YYYYMMDDHHmm
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"` // 手
}

// SpotRow 全市场行情快照单行（东财 clist，M1 全市场日线的每日增量源）。
// 停牌股价格字段上游返回 "-"：解析为 0，但行保留（宇宙字典需要知道它存在），
// 消费方按 Price>0 && Volume>0 判定当日是否落 bar。
type SpotRow struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"` // 最新价（盘后=当日收盘）
	ChangePct    float64 `json:"change_pct"`
	Open         float64 `json:"open"`
	High         float64 `json:"high"`
	Low          float64 `json:"low"`
	PrevClose    float64 `json:"prev_close"` // f18 昨收（除权初筛锚点；停牌股也有值）
	Volume       int64   `json:"volume"`     // 手
	Amount       float64 `json:"amount"`
	TurnoverRate float64 `json:"turnover_rate"` // %
	// 估值与行业（P3b 扩展，f9/f115/f23/f100，2026-07-10 实测锚定）：估值字段亏损为负、
	// 缺失/停牌为 0，消费方按 >0 过滤；Industry 为东财行业板块名（与 m:90+t:2 行业板块
	// f14 名精确匹配），退市壳票上游给 "-" 已归一为空串。
	PE       float64 `json:"pe"`     // 市盈率（动，f9）
	PETTM    float64 `json:"pe_ttm"` // 市盈率（TTM，f115）
	PB       float64 `json:"pb"`     // 市净率（f23）
	Industry string  `json:"industry"`
	DataTime int64   `json:"data_time"` // f124 行情时间戳（unix 秒；停牌股为旧值）
}

// Fundamental 基本面摘要（骨架占位，阶段 4/5 扩展）。
type Fundamental struct {
	Symbol    string    `json:"symbol"`
	Market    string    `json:"market"`
	PE        float64   `json:"pe"`
	PB        float64   `json:"pb"`
	MarketCap float64   `json:"market_cap"`
	Source    string    `json:"source"`
	DataTime  time.Time `json:"data_time"`
}

// Valuation 估值/盘面扩展快照。腾讯行情串自带 PE/PB/市值/换手/振幅/量比/涨跌停价，
// 是免费可得的估值数据来源（快照指标，非三表财务明细）。字段缺失时为 0。
type Valuation struct {
	Symbol       string    `json:"symbol"`
	Market       string    `json:"market"`
	Name         string    `json:"name"`
	PETTM        float64   `json:"pe_ttm"`        // 市盈率 TTM（亏损/无值时为 0 或负）
	PEDynamic    float64   `json:"pe_dynamic"`    // 动态市盈率
	PEStatic     float64   `json:"pe_static"`     // 静态市盈率
	PB           float64   `json:"pb"`            // 市净率
	TotalCap     float64   `json:"total_cap"`     // 总市值（元）
	FloatCap     float64   `json:"float_cap"`     // 流通市值（元）
	TurnoverRate float64   `json:"turnover_rate"` // 换手率 %
	Amplitude    float64   `json:"amplitude"`     // 当日振幅 %
	VolumeRatio  float64   `json:"volume_ratio"`  // 量比
	LimitUp      float64   `json:"limit_up"`      // 涨停价
	LimitDown    float64   `json:"limit_down"`    // 跌停价
	IsST         bool      `json:"is_st"`         // 名称含 ST（风险警示）
	Source       string    `json:"source"`
	DataTime     time.Time `json:"data_time"`
}

// News 新闻条目（骨架占位）。
type News struct {
	Title   string    `json:"title"`
	URL     string    `json:"url"`
	Summary string    `json:"summary"`
	Source  string    `json:"source"`
	PubTime time.Time `json:"pub_time"`
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
	PE           float64 `json:"pe,omitempty"`        // 市盈率（新浪 per 口径，仅作行级过滤，不落 candidate 展示）
	PB           float64 `json:"pb,omitempty"`        // 市净率（新浪榜单自带；0=缺失）
	FloatCap     float64 `json:"float_cap,omitempty"` // 流通市值（元；新浪 nmc 万元已换算；0=缺失）
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

// BoardHeat 板块热度条目（东财 clist 行业/概念板块，M3c 热力图数据源）。
// 面积语义取 Amount（成交额）、颜色语义取 ChangePct；上涨/下跌家数与领涨股供 tooltip。
type BoardHeat struct {
	Code       string  `json:"code"`        // 板块代码，如 BK1201
	Name       string  `json:"name"`        // 板块名称
	ChangePct  float64 `json:"change_pct"`  // 涨跌幅 %
	Amount     float64 `json:"amount"`      // 成交额（元）
	Advances   int     `json:"advances"`    // 上涨家数
	Declines   int     `json:"declines"`    // 下跌家数
	Leader     string  `json:"leader"`      // 领涨股名
	LeaderCode string  `json:"leader_code"` // 领涨股代码（6 位）
	Source     string  `json:"source"`
}

// BoardStock 板块成分股条目（东财 clist，M3c 板块详情页成分表）。
// IsLeader/IsTopGainer 由 service 层按成交额/涨幅第一名标注，数据源解析时恒 false。
type BoardStock struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	ChangePct    float64 `json:"change_pct"`
	Amount       float64 `json:"amount"`        // 成交额（元）
	TurnoverRate float64 `json:"turnover_rate"` // 换手率 %
	TotalCap     float64 `json:"total_cap"`     // 总市值（元）
	FloatCap     float64 `json:"float_cap"`     // 流通市值（元）
	IsLeader     bool    `json:"is_leader"`     // 成交额第一名（板块龙头）
	IsTopGainer  bool    `json:"is_top_gainer"` // 涨幅第一名
	Source       string  `json:"source"`
}

// Breadth 市场涨跌家数/涨跌停统计（市场情绪核心指标）。
type Breadth struct {
	Advances  int       `json:"advances"`   // 上涨家数
	Declines  int       `json:"declines"`   // 下跌家数
	Unchanged int       `json:"unchanged"`  // 平盘家数
	LimitUp   int       `json:"limit_up"`   // 涨停家数
	LimitDown int       `json:"limit_down"` // 跌停家数
	TradeDate string    `json:"trade_date"` // YYYY-MM-DD
	Source    string    `json:"source"`
	DataTime  time.Time `json:"data_time"`
}

// MarketFundFlow 两市资金流（主力/超大单/大单/中单/小单净流入，单位元）。
type MarketFundFlow struct {
	TradeDate string    `json:"trade_date"` // YYYY-MM-DD
	MainNet   float64   `json:"main_net"`   // 主力净流入 = 超大单 + 大单
	SuperNet  float64   `json:"super_net"`  // 超大单净流入
	LargeNet  float64   `json:"large_net"`  // 大单净流入
	MediumNet float64   `json:"medium_net"` // 中单净流入
	SmallNet  float64   `json:"small_net"`  // 小单净流入
	Source    string    `json:"source"`
	DataTime  time.Time `json:"data_time"`
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

// RankingProvider 个股榜单能力。sort: "changepercent"/"amount"/"turnoverratio"/"pb"；
// asc=true 升序（跌幅榜/低PB榜等「不热」方向），false 降序（传统热度榜）。
type RankingProvider interface {
	GetStockRanking(ctx context.Context, market, sort string, asc bool, limit int) ([]StockRank, error)
}

// SectorProvider 板块涨跌榜能力。
type SectorProvider interface {
	GetSectorRanking(ctx context.Context, market string, limit int) ([]SectorRank, error)
}

// BreadthProvider 市场涨跌家数/涨跌停统计能力。
type BreadthProvider interface {
	GetBreadth(ctx context.Context, market string) (*Breadth, error)
}

// FundFlowProvider 两市资金流能力。
type FundFlowProvider interface {
	GetMarketFundFlow(ctx context.Context, market string) (*MarketFundFlow, error)
}

// TradingDaysProvider 交易日（开市日）序列能力，用于回填交易日历。
type TradingDaysProvider interface {
	GetTradingDays(ctx context.Context, market string, limit int) ([]string, error)
}

// BenchmarkBarsProvider 基准指数日线序列能力，用于推荐追踪的相对基准超额收益（alpha）。
// 返回基准名称与按日期升序的日线（含收盘）。cn 基准为上证指数。
type BenchmarkBarsProvider interface {
	GetBenchmarkBars(ctx context.Context, market string, limit int) (string, []Bar, error)
}

// ValuationProvider 估值/盘面扩展快照能力（腾讯行情自带估值字段）。
type ValuationProvider interface {
	GetValuation(ctx context.Context, market, symbol string) (*Valuation, error)
}
