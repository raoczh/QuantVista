package service

import (
	"errors"
	"fmt"
	"sort"

	"quantvista/common"
	"quantvista/model"
)

// S0-6 确定性错误归因报表（纯 SQL/Go，不依赖 LLM）：成熟标签样本按
// 「入场特征桶 × 市场状态 × 策略 × 来源 × 行业」分组统计胜率/中位收益/尾部亏损，
// 定位系统性亏损集中在哪类推荐。LLM 反思（S2，延后至成熟样本 ≥30）是它的补充非替代。
//
// 口径：只统计 entry_mode=next_open 且 recommendation_id>0（真实入选、统一模拟成交）
// 的 matured 样本——影子标签（门控/落选对照）与用户执行事实不混入本报表，
// 各有专属评估（risk-coverage / 执行差异）。

// AttributionCell 单分组的统计格。
type AttributionCell struct {
	Dim    string `json:"dim"` // 分组维度：chg5d_bucket / regime / strategy / source / industry / action
	Key    string `json:"key"` // 桶取值
	Sample int    `json:"sample"`

	WinRate       float64 `json:"win_rate"`        // net_return > 0 比例（%）
	AvgNetPct     float64 `json:"avg_net_pct"`
	MedianNetPct  float64 `json:"median_net_pct"`
	P10NetPct     float64 `json:"p10_net_pct"`      // 尾部亏损分位（10% 分位净收益）
	SevereLossPct float64 `json:"severe_loss_pct"`  // net_return < -5% 比例（%）
	AvgAlphaPct   float64 `json:"avg_alpha_pct"`    // 基准有效样本的扣成本 Alpha 均值
	AlphaSample   int     `json:"alpha_sample"`
}

// AttributionReport 归因报表（单一持有期口径；不同 horizon 分别请求，不混算）。
type AttributionReport struct {
	Type        string `json:"type"`
	HorizonDays int    `json:"horizon_days"`
	Sample      int    `json:"sample"`     // 成熟样本总数（buy+watch，action 维度的分母）
	SampleBuy   int    `json:"sample_buy"` // 其中 buy 样本数（除 action 外各维度的分母）
	Skipped     int    `json:"skipped"`
	Pending     int    `json:"pending"`
	Groups      []AttributionCell `json:"groups"`
	Notes       []string          `json:"notes"`
}

// attributionMinBucket 分组样本量低于该值时仍展示，但前端应标注「样本不足」。
const attributionMinBucket = 5

// RecAttribution 生成归因报表。recType 可空（全部）；horizon 必须 ∈ LabelHorizons。
func RecAttribution(userID int64, recType string, horizon int) (*AttributionReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	valid := false
	for _, h := range model.LabelHorizons {
		if h == horizon {
			valid = true
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("持有期须为 %v 之一", model.LabelHorizons)
	}

	q := common.DB.Where("user_id = ? AND horizon_days = ? AND entry_mode = ? AND recommendation_id > 0",
		userID, horizon, model.EntryModeNextOpen)
	if recType == model.RecTypeShortTerm || recType == model.RecTypeLongTerm {
		q = q.Where("type = ?", recType)
	}
	var rows []model.RecommendationLabel
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}

	rep := &AttributionReport{Type: recType, HorizonDays: horizon}
	var matured, maturedBuy []model.RecommendationLabel
	for _, r := range rows {
		switch r.MaturityStatus {
		case model.LabelMatured:
			matured = append(matured, r)
			if r.Action == model.RecActionBuy {
				maturedBuy = append(maturedBuy, r)
			}
		case model.LabelSkipped:
			rep.Skipped++
		case model.LabelPending:
			rep.Pending++
		}
	}
	rep.Sample = len(matured)
	rep.SampleBuy = len(maturedBuy)

	// action 维度用全量（buy vs watch 对照本身就是一组归因）；其余维度限定 buy——
	// 本报表回答「买入建议的错误集中在哪」，watch 混入会稀释买入胜率与错误归因。
	dims := []struct {
		dim  string
		rows []model.RecommendationLabel
		key  func(model.RecommendationLabel) string
	}{
		{"action", matured, func(l model.RecommendationLabel) string { return l.Action }},
		{"chg5d_bucket", maturedBuy, func(l model.RecommendationLabel) string { return chg5dBucket(l.EntryChg5dPct) }},
		{"regime", maturedBuy, func(l model.RecommendationLabel) string { return orDash(l.Regime) }},
		{"strategy", maturedBuy, func(l model.RecommendationLabel) string { return orDash(l.Strategy) }},
		{"source", maturedBuy, func(l model.RecommendationLabel) string { return orDash(l.Source) }},
		{"industry", maturedBuy, func(l model.RecommendationLabel) string { return orDash(l.Industry) }},
	}
	for _, d := range dims {
		groups := map[string][]model.RecommendationLabel{}
		var order []string
		for _, l := range d.rows {
			k := d.key(l)
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], l)
		}
		sort.Strings(order)
		for _, k := range order {
			rep.Groups = append(rep.Groups, summarizeCell(d.dim, k, groups[k]))
		}
	}
	rep.Notes = append(rep.Notes,
		"口径：统一执行模拟（次日开盘/涨停买不到/T+1/整百股/费率），净收益=扣佣金印花税；Alpha=净收益−上证同区间",
		"只统计真实入选且成熟的样本；影子标签（门控/落选对照）与用户实际成交不混入本报表",
		"action 维度并列展示 buy/watch；其余维度只统计 buy（买入准确性归因，watch 不稀释分组）",
		fmt.Sprintf("单分组样本 <%d 时统计不稳定，仅供方向参考", attributionMinBucket),
	)
	return rep, nil
}

func summarizeCell(dim, key string, list []model.RecommendationLabel) AttributionCell {
	c := AttributionCell{Dim: dim, Key: key, Sample: len(list)}
	if len(list) == 0 {
		return c
	}
	nets := make([]float64, 0, len(list))
	var sumNet, sumAlpha float64
	wins, severe := 0, 0
	for _, l := range list {
		nets = append(nets, l.NetReturnPct)
		sumNet += l.NetReturnPct
		if l.NetReturnPct > 0 {
			wins++
		}
		if l.NetReturnPct < -5 {
			severe++
		}
		if l.HasBench {
			c.AlphaSample++
			sumAlpha += l.AlphaPct
		}
	}
	sort.Float64s(nets)
	n := float64(len(list))
	c.WinRate = round2(float64(wins) / n * 100)
	c.AvgNetPct = round2(sumNet / n)
	c.MedianNetPct = round2(median(nets))
	c.P10NetPct = round2(percentileSorted(nets, 0.10))
	c.SevereLossPct = round2(float64(severe) / n * 100)
	if c.AlphaSample > 0 {
		c.AvgAlphaPct = round2(sumAlpha / float64(c.AlphaSample))
	}
	return c
}

// chg5dBucket 入场特征桶：近 5 日涨幅分档（追高程度）。
func chg5dBucket(v float64) string {
	switch {
	case v < -5:
		return "chg5d<-5%"
	case v < 0:
		return "chg5d -5~0%"
	case v < 5:
		return "chg5d 0~5%"
	case v < 15:
		return "chg5d 5~15%"
	default:
		return "chg5d>15%"
	}
}

func orDash(s string) string {
	if s == "" {
		return "（未知）"
	}
	return s
}

// percentileSorted 已升序序列的 p 分位（线性插值）。
func percentileSorted(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := p * float64(n-1)
	lo := int(pos)
	if lo >= n-1 {
		return sorted[n-1]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[lo+1]*frac
}
