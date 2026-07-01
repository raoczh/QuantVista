package model

import "time"

// StockScore 个股综合评分快照（每 symbol+market+交易日一条，覆盖更新）。
// 评分完全基于真实行情与技术指标计算（无财务/新闻），是技术面强弱的量化打分，非投资建议。
type StockScore struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Symbol    string `gorm:"size:16;index:idx_score_sym_date,unique" json:"symbol"`
	Market    string `gorm:"size:8;index:idx_score_sym_date,unique" json:"market"`
	TradeDate string `gorm:"size:10;index:idx_score_sym_date,unique" json:"trade_date"`

	Total    float64 `gorm:"type:decimal(6,2)" json:"total"`    // 综合分 0-100
	Trend    float64 `gorm:"type:decimal(6,2)" json:"trend"`    // 趋势
	Momentum float64 `gorm:"type:decimal(6,2)" json:"momentum"` // 动量
	Position float64 `gorm:"type:decimal(6,2)" json:"position"` // 位置
	Volume   float64 `gorm:"type:decimal(6,2)" json:"volume"`   // 量能
	Risk     float64 `gorm:"type:decimal(6,2)" json:"risk"`     // 风险（越高越稳）
	Label    string  `gorm:"size:16" json:"label"`
	Price    float64 `gorm:"type:decimal(20,4)" json:"price"`
	BarCount int     `json:"bar_count"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
