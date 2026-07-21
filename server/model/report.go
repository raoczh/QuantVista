package model

import "time"

// DailyReport 收盘后自动生成的每日报告：今日复盘 + 明日选股推荐。
// 每用户每交易日一份（user_id+trade_date 唯一）；由后台任务在交易日
// 15:35 后为开启偏好 enable_daily_report 的用户生成，也可手动重生成。
// 推荐部分直接关联 recommendation_batches（复用推荐域的展示与追踪闭环）。
type DailyReport struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	UserID    int64  `gorm:"index:idx_report_user_date,unique" json:"user_id"`
	TradeDate string `gorm:"size:10;index:idx_report_user_date,unique" json:"trade_date"` // YYYY-MM-DD
	Market    string `gorm:"size:8;default:cn" json:"market"`

	Status string `gorm:"size:16" json:"status"` // success / partial / failed

	// 复盘 prompt 版本（M3c 起落库；-custom 后缀=当时启用了用户自定义模板，供归因）。
	PromptVersion string `gorm:"size:16" json:"prompt_version"`

	// 生成时使用的 LLM（复盘链路；明日推荐批次自带同款字段）。旧行为空，前端兜底。
	LLMConfigID int64  `json:"llm_config_id"`
	Provider    string `gorm:"size:32" json:"provider"`
	Model       string `gorm:"size:64" json:"model"`

	// P0-2 调用关联：日报复盘链路的 trace_id 与运行元数据 manifest（推荐批次的调用
	// 关联由其批次行自带，经 RecommendationBatchID 间接串联）。旧行为空。
	TraceID    string `gorm:"size:40;index" json:"trace_id"`
	LlmRunJSON string `gorm:"type:text" json:"llm_run_json,omitempty"`

	// 今日复盘：LLM 结构化输出 + 输入数据快照（可复现）。列表查询时排除大字段。
	ReviewJSON   string `gorm:"type:text" json:"review_json"`
	SnapshotJSON string `gorm:"type:text" json:"snapshot_json"`

	// 明日推荐批次（0=未生成/失败）；卖点提醒规则以 note 前缀「收盘日报」标记。
	RecommendationBatchID int64 `gorm:"index" json:"recommendation_batch_id"`

	Error       string    `gorm:"size:500" json:"error"`
	TotalTokens int       `json:"total_tokens"`
	LatencyMs   int64     `json:"latency_ms"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const (
	ReportStatusProcessing = "processing" // 任务已创建，后台生成中（异步任务化：生成接口立即返回，前端轮询）
	ReportStatusSuccess    = "success"
	ReportStatusPartial    = "partial" // 复盘/推荐一方失败
	ReportStatusFailed     = "failed"
)
