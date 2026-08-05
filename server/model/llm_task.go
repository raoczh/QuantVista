package model

import "time"

const (
	LLMTaskStatusProcessing = "processing"
	LLMTaskStatusSuccess    = "success"
	LLMTaskStatusFailed     = "failed"
)

// LLMTask 是旧通用任务与新统一作业的兼容结果载体。新作业事实以 JobRun 为准；
// 结果 JSON 仍仅由旧详情接口按需读取，列表查询不会加载大字段。
type LLMTask struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index:idx_llm_task_lookup,priority:1;index:idx_llm_task_user_created,priority:1" json:"-"`
	Kind   string `gorm:"size:64;index:idx_llm_task_lookup,priority:2" json:"kind"`
	// JobRunID 非空表示该兼容结果行已由统一作业事实接管。任务中心据此排除旧行，
	// /llm-tasks 仍可用原 ID 读取结果，保证历史页面与深链兼容。
	JobRunID *int64 `gorm:"uniqueIndex;index" json:"-"`

	RequestHash string `gorm:"size:64;index:idx_llm_task_lookup,priority:3" json:"-"`
	// ActiveKey 仅 processing 时有值；终态清空。唯一索引把同请求防重扩展到多实例。
	ActiveKey  *string `gorm:"size:64;uniqueIndex" json:"-"`
	Status     string  `gorm:"size:16;index:idx_llm_task_lookup,priority:4" json:"status"`
	ResultJSON string  `gorm:"type:mediumtext" json:"-"`
	Error      string  `gorm:"size:512" json:"error,omitempty"`
	ErrorCode  string  `gorm:"size:64" json:"error_code,omitempty"`

	CreatedAt time.Time `gorm:"index:idx_llm_task_user_created,priority:2" json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
