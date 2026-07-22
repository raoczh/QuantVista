package model

import "time"

// PromptTemplate 用户自定义提示词模板：按模块覆盖默认指引，调 prompt 无需重编译。
// 每用户每模块至多一条（唯一约束）；enabled 时在该模块的 LLM 调用中替换默认指引段。
// module 取值：5 个分析模块（market/sector/stock/watchlist/position，替换 moduleGuidance）
// + 4 个扩展模块（M3c）：recommend 推荐角色与纪律 / daily 收盘日报复盘 / qa 个股问答角色 /
// review 分析复核员。列宽 size:16，新增枚举必须取 ≤16 字符的短名。
//
// P0-6：自定义内容一律作为 L3 任务段注入（模块契约/schema 由系统追加不可覆盖，见
// service/prompt.go composeCustomTaskPrompt）；ContentHash/Revision 提供内容级归因——
// 版本串 base-custom.<hash8> 中的 hash8 即 ContentHash 前 8 位，同名版本必对应同一内容。
type PromptTemplate struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	UserID  int64  `gorm:"index:idx_pt_uniq,unique" json:"user_id"`
	Module  string `gorm:"size:16;index:idx_pt_uniq,unique" json:"module"`
	Content string `gorm:"type:text" json:"content"`
	Enabled bool   `json:"enabled"`

	// P0-6 内容归因：ContentHash=sha256(Content) hex 前 16 位；Revision 从 1 起每次内容
	// 变化递增（只切 enabled 不变）。升级前的旧行两者为零值，读取侧现算 hash 兼容。
	ContentHash string `gorm:"size:16" json:"content_hash"`
	Revision    int    `json:"revision"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PromptTemplateRevision 模板内容的不可变快照（P0-6）：每次内容变化落一行，
// (template_id, revision) 唯一，行一旦写入不再更新。审计/manifest 里版本串的 hash8
// 可在此表回查当时的模板原文（llm_call_logs 正文是渲染+组装后的形态，此表是模板
// 原始形态）；删除模板不级联删快照——历史调用的归因链不能随模板删除断掉。
type PromptTemplateRevision struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	TemplateID  int64  `gorm:"index:idx_ptr_uniq,unique" json:"template_id"`
	UserID      int64  `gorm:"index" json:"user_id"`
	Module      string `gorm:"size:16" json:"module"`
	Revision    int    `gorm:"index:idx_ptr_uniq,unique" json:"revision"`
	ContentHash string `gorm:"size:16" json:"content_hash"`
	Content     string `gorm:"type:text" json:"content"`

	CreatedAt time.Time `json:"created_at"`
}

// 扩展提示词模块（M3c）。分析 5 模块沿用 AnalysisModule* 常量。
const (
	PromptModuleRecommend = "recommend" // 推荐：角色与铁律段（recRoleIntro）
	PromptModuleDaily     = "daily"     // 收盘日报：复盘系统提示（dailyReviewSystem）
	PromptModuleQa        = "qa"        // 个股问答：角色段（qaRoleIntro）
	PromptModuleReview    = "review"    // AI 复核：分析复核员系统提示
)
