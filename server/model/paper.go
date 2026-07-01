package model

import "time"

// 模拟交易（纸上交易）：每用户一个虚拟账户，用真实行情成交与估值，练习不承担真实风险。
const (
	PaperSideBuy  = "buy"
	PaperSideSell = "sell"

	PaperDefaultCash = 100000 // 默认初始资金
)

// PaperAccount 用户的模拟账户（每用户一个）。
type PaperAccount struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	UserID      int64     `gorm:"uniqueIndex" json:"user_id"`
	InitialCash float64   `gorm:"type:decimal(20,2)" json:"initial_cash"`
	Cash        float64   `gorm:"type:decimal(20,2)" json:"cash"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PaperHolding 模拟账户内的聚合持仓（每 user+symbol+market 一条）。
// AvgCost 为含买入手续费的每股成本基，便于卖出时算真实净盈亏。
type PaperHolding struct {
	ID       int64   `gorm:"primaryKey" json:"id"`
	UserID   int64   `gorm:"index;index:idx_ph_uniq,unique" json:"user_id"`
	Symbol   string  `gorm:"size:16;index:idx_ph_uniq,unique" json:"symbol"`
	Market   string  `gorm:"size:8;index:idx_ph_uniq,unique" json:"market"`
	Name     string  `gorm:"size:64" json:"name"`
	Quantity float64 `gorm:"type:decimal(20,4)" json:"quantity"`
	AvgCost  float64 `gorm:"type:decimal(20,4)" json:"avg_cost"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PaperTrade 模拟成交流水。
type PaperTrade struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index:idx_pt_user" json:"user_id"`
	Symbol string `gorm:"size:16" json:"symbol"`
	Market string `gorm:"size:8" json:"market"`
	Name   string `gorm:"size:64" json:"name"`
	Side   string `gorm:"size:8" json:"side"` // buy / sell

	Price       float64 `gorm:"type:decimal(20,4)" json:"price"`
	Quantity    float64 `gorm:"type:decimal(20,4)" json:"quantity"`
	Amount      float64 `gorm:"type:decimal(20,2)" json:"amount"` // price*qty
	Fee         float64 `gorm:"type:decimal(20,2)" json:"fee"`
	Tax         float64 `gorm:"type:decimal(20,2)" json:"tax"`
	RealizedPnl float64 `gorm:"type:decimal(20,2)" json:"realized_pnl"` // 卖出时的已实现盈亏（净）

	CreatedAt time.Time `json:"created_at"`
}
