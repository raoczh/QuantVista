package service

import (
	"fmt"
	"sort"
	"strings"
)

// 辩论的可引用事实直接来自冻结快照。主分析没有引用某项事实（尤其是财务风险）
// 不能使后续复核失去它；模型计划价和用户文本也不能进入事实白名单。
func buildDebateEvidenceIndex(snapshot map[string]any, ev *evidenceCheck) ([]debateEvidenceRef, map[string]bool) {
	values := snapshotLabeledValues(snapshot, stockFieldHints(snapshot))
	priority := map[string]int{}
	for i, path := range []string{
		"quote.price", "quote.change_pct", "finance.latest.net_profit", "finance.latest.deduct_profit",
		"finance.comparable.net_profit", "finance.latest.net_profit_yoy", "finance.latest.revenue_yoy",
		"finance.annual.roe", "technicals.ma5", "technicals.ma20", "technicals.ma60", "technicals.atr14",
		"technicals.rsi14", "quote.amount", "valuation.pe_ttm", "valuation.pb", "finance.latest.ocf_ps",
	} {
		priority[path] = i
	}
	var facts []labeledValue
	for _, value := range values {
		if value.Derived || value.Origin != "" || !finiteRecNumber(value.Value) || !debateSnapshotFactPath(value.Path) {
			continue
		}
		facts = append(facts, value)
	}
	rank := func(path string) int {
		if n, ok := priority[path]; ok {
			return n
		}
		return len(priority)
	}
	sort.SliceStable(facts, func(i, j int) bool {
		if a, b := rank(facts[i].Path), rank(facts[j].Path); a != b {
			return a < b
		}
		return facts[i].Path < facts[j].Path
	})
	refs := make([]debateEvidenceRef, 0, debateEvidenceMax)
	allow := make(map[string]bool)
	seen := make(map[string]bool)
	for _, fact := range facts {
		if seen[fact.Path] {
			continue
		}
		seen[fact.Path] = true
		if fact.AsOf == "" {
			if finance, ok := snapshot["finance"].(map[string]any); ok {
				for _, section := range []string{"latest", "annual", "comparable"} {
					if strings.HasPrefix(fact.Path, "finance."+section+".") {
						if report, ok := finance[section].(map[string]any); ok {
							fact.AsOf, _ = report["report_date"].(string)
						}
					}
				}
			}
		}
		id := ""
		// 同一事实已在主分析中核验时复用证据编号，便于页面交叉核对。
		// 只复用与真实快照路径和数值完全一致的编号，不从主分析补造事实。
		if ev != nil {
			for _, item := range ev.Items {
				if item.Matched && item.Origin == "" && item.EvidenceID != "" && !allow[item.EvidenceID] &&
					item.Path == fact.Path && item.SnapValue == fact.Value {
					id = item.EvidenceID
					break
				}
			}
		}
		if id == "" {
			id = fmt.Sprintf("dx-%03d", len(refs)+1)
			for allow[id] {
				id += "x"
			}
		}
		refs = append(refs, debateEvidenceRef{
			EvidenceID: id, Path: fact.Path, Value: fact.Value, Unit: fact.Unit,
			AsOf: fact.AsOf, Source: fact.Source,
		})
		allow[id] = true
		if len(refs) == debateEvidenceMax {
			break
		}
	}
	return refs, allow
}

func debateSnapshotFactPath(path string) bool {
	for _, prefix := range []string{"quote.", "technicals.", "valuation.", "finance.latest.", "finance.annual.", "finance.comparable.", "org_view."} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
