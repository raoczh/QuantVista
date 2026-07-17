package model

import "time"

// S0-1/S0-5（RECOMMENDATION_ACCURACY_PLAN §5）：推荐结果事实表 + 反事实事件表。
//
// 设计要点：
//   - 两类事实分表——模型结果事实（统一执行模拟成交 + 固定持有期标签，本表）供训练
//     与评估；用户执行事实（是否买、真实价格）挂 position 血缘，以 entry_mode=
//     actual_position 并列落行但**不得作为模型训练标签**（含用户选择偏差）。
//   - 统计铁律：最终买入胜率只统计 action=buy AND maturity_status=matured；watch 单独
//     统计「观察判断质量」；pending 不进分母；实际跟买结果与统一模拟结果并列展示不混算。
//   - 影子标签：被过滤/被门控改写/落选（rejected）标的同样记后续收益——
//     RecommendationID=0 且 CandidateEventID>0 的行即影子标签。

// 标签成熟状态。
const (
	LabelPending = "pending" // 持有期尚未走完（或数据尚未覆盖）
	LabelMatured = "matured" // 已按统一执行语义结算
	LabelNoData  = "no_data" // 无信号日之后的日线（长期停牌/退市），无法结算
	LabelSkipped = "skipped" // 统一执行判定无法成交（一字板买不进/次日停牌等），skip_reason 说明
)

// 入场口径。
const (
	EntryModeNextOpen = "next_open"       // 统一执行模拟：信号次日开盘成交（模型结果事实）
	EntryModeActual   = "actual_position" // 用户实际建仓价（执行差异分析用，禁作训练标签）
)

// LabelHorizons 固定持有期（交易日）。
var LabelHorizons = []int{1, 5, 10, 20, 60}

