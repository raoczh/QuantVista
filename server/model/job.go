package model

import "time"

const (
	JobStatusQueued   = "queued"
	JobStatusRunning  = "running"
	JobStatusSuccess  = "success"
	JobStatusDegraded = "degraded"
	JobStatusFailed   = "failed"
	JobStatusCanceled = "canceled"

	JobStepQueued   = "queued"
	JobStepDispatch = "dispatch"
	JobStepExecute  = "execute"
	JobStepPersist  = "persist"
)

// JobRun 是跨业务的作业事实。RequestSnapshot 只保存经过类型校验且有大小上限的
// 业务入参；结果正文继续由原业务表承载，ResultType/ResultID 只保存定位引用。
type JobRun struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"not null;index:idx_job_run_user_created,priority:1;index:idx_job_run_lookup,priority:1" json:"-"`
	Kind   string `gorm:"size:64;not null;index:idx_job_run_lookup,priority:2" json:"kind"`

	RequestHash string  `gorm:"size:64;not null;index:idx_job_run_lookup,priority:3" json:"-"`
	ActiveKey   *string `gorm:"size:64;uniqueIndex:idx_job_run_active" json:"-"`
	ParentID    *int64  `gorm:"index" json:"parent_id,omitempty"`

	Status string `gorm:"size:16;not null;default:queued;index:idx_job_run_lookup,priority:4;check:chk_job_run_status,status IN ('queued','running','success','degraded','failed','canceled')" json:"status"`

	SnapshotVersion int    `gorm:"not null;default:1" json:"-"`
	RequestSnapshot string `gorm:"type:text;not null" json:"-"`
	ResultType      string `gorm:"size:32" json:"result_type,omitempty"`
	ResultID        *int64 `gorm:"index" json:"result_id,omitempty"`

	Error     string `gorm:"size:512" json:"error,omitempty"`
	ErrorCode string `gorm:"size:64" json:"error_code,omitempty"`
	TraceID   string `gorm:"size:40;index" json:"trace_id,omitempty"`
	Provider  string `gorm:"size:32" json:"provider,omitempty"`
	Model     string `gorm:"size:64" json:"model,omitempty"`

	PromptTokens     int   `json:"prompt_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	TotalTokens      int   `json:"total_tokens"`
	LatencyMs        int64 `json:"latency_ms"`
	Total            int   `json:"total"`
	Succeeded        int   `json:"succeeded"`
	Failed           int   `json:"failed"`

	CancelRequested bool       `gorm:"not null;default:false;index" json:"cancel_requested"`
	QueuedAt        time.Time  `gorm:"not null" json:"queued_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	CreatedAt       time.Time  `gorm:"index:idx_job_run_user_created,priority:2" json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// JobStep 记录真实发生的排队、分派、执行与持久化步骤，不保存推测进度。
type JobStep struct {
	ID       int64  `gorm:"primaryKey" json:"id"`
	JobRunID int64  `gorm:"not null;uniqueIndex:idx_job_step_run_seq,priority:1;index" json:"job_run_id"`
	Sequence int    `gorm:"not null;uniqueIndex:idx_job_step_run_seq,priority:2" json:"sequence"`
	Name     string `gorm:"size:24;not null" json:"name"`
	Status   string `gorm:"size:16;not null;check:chk_job_step_status,status IN ('queued','running','success','degraded','failed','canceled')" json:"status"`

	Error     string `gorm:"size:512" json:"error,omitempty"`
	ErrorCode string `gorm:"size:64" json:"error_code,omitempty"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// JobEvent 为 SSE 提供数据库生成的全局单调事件 ID。事件只含状态定位信息，
// 客户端收到后再读取当前事实投影，避免在事件表复制请求或结果正文。
type JobEvent struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID    int64     `gorm:"not null;index:idx_job_event_user_id,priority:1" json:"-"`
	JobRunID  int64     `gorm:"not null;index" json:"job_run_id"`
	Type      string    `gorm:"size:32;not null" json:"type"`
	Status    string    `gorm:"size:16" json:"status,omitempty"`
	CreatedAt time.Time `gorm:"index:idx_job_event_user_id,priority:2" json:"created_at"`
}
