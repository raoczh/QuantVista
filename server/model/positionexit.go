package model

import "time"

// PositionExitAssessment 是统一持仓卖出风险评估的追加式事实。
// positions 表不冗余最新等级；消费方始终按 position_id 取本表最新一条。
const (
	PositionExitLevelNormal  = "normal"
	PositionExitLevelWatch   = "watch"
	PositionExitLevelReview  = "review"
	PositionExitLevelUrgent  = "urgent"
	PositionExitLevelUnknown = "unknown"

	PositionExitSessionIntraday = "intraday"
	PositionExitSessionClose    = "close"

	PositionExitDataReady   = "ready"
	PositionExitDataPartial = "partial"
	PositionExitDataUnknown = "unknown"

	PositionExitAssessmentVersion = "pea1"
)

type PositionExitAssessment struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	UserID     int64  `gorm:"index;index:idx_pea_user_position" json:"user_id"`
	PositionID int64  `gorm:"index;index:idx_pea_user_position" json:"position_id"`
	Symbol     string `gorm:"size:16;index" json:"symbol"`
	Market     string `gorm:"size:8" json:"market"`
	Name       string `gorm:"size:64" json:"name"`

	TradeDate   string    `gorm:"size:10;index" json:"trade_date"`
	Session     string    `gorm:"size:12" json:"session"`
	EvaluatedAt time.Time `gorm:"index" json:"evaluated_at"`

	Level         string `gorm:"size:8;index" json:"level"`
	PrimarySignal string `gorm:"size:64" json:"primary_signal"`
	PrimaryReason string `gorm:"size:512" json:"primary_reason"`
	NextAction    string `gorm:"size:512" json:"next_action"`
	DataStatus    string `gorm:"size:12" json:"data_status"`
	Trend         string `gorm:"size:16" json:"trend"`

	QuoteAsOf       string  `gorm:"size:32" json:"quote_as_of"`
	BarsAsOf        string  `gorm:"size:10" json:"bars_as_of"`
	QuotePrice      float64 `gorm:"type:decimal(20,4)" json:"quote_price"`
	BuyPrice        float64 `gorm:"type:decimal(20,4)" json:"buy_price"`
	ProfitPct       float64 `gorm:"type:decimal(20,4)" json:"profit_pct"`
	PeakPrice       float64 `gorm:"type:decimal(20,4)" json:"peak_price"`
	PeakDrawdownPct float64 `gorm:"type:decimal(20,4)" json:"peak_drawdown_pct"`
	MA20            float64 `gorm:"type:decimal(20,4)" json:"ma20"`
	MA60            float64 `gorm:"type:decimal(20,4)" json:"ma60"`
	ATR14           float64 `gorm:"type:decimal(20,4)" json:"atr14"`
	ATRLine         float64 `gorm:"type:decimal(20,4)" json:"atr_line"`

	SignalsJSON       string `gorm:"type:text" json:"-"`
	EvidenceJSON      string `gorm:"type:text" json:"-"`
	DataGapsJSON      string `gorm:"type:text" json:"-"`
	AlertEventIDsJSON string `gorm:"type:text" json:"-"`
	SellReviewIDsJSON string `gorm:"type:text" json:"-"`
	ParamsJSON        string `gorm:"type:text" json:"-"`
	ParamsHash        string `gorm:"size:64" json:"params_hash"`
	FactHash          string `gorm:"size:64" json:"fact_hash"`
	EventKey          string `gorm:"size:64;uniqueIndex" json:"event_key"`
	Version           string `gorm:"size:16" json:"version"`

	ShouldTodo bool  `gorm:"default:false;index" json:"should_todo"`
	IsUpgrade  bool  `gorm:"default:false" json:"is_upgrade"`
	PreviousID int64 `gorm:"index" json:"previous_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
