package service

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// 通用信任层：程序化证据核验中与模块无关的部分。
// 推荐域的 verifyEvidence（recfactor.go）与 分析/问答/对比/日报 共用同一套
// 数字提取、噪声跳过与容差匹配规则，保证全站「数据核验」徽章口径一致。
// 改动跳过规则/容差时只改这里，各模块自动同步。

// verifyEvidenceValues 对若干段文本提取数字并与合法值域容差比对。
// 跳过规则：无小数点整数中的 ≤99 小整数（rank/池大小/天数/「近5日」「MA20」）、
// 常用窗口参数（evidenceNoiseInts）、1900~2100 年份、≥1e5（六位股票代码）；
// 容差 = max(0.02, |v|*2%)，同时试绝对值匹配（负涨幅 vs 正引用）。
// 这是启发式核对：matched 高说明模型确在引用真实数据；unmatched 交给用户肉眼复核。
func verifyEvidenceValues(texts []string, vals []float64) *evidenceCheck {
	check := &evidenceCheck{}
	for _, line := range texts {
		for _, tok := range evidenceNumRe.FindAllString(line, -1) {
			num, err := strconv.ParseFloat(tok, 64)
			if err != nil {
				continue
			}
			if !strings.Contains(tok, ".") {
				abs := math.Abs(num)
				if abs <= 99 || evidenceNoiseInts[abs] || (abs >= 1900 && abs <= 2100) || abs >= 1e5 {
					continue
				}
			}
			check.Total++
			matched := false
			for _, v := range vals {
				tol := math.Max(0.02, math.Abs(v)*0.02)
				if math.Abs(num-v) <= tol || math.Abs(math.Abs(num)-math.Abs(v)) <= tol {
					matched = true
					break
				}
			}
			if matched {
				check.Matched++
			} else if len(check.Unmatched) < 10 {
				check.Unmatched = append(check.Unmatched, tok)
			}
		}
	}
	return check
}

// decimalNumbersIn 从字符串集合提取带小数点的数字，供把「文本型合法数据源」并入核验值域——
// 如日报快照里的提醒文案（含触发价/MA 值）、问答里用户提问中的假设价位：它们确实喂给了模型、
// 是合法引用来源，但不是 JSON 数值叶子，snapshotValueSet 收集不到，漏掉会把忠实引用误报为幻觉
//（与推荐域 verifyEvidence 的 extra 变参同一道理）。只取带小数点的（整数已被跳过规则处理）。
func decimalNumbersIn(texts []string) []float64 {
	var vals []float64
	for _, t := range texts {
		for _, tok := range evidenceNumRe.FindAllString(t, -1) {
			if !strings.Contains(tok, ".") {
				continue
			}
			if v, err := strconv.ParseFloat(tok, 64); err == nil && v != 0 {
				vals = append(vals, v)
			}
		}
	}
	return vals
}

// snapshotValueSet 从任意快照结构（map/struct，经 JSON 归一化）递归收集数值，
// 组成证据核验的合法值域。exclude 中的键整棵子树跳过——如个股快照的
// recent_bars：30 根 OHLCV 数值密度过大，纳入后几乎任何数字都能撞上 2% 容差，
// 核验会失真。绝对值 ≥1e8 的数值同时并入 /1e8 亿元换算（成交额/市值常以「亿」被引用）。
func snapshotValueSet(snapshot any, exclude ...string) []float64 {
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
	var vals []float64
	var walk func(node any)
	walk = func(node any) {
		switch t := node.(type) {
		case map[string]any:
			for k, v := range t {
				if skip[k] {
					continue
				}
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		case float64:
			if t != 0 {
				vals = append(vals, t)
				if math.Abs(t) >= 1e8 {
					vals = append(vals, t/1e8)
				}
			}
		}
	}
	walk(root)
	return vals
}
