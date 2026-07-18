package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 通用信任层：程序化「数值存在性核验」——把 AI 文本里引用的数字规范化（单位/方向）后，
// 与带字段路径的数据快照值域逐一比对，产出可展开的逐项明细 items[]。
// 推荐域的 verifyEvidence（recfactor.go）与 分析/问答/对比/日报 共用同一套引擎，全站口径一致。
//
// 定位：这是「数值存在性核验」——只验证 AI 引用的数字能否在快照对应字段找到近似值，
// 不代表字段语义正确、更不代表整段结论正确。字段级语义化核验是后续工作。
// 改动跳过规则/容差/方向规则时只改这里，各模块自动同步。

// labeledValue 带字段路径的快照数值（值域的一员）。Path 供 items 明示命中来源；
// Unit 记规范化口径；AsOf 为该字段的数据时间（尽力而为）；Derived 标记亿元衍生值。
type labeledValue struct {
	Path    string
	Value   float64
	Unit    string
	AsOf    string
	Derived bool
}

// evidenceSection 一段待核验文本及其所属模块（供 items 标注数字出处）。
type evidenceSection struct {
	Module string
	Text   string
}

// evidenceItem 单个被核验数字的明细。
type evidenceItem struct {
	Raw       string  `json:"raw"`                 // 原始 token（含符号/单位前的数字文本）
	Value     float64 `json:"value"`               // 规范化后的数值（单位换算后）
	Unit      string  `json:"unit,omitempty"`      // 识别到的单位：亿/万/元/%/手/股/倍
	Direction string  `json:"direction,omitempty"` // up | down（识别到方向词/符号时）
	Sentence  string  `json:"sentence,omitempty"`  // 所在原句（≤60 rune 截断）
	Module    string  `json:"module,omitempty"`    // 所在模块（回答/总结/风险…）
	Count     int     `json:"count,omitempty"`     // 同一 (值,单位,方向) 出现次数
	Matched   bool    `json:"matched"`
	Path      string  `json:"path,omitempty"`       // 命中的快照字段路径（未命中时为最接近符号相反候选）
	SnapValue float64 `json:"snap_value,omitempty"` // 命中的快照值
	Tolerance float64 `json:"tolerance,omitempty"`  // 使用的容差
	AsOf      string  `json:"as_of,omitempty"`      // 命中字段的数据时间
	Reason    string  `json:"reason,omitempty"`     // 未命中原因：not_found | direction_mismatch
}

// 单位后缀 → 缩放系数（%/元/手/股/倍 不缩放，只记单位口径）。
var evidenceUnitScale = map[string]float64{
	"亿元": 1e8, "亿": 1e8, "万元": 1e4, "万": 1e4,
	"元": 1, "%": 1, "％": 1, "手": 1, "股": 1, "倍": 1,
}

// 单位后缀按长度降序尝试（先匹配「亿元」再「亿」）。
var evidenceUnitSuffixes = []string{"亿元", "万元", "亿", "万", "元", "％", "%", "手", "股", "倍"}

// 方向词（出现在数字前窗口内时，约束「跨符号绝对值匹配」）。
var evidenceDownWords = []string{"下跌", "跌破", "回落", "回撤", "净流出", "流出", "下调", "跌", "降"}
var evidenceUpWords = []string{"上涨", "涨至", "突破", "净流入", "流入", "上调", "涨", "升"}

