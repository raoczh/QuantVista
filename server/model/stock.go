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
	Source    string    `gorm:"size:16" json:"source"`    // 数据来源：eastmoney/sina
	DataTime  time.Time `json:"data_time"`                // 数据时间，AI 分析需明确告知
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
