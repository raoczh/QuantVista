package model

import "time"

// 条件提醒。命中判定按交易日 OHLC（避免只看收盘漏判盘中触达）。
const (
	AlertKindPrice     = "price"      // 到价：当日 high/low 触及目标价
	AlertKindPctChange = "pct_change" // 异动：当日涨跌幅达到阈值
	AlertKindMA        = "ma"         // 均线：现价站上/跌破 N 日均线
	AlertKindBreakout  = "breakout"   // 突破：创近 N 日新高/新低

	AlertOpGTE = "gte" // >=（到价/涨幅向上、站上均线、新高突破）
	AlertOpLTE = "lte" // <=（到价/跌幅向下、跌破均线、新低破位）

	AlertStatusActive    = "active"    // 生效中，参与评估
	AlertStatusTriggered = "triggered" // 已命中（once 规则命中后置此，暂停再评估）
	AlertStatusPaused    = "paused"    // 用户暂停
)

// AlertRule 用户设置的条件提醒规则。按 user_id 隔离。
// 不做主动推送——命中仅落库并在待办/相关页面高亮提示（与阶段6 同一「查询即提示」理念）。
type AlertRule struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index:idx_alert_user" json:"user_id"`
	Symbol string `gorm:"size:16" json:"symbol"`
	Market string `gorm:"size:8" json:"market"`
	Name   string `gorm:"size:64" json:"name"` // 标的名称（冗余，便于无行情时展示）

	Kind      string  `gorm:"size:16" json:"kind"`                 // price/pct_change/ma/breakout
	Op        string  `gorm:"size:8" json:"op"`                    // gte/lte
	Threshold float64 `gorm:"type:decimal(20,4)" json:"threshold"` // 价格（price）/ 涨跌幅%（pct_change）
	Period    int     `json:"period"`                              // ma/breakout 的窗口（交易日）
	Once      bool    `json:"once"`                                // 命中后是否自动暂停（默认 true，避免重复提示）
	Note      string  `gorm:"size:256" json:"note"`

	Status        string     `gorm:"size:16;index:idx_alert_status" json:"status"`
	LastValue     float64    `gorm:"type:decimal(20,4)" json:"last_value"` // 最近一次评估的观测值
	LastCheckDate string     `gorm:"size:10" json:"last_check_date"`       // 最近评估对应交易日
	TriggeredAt   *time.Time `json:"triggered_at"`
	TriggerMsg    string     `gorm:"size:256" json:"trigger_msg"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
