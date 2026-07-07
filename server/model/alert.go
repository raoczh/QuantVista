package model

import "time"

// 条件提醒。命中判定按交易日 OHLC（避免只看收盘漏判盘中触达）。
const (
	AlertKindPrice       = "price"        // 到价：当日 high/low 触及目标价
	AlertKindPctChange   = "pct_change"   // 异动：当日涨跌幅达到阈值
	AlertKindMA          = "ma"           // 均线：现价站上/跌破 N 日均线
	AlertKindBreakout    = "breakout"     // 突破：创近 N 日新高/新低
	AlertKindVolumeSurge = "volume_surge" // 放量：当日量达到 N 倍 20 日均量（threshold=倍数）
	AlertKindAmplitude   = "amplitude"    // 振幅：当日振幅达到 x%（(high-low)/prev_close）

	// F1 财报日历类（注意 Kind 列宽 size:16，新增 kind 必须 ≤16 字符）。
	// 不走盘中 15min 行情评估（无 high/low 判定、避免空转拉行情），
	// 由财报数据刷新后每日一评（service/finance.go job → EvaluateEarningsAll）。
	AlertKindEarnDate = "earn_date" // 财报披露临近：距预约披露日 ≤N 自然日（threshold=N）
	AlertKindEarnFcst = "earn_fcst" // 新业绩预告发布（带预增/预亏类型）

	AlertOpGTE = "gte" // >=（到价/涨幅向上、站上均线、新高突破）
	AlertOpLTE = "lte" // <=（到价/跌幅向下、跌破均线、新低破位）

	AlertStatusActive    = "active"    // 生效中，参与评估
	AlertStatusTriggered = "triggered" // 已命中（once 规则命中后置此，暂停再评估）
	AlertStatusPaused    = "paused"    // 用户暂停

	AlertEventUnread    = "unread"    // 命中事件：未读（进入今日待办）
	AlertEventRead      = "read"      // 已读（处理完毕，退出待办）
	AlertEventDismissed = "dismissed" // 已忽略
)

// AlertRule 用户设置的条件提醒规则。按 user_id 隔离。
// 命中落库并在待办/相关页面高亮提示；若用户配置了启用的推送通道且偏好
// 「开启提醒」打开，则额外主动推送（阶段8-③，同日去重，见 service/alert.go）。
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

// AlertEvent 提醒命中明细（PRD 3.16 状态机）。每次规则命中（同日去重）落一条，
// 独立于规则行的最近命中快照——历史可追溯，且用户可逐条标记已读/忽略「完成待办」。
// 今日待办的提醒条目即本表 unread 事件。
type AlertEvent struct {
	ID     int64 `gorm:"primaryKey" json:"id"`
	RuleID int64 `gorm:"index:idx_alert_event_rule" json:"rule_id"`
	UserID int64 `gorm:"index:idx_alert_event_user_status" json:"user_id"`

	Symbol  string `gorm:"size:16" json:"symbol"`
	Market  string `gorm:"size:8" json:"market"`
	Name    string `gorm:"size:64" json:"name"`
	Kind    string `gorm:"size:16" json:"kind"`
	Message string `gorm:"size:256" json:"message"` // 命中说明（同规则 trigger_msg 口径）

	TriggeredAt time.Time `json:"triggered_at"`
	Status      string    `gorm:"size:16;index:idx_alert_event_user_status" json:"status"` // unread/read/dismissed

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
