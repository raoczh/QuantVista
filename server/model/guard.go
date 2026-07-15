package model

import "time"

// 守护事件类型（GuardEvent.Kind）：持仓止损/止盈触达、持仓异动、自选异动。
const (
	GuardKindStopLoss    = "stop_loss"    // 持仓现价触及计划止损
	GuardKindTakeProfit  = "take_profit"  // 持仓现价触及计划止盈
	GuardKindPosMove     = "pos_move"     // 持仓当日异动（|涨跌幅| ≥ 阈值）
	GuardKindWatchMove   = "watch_move"   // 守护范围内自选当日异动（|涨跌幅| ≥ 阈值 或 涨跌停）
)

// GuardEvent 自选/持仓异动守护推送的去重台账（阶段 D）。
// 唯一索引 (user_id, symbol, kind, trade_date)：同一用户同一标的同类事件当日只推一次——
// 15min 一轮评估反复命中同一止损/异动时不刷屏（与 alert_events 的同日去重同思路，但
// 这里的去重键内建唯一索引，靠 INSERT 冲突跳过而非查旧时间戳）。
// 市场按 symbol 隐含 cn（守护只覆盖 A 股持仓/自选），仍存 Market 便于跳转与排查。
type GuardEvent struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	UserID    int64  `gorm:"uniqueIndex:idx_guard_key;index" json:"user_id"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_guard_key" json:"symbol"`
	Kind      string `gorm:"size:16;uniqueIndex:idx_guard_key" json:"kind"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_guard_key" json:"trade_date"`

	Market  string  `gorm:"size:8" json:"market"`
	Name    string  `gorm:"size:64" json:"name"`
	Price   float64 `gorm:"type:decimal(20,4)" json:"price"`   // 触发时现价
	Message string  `gorm:"size:256" json:"message"`           // 推送正文（人话说明）

	CreatedAt time.Time `json:"created_at"`
}
