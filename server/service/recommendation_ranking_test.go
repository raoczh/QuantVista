package service

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"testing"

	"quantvista/model"
)

// 相同展示分不能把完整规则分的差异降成股票代码顺序。基础分和策略加分均调用
// 真实生产函数；同时核对补拉、冻结后的模型输入、量化降级与 score-blind 边界。
func TestRecommendationRankingPreservesSaturatedScores(t *testing.T) {
	strategy := &shortStrategies[0]
	base := ScoreResult{Trend: 90, Momentum: 90, Position: 90, Volume: 90, Risk: 90}
	pool := []candidate{
		{Symbol: "600001", Market: "cn", Price: 10, Factors: &candFactors{High20d: true, BullAlign: true}},
		{Symbol: "600002", Market: "cn", Price: 10, Factors: &candFactors{
			High20d: true, BullAlign: true, VolBoost: 2, RSI14: 60, MACDXUp: true, MACDDif: 1,
		}},
	}
	preheat := make([]recPreheatCandidate, 0, len(pool))
	for i := range pool {
		raw, notes := adjustedCandidateRankingScore(model.RecTypeShortTerm, strategy, pool[i], pool[i].Factors, base)
		if !setCandidateRankingScore(&pool[i], raw) || pool[i].Score != 100 {
			t.Fatalf("示例必须达到展示分上限：%+v", pool[i])
		}
		pool[i].Bonus = notes
		preheat = append(preheat, recPreheatCandidate{Symbol: pool[i].Symbol, BaseScore: raw, NeedsFetch: true})
	}
	if *pool[1].RankingScore <= *pool[0].RankingScore {
		t.Fatal("完整规则分应保留额外确认信号的增量")
	}
	sort.Slice(pool, func(i, j int) bool { return candidateRanksBefore(pool[i], pool[j]) })
	if pool[0].Symbol != "600002" {
		t.Fatalf("高分饱和后错误地按股票代码选出 %s", pool[0].Symbol)
	}
	selected := planRecPreheat(preheat, 1, func(recPreheatCandidate) bool { return true })
	if len(selected) != 1 || selected[0].Symbol != pool[0].Symbol {
		t.Fatalf("补拉预算与完整评分次序不一致：%+v", selected)
	}
	for i := range pool {
		pool[i].Rank = i + 1
	}
	reversed := []candidate{pool[1], pool[0]}
	rows := compactForLLM(model.RecTypeShortTerm, reversed)
	if rows[0]["symbol"] != "600002" || rows[0]["ranking_score"] != *pool[0].RankingScore {
		t.Fatalf("送模顺序或排序依据丢失：%+v", rows)
	}
	fallback := buildQuantFallbackPicks(model.RecTypeShortTerm, reversed, 1)
	if len(fallback) != 1 || fallback[0].Symbol != "600002" {
		t.Fatalf("降级没有沿用冻结的排序：%+v", fallback)
	}
	blind := compactScoreBlindForLLM(model.RecTypeShortTerm, reversed)
	for _, row := range blind {
		for _, field := range []string{"score", "ranking_score", "rank", "score_dims", "strategy_notes"} {
			if _, ok := row[field]; ok {
				t.Fatalf("score-blind 泄漏评分锚点 %s", field)
			}
		}
	}
}

