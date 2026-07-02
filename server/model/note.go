package model

import "time"

// 笔记类别（自由标签的常用值，前端下拉；不强约束）。
const (
	NoteKindDecision = "decision" // 决策记录：为什么买/卖/放弃
	NoteKindReview   = "review"   // 复盘笔记
	NoteKindIdea     = "idea"     // 想法/灵感
	NoteKindEvent    = "event"    // 事件记录：公告/新闻/传闻及其影响
)

// ResearchNote 投资笔记/决策日志：独立于持仓与 AI 报告的自由笔记，
// 可选绑定标的（symbol 为空即通用笔记），按时间线回看「当时为什么这么想」。
// 事件笔记绑定资产而非持仓——卖出后笔记仍在（参考 php-invest/TradeNote）。
type ResearchNote struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index:idx_note_user" json:"user_id"`
	Symbol string `gorm:"size:16;index:idx_note_symbol" json:"symbol"` // 可空 = 不绑定标的
	Market string `gorm:"size:8" json:"market"`
	Name   string `gorm:"size:64" json:"name"` // 标的名称（冗余）

	Kind    string `gorm:"size:16" json:"kind"` // decision/review/idea/event（可空）
	Title   string `gorm:"size:128" json:"title"`
	Content string `gorm:"type:text" json:"content"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
