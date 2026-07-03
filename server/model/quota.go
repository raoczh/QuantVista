package model

import "time"

// UserQuota 每用户 AI 配额。2026-07-03 起标准化为**次数制**：
// 一次用户手动动作（发起分析/推荐/问答/对比 AI 点评）计 1 次，
// 内部 repair、panel 多角色等产生的多次 LLM 请求不重复计次；
// 后台自动任务（如收盘日报）只记 token 审计、不计次。
// TokenUsed/RequestCount 仅作用量审计展示，不参与熔断。
type UserQuota struct {
	UserID       int64     `gorm:"primaryKey" json:"user_id"`
	ActionLimit  int64     `gorm:"default:0" json:"action_limit"`  // 次数上限，0 表示不限
	ActionUsed   int64     `gorm:"default:0" json:"action_used"`   // 已用次数（仅用户手动动作）
	TokenUsed    int64     `gorm:"default:0" json:"token_used"`    // 累计 token（审计参考）
	RequestCount int64     `gorm:"default:0" json:"request_count"` // LLM 调用轮次（审计参考）
	UpdatedAt    time.Time `json:"updated_at"`
}
