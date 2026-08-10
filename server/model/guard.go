package model

import "time"

// 守护事件类型（GuardEvent.Kind）：盘中=持仓止损/止盈触达、持仓异动、自选异动；
// 盘后=持仓公告/龙虎榜/财报披露临近/业绩预告（每日 19:35 轮，数据来自本地缓存表零上游成本）。
const (
	GuardKindStopLoss    = "stop_loss"     // 持仓现价触及计划止损
	GuardKindTakeProfit  = "take_profit"   // 持仓现价触及计划止盈
	GuardKindPosMove     = "pos_move"      // 持仓当日异动（|涨跌幅| ≥ 阈值 或 涨跌停）
	GuardKindWatchMove   = "watch_move"    // 守护范围内自选当日异动（|涨跌幅| ≥ 阈值 或 涨跌停）
	GuardKindPosNotice   = "pos_notice"    // 持仓当日新公告（盘后）
	GuardKindPosLhb      = "pos_lhb"       // 持仓登龙虎榜（盘后）
	GuardKindPosEarnDate = "pos_earn_date" // 持仓财报披露临近（盘后，自动，无需手工配 AlertRule）
	GuardKindPosEarnFcst = "pos_earn_fcst" // 持仓新业绩预告（盘后）
	GuardKindPosLift     = "pos_lift"      // 持仓限售解禁临近（盘后，提前 10 天；B9）
	GuardKindPosExDiv    = "pos_ex_div"    // 持仓除权除息日临近（盘后，提前 3 天；B9）
	GuardKindIpoToday    = "ipo_today"     // 今日可申购新股/可转债（盘后，**不依赖持仓**；B9）
	// D16 卖出复核新增的两类推送：解禁/除权/业绩预告已由上面几类覆盖，
	// 这两类是既有守护没有的（跌破均线是趋势信号、龙虎榜既有 pos_lhb 不分买卖方向）。
	GuardKindPosMaBreak = "pos_ma_break" // 持仓跌破关键均线（盘后，MA20/MA60）
	GuardKindPosLhbSell = "pos_lhb_sell" // 持仓登龙虎榜且席位净卖出（盘后，游资出货）
	// 统一持仓卖出风险评估的通知去重键。review→urgent 使用不同 kind，允许升级再提醒。
	GuardKindPosExitReview = "pos_exit_review"
	GuardKindPosExitUrgent = "pos_exit_urgent"
)

// GuardEvent 自选/持仓异动守护推送的去重台账（阶段 D）。
// 唯一索引 (user_id, symbol, kind, trade_date)：同一用户同一标的同类事件当日只推一次——
// 15min 一轮评估反复命中同一止损/异动时不刷屏（与 alert_events 的同日去重同思路，但
// 这里的去重键内建唯一索引，靠 INSERT 冲突跳过而非查旧时间戳）。
// TradeDate 语义：盘中事件=评估当日；盘后事件=事件自身日期（公告发布日/上榜日/预约披露日），
// 这样窗口重复扫描（每日 19:35 查近几天窗口）天然幂等——同一事件跨天也只推一次。
// 市场按 symbol 隐含 cn（守护只覆盖 A 股持仓/自选），仍存 Market 便于跳转与排查。
type GuardEvent struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	UserID    int64  `gorm:"uniqueIndex:idx_guard_key;index" json:"user_id"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_guard_key" json:"symbol"`
	Kind      string `gorm:"size:16;uniqueIndex:idx_guard_key" json:"kind"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_guard_key" json:"trade_date"`

	Market  string  `gorm:"size:8" json:"market"`
	Name    string  `gorm:"size:64" json:"name"`
	Price   float64 `gorm:"type:decimal(20,4)" json:"price"` // 触发时现价
	Message string  `gorm:"size:256" json:"message"`         // 推送正文（人话说明）

	CreatedAt time.Time `json:"created_at"`
}
