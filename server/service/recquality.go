package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// S2-3 数据质量门控（RECOMMENDATION_ACCURACY_PLAN §5 S2-3）：关键因子缺失面映射
// 置信度上限。**影子模式**——只记录 would_be_confidence_cap / missing_critical_fields /
// data_age / quality_gate_version，不实际封顶置信度、不改写 action；验证「被封顶的
// 标的确实更不可靠」（影子对照报表 gated vs ungated）后才启用强制。
//
// 缺失判定按策略分场景，不搞一刀切「缺三项」：
//   - 通用严重：行情/日线/成交额缺失或过期（任何策略都致命）；
//   - 长线缺财务 = 严重（文档口径「长线缺财务应直接拒绝」，影子期记最低档 cap）；
//   - 短线缺最新财务 = 不严重（不标记）；
//   - 情绪数据缺失 ≠ 情绪中性：SentiNews==0 是「没有数据」，SentiScore≈0 且有新闻才是
//     「中性」——缺失只做独立标记（senti_missing），不降 cap，是否降级留转正评审。
//
// 注：这是本项目的原创硬门——astock 的 quality_gate 只做报告完整性检查并注入后续
// prompt，并未程序化封顶置信度或禁止交易，只借其「必采清单+分级」的检查思路。
const qualityGateVersion = "qg2"

// qg2 判定参数（改判定规则/阈值必须递增 qualityGateVersion，报表按版本可区分）。
// qg1→qg2：日线过期从「自然日 >5」改为「交易日历口径 >3 个开市日无新数据」——
// 自然日口径在春节/国庆长假（可达 8+ 自然日）会把上一交易日的正常行情误判过期，
// 污染质量门控影子样本；日历不可得时回退自然日口径。
const (
	qgStaleTradeDays = 3  // 最近日线之后错过的开市日数超过该值=行情过期（正常 T+1 生成错过 ≤1 个）
	qgStaleAgeDays   = 5  // 回退口径（无交易日历）：自然日差超过该值=过期
	qgMinBars        = 60 // 日线样本下限（MA60/波动率窗口；次新股不足时窗口因子失真）

	// 各缺失项的 would-be 置信度上限（多项命中取最小）。
	qgCapNoFactors  = 30 // 无日线/因子未算：技术面证据整体缺席
	qgCapStaleBars  = 50 // 行情过期：所有价量因子都是旧的
	qgCapNoAmount   = 50 // 成交额缺失：流动性无法判断
	qgCapShortBars  = 60 // 日线样本不足：长窗口因子失真
	qgCapLongNoFin  = 40 // 长线缺财务：长线逻辑的核心证据缺席
)

// qualityGateResult 单条推荐的数据质量影子输出（落 pick 明细 detail_json；
// 影子期不实际封顶，前端只读展示）。
type qualityGateResult struct {
	// WouldBeConfidenceCap 若强制执行，confidence 将被钳制到 ≤ 此值（100=无封顶）。
	WouldBeConfidenceCap int `json:"would_be_confidence_cap"`
	// MissingCriticalFields 命中的关键缺失项（quote/daily_bars/daily_bars_stale/
	// daily_bars_short/amount/fin）；情绪缺失不在此列（见 SentiMissing）。
	MissingCriticalFields []string `json:"missing_critical_fields,omitempty"`
	// SentiMissing 当日无相关新闻（情绪数据缺失，≠情绪中性）——独立标记不降 cap。
	SentiMissing bool `json:"senti_missing,omitempty"`
	// DataAgeDays 生成日距最近日线收盘日的自然日数（-1=无日线）。
	DataAgeDays int `json:"data_age_days"`
	// DataAgeTradeDays 最近日线之后错过的开市日数（-1=无日线或无日历，此时过期
	// 判定回退自然日口径）。长假期间自然日差大而开市日差小，判定以此为准。
	DataAgeTradeDays int    `json:"data_age_trade_days"`
	Version          string `json:"quality_gate_version"`
}