func TestRecommendationRankingPrecisionAndLegacy(t *testing.T) {
	pool := []candidate{
		{Symbol: "600004", RankingScore: fptr(-2.5)},
		{Symbol: "600002", RankingScore: fptr(100.1234564)},
		{Symbol: "600001", RankingScore: fptr(100.1234563)},
		{Symbol: "600003", RankingScore: fptr(0)},
	}
	sort.Slice(pool, func(i, j int) bool { return candidateRanksBefore(pool[i], pool[j]) })
	got := []string{pool[0].Symbol, pool[1].Symbol, pool[2].Symbol, pool[3].Symbol}
	if !reflect.DeepEqual(got, []string{"600001", "600002", "600003", "600004"}) {
		t.Fatalf("必须按可持久化精度比较，并保留零和负值次序：%v", got)
	}
	var legacy candidate
	if err := json.Unmarshal([]byte(`{"symbol":"600005","score":100,"rank":1}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.RankingScore != nil {
		t.Fatal("旧展示分不能伪造完整排序分")
	}
	modern := candidate{Symbol: "600006", Rank: 2}
	if !setCandidateRankingScore(&modern, 110.12345678) || *modern.RankingScore != 110.123457 {
		t.Fatalf("六位小数持久化口径错误：%+v", modern)
	}
	if !recordedCandidateRanksBefore(legacy, modern) {
		t.Fatal("读取历史或降级时必须保留已冻结名次，不能使用新字段重排")
	}
	if !setCandidateRankingScore(&modern, 0) {
		t.Fatal("真实零分必须可保存")
	}
	encoded, err := json.Marshal(modern)
	if err != nil {
		t.Fatal(err)
	}
	var decoded candidate
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.RankingScore == nil || *decoded.RankingScore != 0 {
		t.Fatalf("零分序列化后丢失：%s，%v", encoded, err)
	}
}

func TestRecommendationRankingRejectsInvalidScores(t *testing.T) {
	for _, raw := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e8, -1e8, 99999999.9999999} {
		c := candidate{Score: 72, RankingScore: fptr(72)}
		if setCandidateRankingScore(&c, raw) || c.Score != 72 || *c.RankingScore != 72 {
			t.Fatalf("非法值不能覆盖已有评分：raw=%v candidate=%+v", raw, c)
		}
	}
	reserved := 0
	selected := planRecPreheat([]recPreheatCandidate{
		{Symbol: "invalid", BaseScore: math.NaN(), NeedsFetch: true},
		{Symbol: "valid", BaseScore: 112, NeedsFetch: true},
	}, 1, func(recPreheatCandidate) bool { reserved++; return true })
	if len(selected) != 1 || selected[0].Symbol != "valid" || reserved != 1 {
		t.Fatalf("非法评分不得占用补拉预算：%+v reserved=%d", selected, reserved)
	}
	pick := normalizePick(recPick{QuantRankingScore: fptr(999)}, "600001", candidate{Price: 10})
	if pick.QuantRankingScore != nil {
		t.Fatal("模型自附的排序依据必须由服务端剥除")
	}
}

func TestSelectionFactsRequireCompleteRankingScore(t *testing.T) {
	valid := func() []model.RecommendationCandidateEvent {
		return []model.RecommendationCandidateEvent{
			{Symbol: "600002", RawScore: 100, RankingScore: fptr(118.123456), ScoreRank: 1,
				LLMInputOrder: 1, SentToLLM: true, RankingVersion: "cr3"},
			{Symbol: "600001", RawScore: 100, RankingScore: fptr(102), ScoreRank: 2,
				LLMInputOrder: 2, SentToLLM: true, RankingVersion: "cr3"},
		}
	}
	if opportunity, issue := validateSelectionFacts(valid(), nil); issue != "" || len(opportunity) != 2 {
		t.Fatalf("完整 cr3 事实应可评估：issue=%s rows=%+v", issue, opportunity)
	}
	for name, mutate := range map[string]func([]model.RecommendationCandidateEvent){
		"缺少原始分":    func(rows []model.RecommendationCandidateEvent) { rows[0].RankingScore = nil },
		"非有限分":     func(rows []model.RecommendationCandidateEvent) { rows[0].RankingScore = fptr(math.Inf(1)) },
		"超出保存精度":   func(rows []model.RecommendationCandidateEvent) { rows[0].RankingScore = fptr(118.1234567) },
		"展示分不一致":   func(rows []model.RecommendationCandidateEvent) { rows[0].RawScore = 99 },
		"名次颠倒":     func(rows []model.RecommendationCandidateEvent) { rows[0].RankingScore = fptr(101) },
		"同分代码次序颠倒": func(rows []model.RecommendationCandidateEvent) { rows[0].RankingScore = fptr(102) },
	} {
		t.Run(name, func(t *testing.T) {
			rows := valid()
			mutate(rows)
			if _, issue := validateSelectionFacts(rows, nil); issue != selectionFactRankingOld {
				t.Fatalf("不可重建的排名不得混入效果评估：%s", issue)
			}
		})
	}
	zero := []model.RecommendationCandidateEvent{{Symbol: "600001", RawScore: 0, RankingScore: fptr(0),
		ScoreRank: 1, LLMInputOrder: 1, SentToLLM: true, RankingVersion: "cr3"}}
	if _, issue := validateSelectionFacts(zero, nil); issue != "" {
		t.Fatalf("真实零分不能视为缺失：%s", issue)
	}
}
