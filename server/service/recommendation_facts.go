package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quantvista/model"
)

const recommendationOptimizationFactVersion = "of2"

type recOptimizationFacts struct {
	Version        string    `json:"version"`
	ScoringVersion string    `json:"scoring_version"`
	Candidate      candidate `json:"candidate"`
}

func marshalOptimizationFacts(batch *model.RecommendationBatch, c candidate) (string, string, error) {
	if c.FactAsOf == nil {
		return "", "", nil
	} // 升级前记录不伪装成完整新事实
	if batch.ScoringVersion == recommendationScoringVersion && c.Rank > 0 && (c.ScoreBreakdown == nil || !c.ScoreBreakdown.Valid || c.RankingScore == nil || c.ScoreBreakdown.Total != *c.RankingScore) {
		return "", "", fmt.Errorf("候选 %s 缺少一致的评分分解", c.Symbol)
	}
	b, err := json.Marshal(recOptimizationFacts{Version: recommendationOptimizationFactVersion, ScoringVersion: batch.ScoringVersion, Candidate: c})
	if err != nil {
		return "", "", err
	}
	h := sha256.Sum256(b)
	return string(b), hex.EncodeToString(h[:]), nil
}

func readOptimizationFacts(ev model.RecommendationCandidateEvent) (*recOptimizationFacts, error) {
	if ev.FeatureVersion != recommendationOptimizationFactVersion || ev.FeatureSnapshot == "" || ev.FeatureHash == "" {
		return nil, errors.New("优化事实版本缺失或不支持")
	}
	h := sha256.Sum256([]byte(ev.FeatureSnapshot))
	if hex.EncodeToString(h[:]) != ev.FeatureHash {
		return nil, errors.New("优化事实摘要不一致")
	}
	var facts recOptimizationFacts
	if err := json.Unmarshal([]byte(ev.FeatureSnapshot), &facts); err != nil {
		return nil, err
	}
	c := facts.Candidate
	if facts.Version != ev.FeatureVersion || facts.ScoringVersion != ev.ScoringVersion || c.Symbol != ev.Symbol || c.Market != ev.Market || c.Rank != ev.ScoreRank || round2(c.Price) != round2(ev.RefPrice) || c.FactAsOf == nil {
		return nil, errors.New("优化事实与候选事件身份不一致")
	}
	if c.Rank > 0 {
		if !validRankingAlgorithm(ev.ScoringVersion) {
			return nil, errors.New("生效评分算法版本不支持")
		}
		if c.RankingScore == nil || ev.RankingScore == nil || *c.RankingScore != *ev.RankingScore || c.Score != ev.RawScore {
			return nil, errors.New("优化事实与持久化排序不一致")
		}
		if c.SignalQuality == nil || c.ScoreBreakdown == nil || c.Timing == nil || !c.ScoreBreakdown.Valid || c.ScoringComparison == nil || c.ScoreDims == nil || c.Factors == nil {
			return nil, errors.New("新评分特征不完整")
		}
		for _, value := range []float64{c.ScoreDims.Trend, c.ScoreDims.Momentum, c.ScoreDims.Position, c.ScoreDims.Volume, c.ScoreDims.Risk} {
			if !finiteRecNumber(value) || value < 0 || value > 100 {
				return nil, errors.New("基础技术维度无效")
			}
		}
		if c.ScoreBreakdown.Profile == "" || !model.ValidStrategyScoreProfile(c.ScoreBreakdown.Profile) || c.ScoringComparison.LegacyVersion != "additive_sp1" || !finiteRecNumber(c.ScoringComparison.LegacyScore) {
			return nil, errors.New("评分对照版本或配置无效")
		}
		if c.ScoreBreakdown.Version != recommendationScoringVersion || c.SignalQuality.Version != recommendationSignalVersion || c.Timing.Version != recommendationTimeVersion {
			return nil, errors.New("评分特征版本不支持")
		}
		if ev.ScoringVersion == recommendationScoringVersion && c.ScoreBreakdown.Total != *c.RankingScore {
			return nil, errors.New("生效评分与分项不一致")
		}
		if ev.ScoringVersion == recommendationAdditiveVersion && c.ScoringComparison.LegacyScore != *c.RankingScore {
			return nil, errors.New("加法评分与生效排序值不一致")
		}
		if ev.ScoringVersion == rankingRidgeVersion && (c.ScoringComparison.LearnedVersion != rankingRidgeVersion || c.ScoringComparison.LearnedScore == nil || *c.ScoringComparison.LearnedScore != *c.RankingScore || len(c.ScoringComparison.ModelHash) != 64) {
			return nil, errors.New("学习评分与冻结模型事实不一致")
		}
		total := 0.0
		seen := map[string]bool{}
		for _, part := range c.ScoreBreakdown.Components {
			if part.Key == "" || seen[part.Key] || !finiteRecNumber(part.Value) {
				return nil, errors.New("评分分组无效")
			}
			seen[part.Key] = true
			total += part.Value
		}
		for _, key := range []string{"technical", "setup", "entry", "risk", "fundamentals", "context"} {
			if !seen[key] {
				return nil, errors.New("评分分组缺失")
			}
		}
		if len(seen) != 6 {
			return nil, errors.New("评分分组包含未知字段")
		}
		normalized, ok := normalizeRankingScore(total)
		if !ok || normalized != c.ScoreBreakdown.Total || c.ScoringComparison.QualityVersion != c.ScoreBreakdown.Version || c.ScoringComparison.QualityScore != normalized {
			return nil, errors.New("评分分项不能重建完整排序值")
		}
		at, err := time.ParseInLocation("2006-01-02 15:04", c.QuoteAsOf, time.Local)
		signal, signalErr := time.ParseInLocation("2006-01-02", c.SignalQuality.AsOf, time.Local)
		if err != nil || signalErr != nil || at.After(*c.FactAsOf) || signal.After(at) || c.SignalQuality.AsOf != c.Timing.SignalDate || c.Timing.QuoteAsOf != c.QuoteAsOf || c.Timing.SignalClose <= 0 || !finiteRecNumber(c.Timing.SignalClose) {
			return nil, errors.New("特征时间顺序不成立")
		}
	}
	return &facts, nil
}

func freezeCandidateFactTime(pool []candidate, at time.Time) {
	for i := range pool {
		pool[i].FactAsOf = &at
	}
}