// verifyEvidenceLabeled 对若干段文本提取数字，规范化（单位/方向）后与带路径的快照值域比对，
// 产出计数与逐项明细。核验语义：
//   - 跳过：无小数点整数 ≤99 / 惯用窗口参数 / 1900~2100 年份 / ≥1e5 六位代码（计入 SkippedCount）；
//     但带显式单位（亿/万/元/%/手/股）的整数不跳过（「5 亿」「3000 万」有效）；
//   - 去重：(规范化值, 单位, 方向) 唯一，重复出现累加 item.Count，Total/Matched 按去重项计；
//   - 匹配：带符号直配恒允许（容差 max(0.02,|v|*2%)）；跨符号（绝对值）匹配仅当方向词/符号存在
//     且与快照值符号一致时允许，否则记 direction_mismatch（取消旧的无条件绝对值匹配）；
//   - items 上限 50（Truncated 标记），Unmatched（legacy）保留 ≤10 供旧前端兼容。
func verifyEvidenceLabeled(sections []evidenceSection, vals []labeledValue) *evidenceCheck {
	check := &evidenceCheck{Version: "ev2"}
	type key struct {
		v    float64
		unit string
		dir  string
	}
	seen := make(map[key]int) // key -> items 下标
	items := make([]*evidenceItem, 0, 16)

	for _, sec := range sections {
		text := sec.Text
		runes := []rune(text)
		for _, loc := range evidenceNumRe.FindAllStringIndex(text, -1) {
			tok := text[loc[0]:loc[1]]
			num, err := strconv.ParseFloat(tok, 64)
			if err != nil {
				continue
			}
			// 识别紧跟的单位后缀。
			unit := ""
			after := strings.TrimLeft(text[loc[1]:], " ")
			for _, suf := range evidenceUnitSuffixes {
				if strings.HasPrefix(after, suf) {
					unit = suf
					break
				}
			}
			hasUnit := unit != ""
			// 跳过规则（无小数点整数），但带单位的整数照收。
			if !strings.Contains(tok, ".") && !hasUnit {
				abs := math.Abs(num)
				if abs <= 99 || evidenceNoiseInts[abs] || (abs >= 1900 && abs <= 2100) || abs >= 1e5 {
					check.SkippedCount++
					continue
				}
			}
			// 规范化单位。
			unitNorm := normalizeUnit(unit)
			scale := 1.0
			if s, ok := evidenceUnitScale[unit]; ok {
				scale = s
			}
			scaled := num * scale
			// 匹配候选：换算值优先；带亿/万单位时快照字段可能已以「亿/万」为单位落库
			//（如 main_net_yi=23.5），原值也作候选，避免单位口径错配漏命中。
			cands := []float64{scaled}
			if scale != 1 {
				cands = append(cands, num)
			}
			// 识别方向（前窗口方向词，或 token 自带符号）。
			dir := directionOf(runes, loc[0], text, tok, num)

			k := key{v: math.Round(scaled*1e4) / 1e4, unit: unitNorm, dir: dir}
			if idx, ok := seen[k]; ok {
				items[idx].Count++
				continue
			}
			it := &evidenceItem{
				Raw: tok, Value: round2(scaled), Unit: unitNorm, Direction: dir,
				Module: sec.Module, Count: 1,
				Sentence: sentenceAround(text, loc[0], loc[1]),
			}
			matchLabeled(it, cands, dir, vals)
			seen[k] = len(items)
			items = append(items, it)
		}
	}

	check.Total = len(items)
	for _, it := range items {
		if it.Matched {
			check.Matched++
		}
	}
	check.UnmatchedTotal = check.Total - check.Matched
	if len(items) > 50 {
		check.Truncated = true
		items = items[:50]
	}
	check.Items = make([]evidenceItem, 0, len(items))
	for _, it := range items {
		check.Items = append(check.Items, *it)
		if !it.Matched && len(check.Unmatched) < 10 {
			check.Unmatched = append(check.Unmatched, it.Raw)
		}
	}
	return check
}

// matchLabeled 在值域中为一个规范化数字找命中项，回填明细。cands 为候选值（换算值 + 原值）。
func matchLabeled(it *evidenceItem, cands []float64, dir string, vals []labeledValue) {
	var oppo *labeledValue // 仅差符号的候选（用于 direction_mismatch 说明）
	for i := range vals {
		v := vals[i]
		tol := math.Max(0.02, math.Abs(v.Value)*0.02)
		for _, scaled := range cands {
			// 带符号直配恒允许。
			if math.Abs(scaled-v.Value) <= tol {
				it.Matched = true
				it.Path = v.Path
				it.SnapValue = round2(v.Value)
				it.Tolerance = round2(tol)
				it.AsOf = v.AsOf
				return
			}
			// 绝对值相等但符号相反：仅当方向词与快照符号一致时才放行。
			if math.Abs(math.Abs(scaled)-math.Abs(v.Value)) <= tol {
				if dir != "" && directionMatchesSign(dir, v.Value) {
					it.Matched = true
					it.Path = v.Path
					it.SnapValue = round2(v.Value)
					it.Tolerance = round2(tol)
					it.AsOf = v.AsOf
					return
				}
				if oppo == nil {
					vv := v
					oppo = &vv
				}
			}
		}
	}
	if oppo != nil {
		it.Reason = "direction_mismatch"
		it.Path = oppo.Path
		it.SnapValue = round2(oppo.Value)
	} else {
		it.Reason = "not_found"
	}
}

// directionMatchesSign 方向词与快照值符号是否一致（down↔负、up↔正）。
func directionMatchesSign(dir string, snap float64) bool {
	if dir == "down" {
		return snap < 0
	}
	if dir == "up" {
		return snap > 0
	}
	return false
}

// directionOf 判定数字的方向：token 自带负号 → down、正号 → up；否则看前 ≤6 rune 窗口方向词。
func directionOf(runes []rune, byteStart int, text, tok string, num float64) string {
	if strings.HasPrefix(tok, "-") || num < 0 {
		return "down"
	}
	if strings.HasPrefix(tok, "+") {
		return "up"
	}
	// 前窗口：取 token 起始字节位置对应的 rune 索引前 6 个 rune。
	prefix := text[:byteStart]
	pr := []rune(prefix)
	start := len(pr) - 6
	if start < 0 {
		start = 0
	}
	window := string(pr[start:])
	// down 优先（「跌」比「涨」更需要严格拦截误报）。
	for _, w := range evidenceDownWords {
		if strings.Contains(window, w) {
			return "down"
		}
	}
	for _, w := range evidenceUpWords {
		if strings.Contains(window, w) {
			return "up"
		}
	}
	return ""
}

