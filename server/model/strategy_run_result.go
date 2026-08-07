package model

import "time"

const (
	StrategyRunKindScan     = "screener_scan"
	StrategyRunKindBacktest = "strategy_backtest"

	StrategyIdentityBuiltin = "builtin"
	StrategyIdentityCustom  = "custom"
	StrategyIdentityAdhoc   = "adhoc"
)

// StrategyRunResult 是扫描与条件树回测的持久结果事实。RequestJSON 保存入队时已经
// 规范化并固定 revision 的业务请求；ResultJSON 只在成功终态写入，之后没有更新入口。
// JobRun 与 ResearchArtifact 仅引用本行，不复制请求中的条件树或结果正文。
type StrategyRunResult struct {
	ID     int64 `gorm:"primaryKey" json:"id"`
	UserID int64 `gorm:"not null;index:idx_strategy_run_user_kind_created,priority:1" json:"-"`

	Kind               string `gorm:"size:32;not null;index:idx_strategy_run_user_kind_created,priority:2" json:"kind"`
	StrategyIdentity   string `gorm:"size:16;not null" json:"strategy_identity"`
	StrategyKey        string `gorm:"size:64" json:"strategy_key,omitempty"`
	StrategyID         *int64 `gorm:"index" json:"strategy_id,omitempty"`
	StrategyRevisionID *int64 `gorm:"index" json:"strategy_revision_id,omitempty"`
	StrategyRevision   int    `gorm:"not null;default:0" json:"strategy_revision"`
	StrategyHash       string `gorm:"size:64;not null;index" json:"strategy_hash"`
	StrategyName       string `gorm:"size:64;not null" json:"strategy_name"`

	RequestJSON string     `gorm:"type:text;not null" json:"-"`
	RequestHash string     `gorm:"size:64;not null;index" json:"request_hash"`
	AsOf        *time.Time `gorm:"index" json:"as_of,omitempty"`
	ResultJSON  string     `gorm:"type:longtext" json:"-"`
	ContentHash string     `gorm:"size:64;index" json:"content_hash,omitempty"`

	Status    string `gorm:"size:16;not null;default:queued;index;check:chk_strategy_run_status,status IN ('queued','running','success','failed','canceled')" json:"status"`
	Error     string `gorm:"size:512" json:"error,omitempty"`
	ErrorCode string `gorm:"size:64" json:"error_code,omitempty"`
	JobRunID  int64  `gorm:"not null;uniqueIndex" json:"job_run_id"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `gorm:"index:idx_strategy_run_user_kind_created,priority:3" json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}
