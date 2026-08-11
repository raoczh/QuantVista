package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// DiscoveryRun 是全市场一次日更候选发现的系统级事实。它没有用户归属：
// owner_type 固定为 system，owner_user_id 保持 NULL，供所有用户消费同一份结果。
type CandidateDiscoveryRun struct {
	ID               int64      `gorm:"primaryKey" json:"id"`
	JobRunID         *int64     `gorm:"uniqueIndex;index" json:"job_run_id,omitempty"`
	OwnerType        string     `gorm:"size:16;not null;default:system;index" json:"owner_type"`
	OwnerUserID      *int64     `gorm:"column:owner_user_id;index" json:"owner_user_id,omitempty"`
	Market           string     `gorm:"size:8;not null;uniqueIndex:idx_cdr_identity,priority:1" json:"market"`
	TradeDate        string     `gorm:"size:10;not null;uniqueIndex:idx_cdr_identity,priority:2" json:"trade_date"`
	AsOf             time.Time  `gorm:"not null;index" json:"as_of"`
	DiscoveryVersion string     `gorm:"size:16;not null;uniqueIndex:idx_cdr_identity,priority:3" json:"discovery_version"`
	FactorVersion    string     `gorm:"size:16;not null" json:"factor_version"`
	SourceVersions   string     `gorm:"type:text" json:"source_versions,omitempty"`
	ParameterHash    string     `gorm:"size:64;not null;uniqueIndex:idx_cdr_identity,priority:4" json:"parameter_hash"`
	Status           string     `gorm:"size:16;not null;default:processing;index" json:"status"` // processing/success/partial/failed
	UniverseCount    int        `json:"universe_count"`
	ScannedCount     int        `json:"scanned_count"`
	EligibleCount    int        `json:"eligible_count"`
	SuccessCount     int        `json:"success_count"`
	FailedCount      int        `json:"failed_count"`
	PartialReason    string     `gorm:"size:512" json:"partial_reason,omitempty"`
	Error            string     `gorm:"size:512" json:"error,omitempty"`
	StartedAt        time.Time  `gorm:"not null" json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// CandidateDiscoveryItem 是某次发现运行在单个固定通道中的候选事实。
// 历史 score 只表示当日通道排序事实，消费推荐时必须重新读取行情并重新过硬门。
type CandidateDiscoveryItem struct {
	ID               int64     `gorm:"primaryKey" json:"id"`
	RunID            int64     `gorm:"not null;index:idx_cdi_run" json:"run_id"`
	TradeDate        string    `gorm:"size:10;not null;uniqueIndex:idx_cdi_day_channel_symbol,priority:1" json:"trade_date"`
	Market           string    `gorm:"size:8;not null;uniqueIndex:idx_cdi_day_channel_symbol,priority:2" json:"market"`
	DiscoveryVersion string    `gorm:"size:16;not null;uniqueIndex:idx_cdi_day_channel_symbol,priority:3" json:"discovery_version"`
	Channel          string    `gorm:"size:32;not null;uniqueIndex:idx_cdi_day_channel_symbol,priority:4" json:"channel"`
	Symbol           string    `gorm:"size:16;not null;uniqueIndex:idx_cdi_day_channel_symbol,priority:5" json:"symbol"`
	Name             string    `gorm:"size:64" json:"name"`
	Rank             int       `gorm:"not null" json:"rank"`
	Score            float64   `gorm:"type:decimal(12,4);not null" json:"score"`
	FirstSeenDate    string    `gorm:"size:10" json:"first_seen_date"`
	ConsecutiveDays  int       `json:"consecutive_days"`
	RankChange       int       `json:"rank_change"`
	ScoreChange      float64   `gorm:"type:decimal(12,4)" json:"score_change"`
	SourceFacts      string    `gorm:"type:text" json:"source_facts,omitempty"`
	FactorSummary    string    `gorm:"type:text" json:"factor_summary,omitempty"`
	DataStatus       string    `gorm:"size:16;not null;default:ready" json:"data_status"` // ready/partial/unknown
	PartialReason    string    `gorm:"size:512" json:"partial_reason,omitempty"`
	AsOf             time.Time `gorm:"not null" json:"as_of"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (CandidateDiscoveryRun) TableName() string  { return "candidate_discovery_runs" }
func (CandidateDiscoveryItem) TableName() string { return "candidate_discovery_items" }

func (r *CandidateDiscoveryRun) BeforeCreate(_ *gorm.DB) error {
	if r.OwnerType == "" {
		r.OwnerType = JobOwnerSystem
	}
	if r.OwnerType != JobOwnerSystem || r.OwnerUserID != nil {
		return errors.New("全市场发现运行必须属于 system/global，不能绑定用户")
	}
	return nil
}
