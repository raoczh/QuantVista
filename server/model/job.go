package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

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

	JobOwnerUser   = "user"
	JobOwnerSystem = "system"
)

// JobRun 是跨业务的作业事实。RequestSnapshot 只保存经过类型校验且有大小上限的
// 业务入参；结果正文继续由原业务表承载，ResultType/ResultID 只保存定位引用。
type JobRun struct {
	ID int64 `gorm:"primaryKey" json:"id"`
	// UserID 是旧调用的 Go 层兼容入口，不映射数据库。用户 owner 的真实外键为
	// OwnerUserID；系统 owner 必须为 NULL，禁止用 0 冒充用户。
	UserID      int64  `gorm:"-" json:"-"`
	OwnerType   string `gorm:"size:16;not null;default:user;index:idx_job_run_owner_created,priority:1" json:"owner"`
	OwnerUserID *int64 `gorm:"column:user_id;index:idx_job_run_user_created,priority:1;index:idx_job_run_lookup,priority:1" json:"owner_user_id,omitempty"`
	TriggeredBy *int64 `gorm:"index" json:"triggered_by,omitempty"`
	Kind        string `gorm:"size:64;not null;index:idx_job_run_lookup,priority:2;index:idx_job_run_kind_status,priority:1" json:"kind"`

	RequestHash string  `gorm:"size:64;not null;index:idx_job_run_lookup,priority:3" json:"-"`
	ActiveKey   *string `gorm:"size:64;uniqueIndex:idx_job_run_active" json:"-"`
	ParentID    *int64  `gorm:"index" json:"parent_id,omitempty"`

	Status string `gorm:"size:16;not null;default:queued;index:idx_job_run_lookup,priority:4;index:idx_job_run_kind_status,priority:2;index:idx_job_run_status_queued,priority:1;check:chk_job_run_status,status IN ('queued','running','success','degraded','failed','canceled')" json:"status"`

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
	QueuedAt        time.Time  `gorm:"not null;index:idx_job_run_status_queued,priority:2" json:"queued_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	CreatedAt       time.Time  `gorm:"index:idx_job_run_user_created,priority:2;index:idx_job_run_owner_created,priority:2" json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (j *JobRun) BeforeCreate(_ *gorm.DB) error {
	return j.normalizeOwner()
}

func (j *JobRun) AfterFind(_ *gorm.DB) error {
	if j.OwnerUserID != nil {
		j.UserID = *j.OwnerUserID
	} else {
		j.UserID = 0
	}
	return nil
}

func (j *JobRun) normalizeOwner() error {
	if j.OwnerType == "" {
		j.OwnerType = JobOwnerUser
	}
	switch j.OwnerType {
	case JobOwnerUser:
		if j.OwnerUserID == nil && j.UserID > 0 {
			userID := j.UserID
			j.OwnerUserID = &userID
		}
		if j.OwnerUserID == nil || *j.OwnerUserID <= 0 {
			return errors.New("用户作业必须包含正数 owner_user_id")
		}
		j.UserID = *j.OwnerUserID
		if j.TriggeredBy != nil {
			return errors.New("用户作业不能设置 triggered_by")
		}
	case JobOwnerSystem:
		if j.UserID != 0 || j.OwnerUserID != nil {
			return errors.New("系统作业不能设置 user_id")
		}
		if j.TriggeredBy != nil && *j.TriggeredBy <= 0 {
			return errors.New("triggered_by 必须是正数用户 ID")
		}
	default:
		return errors.New("无效的作业 owner")
	}
	return nil
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
	ID          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID      int64     `gorm:"-" json:"-"`
	OwnerType   string    `gorm:"size:16;not null;default:user;index:idx_job_event_owner_id,priority:1" json:"owner"`
	OwnerUserID *int64    `gorm:"column:user_id;index:idx_job_event_user_id,priority:1" json:"owner_user_id,omitempty"`
	JobRunID    int64     `gorm:"not null;index" json:"job_run_id"`
	Type        string    `gorm:"size:32;not null" json:"type"`
	Status      string    `gorm:"size:16" json:"status,omitempty"`
	CreatedAt   time.Time `gorm:"index:idx_job_event_user_id,priority:2;index:idx_job_event_owner_id,priority:2" json:"created_at"`
}

func (e *JobEvent) BeforeCreate(_ *gorm.DB) error {
	if e.OwnerType == "" {
		e.OwnerType = JobOwnerUser
	}
	if e.OwnerType == JobOwnerUser {
		if e.OwnerUserID == nil && e.UserID > 0 {
			userID := e.UserID
			e.OwnerUserID = &userID
		}
		if e.OwnerUserID == nil || *e.OwnerUserID <= 0 {
			return errors.New("用户作业事件必须包含正数 owner_user_id")
		}
		e.UserID = *e.OwnerUserID
		return nil
	}
	if e.OwnerType != JobOwnerSystem || e.UserID != 0 || e.OwnerUserID != nil {
		return errors.New("无效的作业事件 owner")
	}
	return nil
}

func (e *JobEvent) AfterFind(_ *gorm.DB) error {
	if e.OwnerUserID != nil {
		e.UserID = *e.OwnerUserID
	} else {
		e.UserID = 0
	}
	return nil
}
