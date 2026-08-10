package model

import "time"

const (
	OnboardingStatusInProgress = "in_progress"
	OnboardingStatusCompleted  = "completed"

	OnboardingStepNotStarted = "not_started"
	OnboardingStepCompleted  = "completed"
	OnboardingStepSkipped    = "skipped"
)

// OnboardingProgress 是一次独立、版本化的首次使用引导事实。相同版本重跑会增加 Run，
// 不覆盖此前完成/跳过记录；当前进度始终取当前版本最大的 Run。
type OnboardingProgress struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	UserID  int64  `gorm:"not null;index;uniqueIndex:idx_onboarding_run,priority:1" json:"-"`
	Version int    `gorm:"not null;uniqueIndex:idx_onboarding_run,priority:2" json:"version"`
	Run     int    `gorm:"not null;uniqueIndex:idx_onboarding_run,priority:3" json:"run"`
	Status  string `gorm:"size:16;not null;default:in_progress;index" json:"status"`

	PreferenceStatus string     `gorm:"size:16;not null;default:not_started" json:"preference_status"`
	PreferenceAt     *time.Time `json:"preference_at,omitempty"`
	PortfolioStatus  string     `gorm:"size:16;not null;default:not_started" json:"portfolio_status"`
	PortfolioAt      *time.Time `json:"portfolio_at,omitempty"`
	AlertStatus      string     `gorm:"size:16;not null;default:not_started" json:"alert_status"`
	AlertAt          *time.Time `json:"alert_at,omitempty"`

	AlertRuleID   int64      `gorm:"index" json:"alert_rule_id,omitempty"`
	AlertTestedAt *time.Time `json:"alert_tested_at,omitempty"`
	DeferredUntil *time.Time `gorm:"index" json:"deferred_until,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}
