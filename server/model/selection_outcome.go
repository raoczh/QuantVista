package model

import "time"

const (
	// SelectionOutcomeVersion 是统一 fixed-hold 结果口径版本。执行语义变化必须递增，
	// 旧版本结果保留，不原地改写。
	SelectionOutcomeVersion = "so1"
	// SelectionOutcomeSchemaVersion 是持久化字段契约版本，与计算口径版本分开演进。
	SelectionOutcomeSchemaVersion = "selection_outcome.v1"
)

// SelectionOutcomeHorizons 是 selection 评估固定持有期（交易日）。不包含 l2 的
// 1 日计划标签，也不读取推荐止盈止损。
var SelectionOutcomeHorizons = []int{5, 10, 20, 60}

// RecommendationSelectionOutcome 是批次内标的统一 fixed-hold 事实。
//
// 一行只描述 batch+symbol+horizon 的执行结局，不携带 AI/Quant/challenger 组别；
// 各组在报表读取生成时事实后按同一行配对，避免同一标的因组别不同产生口径漂移。
type RecommendationSelectionOutcome struct {
	ID int64 `gorm:"primaryKey" json:"id"`

	BatchID        int64  `gorm:"uniqueIndex:idx_selection_outcome_key,priority:1;index" json:"batch_id"`
	Symbol         string `gorm:"size:16;uniqueIndex:idx_selection_outcome_key,priority:2" json:"symbol"`
	HorizonDays    int    `gorm:"uniqueIndex:idx_selection_outcome_key,priority:3" json:"horizon_days"`
	OutcomeVersion string `gorm:"size:8;uniqueIndex:idx_selection_outcome_key,priority:4" json:"outcome_version"`

	CandidateEventID int64  `gorm:"index" json:"candidate_event_id"`
	UserID           int64  `gorm:"index" json:"user_id"`
	Market           string `gorm:"size:8" json:"market"`
	Name             string `gorm:"size:64" json:"name"`
	Type             string `gorm:"size:16;index" json:"type"`
	RankingVersion   string `gorm:"size:16" json:"ranking_version"`
	SchemaVersion    string `gorm:"size:32" json:"schema_version"`
	EntryMode        string `gorm:"size:16" json:"entry_mode"`

	SignalDate string    `gorm:"size:10;index" json:"signal_date"`
	SignalAsOf time.Time `json:"signal_asof"`
	EntryDate  string    `gorm:"size:10" json:"entry_date"`
	EntryPrice float64   `gorm:"type:decimal(20,4)" json:"entry_price"`
	ExitDate   string    `gorm:"size:10" json:"exit_date"`
	ExitPrice  float64   `gorm:"type:decimal(20,4)" json:"exit_price"`

	GrossReturnPct float64 `gorm:"type:decimal(12,4)" json:"gross_return_pct"`
	NetReturnPct   float64 `gorm:"type:decimal(12,4)" json:"net_return_pct"`
	BenchReturnPct float64 `gorm:"type:decimal(12,4)" json:"bench_return_pct"`
	AlphaPct       float64 `gorm:"type:decimal(12,4)" json:"alpha_pct"`
	HasBench       bool    `json:"has_bench"`
	MfePct         float64 `gorm:"type:decimal(12,4)" json:"mfe_pct"`
	MaePct         float64 `gorm:"type:decimal(12,4)" json:"mae_pct"`

	MaturityStatus string `gorm:"size:16;index" json:"maturity_status"`
	SkipReason     string `gorm:"size:64" json:"skip_reason"`
	NoDataReason   string `gorm:"size:64" json:"no_data_reason"`
	Forced         bool   `gorm:"column:forced" json:"forced"`
	ForcedReason   string `gorm:"size:64" json:"forced_reason"`
	Deferred       int    `json:"deferred"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
