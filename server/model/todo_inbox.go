package model

import "time"

// TodoInboxState 只记录用户对某个业务事实版本的收件箱交互状态。
// 标题、正文、股票和任务请求继续由原业务表承载，避免形成第二套事实。
type TodoInboxState struct {
	ID     int64 `gorm:"primaryKey" json:"id"`
	UserID int64 `gorm:"not null;uniqueIndex:idx_todo_inbox_source,priority:1;index" json:"user_id"`

	SourceKind    string `gorm:"size:32;not null;uniqueIndex:idx_todo_inbox_source,priority:2" json:"source_kind"`
	SourceID      int64  `gorm:"not null;uniqueIndex:idx_todo_inbox_source,priority:3" json:"source_id"`
	SourceVersion string `gorm:"size:96;not null" json:"source_version"`

	Read         bool       `gorm:"not null;default:false" json:"read"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
	MutedDate    string     `gorm:"size:10;not null;default:''" json:"muted_date"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