// normalizeUnit 单位口径归一（亿元→亿、万元→万、％→%）。
func normalizeUnit(u string) string {
	switch u {
	case "亿元":
		return "亿"
	case "万元":
		return "万"
	case "％":
		return "%"
	default:
		return u
	}
}

// sentenceAround 定位 token 所在句子（按中文句读切分），过长截断至 60 rune。
func sentenceAround(text string, start, end int) string {
	seps := "。！？；\n"
	from := 0
	if i := strings.LastIndexAny(text[:start], seps); i >= 0 {
		_, size := utf8.DecodeRuneInString(text[i:])
		from = i + size
	}
	to := len(text)
	if i := strings.IndexAny(text[end:], seps); i >= 0 {
		to = end + i
	}
	sent := strings.TrimSpace(text[from:to])
	r := []rune(sent)
	if len(r) <= 60 {
		return sent
	}
	return string(r[:59]) + "…"
}

// snapshotLabeledValues 从快照结构（map/struct，经 JSON 归一化）递归收集带路径的数值。
// exclude 中的键整棵子树跳过（如 recent_bars）。≥1e8 追加亿元衍生值（Derived）。
// asOfHints：路径前缀 → 数据时间（"" 通配全部），命中前缀者填 AsOf。
func snapshotLabeledValues(snapshot any, asOfHints map[string]string, exclude ...string) []labeledValue {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	skip := make(map[string]bool, len(exclude))
	for _, k := range exclude {
		skip[k] = true
	}
	asOfFor := func(path string) string {
		best := ""
		bestLen := -1
		for pre, ts := range asOfHints {
			if strings.HasPrefix(path, pre) && len(pre) > bestLen {
				best, bestLen = ts, len(pre)
			}
		}
		return best
	}
	var vals []labeledValue
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch t := node.(type) {
		case map[string]any:
			for k, v := range t {
				if skip[k] {
					continue
				}
				child := k
				if path != "" {
					child = path + "." + k
				}
				walk(v, child)
			}
		case []any:
			for i, v := range t {
				seg := fmt.Sprintf("%s[%d]", path, i)
				// 数组元素为含 symbol 的对象时用 symbol 作段名，可读性更好。
				if m, ok := v.(map[string]any); ok {
					if sym, ok := m["symbol"].(string); ok && sym != "" {
						seg = fmt.Sprintf("%s[%s]", path, sym)
					}
				}
				walk(v, seg)
			}
		case float64:
			if t != 0 {
				vals = append(vals, labeledValue{Path: path, Value: t, AsOf: asOfFor(path)})
				if math.Abs(t) >= 1e8 {
					vals = append(vals, labeledValue{Path: path + "(亿)", Value: t / 1e8, Unit: "亿", AsOf: asOfFor(path), Derived: true})
				}
			}
		}
	}
	walk(root, "")
	return vals
}

// textLabeledValues 从文本型合法来源（新闻标题/用户提问/提醒文案）提取数值，标注来源为 Path。
// 取带小数点的数字 + 带亿/万单位的整数（换算后入域，修「3000 万」进不了值域的旧误报）。
func textLabeledValues(label string, texts []string) []labeledValue {
	var vals []labeledValue
	for _, t := range texts {
		for _, loc := range evidenceNumRe.FindAllStringIndex(t, -1) {
			tok := t[loc[0]:loc[1]]
			v, err := strconv.ParseFloat(tok, 64)
			if err != nil || v == 0 {
				continue
			}
			after := strings.TrimLeft(t[loc[1]:], " ")
			unit := ""
			for _, suf := range []string{"亿元", "万元", "亿", "万"} {
				if strings.HasPrefix(after, suf) {
					unit = suf
					break
				}
			}
			if !strings.Contains(tok, ".") && unit == "" {
				continue // 无小数、无亿/万单位的整数不取（交给核验侧跳过规则）
			}
			scale := 1.0
			if s, ok := evidenceUnitScale[unit]; ok {
				scale = s
			}
			vals = append(vals, labeledValue{Path: label, Value: v * scale, Unit: normalizeUnit(unit)})
		}
	}
	return vals
}

// stockAsOfHints 从个股快照的新鲜度元数据组装 as_of 提示（旧快照无元数据返回空 map）。
func stockAsOfHints(snap map[string]any) map[string]string {
	hints := map[string]string{}
	if s, ok := snap["quote_as_of"].(string); ok && s != "" {
		hints["quote."] = s
	}
	if s, ok := snap["bars_as_of"].(string); ok && s != "" {
		hints["technicals."] = s
		hints["quant_score."] = s
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}
