package service

import "math"

// normalizeRankingScore 与事件表 decimal(14,6) 使用相同精度。排序不能依赖落库后
// 会丢失的小数，也不能把 NaN/Inf 或无法保存的值混入名单。
func normalizeRankingScore(value float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) >= 1e8 {
		return 0, false
	}
	value = math.Round(value*1e6) / 1e6
	return value, math.Abs(value) < 1e8
}

// setCandidateRankingScore 保留未封顶的排序值；Score 继续用于既有 0～100 展示。
// 指针区分真实零分与历史记录缺少排序事实，禁止从展示分伪造新版本原始分。
func setCandidateRankingScore(c *candidate, value float64) bool {
	value, ok := normalizeRankingScore(value)
	if !ok {
		return false
	}
	c.RankingScore = &value
	c.Score = round2(clamp0100(value))
	return true
}

func candidateRankingValue(c candidate) (float64, bool) {
	if c.RankingScore != nil {
		return normalizeRankingScore(*c.RankingScore)
	}
	// 仅用于读取旧快照与兼容旧调用；新评分路径必须设置 RankingScore。
	return normalizeRankingScore(c.Score)
}

func candidateRanksBefore(a, b candidate) bool {
	av, aOK := candidateRankingValue(a)
	bv, bOK := candidateRankingValue(b)
	if aOK != bOK {
		return aOK
	}
	if aOK && av != bv {
		return av > bv
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	return a.Market < b.Market
}

// recordedCandidateRanksBefore 使用已经冻结的名次，供真实模型输入和量化降级
// 共用。旧快照没有名次或并列时才参考已保存的排序值，不重算行情或因子。
func recordedCandidateRanksBefore(a, b candidate) bool {
	if (a.Rank > 0) != (b.Rank > 0) {
		return a.Rank > 0
	}
	if a.Rank > 0 && a.Rank != b.Rank {
		return a.Rank < b.Rank
	}
	return candidateRanksBefore(a, b)
}
