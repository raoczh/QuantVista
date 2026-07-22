package model

import "time"

// 推荐类型与状态。
const (
	RecTypeShortTerm = "short_term"
	RecTypeLongTerm  = "long_term"

	RecStatusProcessing = "processing" // 任务已创建，后台生成中（异步任务化：生成接口立即返回，前端轮询详情）
	RecStatusSuccess    = "success"    // 生成并校验通过
	RecStatusDegraded   = "degraded"   // 有输出但校验后无有效标的（宁缺毋滥拒选），或 AI 超时后的量化降级（items 带 degraded_source）
	RecStatusFailed     = "failed"     // 调用失败

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
	// Title 生成时由筛选条件组合固化（如「短线·动量突破·≤30元·3只」）。
	// 历史列表直接展示，不再依赖前端用「当前所选类型的策略列表」动态查名
	//（旧做法导致跨类型批次显示原始 key 如 "value"，且随类型切换变化）。
	Title  string `gorm:"size:128" json:"title"`
	Status string `gorm:"size:16" json:"status"`
	Error  string `gorm:"size:512" json:"error"`

	CandidateCount int    `json:"candidate_count"`                                 // 候选池标的数（未被过滤的）
	CandidatePool  string `gorm:"type:mediumtext" json:"candidate_pool,omitempty"` // 候选池快照 JSON，含被过滤标的与原因、量化因子与评分（列表查询不返回；mediumtext——全景快照含因子明细，TEXT 64KB 会被大池撑爆）
	DataSnapshot   string `gorm:"type:text" json:"data_snapshot,omitempty"`  // 喂给模型的数据 JSON（列表查询不返回）
	RejectedJSON   string `gorm:"type:text" json:"rejected_json,omitempty"`  // 池内落选标的一句话理由 [{symbol,name,reason}]（列表查询不返回）
	FiltersJSON    string `gorm:"type:text" json:"filters_json,omitempty"`   // 本次生效的筛选条件快照（透明可回显）
	ReviewJSON     string `gorm:"type:text" json:"review_json,omitempty"`    // AI 复核员结论 JSON（verify 模式；列表查询不返回）

	// Regime S1-1 大盘闸门三档判定（offense/neutral/defense；空=未判定，如旧记录/数据缺失）。
	// 影子模式：只落库与展示，不改写 action；强制降级由 feature flag 控制（默认关）。
	Regime     string `gorm:"size:16" json:"regime"`
	RegimeJSON string `gorm:"type:text" json:"regime_json,omitempty"` // 判定依据明细 + 仓位模型参数快照（可回溯）

	LLMConfigID     int64  `json:"llm_config_id"`
	Provider        string `gorm:"size:32" json:"provider"`
	Model           string `gorm:"size:64" json:"model"`
	PromptVersion   string `gorm:"size:32" json:"prompt_version"` // P0-6：-custom.<hash8> 后缀需 32 宽
	StrategyVersion string `gorm:"size:16" json:"strategy_version"`

	// P0-2 调用关联：批次 trace_id（llm_call_logs 同值，串联主调/repair/复核/反方/降级）
	// 与运行元数据 manifest 数组。旧记录为空；llm_run_json 列表查询不返回。
	TraceID    string `gorm:"size:40;index" json:"trace_id"`
	LlmRunJSON string `gorm:"type:text" json:"llm_run_json,omitempty"`

	PromptTokens     int   `json:"prompt_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	TotalTokens      int   `json:"total_tokens"`
	LatencyMs        int64 `json:"latency_ms"`

	// FactsRecorded S0-5 事实账本完整性状态：candidate_events + 影子标签全部落库成功后置 true。
	// 显式 column tag 防 GORM 蛇形化坑；旧行/落库失败批次为 false，供 tracking 扫描排查
	// （持久化补建不可靠——快照丢失未导出的价格版本锚、gates 未持久化、事件表无唯一键
	// 重跑会重复，故失败批次仅标记不自动重建，见 reclabel.go recordBatchFacts 注释）。
	FactsRecorded bool `gorm:"column:facts_recorded" json:"facts_recorded"`

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
	// 价格版本（S0-4 防前复权重锚）：生成时点最近收盘日与该日收盘价。追踪/标签结算时
	// 与 daily_bars 同日收盘比对，偏差超容差判定序列已重锚，价位按复权因子调整。
	RefDate  string  `gorm:"size:10" json:"ref_date"`
	RefClose float64 `gorm:"type:decimal(20,4)" json:"ref_close"`

	DetailJSON string `gorm:"type:text" json:"detail_json,omitempty"` // 结构化明细 JSON

	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
}
