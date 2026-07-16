package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

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
const qualityGateVersion = "qg1"

// qg1 判定参数（改判定规则/阈值必须递增 qualityGateVersion，报表按版本可区分）。
const (
	qgStaleAgeDays = 5  // 最近日线距生成日超过该自然日数=行情过期（覆盖春节等长假：正常 T+1 生成距上一收盘 ≤4 天）
	qgMinBars      = 60 // 日线样本下限（MA60/波动率窗口；次新股不足时窗口因子失真）

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
	DataAgeDays int    `json:"data_age_days"`
	Version     string `json:"quality_gate_version"`
}

// computeQualityGate 纯函数：按策略场景判定单只候选的数据质量。today 取 YYYY-MM-DD。
// 返回 nil 表示数据齐全无标记（cap=100 且情绪不缺失时不落明细，省 JSON 噪声）。
func computeQualityGate(recType string, c candidate, today string) *qualityGateResult {
	res := &qualityGateResult{WouldBeConfidenceCap: 100, DataAgeDays: -1, Version: qualityGateVersion}
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
			if res.DataAgeDays > qgStaleAgeDays {
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

// applyQualityGateShadow 影子接线：对每条最终 pick 计算质量门控并写入明细（只读展示，
// 不改写 confidence/action）；仅当 would-be cap 真的低于该条置信度（即强制模式会改变
// 信号）时产出 gateNote 进反事实事件表——cap 不构成约束的标记只留在明细里。
func applyQualityGateShadow(recType string, picks []recPick, pool map[string]candidate) []gateNote {
	today := time.Now().Format("2006-01-02")
	var gates []gateNote
	for i := range picks {
		qg := computeQualityGate(recType, pool[picks[i].Symbol], today)
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
