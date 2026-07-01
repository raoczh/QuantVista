package model

import "time"

// Stock 股票基础信息（多市场）。
type Stock struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Symbol    string    `gorm:"size:16;index:idx_symbol_market,unique" json:"symbol"` // 如 600000
	Market    string    `gorm:"size:8;index:idx_symbol_market,unique" json:"market"`  // cn/us/hk
	Name      string    `gorm:"size:64" json:"name"`
	Industry  string    `gorm:"size:64" json:"industry"`
	Currency  string    `gorm:"size:8;default:CNY" json:"currency"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StockQuote 最新行情快照（每 symbol+market 一条，覆盖更新）。
// 价格用 decimal 列类型，避免浮点累计误差（骨架以 float64 承载，后续可换 decimal 库）。
type StockQuote struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Symbol    string    `gorm:"size:16;index:idx_q_symbol_market,unique" json:"symbol"`
	Market    string    `gorm:"size:8;index:idx_q_symbol_market,unique" json:"market"`
	Price     float64   `gorm:"type:decimal(20,4)" json:"price"`
	ChangePct float64   `gorm:"type:decimal(10,4)" json:"change_pct"`
	Open      float64   `gorm:"type:decimal(20,4)" json:"open"`
	High      float64   `gorm:"type:decimal(20,4)" json:"high"`
	Low       float64   `gorm:"type:decimal(20,4)" json:"low"`
	PrevClose float64   `gorm:"type:decimal(20,4)" json:"prev_close"`
	Volume    int64     `json:"volume"`
	Amount    float64   `gorm:"type:decimal(24,4)" json:"amount"`
	Source    string    `gorm:"size:16" json:"source"` // 数据来源：eastmoney/sina
	DataTime  time.Time `json:"data_time"`             // 数据时间，AI 分析需明确告知
	UpdatedAt time.Time `json:"updated_at"`
}

// DailyBar 日线 OHLC，供追踪/回撤/复权计算复用。
type DailyBar struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Symbol    string    `gorm:"size:16;index:idx_bar_symbol_date,unique" json:"symbol"`
	Market    string    `gorm:"size:8;index:idx_bar_symbol_date,unique" json:"market"`
	TradeDate string    `gorm:"size:10;index:idx_bar_symbol_date,unique" json:"trade_date"` // YYYY-MM-DD
	Open      float64   `gorm:"type:decimal(20,4)" json:"open"`
	High      float64   `gorm:"type:decimal(20,4)" json:"high"`
	Low       float64   `gorm:"type:decimal(20,4)" json:"low"`
	Close     float64   `gorm:"type:decimal(20,4)" json:"close"`
	Volume    int64     `json:"volume"`
	Amount    float64   `gorm:"type:decimal(24,4)" json:"amount"`
	Source    string    `gorm:"size:16" json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// TradingCalendar 交易日历，用于按交易日计算有效期、持有周期和数据新鲜度。
type TradingCalendar struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Market    string `gorm:"size:8;index:idx_calendar_market_date,unique" json:"market"`
	TradeDate string `gorm:"size:10;index:idx_calendar_market_date,unique" json:"trade_date"` // YYYY-MM-DD
	// 不设 gorm default:true——GORM 会把零值(false)当作"用默认值"而从 INSERT 中省略，
	// 导致回填的休市日被 DB 默认值写成 true。去掉 default，false 才会被真实写入。
	IsOpen    bool      `json:"is_open"`
	CreatedAt time.Time `json:"created_at"`
}

// MarketSnapshot 市场情绪快照：涨跌家数、涨跌停。
// 定时落库形成历史序列，供首页情绪卡片与后续 AI 市场分析复用（明确带来源与数据时间）。
type MarketSnapshot struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Market    string    `gorm:"size:8;index:idx_snap_market_time" json:"market"`
	TradeDate string    `gorm:"size:10;index:idx_snap_market_date" json:"trade_date"` // YYYY-MM-DD，快照对应的交易日
	Advances  int       `json:"advances"`                                             // 上涨家数
	Declines  int       `json:"declines"`                                             // 下跌家数
	Unchanged int       `json:"unchanged"`                                            // 平盘家数
	LimitUp   int       `json:"limit_up"`                                             // 涨停家数
	LimitDown int       `json:"limit_down"`                                           // 跌停家数
	Source    string    `gorm:"size:16" json:"source"`
	DataTime  time.Time `gorm:"index:idx_snap_market_time" json:"data_time"`
	CreatedAt time.Time `json:"created_at"`
}

// DataSyncLog 数据同步任务审计：记录批量日线同步/日历回填等后台任务的执行结果。
// 便于排查数据缺口与数据源限流（对应 phase0 review P1#3 的 data_sync_logs 缺口）。
type DataSyncLog struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	Task       string    `gorm:"size:32;index" json:"task"` // sync_daily_bars / backfill_calendar / snapshot_market
	Market     string    `gorm:"size:8" json:"market"`
	Status     string    `gorm:"size:16" json:"status"`   // success / partial / failed
	Total      int       `json:"total"`                   // 计划处理条目数
	Succeeded  int       `json:"succeeded"`               // 成功条目数
	Failed     int       `json:"failed"`                  // 失败条目数
	DurationMs int64     `json:"duration_ms"`             // 耗时（毫秒）
	Message    string    `gorm:"size:512" json:"message"` // 摘要或首个错误
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}
