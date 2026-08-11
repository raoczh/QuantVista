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

// PositionExitOutcome 是卖出评估的前向结果台账（问题 1 补强）：pea1 的全部阈值
//（ATR 倍数、MA 周期、共振升级规则）目前是保守拍定的基线，没有后验测量就永远
// 无法基于证据调参。每条盘后（close）评估在 T+H 成熟后回填一次前向收益/最大
// 不利与有利偏移；normal 级同样回填，作为「有信号 vs 无信号」的对照分母。
// 纯测量、零 LLM、只写一次（幂等），不改任何生产提醒行为。
//
// 口径：BasePrice 与 T+H 收盘取自**同一条当前前复权日线序列**（T 日 close），
// 避免评估时实价与后来被除权重锚重写的历史序列错位；窗口内含除权时两端同源，
// 收益内部一致。这是研究口径，不代表真实可成交价。
type PositionExitOutcome struct {
	ID           int64 `gorm:"primaryKey" json:"id"`
	AssessmentID int64 `gorm:"not null;uniqueIndex:idx_peo_uniq,priority:1" json:"assessment_id"`
	Horizon      int   `gorm:"not null;uniqueIndex:idx_peo_uniq,priority:2" json:"horizon"` // 前向交易日数（5/10）

	UserID     int64  `gorm:"not null;index" json:"user_id"`
	PositionID int64  `gorm:"not null;index" json:"position_id"`
	Symbol     string `gorm:"size:16;not null;index" json:"symbol"`
	Market     string `gorm:"size:8;not null" json:"market"`
	TradeDate  string `gorm:"size:10;not null;index" json:"trade_date"` // 评估交易日 T

	Level         string `gorm:"size:8;not null;index" json:"level"`
	PrimarySignal string `gorm:"size:64;not null;index" json:"primary_signal"`
	ParamsHash    string `gorm:"size:64" json:"params_hash"`

	BasePrice        float64 `gorm:"type:decimal(20,4)" json:"base_price"`
	ForwardReturnPct float64 `gorm:"type:decimal(12,4)" json:"forward_return_pct"` // T+H 收盘 vs T 收盘
	MaePct           float64 `gorm:"type:decimal(12,4)" json:"mae_pct"`            // (T,T+H] 最低价 vs T 收盘
	MfePct           float64 `gorm:"type:decimal(12,4)" json:"mfe_pct"`            // (T,T+H] 最高价 vs T 收盘
	BarsUsed         int     `json:"bars_used"`

	CreatedAt time.Time `json:"created_at"`
}

func (PositionExitOutcome) TableName() string { return "position_exit_outcomes" }
