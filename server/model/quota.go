package model

import "time"

// UserQuota 每用户 token 配额，成本控制基础（阶段 4 AI 调用时扣减/熔断）。
type UserQuota struct {
	UserID       int64     `gorm:"primaryKey" json:"user_id"`
	TokenLimit   int64     `gorm:"default:0" json:"token_limit"` // 0 表示不限
	TokenUsed    int64     `gorm:"default:0" json:"token_used"`
	RequestCount int64     `gorm:"default:0" json:"request_count"`
	UpdatedAt    time.Time `json:"updated_at"`
}
