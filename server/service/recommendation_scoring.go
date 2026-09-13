package service

import (
	"fmt"
	"math"
	"strings"
)

const recommendationScoringVersion = "qr2"

type recScoreComponent struct {
	Key    string   `json:"key"`
	Value  float64  `json:"value"`
	Status string   `json:"status"`
	Notes  []string `json:"notes,omitempty"`
}

type recScoreBreakdown struct {
	Version    string              `json:"version"`
	Profile    string              `json:"profile"`
	Valid      bool                `json:"valid"`
	Total      float64             `json:"total"`
	Components []recScoreComponent `json:"components"`
	Missing    []string            `json:"missing,omitempty"`
}

type recScoringComparison struct {
	LegacyVersion  string   `json:"legacy_version"`
	LegacyScore    float64  `json:"legacy_score"`
	QualityVersion string   `json:"quality_version"`
	QualityScore   float64  `json:"quality_score"`
	LearnedVersion string   `json:"learned_version,omitempty"`
	LearnedScore   *float64 `json:"learned_score,omitempty"`
	ModelHash      string   `json:"model_hash,omitempty"`
}

func qualityBaseScore(recType, profile string, sc ScoreResult, f *candFactors) (float64, []string, bool) {
	return qualityBaseScoreWithIntent(recType, profile, "", sc, f)
}

func qualityBaseScoreWithIntent(recType, profile, intent string, sc ScoreResult, f *candFactors) (float64, []string, bool) {
	if f == nil {
		return 0, []string{"技术因子"}, false
	}
	wt, wm, wp, wv, wr := strategyDimWeights(recType, profile)
	// 反转池的目标是确认超卖修复，不能在基础分阶段再次偏爱创高追涨。
	if intent == "reversal" {
		wt, wm, wp, wv, wr = 0.10, 0.05, 0.20, 0.25, 0.40
		sc.Position = 100 - sc.Position
	}
	if intent == "consolidation" {
		wt, wm, wp, wv, wr = 0.25, 0.05, 0.10, 0.15, 0.45
	}
	var total, weight float64
	var missing []string
	for _, d := range []struct {
		name          string
		value, weight float64
		available     bool
	}{
		{"趋势（60 日日线）", sc.Trend, wt, f.BarCount >= 60},
		{"动量（20 日收益）", sc.Momentum, wm, f.BarCount >= 21},
		{"位置（60 日日线）", sc.Position, wp, f.BarCount >= 60},
		{"成交量（20 日日线）", sc.Volume, wv, f.BarCount >= 20 && f.VolBoost > 0 && f.Vol5v20 > 0},
		{"波动风险（20 日收益）", sc.Risk, wr, f.BarCount >= 21},
	} {
		if d.available && finiteRecNumber(d.value) {
			total += d.value * d.weight
			weight += d.weight
		} else {
			missing = append(missing, d.name)
		}
	}
	if weight < 0.6 || weight == 0 {
		return 0, missing, false
	}
	return total / weight, missing, true
}

