package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"quantvista/common"
	"quantvista/model"
)

// 推荐筛选器（阶段②用户硬过滤）：股价区间 / 流通市值 / 换手率 / 追高保护 / 涨停不可买。
// 与 candidateFilter（ST/退市/流动性/黑名单等基础准入）分层：基础准入不合格的标的
// 不进池快照（噪声）；用户筛选不合格的标的保留在池快照中并标注 excluded 原因——
// 「为什么这只没进候选」全透明（学问财的条件回显）。

// RecFilters 用户可配置的候选筛选条件。零值字段 = 不过滤。
// 请求未携带时回退用户偏好（UserPreference.RecFiltersJSON），偏好也无则按类型给保护性默认。
type RecFilters struct {
	PriceMin      float64 `json:"price_min"`        // 股价下限（元）
	PriceMax      float64 `json:"price_max"`        // 股价上限（元）——资金有限时的核心筛选
	FloatCapMinYi float64 `json:"float_cap_min_yi"` // 流通市值下限（亿元）
	FloatCapMaxYi float64 `json:"float_cap_max_yi"` // 流通市值上限（亿元）——排除超大盘「大票」
	TurnoverMin   float64 `json:"turnover_min"`     // 换手率下限（%）
	TurnoverMax   float64 `json:"turnover_max"`     // 换手率上限（%）；系统另有两级硬规则：>30% 一律排除、20~30% 高位排除
	MaxGain5dPct  float64 `json:"max_gain_5d_pct"`  // 近 5 日累计涨幅上限（%，追高保护；0=不限）
	ExcludeLimitUp bool   `json:"exclude_limit_up"` // 排除当日已封涨停（涨停买不进）
}

// defaultRecFilters 各类型的保护性默认：短线带追高保护，长线宽松。
// 价格/市值默认不限（个人偏好差异大，由用户在偏好或表单里配置）。
func defaultRecFilters(recType string) RecFilters {
	f := RecFilters{ExcludeLimitUp: true}
	if recType == model.RecTypeShortTerm {
		f.MaxGain5dPct = 25 // 散户实战共识：近5日涨幅>25% 追高风险大（20cm 板另计，见 gainCapFor）
	}
	return f
}

// sanitizeRecFilters 归一化用户输入：负值归零、区间反转交换、越界钳制。
func sanitizeRecFilters(f RecFilters) RecFilters {
	clampNonNeg := func(v *float64, max float64) {
		if *v < 0 {
			*v = 0
		}
		if *v > max {
			*v = max
		}
	}
	clampNonNeg(&f.PriceMin, 100000)
	clampNonNeg(&f.PriceMax, 100000)
	clampNonNeg(&f.FloatCapMinYi, 1e6)
	clampNonNeg(&f.FloatCapMaxYi, 1e6)
	// 换手区间钳制：上限=绝对硬顶 30%（防「必然空池」死局）；下限额外钳到 25%，
	// 给 [min,30] 留至少 5 个百分点的可行带（min=30 会退化成测度零区间）。
	clampNonNeg(&f.TurnoverMin, 25)
	clampNonNeg(&f.TurnoverMax, deadTurnoverHardPct)
	clampNonNeg(&f.MaxGain5dPct, 1000)
	if f.PriceMin > 0 && f.PriceMax > 0 && f.PriceMin > f.PriceMax {
		f.PriceMin, f.PriceMax = f.PriceMax, f.PriceMin
	}
	if f.FloatCapMinYi > 0 && f.FloatCapMaxYi > 0 && f.FloatCapMinYi > f.FloatCapMaxYi {
		f.FloatCapMinYi, f.FloatCapMaxYi = f.FloatCapMaxYi, f.FloatCapMinYi
	}
	if f.TurnoverMin > 0 && f.TurnoverMax > 0 && f.TurnoverMin > f.TurnoverMax {
		f.TurnoverMin, f.TurnoverMax = f.TurnoverMax, f.TurnoverMin
	}
	return f
}

// loadUserRecFilters 读取用户偏好里的默认筛选；缺失/解析失败回退类型默认。
func loadUserRecFilters(userID int64, recType string) RecFilters {
	def := defaultRecFilters(recType)
	if common.DB == nil {
		return def
	}
	var pref model.UserPreference
	if err := common.DB.Select("rec_filters_json").Where("user_id = ?", userID).First(&pref).Error; err != nil {
		return def
	}
	if strings.TrimSpace(pref.RecFiltersJSON) == "" {
		return def
	}
	var f RecFilters
	if json.Unmarshal([]byte(pref.RecFiltersJSON), &f) != nil {
		return def
	}
	return sanitizeRecFilters(f)
}

