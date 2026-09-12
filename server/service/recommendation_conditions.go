package service

import (
	"fmt"
	"math"
	"strings"
)

const (
	strategyMatched = "matched"
	strategyMissed  = "missed"
	strategyUnknown = "unknown"
)

// 三值逻辑：false AND unknown 为 false，true OR unknown 为 true。
// 未知不会被 is_false、未命中比例或 OR 分支误当作否定证据。
func strategyNodeState(t *FactorTable, n *CondNode) string {
	if n == nil {
		return strategyUnknown
	}
	children, any := n.All, false
	if len(n.Any) > 0 {
		children, any = n.Any, true
	}
	if len(children) > 0 {
		unknown := false
		for i := range children {
			state := strategyNodeState(t, &children[i])
			if any && state == strategyMatched {
				return strategyMatched
			}
			if !any && state == strategyMissed {
				return strategyMissed
			}
			unknown = unknown || state == strategyUnknown
		}
		if unknown {
			return strategyUnknown
		}
		if any {
			return strategyMissed
		}
		return strategyMatched
	}
	for _, key := range []string{n.Factor, n.Ref} {
		if key == "" {
			continue
		}
		col := t.Col(key)
		if len(col) == 0 || !finiteRecNumber(col[0]) {
			return strategyUnknown
		}
	}
	if evalCondRow(t, n, 0) {
		return strategyMatched
	}
	return strategyMissed
}

func strategyConditionReason(strat *strategyTemplate, h *StrategyHit) string {
	if strat == nil || strat.screen == nil {
		return ""
	}
	if h == nil || h.Total == 0 || h.Status == strategyUnknown {
		return "策略条件数据不足，无法确认所选策略成立"
	}
	if !h.Full {
		return "不满足所选策略的必要条件：" + strings.Join(h.Missed, "；")
	}
	return ""
}

type strategyCurrentCheck struct {
	Status  string   `json:"status"`
	Checked int      `json:"checked"`
	Missed  []string `json:"missed,omitempty"`
	Unknown []string `json:"unknown,omitempty"`
}

// 只复核可明确解释为当前价格约束的条件。K 线阴阳、当日量价、金叉等仍由
// 完整收盘日线判定；不能给历史 K 线换一个现价便宣称盘中形态已经成立。
func isCurrentPriceCondition(n *CondNode) bool {
	if n.Factor == "close" {
		return n.Ref == "" || strings.HasPrefix(n.Ref, "ma") || strings.HasPrefix(n.Ref, "boll_")
	}
	switch n.Factor {
	case "above_ma20", "above_ma60", "above_ma120", "bias_20", "bias_250", "boll_pos", "chg_5d", "chg_20d", "chg_60d":
		return true
	}
	return false
}

func currentStrategyCheck(strat *strategyTemplate, c candidate, price float64) *strategyCurrentCheck {
	if strat == nil || strat.screen == nil || strat.tree == nil {
		return nil
	}
	if c.StrategyHit == nil || len(c.StrategyHit.row) != len(factorDefs) {
		return &strategyCurrentCheck{Status: strategyUnknown, Unknown: []string{"缺少冻结的收盘因子"}}
	}
	base := singleRowFactorTable(c.StrategyHit.row)
	vals := append([]float64(nil), c.StrategyHit.row...)
	set := func(key string, value float64) { vals[factorIndex[key]] = value }
	get := func(key string) float64 { return vals[factorIndex[key]] }
	set("close", price)
	for _, key := range []string{"ma20", "ma60", "ma120", "ma250"} {
		ma := get(key)
		if key != "ma250" {
			v := math.NaN()
			if ma > 0 && finiteRecNumber(ma) && price > 0 {
				v = 0
				if price >= ma {
					v = 1
				}
			}
			set("above_"+key, v)
		}
		if key == "ma20" || key == "ma250" {
			v := math.NaN()
			if ma > 0 && finiteRecNumber(ma) {
				v = round2((price/ma - 1) * 100)
			}
			set("bias_"+strings.TrimPrefix(key, "ma"), v)
		}
	}
	set("boll_pos", math.NaN())
	if hi, lo := get("boll_up"), get("boll_low"); hi > lo && lo > 0 && finiteRecNumber(hi) {
		set("boll_pos", round2((price-lo)/(hi-lo)*100))
	}
	for _, days := range []int{5, 20, 60} {
		value, ok := currentReturnAt(c, price, days)
		if !ok {
			value = math.NaN()
		}
		set(fmt.Sprintf("chg_%dd", days), value)
	}
	live := singleRowFactorTable(vals)
	var walk func(*CondNode) strategyCurrentCheck
	walk = func(n *CondNode) strategyCurrentCheck {
		children, any := n.All, false
		if len(n.Any) > 0 {
			children, any = n.Any, true
		}
		if len(children) == 0 {
			if !isCurrentPriceCondition(n) {
				return strategyCurrentCheck{Status: strategyNodeState(base, n)}
			}
			out := strategyCurrentCheck{Status: strategyNodeState(live, n), Checked: 1}
			if out.Status == strategyMissed {
				out.Missed = []string{describeLeafWithValue(live, n)}
			}
			if out.Status == strategyUnknown {
				out.Unknown = []string{describeLeafWithValue(live, n)}
			}
			return out
		}
		out := strategyCurrentCheck{Status: strategyMatched}
		matched, missed, unknown := false, false, false
		for i := range children {
			r := walk(&children[i])
			out.Checked += r.Checked
			out.Missed = append(out.Missed, r.Missed...)
			out.Unknown = append(out.Unknown, r.Unknown...)
			matched = matched || r.Status == strategyMatched
			missed = missed || r.Status == strategyMissed
			unknown = unknown || r.Status == strategyUnknown
		}
		if any {
			if matched {
				out.Missed = nil
				out.Unknown = nil
			} else if unknown {
				out.Status = strategyUnknown
			} else {
				out.Status = strategyMissed
			}
		} else if missed {
			out.Status = strategyMissed
		} else if unknown {
			out.Status = strategyUnknown
		}
		return out
	}
	result := walk(strat.tree)
	return &result
}

func currentStrategyReason(check *strategyCurrentCheck) string {
	if check == nil || check.Status == strategyMatched {
		return ""
	}
	if check.Status == strategyUnknown {
		return "当前价格约束数据不足，无法确认所选策略仍适用"
	}
	return "当前价格已不满足所选策略约束：" + strings.Join(check.Missed, "；")
}
