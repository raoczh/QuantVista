package model

import "time"

// PromptTemplate 用户自定义提示词模板：按模块覆盖默认指引，调 prompt 无需重编译。
// 每用户每模块至多一条（唯一约束）；enabled 时在该模块的 LLM 调用中替换默认指引段。
// module 取值：5 个分析模块（market/sector/stock/watchlist/position，替换 moduleGuidance）
// + 4 个扩展模块（M3c）：recommend 推荐角色与纪律 / daily 收盘日报复盘 / qa 个股问答角色 /
// review 分析复核员。列宽 size:16，新增枚举必须取 ≤16 字符的短名。
type PromptTemplate struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	UserID  int64  `gorm:"index:idx_pt_uniq,unique" json:"user_id"`
	Module  string `gorm:"size:16;index:idx_pt_uniq,unique" json:"module"`
	Content string `gorm:"type:text" json:"content"`
	Enabled bool   `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 扩展提示词模块（M3c）。分析 5 模块沿用 AnalysisModule* 常量。
const (
	PromptModuleRecommend = "recommend" // 推荐：角色与铁律段（recRoleIntro）
	PromptModuleDaily     = "daily"     // 收盘日报：复盘系统提示（dailyReviewSystem）
	PromptModuleQa        = "qa"        // 个股问答：角色段（qaRoleIntro）
	PromptModuleReview    = "review"    // AI 复核：分析复核员系统提示
)
