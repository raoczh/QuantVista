package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

const (
	CandidateAuditStatusProcessing = "processing"
	CandidateAuditStatusSuccess    = "success"
	CandidateAuditStatusPartial    = "partial"
	CandidateAuditStatusFailed     = "failed"

	CandidateAuditTypeObservation   = "observation"
	CandidateAuditTypeLeaderHit     = "leader_hit"
	CandidateAuditTypeMissedLeader  = "missed_leader"
	CandidateAuditTypeFalsePositive = "false_positive"
)

// CandidateAuditRun 是相邻两个有效交易日之间的一次全局推荐审计事实。
// 它只负责编排共享市场事实；用户明细保存在 CandidateAuditItem 并按 user_id 隔离。
type CandidateAuditRun struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	JobRunID    *int64 `gorm:"uniqueIndex" json:"job_run_id,omitempty"`
	OwnerType   string `gorm:"size:16;not null;default:system;index" json:"owner_type"`
	OwnerUserID *int64 `gorm:"column:owner_user_id;index" json:"owner_user_id,omitempty"`

	Market        string `gorm:"size:8;not null;uniqueIndex:idx_car_identity,priority:1" json:"market"`
	SignalDate    string `gorm:"size:10;not null;uniqueIndex:idx_car_identity,priority:2;index" json:"signal_date"`
	OutcomeDate   string `gorm:"size:10;not null;uniqueIndex:idx_car_identity,priority:3;index" json:"outcome_date"`
	AuditVersion  string `gorm:"size:16;not null;uniqueIndex:idx_car_identity,priority:4" json:"audit_version"`
	ParameterHash string `gorm:"size:64;not null;uniqueIndex:idx_car_identity,priority:5" json:"parameter_hash"`

	DiscoveryVersion string     `gorm:"size:16;not null" json:"discovery_version"`
	FactorVersion    string     `gorm:"size:16;not null" json:"factor_version"`
	RankingVersions  string     `gorm:"type:text" json:"ranking_versions"`
	OutcomeVersion   string     `gorm:"size:16;not null" json:"outcome_version"`
	ParameterJSON    string     `gorm:"type:text;not null" json:"parameter_json"`
	SignalDataAsOf   *time.Time `json:"signal_data_as_of,omitempty"`
	DataAsOf         time.Time  `gorm:"not null;index" json:"data_as_of"`

	Status               string `gorm:"size:16;not null;default:processing;index" json:"status"`
	UniverseCount        int    `json:"universe_count"`
	ScannedCount         int    `json:"scanned_count"`
	ExecutableCount      int    `json:"executable_count"`
	OpportunityCount     int    `json:"opportunity_count"`
	BatchCount           int    `json:"batch_count"`
	UserCount            int    `json:"user_count"`
	ItemCount            int    `json:"item_count"`
	MissedCount          int    `json:"missed_count"`
	FalsePositiveCount   int    `json:"false_positive_count"`
	ExcludedST           int    `json:"excluded_st"`
	ExcludedSuspended    int    `json:"excluded_suspended"`
	ExcludedLiquidity    int    `json:"excluded_liquidity"`
	ExcludedUnexecutable int    `json:"excluded_unexecutable"`
	ExcludedNoData       int    `json:"excluded_no_data"`

	CoverageJSON string     `gorm:"type:text" json:"coverage_json"`
	GapJSON      string     `gorm:"type:text" json:"gap_json"`
	ContentHash  string     `gorm:"size:64" json:"content_hash"`
	Error        string     `gorm:"size:512" json:"error,omitempty"`
	StartedAt    time.Time  `gorm:"not null" json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// CandidateAuditItem 是单个用户批次、单个标的在该交易日对上的审计明细。
// audit_type 区分漏选龙头、误选/早期不利、命中龙头和必要的候选对照观察。
type CandidateAuditItem struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	RunID     int64  `gorm:"not null;index;uniqueIndex:idx_cai_identity,priority:1" json:"run_id"`
	UserID    int64  `gorm:"not null;index:idx_cai_user_date,priority:1;uniqueIndex:idx_cai_identity,priority:2" json:"user_id"`
	BatchID   int64  `gorm:"not null;index;uniqueIndex:idx_cai_identity,priority:3" json:"batch_id"`
	Symbol    string `gorm:"size:16;not null;uniqueIndex:idx_cai_identity,priority:4" json:"symbol"`
	AuditType string `gorm:"size:24;not null;uniqueIndex:idx_cai_identity,priority:5;index" json:"audit_type"`

	Market            string `gorm:"size:8;not null" json:"market"`
	SignalDate        string `gorm:"size:10;not null" json:"signal_date"`
	OutcomeDate       string `gorm:"size:10;not null;index:idx_cai_user_date,priority:2" json:"outcome_date"`
	Name              string `gorm:"size:64" json:"name"`
	RecType           string `gorm:"size:16;index" json:"rec_type"`
	Regime            string `gorm:"size:16" json:"regime"`
	DiscoveryVersion  string `gorm:"size:16" json:"discovery_version"`
	RankingVersion    string `gorm:"size:16" json:"ranking_version"`
	HoldingPeriodDays int    `json:"holding_period_days"`

	ConclusionCode    string `gorm:"size:48;not null" json:"conclusion_code"`
	FunnelStage       string `gorm:"size:16;not null;index" json:"funnel_stage"`
	PrimaryReasonCode string `gorm:"size:64;not null;index" json:"primary_reason_code"`
	ReasonFactsJSON   string `gorm:"type:text" json:"reason_facts_json"`
	InputRefsJSON     string `gorm:"type:text" json:"input_refs_json"`
	OutcomeRefsJSON   string `gorm:"type:text" json:"outcome_refs_json"`

	Source                string `gorm:"size:32;index" json:"source"`
	SourceSet             string `gorm:"size:256" json:"source_set"`
	Channel               string `gorm:"size:32;index" json:"channel"`
	ChannelSet            string `gorm:"size:256" json:"channel_set"`
	CandidateEventID      int64  `gorm:"index" json:"candidate_event_id"`
	DiscoveryItemID       int64  `gorm:"index" json:"discovery_item_id"`
	RecommendationID      int64  `gorm:"index" json:"recommendation_id"`
	RecommendationLabelID int64  `gorm:"index" json:"recommendation_label_id"`
	SelectionOutcomeID    int64  `gorm:"index" json:"selection_outcome_id"`

	OpportunityRank int     `json:"opportunity_rank"`
	ScoreRank       int     `json:"score_rank"`
	LLMInputOrder   int     `json:"llm_input_order"`
	Action          string  `gorm:"size:16" json:"action"`
	RejectionReason string  `gorm:"size:256" json:"rejection_reason"`
	RefPrice        float64 `gorm:"type:decimal(20,4)" json:"ref_price"`

	OutcomeStatus           string  `gorm:"size:24;not null;index" json:"outcome_status"`
	EntryDate               string  `gorm:"size:10" json:"entry_date"`
	EntryPrice              float64 `gorm:"type:decimal(20,4)" json:"entry_price"`
	ObservationPrice        float64 `gorm:"type:decimal(20,4)" json:"observation_price"`
	GrossReturnPct          float64 `gorm:"type:decimal(12,4)" json:"gross_return_pct"`
	NetReturnPct            float64 `gorm:"type:decimal(12,4)" json:"net_return_pct"`
	BenchReturnPct          float64 `gorm:"type:decimal(12,4)" json:"bench_return_pct"`
	AlphaPct                float64 `gorm:"type:decimal(12,4)" json:"alpha_pct"`
	HasBench                bool    `json:"has_bench"`
	MfePct                  float64 `gorm:"type:decimal(12,4)" json:"mfe_pct"`
	MaePct                  float64 `gorm:"type:decimal(12,4)" json:"mae_pct"`
	SelectionMaturityStatus string  `gorm:"size:16" json:"selection_maturity_status"`
	Forced                  bool    `gorm:"column:forced" json:"forced"`

	CreatedAt time.Time `json:"created_at"`
}

func (CandidateAuditRun) TableName() string  { return "candidate_audit_runs" }
func (CandidateAuditItem) TableName() string { return "candidate_audit_items" }

func (r *CandidateAuditRun) BeforeCreate(_ *gorm.DB) error {
	if r.OwnerType == "" {
		r.OwnerType = JobOwnerSystem
	}
	if r.OwnerType != JobOwnerSystem || r.OwnerUserID != nil {
		return errors.New("每日候选审计运行必须属于 system/global，不能绑定用户")
	}
	return nil
}
