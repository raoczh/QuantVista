package model

import "time"

// S2-1（RECOMMENDATION_ACCURACY_PLAN §5 S2-1）：反思记忆表——**本批只建表结构**，
// LLM 反思生成与生成端注入延后：启用门槛 = 成熟标签样本 ≥30 条（排序原则见文档
// §5 S2「确定性统计先行」）。届时由标签成熟事件触发一次轻量反思（固定三问、限定
// 2-4 句教训），注入前后批次分开标记版本做影子配对，无增量则撤。
//
// 防泄漏设计：AvailableFrom 是该条教训「最早可注入生成端」的时点——历史回放时只能
// 注入 available_from ≤ 生成时点的教训，否则记忆层会把未来结算结果泄漏进过去的推荐。

// RecommendationReflection 单条推荐 × 单一持有期的成熟结算反思。
type RecommendationReflection struct {
	ID int64 `gorm:"primaryKey" json:"id"`
	// 组合唯一：同一推荐同一持有期只反思一次（版本升级原地更新）。
	RecommendationID int64 `gorm:"uniqueIndex:idx_rr_key" json:"recommendation_id"`
	HorizonDays      int   `gorm:"uniqueIndex:idx_rr_key" json:"horizon_days"`

	UserID   int64  `gorm:"index:idx_rr_user" json:"user_id"`
	Symbol   string `gorm:"size:16;index:idx_rr_symbol" json:"symbol"`
	Strategy string `gorm:"size:32" json:"strategy"`
	RecType  string `gorm:"size:16" json:"rec_type"` // short_term / long_term

	Outcome   string  `gorm:"size:16" json:"outcome"` // win / loss / take_profit / stop_loss（成熟标签结算口径）
	ReturnPct float64 `gorm:"type:decimal(12,4)" json:"return_pct"` // 扣成本净收益 %
	AlphaPct  float64 `gorm:"type:decimal(12,4)" json:"alpha_pct"`  // 扣成本 Alpha %

	// Lesson LLM 反思教训（2-4 句，固定三问：方向对不对·论点哪部分成立或失败·一条
	// 可迁移教训）；FactorDigest 入场因子摘要（JSON，供按特征检索同类教训）。
	Lesson       string `gorm:"type:text" json:"lesson"`
	FactorDigest string `gorm:"type:text" json:"factor_digest"`

	// LabelMaturedAt 对应标签成熟时刻；AvailableFrom 教训最早可注入时点（防回放泄漏）。
	LabelMaturedAt time.Time `json:"label_matured_at"`
	AvailableFrom  time.Time `gorm:"index:idx_rr_avail" json:"available_from"`

	ReflectionVersion string    `gorm:"size:8" json:"reflection_version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}