// 技术确认只保留形态质量这一组；不再在已有趋势/动量分之外逐项叠加 RSI、MACD、
// 创高和均线多头等高度相关的“强势证据”。每个组独立限幅，原始观测仍完整保存。
func scoreQualityCandidate(recType string, strat *strategyTemplate, c candidate, f *candFactors, sc ScoreResult) (*recScoreBreakdown, []string) {
	b := &recScoreBreakdown{Version: recommendationScoringVersion, Profile: strat.baseKey}
	base, missing, valid := qualityBaseScoreWithIntent(recType, strat.baseKey, strat.Intent, sc, f)
	b.Valid, b.Missing = valid, append(b.Missing, missing...)
	var notes []string
	add := func(key string, value float64, status string, why ...string) {
		value = math.Round(value*1e6) / 1e6
		b.Components = append(b.Components, recScoreComponent{Key: key, Value: value, Status: status, Notes: why})
		b.Total += value
		if len(why) > 0 {
			label := map[string]string{"technical": "技术基础", "setup": "形态质量", "entry": "入场质量", "risk": "交易风险", "fundamentals": "财务质量", "context": "信息背景"}[key]
			notes = append(notes, fmt.Sprintf("%s：%s（本组合计 %+.1f）", label, strings.Join(why, "；"), value))
		}
	}
	add("technical", base, "available", "按所选策略合成可用技术维度")
	if !valid {
		b.Missing = append(b.Missing, "可用技术权重不足 60%")
	}
	q := c.SignalQuality
	if q == nil || q.ATR == nil {
		b.Valid = false
		b.Missing = append(b.Missing, "信号质量与波动参照")
		return b, notes
	}
	has := func(v *float64, lo, hi float64) bool { return v != nil && *v >= lo && *v <= hi }
	setup := 0.0
	var setupNotes []string
	switch strat.baseKey {
	case "momentum", "growth":
		if q.BreakoutConfirmed != nil && *q.BreakoutConfirmed && has(q.CloseLocation, 0.65, 1) {
			setup += 5
			setupNotes = append(setupNotes, "收盘突破且收在日内较高位置")
		}
		if has(q.Compression, 0, 0.8) {
			setup += 4
			setupNotes = append(setupNotes, "突破前振幅收敛")
		}
		if has(q.VolumeContraction, 0, 0.9) && f.VolBoost >= 1.2 && f.VolBoost <= 4 {
			setup += 3
			setupNotes = append(setupNotes, "整理缩量后温和放量")
		}
	case "pullback":
		if has(q.PullbackDepthATR, 0.5, 4) && f.Chg20d > 0 {
			setup += 4
			setupNotes = append(setupNotes, "上升段出现可核对的回踩")
		}
		if q.Stabilized != nil && *q.Stabilized {
			setup += 4
			setupNotes = append(setupNotes, "完整日线低点与收盘同时企稳")
		} else if q.HigherLow != nil && *q.HigherLow {
			setup += 2
			setupNotes = append(setupNotes, "近期低点抬高，仍待收盘确认")
		}
		if f.Vol5v20 > 0 && f.Vol5v20 < 0.9 {
			setup += 3
			setupNotes = append(setupNotes, "回踩期间量能低于中期均量")
		}
	case "active":
		if has(q.DemandBalance5, 0.2, 1) && has(q.CloseLocation, 0.6, 1) {
			setup += 5
			setupNotes = append(setupNotes, "上涨日量能占优且收盘承接较好")
		}
		if has(q.RangeShock, 0, 2) && c.TurnoverRate >= 3 && c.TurnoverRate <= 15 {
			setup += 3
			setupNotes = append(setupNotes, "活跃换手尚未伴随异常振幅")
		}
	case "value", "leader", "balanced":
		if q.Stabilized != nil && *q.Stabilized && has(q.Compression, 0, 1) {
			setup += 4
			setupNotes = append(setupNotes, "波动收敛且最新收盘企稳")
		}
		if has(q.Efficiency20, 0.2, 0.8) {
			setup += 2
			setupNotes = append(setupNotes, "中期上涨路径较平稳")
		}
	}
	// 反转、整理、低波动等内置形态在基础风格之外保留自己的确认要求。
	switch strat.Intent {
	case "reversal":
		if q.Stabilized == nil || !*q.Stabilized {
			setup = math.Min(setup, 0)
			setupNotes = []string{"反转形态尚未取得企稳确认"}
		}
	case "consolidation":
		if has(q.Compression, 0, 0.8) {
			setup = math.Max(setup, 5)
			setupNotes = append(setupNotes, "整理形态有振幅收敛支持")
		}
	case "quality":
		if has(q.RangeShock, 0, 1.5) && f.Drawdown20 < 10 {
			setup = math.Max(setup, 4)
			setupNotes = append(setupNotes, "稳定形态未出现明显振幅冲击")
		}
	}
	add("setup", bounded(setup, -8, 12), "available", setupNotes...)
	entry := entryQualityFor(strat.baseKey, strat.Intent, c, q)
	entryDelta := 0.0
	switch entry.Status {
	case "extended":
		distance := *q.MA20DistanceATR
		if q.BreakoutDistanceATR != nil && q.BreakoutRun > 1 {
			distance = math.Max(distance, *q.BreakoutDistanceATR)
		}
		entryDelta = -bounded(8+4*math.Max(0, distance-2), 8, 28)
	case "waiting_confirmation":
		if q.Stabilized != nil && !*q.Stabilized && (strat.baseKey == "pullback" || strat.baseKey == "value" || strat.Intent == "reversal") {
			entryDelta = -4
		}
	case "insufficient":
		entryDelta = -3
	case "aligned":
		if has(q.SupportDistanceATR, 0, 1) {
			entryDelta = 3
			entry.Reasons = append(entry.Reasons, "现价靠近已知结构支撑")
		}
	}
	add("entry", entryDelta, entry.Status, entry.Reasons...)
	risk := 0.0
	var riskNotes []string
	if has(q.CloseLocation, 0, 0.35) && has(q.RangeShock, 2, math.Inf(1)) {
		risk -= 5
		riskNotes = append(riskNotes, "放大振幅后收盘承接偏弱")
	}
	if has(q.UpperWick, 0.5, 1) && f.VolBoost > 2 {
		risk -= 4
		riskNotes = append(riskNotes, "放量伴随较长上影线")
	}
	if c.TurnoverRate > deadTurnoverPct {
		risk -= bounded((c.TurnoverRate-deadTurnoverPct)/2+3, 3, 8)
		riskNotes = append(riskNotes, "高换手仍需计入交易拥挤风险")
	}
	add("risk", bounded(risk, -10, 0), "available", riskNotes...)
	finance, financeNotes, financeStatus := qualityFinanceScore(strat.baseKey, c)
	b.Missing = append(b.Missing, financeMissingFor(strat.baseKey, c.Fin)...)
	add("fundamentals", finance, financeStatus, financeNotes...)
	// 机构与新闻同组限幅；资金流已经进入量能维，避免再次按连续流入天数加分。
	positive, negative := 0.0, 0.0
	var contextNotes []string
	if c.OrgNetYi >= 0.1 && c.OrgBuys > 0 {
		positive = 4
		contextNotes = append(contextNotes, "存在机构净买入记录")
	}
	if c.OrgNetYi <= -0.1 {
		negative = -4
		contextNotes = append(contextNotes, "存在机构净卖出记录")
	}
	if c.SentiNews > 0 {
		if c.SentiScore >= 0.3 {
			positive = math.Max(positive, 2)
			contextNotes = append(contextNotes, "关联新闻聚合偏正面")
		}
		if c.SentiScore <= -0.3 {
			negative = math.Min(negative, -4)
			contextNotes = append(contextNotes, "关联新闻聚合偏负面")
		}
	}
	add("context", bounded(positive+negative, -6, 4), "observed", contextNotes...)
	b.Total, _ = normalizeRankingScore(b.Total)
	return b, notes
}

