package model

import "time"

const (
	LLMCallStatusSuccess = "success"
	LLMCallStatusError   = "error"

	// 结构化方法枚举（P0-2）：本项目当前仅两态；json_schema/function_calling 待 P0-5
	// capability matrix 后引入，届时在此扩枚举而非另起词表。
	LLMStructuredJSONObject = "json_object"
	LLMStructuredFreeText   = "free_text"
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
	FirstChunkMs int64  `json:"first_chunk_ms"`
	RequestBody  string `gorm:"type:text" json:"request_body"`
	ResponseBody string `gorm:"type:text" json:"response_body"`

	// ---- P0-2/P0-8 调用关联与完整性元数据（2026-07 起写入；旧行为空，读取兼容）----
	// TraceID 业务结果级追溯 ID：同一业务运行（主调/repair/复核/反方/交易计划/降级）共享；
	// 业务表（analysis_records/recommendation_batches/daily_reports/ai_conversations）落同值，
	// 双向可查。RunID 逻辑调用组 ID（主调与其 repair 轮同组）；ParentRunID 派生调用回指主调。
	TraceID     string `gorm:"size:40;index" json:"trace_id"`
	RunID       string `gorm:"size:40;index" json:"run_id"`
	ParentRunID string `gorm:"size:40" json:"parent_run_id"`
	// Attempt 1 基轮次（1=首轮，>1=repair）；Repair = attempt>1（冗余落列便于筛查）。
	// 0 表示旧记录/未接线路径未记录。
	Attempt int  `json:"attempt"`
	Repair  bool `json:"repair"`
	// StructuredMethod 本次请求实际生效的结构化方法（json_object/free_text）——JSON mode
	// 因端点不支持回落时记录回落后的真实形态，与请求入口意图区分（同 Stream 字段纪律）。
	StructuredMethod string `gorm:"size:16" json:"structured_method"`
	SchemaVersion    string `gorm:"size:32" json:"schema_version"`
	PromptVersion    string `gorm:"size:32" json:"prompt_version"` // P0-6：-custom.<hash8> 后缀需 32 宽
	// PromptHash 初始业务消息序列（ac1 注入前）的 sha256；DataHash 输入数据快照的 sha256。
	PromptHash string `gorm:"size:80" json:"prompt_hash"`
	DataHash   string `gorm:"size:80" json:"data_hash"`
	// FinishState 规范化终态枚举（stop/tool_calls/completed/length/max_tokens/content_filter/
	// failed/cancelled/eof_without_marker/error/unknown；空=成功但上游未报告）；
	// FinishStateRaw 上游原始 finish_reason（chat）/status（responses）。
	FinishState    string `gorm:"size:24" json:"finish_state"`
	FinishStateRaw string `gorm:"size:64" json:"finish_state_raw"`

	CreatedAt time.Time `gorm:"index" json:"created_at"`
}
