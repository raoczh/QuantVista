package model

import "time"

// 个股 AI 问答会话。首轮注入一次个股数据快照，之后多轮追问复用该快照，不重复拉数据。
const (
	QaRoleUser      = "user"
	QaRoleAssistant = "assistant"
)

// AiConversation 一个针对某只个股的问答会话。按 user_id 隔离。
type AiConversation struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index:idx_conv_user" json:"user_id"`
	Symbol string `gorm:"size:16" json:"symbol"`
	Market string `gorm:"size:8" json:"market"`
	Name   string `gorm:"size:64" json:"name"`
	Title  string `gorm:"size:128" json:"title"` // 会话标题（取首个问题）

	LLMConfigID int64  `json:"llm_config_id"`
	Provider    string `gorm:"size:32" json:"provider"`
	Model       string `gorm:"size:64" json:"model"`

	// 问答 prompt 版本（M3c 起落库，会话创建时固化；-custom.<hash8> 后缀=启用了自定义模板，
	// hash8 归因到模板内容，P0-6 起需 32 宽）。
	PromptVersion string `gorm:"size:32" json:"prompt_version"`

	// P0-2 调用关联：会话级 trace_id（该会话全部提问的 LLM 调用共享；llm_call_logs
	// 同值可双向查询）。旧会话为空，首次新提问时补写。
	TraceID string `gorm:"size:40;index" json:"trace_id"`

	// 首轮采集的个股数据快照（JSON），后续追问复用，避免重复拉数据；列表查询不返回。
	DataSnapshot string `gorm:"type:text" json:"data_snapshot,omitempty"`

	MessageCount int `json:"message_count"`
	TotalTokens  int `json:"total_tokens"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AiConversationMessage 会话中的一条消息（user 提问 / assistant 回答）。
type AiConversationMessage struct {
	ID             int64  `gorm:"primaryKey" json:"id"`
	ConversationID int64  `gorm:"index:idx_msg_conv" json:"conversation_id"`
	UserID         int64  `gorm:"index:idx_msg_user" json:"user_id"`
	Role           string `gorm:"size:16" json:"role"` // user / assistant
	Content        string `gorm:"type:text" json:"content"`

	// assistant 回答的证据数字核验结果（服务端回填，JSON；user 消息为空）。旧消息无此列，前端 v-if 兜底。
	CheckJSON string `gorm:"type:text" json:"check_json,omitempty"`

	// P0-2 调用关联：本轮回答对应的 run_id（llm_call_logs 同值；user 消息与旧消息为空）。
	RunID string `gorm:"size:40" json:"run_id,omitempty"`

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	CreatedAt time.Time `json:"created_at"`
}
