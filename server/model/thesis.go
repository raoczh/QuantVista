package model

import "time"

// 投资逻辑卡片状态。
const (
	ThesisStatusActive      = "active"      // 逻辑成立，持续跟踪
	ThesisStatusInvalidated = "invalidated" // 逻辑已失效（触发失效条件/假设被证伪）
	ThesisStatusArchived    = "archived"    // 归档（已卖出/不再关注）
)

// ThesisCard 投资逻辑卡片：为自选/持仓标的保存结构化研究假设——为什么关注、
// 核心逻辑、关键证据、风险、失效条件（kill switches）与下次复盘日期。
// 每用户每标的一张（唯一），到期未复盘会进今日待办；参考 ai-berkshire 的
// 反偏见框架：失效条件是必填思维，不是附加项。
type ThesisCard struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"uniqueIndex:uk_thesis_user_symbol,priority:1" json:"user_id"`
	Symbol string `gorm:"size:16;uniqueIndex:uk_thesis_user_symbol,priority:2" json:"symbol"`
	Market string `gorm:"size:8;uniqueIndex:uk_thesis_user_symbol,priority:3" json:"market"`
	Name   string `gorm:"size:64" json:"name"` // 标的名称（冗余，便于无行情时展示）

	Thesis       string `gorm:"type:text" json:"thesis"`        // 核心投资逻辑（为什么值得关注/持有）
	KeyEvidence  string `gorm:"type:text" json:"key_evidence"`  // 关键证据，一行一条
	Risks        string `gorm:"type:text" json:"risks"`         // 主要风险，一行一条
	KillSwitches string `gorm:"type:text" json:"kill_switches"` // 失效条件，一行一条（触发即应复盘/放弃）
	TrackMetrics string `gorm:"type:text" json:"track_metrics"` // 需持续跟踪的指标，一行一条

	NextReviewDate string `gorm:"size:10" json:"next_review_date"` // YYYY-MM-DD，到期进待办
	Status         string `gorm:"size:16;index:idx_thesis_status" json:"status"`
	InvalidReason  string `gorm:"size:255" json:"invalid_reason"` // 判定失效时记录原因

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
