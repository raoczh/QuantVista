package model

import "time"

// D16 卖出复核待办：持仓命中利空事件时生成的「该不该卖」复核项。
//
// 与既有三张表的边界（改动前先读）：
//   - `GuardEvent` 是**推送去重台账**（只为「这条推没推过」，无状态机、不进页面）；
//   - `AlertEvent` 是**用户手配规则的命中明细**（规则由用户显式创建）；
//   - `SellReview` 是**系统自动发现的持仓利空**（用户无需配置任何规则），带
//     open/resolved/dismissed 状态机，是今日待办「与我的账本有关」的主体。
//
// 唯一键 `(user_id, position_id, trigger, trade_date)`：同一笔持仓的同类事件、
// 同一事件日期只生成一条。TradeDate 存**事件自身日期**（解禁日/预告发布日/
// 跌破均线的那个交易日/上榜日/除权日），与 B9 盘后 guard 的 trade_date 语义一致——
// 窗口重复扫描天然幂等，服务缺勤后补跑也不会重复生成。
//
// **幂等靠 OnConflict{DoNothing}**：已被用户处理（resolved）或忽略（dismissed）的行
// 绝不能被下一轮扫描拉回 open（同 B8 除权建议的先例，否则用户被反复要求处理同一件事）。
type SellReview struct {
	ID         int64 `gorm:"primaryKey" json:"id"`
	UserID     int64 `gorm:"index;uniqueIndex:idx_sellreview_uniq,priority:1" json:"user_id"`
	PositionID int64 `gorm:"index;uniqueIndex:idx_sellreview_uniq,priority:2" json:"position_id"`

	Symbol string `gorm:"size:16;index" json:"symbol"`
	Market string `gorm:"size:8" json:"market"`
	Name   string `gorm:"size:64" json:"name"`

	// Trigger 触发类型（≤16 字符，与 GuardEvent.Kind 同款列宽纪律）。
	Trigger string `gorm:"size:16;uniqueIndex:idx_sellreview_uniq,priority:3" json:"trigger"`
	// TradeDate 事件自身日期（YYYY-MM-DD），不是扫描日。
	TradeDate string `gorm:"size:10;uniqueIndex:idx_sellreview_uniq,priority:4" json:"trade_date"`

	Severity string `gorm:"size:8" json:"severity"` // high / med / low
	Title    string `gorm:"size:128" json:"title"`  // 一句话标题（事件本身）
	Detail   string `gorm:"size:512" json:"detail"` // **该事件对我这笔持仓的具体影响**（含成本与浮盈亏）

	// 生成时的账面快照：复盘时要能还原「当时我的处境」，事后再算会拿到今天的价格。
	// **QuoteOK=false 时 Price/ProfitPct 恒为 0 且 Detail 如实声明行情不可用**——
	// 绝不用旧价冒充（fail-closed 一以贯之）。
	BuyPrice  float64 `gorm:"type:decimal(20,4)" json:"buy_price"`
	Quantity  float64 `gorm:"type:decimal(20,4)" json:"quantity"`
	Price     float64 `gorm:"type:decimal(20,4)" json:"price"`
	ProfitPct float64 `gorm:"type:decimal(20,4)" json:"profit_pct"`
	QuoteOK   bool    `gorm:"default:false" json:"quote_ok"`

	Status     string     `gorm:"size:16;index" json:"status"` // open / resolved / dismissed
	ResolvedAt *time.Time `json:"resolved_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 卖出复核触发类型。新增类型必须 ≤16 字符（列宽）。
const (
	SellReviewLift     = "lift"      // 限售解禁临近（A 股头号利空）
	SellReviewEarnFcst = "earn_fcst" // 业绩预告变脸（预减/预亏/首亏/续亏）
	SellReviewMaBreak  = "ma_break"  // 跌破关键均线（MA20 / MA60）
	SellReviewLhbSell  = "lhb_sell"  // 龙虎榜净卖出（游资出货）
	SellReviewExDiv    = "ex_div"    // 除权除息日临近（账面数字将变化）
)

// 卖出复核状态机。
const (
	SellReviewStatusOpen      = "open"      // 待处理（进今日待办）
	SellReviewStatusResolved  = "resolved"  // 已复核（用户看过并作出决定）
	SellReviewStatusDismissed = "dismissed" // 已忽略（终态，不再提示）
)

// 严重度。
const (
	SellReviewSeverityHigh = "high"
	SellReviewSeverityMed  = "med"
	SellReviewSeverityLow  = "low"
)

// SellReviewTriggerLabel 触发类型的中文标签（后端消息与前端标签同源，避免两处漂移）。
func SellReviewTriggerLabel(trigger string) string {
	switch trigger {
	case SellReviewLift:
		return "限售解禁"
	case SellReviewEarnFcst:
		return "业绩预告变脸"
	case SellReviewMaBreak:
		return "跌破关键均线"
	case SellReviewLhbSell:
		return "龙虎榜净卖出"
	case SellReviewExDiv:
		return "除权除息"
	}
	return "利空事件"
}