// Describe 生成人类可读的条件清单（前端回显 + 组合进批次标题）。空切片=无附加条件。
func (f RecFilters) Describe() []string {
	var out []string
	switch {
	case f.PriceMin > 0 && f.PriceMax > 0:
		out = append(out, fmt.Sprintf("股价%s~%s元", trimFloat(f.PriceMin), trimFloat(f.PriceMax)))
	case f.PriceMax > 0:
		out = append(out, fmt.Sprintf("股价≤%s元", trimFloat(f.PriceMax)))
	case f.PriceMin > 0:
		out = append(out, fmt.Sprintf("股价≥%s元", trimFloat(f.PriceMin)))
	}
	switch {
	case f.FloatCapMinYi > 0 && f.FloatCapMaxYi > 0:
		out = append(out, fmt.Sprintf("流通市值%s~%s亿", trimFloat(f.FloatCapMinYi), trimFloat(f.FloatCapMaxYi)))
	case f.FloatCapMaxYi > 0:
		out = append(out, fmt.Sprintf("流通市值≤%s亿", trimFloat(f.FloatCapMaxYi)))
	case f.FloatCapMinYi > 0:
		out = append(out, fmt.Sprintf("流通市值≥%s亿", trimFloat(f.FloatCapMinYi)))
	}
	switch {
	case f.TurnoverMin > 0 && f.TurnoverMax > 0:
		out = append(out, fmt.Sprintf("换手%s%%~%s%%", trimFloat(f.TurnoverMin), trimFloat(f.TurnoverMax)))
	case f.TurnoverMax > 0:
		out = append(out, fmt.Sprintf("换手≤%s%%", trimFloat(f.TurnoverMax)))
	case f.TurnoverMin > 0:
		out = append(out, fmt.Sprintf("换手≥%s%%", trimFloat(f.TurnoverMin)))
	}
	if f.MaxGain5dPct > 0 {
		out = append(out, fmt.Sprintf("排除近5日涨幅>%s%%", trimFloat(f.MaxGain5dPct)))
	}
	if f.ExcludeLimitUp {
		out = append(out, "排除已涨停")
	}
	return out
}

// trimFloat 去掉无意义的小数尾零（8.00 → 8，8.50 → 8.5）。
func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// limitUpPctFor A 股按代码/名称判涨停幅度（%）：主板 10、创业板/科创板 20、北交所 30、ST 5。
func limitUpPctFor(symbol, name string) float64 {
	up := strings.ToUpper(name)
	if strings.Contains(up, "ST") {
		return 5
	}
	switch {
	case strings.HasPrefix(symbol, "30"), strings.HasPrefix(symbol, "68"):
		return 20
	case strings.HasPrefix(symbol, "8"), strings.HasPrefix(symbol, "4"), strings.HasPrefix(symbol, "92"):
		return 30
	}
	return 10
}

// gainCapFor 追高保护上限按板块放大：20cm 板的 5 日涨幅上限放大 1.6 倍（25% → 40%）。
func gainCapFor(base float64, symbol string) float64 {
	if base <= 0 {
		return 0
	}
	if limitUpPctFor(symbol, "") >= 20 {
		return base * 1.6
	}
	return base
}

// isAtLimitUp 当日是否已封涨停（买不进）。优先用腾讯估值快照的涨停价，
// 缺失时按涨跌幅接近板块涨停幅度近似判定。
func isAtLimitUp(c candidate) bool {
	if c.Price <= 0 {
		return false
	}
	if c.LimitUp > 0 {
		return c.Price >= c.LimitUp-0.005
	}
	lim := limitUpPctFor(c.Symbol, c.Name)
	return c.ChangePct >= lim-0.3
}