// RecommendationLabel 推荐结果事实表：一条推荐（或一条影子事件）× 一个持有期 × 一种
// 入场口径 = 一行。由标签推进任务（reclabel.go）按统一执行模拟器结算。
type RecommendationLabel struct {
	ID int64 `gorm:"primaryKey" json:"id"`
	// RecommendationID 关联推荐条目；0=影子标签（此时 CandidateEventID 必非 0）。
	// 组合唯一：同一来源行同一持有期同一入场口径只有一行。
	RecommendationID int64 `gorm:"uniqueIndex:idx_rl_key" json:"recommendation_id"`
	CandidateEventID int64 `gorm:"uniqueIndex:idx_rl_key" json:"candidate_event_id"`
	HorizonDays      int   `gorm:"uniqueIndex:idx_rl_key" json:"horizon_days"`
	// EntryMode next_open=统一模拟 / actual_position=实际建仓（并列不混算）。
	EntryMode string `gorm:"size:16;uniqueIndex:idx_rl_key" json:"entry_mode"`

	BatchID int64  `gorm:"index:idx_rl_batch" json:"batch_id"`
	UserID  int64  `gorm:"index:idx_rl_user" json:"user_id"`
	Symbol  string `gorm:"size:16" json:"symbol"`
	Market  string `gorm:"size:8" json:"market"`
	Type    string `gorm:"size:16" json:"type"`   // short_term / long_term
	Action  string `gorm:"size:16" json:"action"` // buy / watch；影子标签为事件的 raw/would_be 动作

	// SignalDate 信号日（推荐日或其前最近交易日）；SignalAsOf 生成时刻（区分盘中/盘后）。
	SignalDate string    `gorm:"size:10;index:idx_rl_signal" json:"signal_date"`
	SignalAsOf time.Time `json:"signal_asof"`

	EntryDate  string  `gorm:"size:10" json:"entry_date"`
	EntryPrice float64 `gorm:"type:decimal(20,4)" json:"entry_price"`
	ExitDate   string  `gorm:"size:10" json:"exit_date"`
	ExitPrice  float64 `gorm:"type:decimal(20,4)" json:"exit_price"`

	GrossReturnPct float64 `gorm:"type:decimal(12,4)" json:"gross_return_pct"` // 价格收益 %
	NetReturnPct   float64 `gorm:"type:decimal(12,4)" json:"net_return_pct"`   // 扣佣金印花税后 %
	BenchReturnPct float64 `gorm:"type:decimal(12,4)" json:"bench_return_pct"` // 上证同区间 %
	AlphaPct       float64 `gorm:"type:decimal(12,4)" json:"alpha_pct"`        // NetReturn − Bench（扣成本 Alpha）
	HasBench       bool    `json:"has_bench"`                                  // 基准缺失时 alpha 无意义
	MfePct         float64 `gorm:"type:decimal(12,4)" json:"mfe_pct"`          // 持有期内最大有利波动 %（相对入场价）
	MaePct         float64 `gorm:"type:decimal(12,4)" json:"mae_pct"`          // 最大不利波动 %（负数）

	HitTakeProfit bool `json:"hit_take_profit"` // 止盈障碍先触（仅带计划价的条目）
	HitStopLoss   bool `json:"hit_stop_loss"`

	MaturityStatus string `gorm:"size:16;index:idx_rl_status" json:"maturity_status"` // pending/matured/no_data/skipped
	SkipReason     string `gorm:"size:64" json:"skip_reason"`

	// 用户执行事实（entry_mode=actual_position 行）：持仓血缘与真实买入价。
	PositionID     int64   `json:"position_id"`
	ActualBuyPrice float64 `gorm:"type:decimal(20,4)" json:"actual_buy_price"`

	// 归因维度冗余（S0-6 确定性错误归因报表直接分组，免 join 快照 JSON）。
	Strategy      string  `gorm:"size:32" json:"strategy"`
	Source        string  `gorm:"size:32" json:"source"`   // 候选首来源
	Industry      string  `gorm:"size:32" json:"industry"` // 生成时行业（宇宙快照口径，可空）
	Regime        string  `gorm:"size:16" json:"regime"`   // 生成时市场状态（S1-1）
	EntryChg5dPct float64 `gorm:"type:decimal(12,4)" json:"entry_chg_5d_pct"`
	EntryTurnover float64 `gorm:"type:decimal(12,4)" json:"entry_turnover"`
	EntryScore    float64 `gorm:"type:decimal(12,4)" json:"entry_score"`

	// 价格版本（S0-4 防前复权重锚）：生成时点最近收盘日与收盘价。结算时若 daily_bars
	// 同日收盘偏差超容差，判定序列已重锚，计划价按复权因子调整后再结算。
	RefDate  string  `gorm:"size:10" json:"ref_date"`
	RefClose float64 `gorm:"type:decimal(20,4)" json:"ref_close"`

	LabelVersion string    `gorm:"size:8" json:"label_version"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// 候选事件阶段（candidate_stage）。
const (
	CandStageFiltered = "filtered" // 用户筛选/风控排除（excluded 非空）
	CandStagePoolFull = "pool_full" // 评分名额已满未入评
	CandStageScored   = "scored"    // 完成量化评分但未进 LLM 名单
	CandStageLLMList  = "llm_list"  // 进入 LLM 名单但未入选（rejected/未提及）
	CandStagePicked   = "picked"    // 最终入选
)

// 门控类型（gate_type，影子门控记录）。
const (
	GateRegimeShadow  = "regime_shadow"  // S1-1 大盘闸门影子：defense 档「若强制会 buy→watch」
	GateCorrelation   = "correlation"    // S1-3 相关性去重（名单阶段生效）
	GateIndustryCap   = "industry_cap"   // S1-3 同行业名额上限（名单阶段生效）
	GateBearShadow    = "bear_shadow"    // S2-2 反方研究员影子：severity=high「若强制会 buy→watch」
	GateQualityShadow = "quality_shadow" // S2-3 数据质量门控影子：would_be_confidence_cap 只记不封顶
)

// RecommendationCandidateEvent 反事实事件表（S0-5）：候选池内每只标的在本批次的
// 去向事实——为什么没进名单/没被推荐/被门控改写。影子标签挂在本表行上，
// 支撑「没有门控会怎样」「错失机会率」「gated vs ungated 配对」的稳定重建。
type RecommendationCandidateEvent struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	BatchID int64  `gorm:"index:idx_rce_batch" json:"batch_id"`
	UserID  int64  `gorm:"index:idx_rce_user" json:"user_id"`
	Symbol  string `gorm:"size:16" json:"symbol"`
	Market  string `gorm:"size:8" json:"market"`
	Name    string `gorm:"size:64" json:"name"`

	CandidateStage string  `gorm:"size:16" json:"candidate_stage"` // filtered/pool_full/scored/llm_list/picked
	RawScore       float64 `gorm:"type:decimal(12,4)" json:"raw_score"`
	RawAction      string  `gorm:"size:16" json:"raw_action"`       // LLM 原始动作（picked 条目）
	WouldBeAction  string  `gorm:"size:16" json:"would_be_action"`  // 影子门控若强制执行会改写成的动作
	PostGateAction string  `gorm:"size:16" json:"post_gate_action"` // 实际最终动作（影子期与 raw 相同）
	GateType       string  `gorm:"size:32" json:"gate_type"`        // 主门控（多门控命中时按优先级取最强）
	// GateTypes 全部命中门控（逗号分隔，含主门控）——同一标的同时命中 regime/bear/
	// quality 时各门控都保有样本，影子对照报表按此分别归组（只看 GateType 会让次要
	// 门控永久丢失样本，无法分别验证增量效果）。旧行为空=只有 GateType 一个。
	GateTypes   string `gorm:"size:128" json:"gate_types"`
	GateVersion string `gorm:"size:16" json:"gate_version"`

	RejectionReason  string `gorm:"size:256" json:"rejection_reason"` // excluded 原因 / LLM 落选理由
	Source           string `gorm:"size:32" json:"source"`            // 候选首来源
	SentToLLM        bool   `json:"sent_to_llm"`
	RefPrice         float64 `gorm:"type:decimal(20,4)" json:"ref_price"` // 事件时点现价（影子标签入场锚）
	OpportunitySetID string  `gorm:"size:32" json:"opportunity_set_id"`   // S3 召回评估预留

	CreatedAt time.Time `json:"created_at"`
}