// computeQualityGate 纯函数：按策略场景判定单只候选的数据质量。today 取 YYYY-MM-DD；
// openDays 为近段升序开市日清单（交易日历口径的过期判定；nil/空=无日历，回退自然日）。
// 返回 nil 表示数据齐全无标记（cap=100 且情绪不缺失时不落明细，省 JSON 噪声）。
func computeQualityGate(recType string, c candidate, today string, openDays []string) *qualityGateResult {
	res := &qualityGateResult{WouldBeConfidenceCap: 100, DataAgeDays: -1, DataAgeTradeDays: -1, Version: qualityGateVersion}
	miss := func(capV int, field string) {
		if capV < res.WouldBeConfidenceCap {
			res.WouldBeConfidenceCap = capV
		}
		res.MissingCriticalFields = append(res.MissingCriticalFields, field)
	}

	// 通用严重：行情现价缺失（防御性——准入门槛已拒，进池即 >0）。
	if c.Price <= 0 {
		miss(qgCapNoFactors, "quote")
	}
	// 通用严重：日线/因子缺席、过期、样本不足。
	if c.Factors == nil {
		miss(qgCapNoFactors, "daily_bars")
	} else {
		if c.lastBarDate != "" {
			res.DataAgeDays = daysBetween(c.lastBarDate, today)
			stale := false
			if len(openDays) > 0 {
				res.DataAgeTradeDays = tradeDaysBetween(openDays, c.lastBarDate, today)
				stale = res.DataAgeTradeDays > qgStaleTradeDays
			} else {
				stale = res.DataAgeDays > qgStaleAgeDays // 无日历回退自然日（长假可能误判，日历回填后自愈）
			}
			if stale {
				miss(qgCapStaleBars, "daily_bars_stale")
			}
		}
		if c.Factors.BarCount < qgMinBars {
			miss(qgCapShortBars, "daily_bars_short")
		}
	}
	// 通用严重：成交额缺失（防御性——S0-4 后建池已按中位数补齐、补不到即拒绝）。
	if c.Amount <= 0 {
		miss(qgCapNoAmount, "amount")
	}
	// 策略分场景：长线缺财务=严重；短线缺最新财务不标记。
	if recType == model.RecTypeLongTerm && c.Fin == nil {
		miss(qgCapLongNoFin, "fin")
	}
	// 情绪缺失 ≠ 情绪中性：只做区分标记，不参与 cap。
	res.SentiMissing = c.SentiNews == 0

	if res.WouldBeConfidenceCap >= 100 && !res.SentiMissing {
		return nil
	}
	sort.Strings(res.MissingCriticalFields)
	return res
}

// tradeDaysBetween 升序开市日清单 openDays 中，(from, to] 区间内的开市日数——
// 即 from 之后到 to 为止错过了多少个交易日。from/to 为 YYYY-MM-DD。
func tradeDaysBetween(openDays []string, from, to string) int {
	lo := sort.SearchStrings(openDays, from) // 第一个 ≥ from
	if lo < len(openDays) && openDays[lo] == from {
		lo++ // 不含 from 当日
	}
	hi := sort.SearchStrings(openDays, to)
	if hi < len(openDays) && openDays[hi] == to {
		hi++ // 含 to 当日
	}
	if hi < lo {
		return 0
	}
	return hi - lo
}

// recentOpenDays 近 lookback 自然日内的升序开市日清单（供质量门控的交易日口径过期
// 判定）；日历未回填/查询失败返回 nil（调用方回退自然日口径）。
func recentOpenDays(today string, lookback int) []string {
	if common.DB == nil {
		return nil
	}
	from := ""
	if t, err := time.Parse("2006-01-02", today); err == nil {
		from = t.AddDate(0, 0, -lookback).Format("2006-01-02")
	}
	var days []string
	common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date > ? AND trade_date <= ?", "cn", true, from, today).
		Order("trade_date").Pluck("trade_date", &days)
	return days
}

// applyQualityGateShadow 影子接线：对每条最终 pick 计算质量门控并写入明细（只读展示，
// 不改写 confidence/action）；仅当 would-be cap 真的低于该条置信度（即强制模式会改变
// 信号）时产出 gateNote 进反事实事件表——cap 不构成约束的标记只留在明细里。
func applyQualityGateShadow(recType string, picks []recPick, pool map[string]candidate) []gateNote {
	today := time.Now().Format("2006-01-02")
	openDays := recentOpenDays(today, 45) // 45 自然日窗口足够覆盖过期阈值 + 最长假期
	var gates []gateNote
	for i := range picks {
		qg := computeQualityGate(recType, pool[picks[i].Symbol], today, openDays)
		if qg == nil {
			continue
		}
		picks[i].QualityGate = qg
		if qg.WouldBeConfidenceCap < int(picks[i].Confidence) {
			gates = append(gates, gateNote{
				Symbol:      picks[i].Symbol,
				GateType:    model.GateQualityShadow,
				GateVersion: qualityGateVersion,
				Reason: fmt.Sprintf("数据质量门控（影子）：缺失 [%s]，若强制执行置信度 %d 将封顶至 %d（当前未封顶）",
					strings.Join(qg.MissingCriticalFields, ","), int(picks[i].Confidence), qg.WouldBeConfidenceCap),
			})
		}
	}
	return gates
}