func qualityFinanceScore(profile string, c candidate) (float64, []string, string) {
	if !profileUsesFinance(profile) {
		return 0, nil, "not_applicable"
	}
	if c.Fin == nil {
		return 0, nil, "missing"
	}
	fin := c.Fin
	status := "available"
	var notes []string
	if missing := financeMissingFor(profile, fin); len(missing) > 0 {
		status = "partial"
		notes = append(notes, "财务证据缺少："+strings.Join(missing, "、"))
	}
	score := 0.0
	if c.PETTM < 0 {
		score -= 6
		notes = append(notes, "PE 为负，不能将低估值解释为盈利质量")
	}
	if fin.has(fin.NetProfitYoY) && *fin.NetProfitYoY <= -30 {
		score -= 5
		notes = append(notes, "净利润同比明显下降")
	}
	switch profile {
	case "value":
		peOK := c.PETTM > 0 && c.PETTM <= 25
		pbOK := c.PB > 0 && c.PB <= 3
		if c.IndustryPeers != nil && c.IndustryPeers.PEPercentile != nil {
			peOK = *c.IndustryPeers.PEPercentile <= 35
			notes = append(notes, "PE 已结合本批同行候选分位核对")
		} else {
			notes = append(notes, "可比同行不足，PE 仅用绝对参照，不能据此断言行业低估")
		}
		if c.IndustryPeers != nil && c.IndustryPeers.PBPercentile != nil {
			pbOK = *c.IndustryPeers.PBPercentile <= 50
		}
		if peOK && fin.hasAnnualROE() && *fin.AnnualROE >= 8 {
			score += 5
			notes = append(notes, "估值与盈利能力同时满足价值参照")
		}
		if pbOK && fin.hasAnnualROE() && *fin.AnnualROE >= 10 {
			score += 2
			notes = append(notes, "净资产定价与最近年报 ROE 相互支持")
		}
		if fin.has(fin.NetProfitYoY) && *fin.NetProfitYoY >= 10 {
			score += 3
			notes = append(notes, "净利润保持正增长")
		}
	case "growth":
		if fin.has(fin.RevenueYoY) && fin.has(fin.NetProfitYoY) && *fin.RevenueYoY >= 10 && *fin.NetProfitYoY >= 15 {
			score += 8
			notes = append(notes, "营收与净利润双增长")
		}
		if fin.hasAnnualROE() && *fin.AnnualROE >= 12 {
			score += 3
			notes = append(notes, "最近年报 ROE 支持年度盈利质量")
		}
		if fin.has(fin.RevenueYoY) && *fin.RevenueYoY < 0 {
			score -= 4
			notes = append(notes, "营收同比下降，成长依据减弱")
		}
	case "leader":
		if fin.hasAnnualROE() && fin.has(fin.NetProfitYoY) && *fin.AnnualROE >= 15 && *fin.NetProfitYoY >= 0 {
			score += 7
			notes = append(notes, "最近年报 ROE 较高且最新报告净利润同比未下滑")
		}
		if c.TotalCap >= 500e8 {
			score += 1
			notes = append(notes, "市值规模较大，行业地位仍需另行核实")
		}
	}
	return bounded(score, -12, 12), notes, status
}

// 原有加法规则继续可重放。比较只代表同一机会集上的评分消融，不冒充旧完整流水线。
func scoreComparison(recType string, strat *strategyTemplate, c candidate, f *candFactors, sc ScoreResult, b *recScoreBreakdown) *recScoringComparison {
	old := c
	old.SignalQuality = nil
	legacy, _ := adjustedCandidateRankingScore(recType, strat, old, f, sc)
	legacy, _ = normalizeRankingScore(legacy)
	out := &recScoringComparison{LegacyVersion: recommendationAdditiveVersion, LegacyScore: legacy, QualityVersion: recommendationScoringVersion, QualityScore: b.Total}
	if strat.scoringAlgorithm() == rankingRidgeVersion && validateRecScoringRuntime(strat.scoring) == nil {
		value, ok := normalizeRankingScore(learnedCandidateRanking(c, sc, strat.scoring.Model))
		if ok {
			out.LearnedVersion = rankingRidgeVersion
			out.LearnedScore = &value
			out.ModelHash = strat.scoring.ArtifactHash
		}
	}
	return out
}

func qualityEntryBlocksExecution(entry *recEntryQuality) bool {
	return entry != nil && entry.Status != "aligned"
}