// applyQuoteFilters 阶段②：对候选执行用户筛选（仅依赖行情/估值快照的条件），
// 返回排除原因；空串=通过。近 5 日涨幅条件依赖日线，在阶段③评分后判（applyGainFilter）。
// 估值字段缺失（=0）时对应条件跳过不判（不惩罚数据缺口，保持透明由前端标注）。
func applyQuoteFilters(c candidate, f RecFilters) string {
	// ETF/场内基金不适用个股推荐逻辑（无个股估值、涨停幅按代码前缀会被误判）：
	// 透明标记排除，仅经自选进入的基金会走到这里（榜单源 hs_a 只含个股）。
	if isCNFund(c.Symbol) {
		return "ETF/基金不参与个股推荐"
	}
	if f.PriceMin > 0 && c.Price < f.PriceMin {
		return fmt.Sprintf("股价 %.2f 低于下限 %s", c.Price, trimFloat(f.PriceMin))
	}
	if f.PriceMax > 0 && c.Price > f.PriceMax {
		return fmt.Sprintf("股价 %.2f 超出上限 %s（资金约束）", c.Price, trimFloat(f.PriceMax))
	}
	if c.FloatCap > 0 {
		capYi := c.FloatCap / 1e8
		if f.FloatCapMinYi > 0 && capYi < f.FloatCapMinYi {
			return fmt.Sprintf("流通市值 %.0f 亿低于下限 %s 亿", capYi, trimFloat(f.FloatCapMinYi))
		}
		if f.FloatCapMaxYi > 0 && capYi > f.FloatCapMaxYi {
			return fmt.Sprintf("流通市值 %.0f 亿超出上限 %s 亿", capYi, trimFloat(f.FloatCapMaxYi))
		}
	}
	if c.TurnoverRate > 0 {
		if f.TurnoverMin > 0 && c.TurnoverRate < f.TurnoverMin {
			return fmt.Sprintf("换手率 %.2f%% 低于下限 %s%%（缺乏活跃度）", c.TurnoverRate, trimFloat(f.TurnoverMin))
		}
		if f.TurnoverMax > 0 && c.TurnoverRate > f.TurnoverMax {
			return fmt.Sprintf("换手率 %.2f%% 超出上限 %s%%（换手过热）", c.TurnoverRate, trimFloat(f.TurnoverMax))
		}
	}
	if c.TurnoverRate > deadTurnoverHardPct {
		return fmt.Sprintf("换手率 %.2f%% 超过 %v%%（极端换手，无论位置高低大概率异常）", c.TurnoverRate, deadTurnoverHardPct)
	}
	if f.ExcludeLimitUp && isAtLimitUp(c) {
		return "当日已封涨停，实际无法买入"
	}
	return ""
}

// 换手率两级阈值：>30% 无条件硬拦（极端换手）；20~30% 推迟到阶段③结合 60 日
// 区间位置判定——高位放量是「死亡换手」出货形态要排除，低位放量可能是启动，
// 一刀切会把换手率榜来源几乎清空（该榜前排常年 20%+），保留但评分标注风险。
// sanitizeRecFilters 的用户区间钳制上限必须等于 deadTurnoverHardPct（防空池死局）。
const (
	deadTurnoverPct     = 20.0
	deadTurnoverHardPct = 30.0
)

// applyTurnoverPosFilter 阶段③补充：换手 20~30% 区间的位置判定（依赖 pos_60 因子）。
// 空串=通过。高位（60 日区间位置 ≥65%）+高换手=经典出货形态，排除并给透明原因；
// 低位高换手保留，由 scorePool 统一扣分标注（放量启动与对倒出货并存，交给 LLM 与用户权衡）。
func applyTurnoverPosFilter(c candidate, factors *candFactors) string {
	if factors == nil || c.TurnoverRate <= deadTurnoverPct {
		return ""
	}
	if factors.Pos60 >= 65 {
		return fmt.Sprintf("换手率 %.1f%% 且处 60 日区间 %.0f%% 高位（高位死亡换手，大概率出货）", c.TurnoverRate, factors.Pos60)
	}
	return ""
}

// applyGainFilter 阶段③补充：近 5 日涨幅追高保护（依赖日线因子）。空串=通过。
func applyGainFilter(c candidate, factors *candFactors, f RecFilters) string {
	if f.MaxGain5dPct <= 0 || factors == nil {
		return ""
	}
	cap := gainCapFor(f.MaxGain5dPct, c.Symbol)
	if factors.Chg5d > cap {
		return fmt.Sprintf("近5日涨幅 %.1f%% 超过 %s%%（追高保护）", factors.Chg5d, trimFloat(cap))
	}
	return ""
}
