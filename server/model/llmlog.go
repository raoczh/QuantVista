package model

import "time"

const (
	LLMCallStatusSuccess = "success"
	LLMCallStatusError   = "error"
)

// LLMCallLog 记录一次真实的上游 LLM 请求。请求与响应正文仅供管理员审计，
// 列表查询必须显式排除两个 TEXT 字段，避免普通翻页返回大体积敏感数据。
type LLMCallLog struct {
	ID               int64     `gorm:"primaryKey" json:"id"`
	UserID           int64     `gorm:"index" json:"user_id"`
	Module           string    `gorm:"size:32;index" json:"module"`
	LLMConfigID      int64     `gorm:"index" json:"llm_config_id"`
	Provider         string    `gorm:"size:32" json:"provider"`
	Model            string    `gorm:"size:64" json:"model"`
	EndpointType     string    `gorm:"size:24" json:"endpoint_type"`
	Stream           bool      `json:"stream"`
	Status           string    `gorm:"size:16;index" json:"status"`
	ErrorMsg         string    `gorm:"size:512" json:"error_msg"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	LatencyMs        int64     `json:"latency_ms"`
	// FirstChunkMs 流式请求首个 data 块到达耗时（非流式恒 0）。
	// ≈latency_ms 说明上游忽略 stream 整包返回（假流式网关），排查 60s 超时归属层时先看它。
	FirstChunkMs int64     `json:"first_chunk_ms"`
	RequestBody  string    `gorm:"type:text" json:"request_body"`
	ResponseBody string    `gorm:"type:text" json:"response_body"`
	CreatedAt    time.Time `gorm:"index" json:"created_at"`
}
