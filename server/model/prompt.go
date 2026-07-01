package model

import "time"

// PromptTemplate 用户自定义的分析提示词模板：按分析模块覆盖默认的分析维度指引。
// 每用户每模块至多一条（唯一约束）；enabled 时在该模块的分析中替换默认 moduleGuidance。
type PromptTemplate struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	UserID  int64  `gorm:"index:idx_pt_uniq,unique" json:"user_id"`
	Module  string `gorm:"size:16;index:idx_pt_uniq,unique" json:"module"` // market/sector/stock/watchlist/position
	Content string `gorm:"type:text" json:"content"`
	Enabled bool   `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
