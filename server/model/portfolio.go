package model

import "time"

// 自选股与持仓：均按 user_id 隔离，标的用 symbol+market 自然键（与数据源/行情一致，
// 不依赖 stocks 表主键；stocks 表在查询时惰性 upsert，这里冗余 name 便于无行情时展示）。

// Watchlist 自选股分组。
type Watchlist struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	UserID    int64     `gorm:"index" json:"user_id"`
	Name      string    `gorm:"size:64" json:"name"`
	SortOrder int       `gorm:"default:0" json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 机会池漏斗阶段：自选条目的研究进度（空 = 未标注，兼容旧数据）。
// 漏斗：发现 → 初筛 → 重点观察 → 等待价格 → 已生成计划 → 已买入；
// 任一阶段可转 已放弃（记录当时价格与原因，供「错过机会」复盘）/ 已复盘。
const (
	StageDiscovered   = "discovered"
	StageScreening    = "screening"
	StageWatching     = "watching"
	StageWaitingPrice = "waiting_price"
	StagePlanned      = "planned"
	StageBought       = "bought"
	StagePassed       = "passed"
	StageReviewed     = "reviewed"
)

// WatchlistItem 自选股条目。唯一约束 user_id+watchlist_id+symbol+market，避免同组重复添加。
type WatchlistItem struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	UserID      int64     `gorm:"index;index:idx_wli_uniq,unique" json:"user_id"`
	WatchlistID int64     `gorm:"index;index:idx_wli_uniq,unique" json:"watchlist_id"`
	Symbol      string    `gorm:"size:16;index:idx_wli_uniq,unique" json:"symbol"`
	Market      string    `gorm:"size:8;index:idx_wli_uniq,unique" json:"market"`
	Name        string    `gorm:"size:64" json:"name"`
	Note        string    `gorm:"size:512" json:"note"`           // 备注
	FocusReason string    `gorm:"size:512" json:"focus_reason"`   // 关注原因
	IsPinned    bool      `gorm:"default:false" json:"is_pinned"` // 重点关注

	ResearchStage string     `gorm:"size:16;index:idx_wli_stage" json:"research_stage"` // 机会池漏斗阶段
	PassedReason  string     `gorm:"size:255" json:"passed_reason"`                     // 放弃原因（stage=passed）
	PassedPrice   float64    `gorm:"type:decimal(20,4)" json:"passed_price"`            // 放弃时价格（错过机会复盘基准）
	StageAt       *time.Time `json:"stage_at"`                                          // 阶段变更时间

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 持仓类型与状态。
const (
	PositionTypeShortTerm = "short_term"
	PositionTypeLongTerm  = "long_term"

	PositionStatusHolding = "holding"
	PositionStatusClosed  = "closed"
)

// Position 已购入持仓。买入必填 buy_price/buy_date/quantity；卖出后填 sell_* 与复盘。
// 盈亏在读取时用实时行情计算，不落库快照（保证展示始终最新）。
type Position struct {
	ID           int64  `gorm:"primaryKey" json:"id"`
	UserID       int64  `gorm:"index" json:"user_id"`
	Symbol       string `gorm:"size:16;index" json:"symbol"`
	Market       string `gorm:"size:8" json:"market"`
	Name         string `gorm:"size:64" json:"name"`
	PositionType string `gorm:"size:16;default:long_term" json:"position_type"` // short_term / long_term
	Status       string `gorm:"size:16;default:holding" json:"status"`          // holding / closed
	Currency     string `gorm:"size:8;default:CNY" json:"currency"`

	BuyPrice float64 `gorm:"type:decimal(20,4)" json:"buy_price"`
	BuyDate  string  `gorm:"size:10" json:"buy_date"` // YYYY-MM-DD
	Quantity float64 `gorm:"type:decimal(20,4)" json:"quantity"`
	BuyFee   float64 `gorm:"type:decimal(20,4)" json:"buy_fee"`
	BuyTax   float64 `gorm:"type:decimal(20,4)" json:"buy_tax"`

	BuyReason string `gorm:"size:512" json:"buy_reason"`
	UserNote  string `gorm:"size:512" json:"user_note"`

	SellPrice  float64 `gorm:"type:decimal(20,4)" json:"sell_price"`
	SellDate   string  `gorm:"size:10" json:"sell_date"`
	SellFee    float64 `gorm:"type:decimal(20,4)" json:"sell_fee"`
	SellTax    float64 `gorm:"type:decimal(20,4)" json:"sell_tax"`
	SellReason string  `gorm:"size:512" json:"sell_reason"`
	ReviewNote string  `gorm:"size:512" json:"review_note"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
