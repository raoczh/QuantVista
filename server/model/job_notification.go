package model

import "time"

const (
	JobFailureNoticeClaimed     = "claimed"
	JobFailureNoticeDispatching = "dispatching"
	JobFailureNoticeAttempted   = "attempted"
	JobFailureNoticeMerged      = "merged"
	JobFailureNoticeSuppressed  = "suppressed"
)

// JobFailureNotification 是用户作业失败通知的幂等事实。每个 JobRun 最多一行；
// GroupKey 只在短窗内第一条通知上有值，后续同类失败通过 MergeRootID 合并。
// attempted 只表示已调用 best-effort 通知器，不表示任一外部通道确认送达。
type JobFailureNotification struct {
	ID       int64  `gorm:"primaryKey" json:"id"`
	JobRunID int64  `gorm:"not null;uniqueIndex" json:"job_run_id"`
	UserID   int64  `gorm:"not null;index:idx_job_failure_notice_user_kind,priority:1" json:"user_id"`
	Kind     string `gorm:"size:64;not null;index:idx_job_failure_notice_user_kind,priority:2" json:"kind"`

	GroupKey    *string `gorm:"size:64;uniqueIndex" json:"-"`
	MergeRootID *int64  `gorm:"index" json:"merge_root_id,omitempty"`
	MergeCount  int     `gorm:"not null;default:1" json:"merge_count"`

	ErrorCode    string `gorm:"size:64" json:"error_code"`
	ErrorSummary string `gorm:"size:256" json:"error_summary"`
	Status       string `gorm:"size:16;not null" json:"status"`

	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}
