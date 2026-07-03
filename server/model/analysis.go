package model

import "time"

// 分析模块与状态取值。
const (
	AnalysisModuleMarket    = "market"    // 全市场
	AnalysisModuleSector    = "sector"    // 板块
	AnalysisModuleStock     = "stock"     // 个股
	AnalysisModuleWatchlist = "watchlist" // 自选股
	AnalysisModulePosition  = "position"  // 持仓

	AnalysisStatusSuccess  = "success"  // 结构化输出校验通过
	AnalysisStatusDegraded = "degraded" // 模型有输出但结构化校验失败，降级保存原文
	AnalysisStatusFailed   = "failed"   // 调用失败（网络/鉴权/配额等），无有效结果

	AnalysisRatingBullish = "bullish"
	AnalysisRatingNeutral = "neutral"
	AnalysisRatingBearish = "bearish"

	AnalysisModeStandard = ""      // 标准单视角分析
	AnalysisModePanel    = "panel" // 多角色观点（仅个股模块）
)

// AnalysisRecord 一次 AI 分析的完整档案。按 user_id 隔离。
// 为可复现：落库 data_snapshot（当时喂给模型的数据上下文）+ prompt/策略版本号；
// result_json 为结构化结果（降级时存原文），列表查询不返回重字段（见 service）。
type AnalysisRecord struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index:idx_ar_user" json:"user_id"`
	Module string `gorm:"size:16;index:idx_ar_user" json:"module"`
	Market string `gorm:"size:8" json:"market"`
	Symbol string `gorm:"size:16" json:"symbol"` // 个股模块的标的
	Target string `gorm:"size:64" json:"target"` // 展示用标签（个股名/「全市场」/分组名等）
	Title  string `gorm:"size:128" json:"title"`

	Status     string `gorm:"size:16" json:"status"`
	Mode       string `gorm:"size:16" json:"mode"`      // ""=标准 / "panel"=多角色观点
	Rating     string `gorm:"size:16" json:"rating"`    // 抽取到列，便于列表快速展示
	Confidence int    `json:"confidence"`               // 0-100
	Summary    string `gorm:"size:1024" json:"summary"` // 一句话总览（降级时为截断原文）
	Error      string `gorm:"size:512" json:"error"`    // 失败/降级原因

	// 重字段：列表不返回，仅详情返回。
	ResultJSON   string `gorm:"type:text" json:"result_json,omitempty"`   // 结构化结果 JSON（降级存原文）
	DataSnapshot string `gorm:"type:text" json:"data_snapshot,omitempty"` // 数据上下文快照，供复现

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
