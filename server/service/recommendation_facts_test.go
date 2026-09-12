package service

import (
	"encoding/json"
	"testing"
	"time"

	"quantvista/model"
)

func optimizationFactEvent(t *testing.T) model.RecommendationCandidateEvent {
	t.Helper()
	c, f, sc := qualityScenarioCandidate(qualityScenarioBars(false))
	c.ScoreDims = &scoreDims{Trend: sc.Trend, Momentum: sc.Momentum, Position: sc.Position, Volume: sc.Volume, Risk: sc.Risk}
	strat := &shortStrategies[0]
	c.ScoreBreakdown, _ = scoreQualityCandidate(model.RecTypeShortTerm, strat, c, f, sc)
	c.ScoringComparison = scoreComparison(model.RecTypeShortTerm, strat, c, f, sc, c.ScoreBreakdown)
	setCandidateRankingScore(&c, c.ScoreBreakdown.Total)
	c.Rank = 1
	at, _ := time.ParseInLocation("2006-01-02 15:04", c.QuoteAsOf, time.Local)
	at = at.Add(time.Minute)
	c.FactAsOf = &at
	batch := &model.RecommendationBatch{ScoringVersion: recommendationScoringVersion}
	raw, hash, err := marshalOptimizationFacts(batch, c)
	if err != nil {
		t.Fatal(err)
	}
	return model.RecommendationCandidateEvent{Symbol: c.Symbol, Market: c.Market, ScoreRank: c.Rank, RawScore: c.Score, RankingScore: c.RankingScore, RefPrice: c.Price, ScoringVersion: batch.ScoringVersion, FeatureVersion: recommendationOptimizationFactVersion, FeatureSnapshot: raw, FeatureHash: hash}
}

func TestOptimizationFactsRoundTripAndTamperRejection(t *testing.T) {
	ev := optimizationFactEvent(t)
	facts, err := readOptimizationFacts(ev)
	if err != nil || facts.Candidate.SignalQuality == nil {
		t.Fatalf("完整事实应能回放: %v", err)
	}
	tampered := ev
	tampered.FeatureSnapshot += " "
	if _, err := readOptimizationFacts(tampered); err == nil {
		t.Fatal("摘要应拒绝被改动的快照")
	}
	other := ev
	other.Symbol = "600200"
	if _, err := readOptimizationFacts(other); err == nil {
		t.Fatal("快照不得挂到其他股票")
	}
	// 即使重新编码，分组明细也必须能重建总分；不能仅靠一个 valid=true 标记。
	var corrupt recOptimizationFacts
	if err := json.Unmarshal([]byte(ev.FeatureSnapshot), &corrupt); err != nil {
		t.Fatal(err)
	}
	corrupt.Candidate.ScoreBreakdown.Components[0].Value++
	raw, hash, err := marshalOptimizationFacts(&model.RecommendationBatch{ScoringVersion: ev.ScoringVersion}, corrupt.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	other = ev
	other.FeatureSnapshot, other.FeatureHash = raw, hash
	if _, err := readOptimizationFacts(other); err == nil {
		t.Fatal("分项被改写后不能通过重放校验")
	}
	if _, err := readOptimizationFacts(model.RecommendationCandidateEvent{}); err == nil {
		t.Fatal("旧记录不得冒充新样本")
	}
}

func TestIndustryPeersUseFrozenComparableCohort(t *testing.T) {
	pool := make([]candidate, 6)
	industries := map[string]string{}
	for i := range pool {
		pool[i] = candidate{Symbol: string(rune('a' + i)), PETTM: float64(5 + i*3), PB: float64(i + 1), TotalCap: float64(i+1) * 1e8, QuoteAsOf: "2026-09-11 15:00"}
		industries[pool[i].Symbol] = "同一行业"
	}
	attachRecommendationPeers(pool, industries)
	if pool[5].IndustryPeers.PEPercentile == nil || *pool[5].IndustryPeers.PEPercentile < 80 {
		t.Fatal("绝对 PE 不高也可能在同行内偏贵")
	}
	fin := &candFin{Report: "年报", ROE: 12, NetProfitYoY: 15}
	pool[0].Fin, pool[5].Fin = fin, fin
	low, _, _ := qualityFinanceScore("value", pool[0])
	high, _, _ := qualityFinanceScore("value", pool[5])
	if low <= high {
		t.Fatal("价值评分应区分同行相对定价")
	}
	pool[4].Excluded = "行情失效"
	pool[5].Excluded = "行情失效"
	attachRecommendationPeers(pool, industries)
	if pool[0].IndustryPeers.PEPercentile != nil {
		t.Fatal("可用同行不足 5 个时不能输出伪分位")
	}
}
