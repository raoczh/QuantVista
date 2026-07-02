package model

import "time"

// 推荐类型与状态。
const (
	RecTypeShortTerm = "short_term"
	RecTypeLongTerm  = "long_term"

	RecStatusSuccess  = "success"  // 生成并校验通过
	RecStatusDegraded = "degraded" // 有输出但结构化/反编造校验后无有效标的，降级
	RecStatusFailed   = "failed"   // 调用失败

	RecActionBuy   = "buy"   // 可考虑买入
	RecActionWatch = "watch" // 观察等待
)

// RecommendationBatch 一次推荐生成的批次。按 user_id 隔离。
// 为「不允许 AI 无依据编造」：候选池(candidate_pool)由真实数据构建并落库，
// 生成后校验每个标的必须∈候选池；data_snapshot + 版本号保证可复现。
type RecommendationBatch struct {
	ID       int64  `gorm:"primaryKey" json:"id"`
	UserID   int64  `gorm:"index:idx_rb_user" json:"user_id"`
	Type     string `gorm:"size:16;index:idx_rb_user" json:"type"` // short_term / long_term
	Market   string `gorm:"size:8" json:"market"`
	Strategy string `gorm:"size:32" json:"strategy"` // 策略模板 key
	Status   string `gorm:"size:16" json:"status"`
	Error    string `gorm:"size:512" json:"error"`

	CandidateCount int    `json:"candidate_count"`                           // 候选池标的数
	CandidatePool  string `gorm:"type:text" json:"candidate_pool,omitempty"` // 候选池快照 JSON（列表查询不返回）
	DataSnapshot   string `gorm:"type:text" json:"data_snapshot,omitempty"`  // 喂给模型的数据 JSON（列表查询不返回）
	RejectedJSON   string `gorm:"type:text" json:"rejected_json,omitempty"`  // 池内落选标的一句话理由 [{symbol,name,reason}]（列表查询不返回）

	LLMConfigID     int64  `json:"llm_config_id"`
	Provider        string `gorm:"size:32" json:"provider"`
	Model           string `gorm:"size:64" json:"model"`
	PromptVersion   string `gorm:"size:16" json:"prompt_version"`
	StrategyVersion string `gorm:"size:16" json:"strategy_version"`

	PromptTokens     int   `json:"prompt_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	TotalTokens      int   `json:"total_tokens"`
	LatencyMs        int64 `json:"latency_ms"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Recommendation 单条推荐标的。DetailJSON 存该标的完整结构化字段（短线/长线字段不同），
// 另抽取少量列便于列表展示。ref_price 为生成当时现价（供后续追踪基准）。
type Recommendation struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	BatchID int64  `gorm:"index" json:"batch_id"`
	UserID  int64  `gorm:"index" json:"user_id"`
	Symbol  string `gorm:"size:16" json:"symbol"`
	Market  string `gorm:"size:8" json:"market"`
	Name    string `gorm:"size:64" json:"name"`

	Action     string  `gorm:"size:16" json:"action"` // buy / watch
	Confidence int     `json:"confidence"`            // 0-100
	Summary    string  `gorm:"size:512" json:"summary"`
	RefPrice   float64 `gorm:"type:decimal(20,4)" json:"ref_price"`

	DetailJSON string `gorm:"type:text" json:"detail_json,omitempty"` // 结构化明细 JSON

	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
}
