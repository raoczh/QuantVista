package model

import "time"

// 推荐追踪结局。短线用价格触发 + 有效期判定，长线仅跟踪表现（复盘按周期，不做价格触发）。
const (
	RecOutcomeActive     = "active"      // 短线：进行中，未触发止盈/止损/过期
	RecOutcomeTakeProfit = "take_profit" // 触发止盈（当日 high 达到止盈价）
	RecOutcomeStopLoss   = "stop_loss"   // 触发止损（当日 low 跌破止损价）
	RecOutcomeExpired    = "expired"     // 已过有效期（按交易日），且未触发止盈/止损
	RecOutcomeTracking   = "tracking"    // 长线：跟踪中（无价格触发概念）
	RecOutcomeNoData     = "no_data"     // 暂无日线数据，无法评估
)

// RecommendationStatus 推荐追踪状态（与 Recommendation 一一对应，recommendation_id 唯一）。
//
// 设计要点：
//   - 价格序列复用 daily_bars（原始不复权；本项目暂无 corporate_actions 复权表，短周期影响有限，note 中如实标注）。
//   - 止盈/止损按当日 high/low 判断（避免只看收盘漏判盘中触达），取最早触发者为主结局。
//   - 有效期按 trading_calendar 交易日计算，而非自然日。
//   - 相对基准超额收益 alpha = 区间收益 - 基准（上证指数）同区间收益；基准不可得时 alpha 记 0 并在 note 说明。
//   - 定时后台任务刷新；亦可按用户手动触发。冗余 type/action 便于历史表现统计聚合。
type RecommendationStatus struct {
	ID               int64  `gorm:"primaryKey" json:"id"`
	RecommendationID int64  `gorm:"uniqueIndex:idx_rs_rec" json:"recommendation_id"`
	BatchID          int64  `gorm:"index:idx_rs_batch" json:"batch_id"`
	UserID           int64  `gorm:"index:idx_rs_user" json:"user_id"`
	Symbol           string `gorm:"size:16" json:"symbol"`
	Market           string `gorm:"size:8" json:"market"`
	Type             string `gorm:"size:16" json:"type"`   // short_term / long_term（冗余，便于统计）
	Action           string `gorm:"size:16" json:"action"` // buy / watch

	RefPrice       float64 `gorm:"type:decimal(20,4)" json:"ref_price"`        // 生成时现价（追踪基准）
	CurrentPrice   float64 `gorm:"type:decimal(20,4)" json:"current_price"`    // 最新价（末根日线收盘或当日实时）
	PeriodHigh     float64 `gorm:"type:decimal(20,4)" json:"period_high"`      // 追踪期内最高（不复权）
	PeriodLow      float64 `gorm:"type:decimal(20,4)" json:"period_low"`       // 追踪期内最低
	ReturnPct      float64 `gorm:"type:decimal(12,4)" json:"return_pct"`       // 当前收益率 %
	MaxGainPct     float64 `gorm:"type:decimal(12,4)" json:"max_gain_pct"`     // 最大涨幅 %（相对 ref）
	MaxDrawdownPct float64 `gorm:"type:decimal(12,4)" json:"max_drawdown_pct"` // 最大回撤 %（正数表示回撤幅度）
	BenchReturnPct float64 `gorm:"type:decimal(12,4)" json:"bench_return_pct"` // 基准同区间收益 %
	AlphaPct       float64 `gorm:"type:decimal(12,4)" json:"alpha_pct"`        // 超额收益 = ReturnPct - BenchReturnPct

	Outcome          string `gorm:"size:16" json:"outcome"`
	ReviewNeeded     bool   `json:"review_needed"` // 是否需要复盘（触发止盈/止损/过期）
	HitTakeProfit    bool   `json:"hit_take_profit"`
	HitStopLoss      bool   `json:"hit_stop_loss"`
	ElapsedTradeDays int    `json:"elapsed_trade_days"`            // 从推荐日到今的已过交易日
	ValidDays        int    `json:"valid_days"`                    // 短线有效期（交易日，来自推荐明细）
	BarsCount        int    `json:"bars_count"`                    // 参与计算的日线条数
	LastEvalDate     string `gorm:"size:10" json:"last_eval_date"` // 最后评估对应的交易日
	Note             string `gorm:"size:256" json:"note"`          // 数据缺口/降级说明

	UpdatedAt time.Time `json:"updated_at"`
}
