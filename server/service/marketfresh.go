package service

import (
	"fmt"
	"time"
)

// 行情新鲜度判定（cn 市场）：把「行情数据时间是否可接受」从「距采集时刻的固定 15 分钟」
// 升级为「按交易日历与交易时段判定的最近有效行情日」。纯函数不碰 DB——交易日布尔与
// 上一开市日由包装层（MarketService.GetFreshQuote）查库后传入，便于单测。
//
// 关键动机：休市/午休/节假日期间上游返回的仍是最近一次成交的行情，用固定 15 分钟规则会把
// 每个盘后/午间的正常收盘数据都误判成「偏旧」，扣 sys_confidence 且措辞失真；而交易时段内
// 主源若返回昨日价格却被 routeCap 当成功接收，则真正该换源的旧行情被静默放行。

// A 股连续竞价时段边界（本地时区，分钟数）。
const (
	sessionPreAuctionMin = 9*60 + 15 // 09:15 开盘集合竞价开始
	sessionQuoteReadyMin = 9*60 + 25 // 09:25 集合竞价出价，视为当日行情已可得
	sessionOpenMin       = 9*60 + 30 // 09:30 连续竞价开始
	sessionAmBreakMin    = 11*60 + 30 // 11:30 午间休市
	sessionPmOpenMin     = 13 * 60    // 13:00 下午开盘
	sessionCloseMin      = 15 * 60    // 15:00 收盘
)

// quoteStaleAfter 交易时段内行情数据时间与当前时刻的最大容忍延迟。
const quoteStaleAfter = 10 * time.Minute

// 市场时段常量。
const (
	marketStateTrading   = "trading"
	marketStateBreak     = "break"
	marketStatePreOpen   = "pre_open"
	marketStatePostClose = "post_close"
	marketStateClosed    = "closed"
)

// 行情新鲜度状态。
const (
	freshStatusFresh   = "fresh"
	freshStatusStale   = "stale"
	freshStatusUnknown = "unknown" // 非 cn 市场无日历判定
)

// quoteFreshInfo 一次行情新鲜度判定结果。
type quoteFreshInfo struct {
	Status       string // fresh | stale | unknown
	MarketState  string // trading | break | pre_open | post_close | closed
	ExpectedDate string // 期望的最近有效行情交易日（YYYY-MM-DD）
}

// cnMarketState 判定当前市场时段。非交易日恒 closed。
func cnMarketState(now time.Time, isTradingDay bool) string {
	if !isTradingDay {
		return marketStateClosed
	}
	m := now.Hour()*60 + now.Minute()
	switch {
	case m < sessionOpenMin:
		return marketStatePreOpen
	case m < sessionAmBreakMin:
		return marketStateTrading
	case m < sessionPmOpenMin:
		return marketStateBreak
	case m < sessionCloseMin:
		return marketStateTrading
	default:
		return marketStatePostClose
	}
}

// expectedQuoteDate 期望的最近有效行情交易日：
//   - 交易日且已过 09:25（集合竞价出价）→ 今天；
//   - 交易日但盘前（<09:25）→ 上一开市日；
//   - 非交易日 → 最近开市日（prevOpen）。
func expectedQuoteDate(now time.Time, isTradingDay bool, prevOpen string) string {
	if isTradingDay {
		if now.Hour()*60+now.Minute() >= sessionQuoteReadyMin {
			return now.Format("2006-01-02")
		}
	}
	return prevOpen
}

// quoteFreshness 判定行情数据时间是否新鲜。dataTime 为上游返回的行情时刻。
//   - 零值 → stale（解析失败或缺时间戳，无法确认）；
//   - 日期须与 expected 一致（pre_open 额外宽容「今天」——09:15~09:30 竞价期两态并存）；
//   - trading 态还要求距今 ≤ quoteStaleAfter，午休恢复期（13:00~13:10）宽容 11:25 后的午盘末价；
//   - break/post_close/closed 态：日期一致即视为新鲜（收盘/午间的最新价就是当下最新）。
func quoteFreshness(dataTime, now time.Time, state, expected string) quoteFreshInfo {
	info := quoteFreshInfo{Status: freshStatusStale, MarketState: state, ExpectedDate: expected}
	if dataTime.IsZero() {
		return info
	}
	d := dataTime.Format("2006-01-02")
	today := now.Format("2006-01-02")
	dateOK := d == expected || (state == marketStatePreOpen && d == today)
	if !dateOK {
		return info
	}
	switch state {
	case marketStateTrading:
		if now.Sub(dataTime) <= quoteStaleAfter {
			info.Status = freshStatusFresh
			return info
		}
		// 午休恢复宽容：刚过 13:00 时上游可能仍是 11:30 的午盘末价。
		m := now.Hour()*60 + now.Minute()
		if m >= sessionPmOpenMin && m < sessionPmOpenMin+10 {
			dm := dataTime.Hour()*60 + dataTime.Minute()
			if dm >= sessionAmBreakMin-5 {
				info.Status = freshStatusFresh
			}
		}
		return info
	default:
		info.Status = freshStatusFresh
		return info
	}
}

// stockFreshnessNote 依据新鲜度状态生成快照文案：
//   - stale → note（触发 sys_confidence 扣档的 freshness_note）；
//   - 非 trading 且 fresh → sessionNote（正常收盘/午间口径，不扣档）。
func stockFreshnessNote(fi quoteFreshInfo, dataTime time.Time) (note, sessionNote string) {
	dt := "未知时间"
	if !dataTime.IsZero() {
		dt = dataTime.Format("2006-01-02 15:04")
	}
	if fi.Status == freshStatusStale {
		note = fmt.Sprintf("行情仅更新至 %s（期望交易日 %s），已尝试全部数据源仍未取到更新数据，可能停牌、休市或数据源延迟，不代表实时盘面", dt, fi.ExpectedDate)
		return note, ""
	}
	if fi.MarketState != marketStateTrading {
		sessionNote = fmt.Sprintf("当前为非交易时段（%s），行情为最近交易日 %s 的收盘（或午间阶段）口径，非实时盘面", fi.MarketState, fi.ExpectedDate)
	}
	return "", sessionNote
}
