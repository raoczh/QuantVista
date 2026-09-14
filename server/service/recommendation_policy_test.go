package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func rankingArtifactFixture(t *testing.T, eligible bool) model.RankingModelArtifact {
	t.Helper()
	d := researchSyntheticDataset(60)
	m, err := fitRankingRidge(d.Samples, "net", 1)
	if err != nil {
		t.Fatal(err)
	}
	rep := &RankingResearchReport{Version: rankingResearchVersion, FeatureVersion: rankingResearchFeatureVersion, Request: RankingResearchRequest{Source: "recommendations", RecType: model.RecTypeShortTerm, Profile: "momentum", Horizon: 10, Target: "net", AsOf: "2026-01-01"}, DatasetHash: strings.Repeat("a", 64), Evaluated: true, PromotionReady: eligible}
	if !eligible {
		rep.PromotionReasons = []string{"合成 fixture：仅供代码验证，缺少真实验证证据"}
	}
	data, err := json.Marshal(rankingArtifactPayload{Version: "ra1", Report: rep, Model: m})
	if err != nil {
		t.Fatal(err)
	}
	return model.RankingModelArtifact{RecType: rep.Request.RecType, Profile: rep.Request.Profile, Horizon: rep.Request.Horizon, Target: rep.Request.Target, Version: rankingRidgeVersion, DatasetHash: rep.DatasetHash, ArtifactHash: rankingJSONHash(data), Eligible: eligible, AsOf: rep.Request.AsOf, Payload: string(data), CreatedBy: 1}
}

func TestRankingPolicyUpgradeDoesNotRewriteHistory(t *testing.T) {
	for _, previous := range []string{"qr1", "qr2"} {
		t.Run(previous, func(t *testing.T) {
			setupTestDB(t)
			policy := model.RankingScoringPolicy{Key: "short_term:momentum", RecType: model.RecTypeShortTerm, Profile: "momentum", Algorithm: previous}
			if err := common.DB.Create(&policy).Error; err != nil {
				t.Fatal(err)
			}
			runtime, err := loadRecScoringRuntime(t.Context(), common.DB, model.RecTypeShortTerm, "momentum")
			if err != nil || runtime.Algorithm != recommendationScoringVersion {
				t.Fatalf("新任务应使用当前规则：%+v %v", runtime, err)
			}
			var stored model.RankingScoringPolicy
			if err := common.DB.Where("`key` = ?", policy.Key).First(&stored).Error; err != nil || stored.Algorithm != previous {
				t.Fatal("读取新任务配置不能改写旧政策记录")
			}
			if validateRecScoringRuntime(recScoringRuntime{Algorithm: previous}) == nil {
				t.Fatal("已冻结旧任务的版本不能伪装成当前实现")
			}
		})
	}
}

func TestRankingArtifactRejectsDifferentFeatureDefinition(t *testing.T) {
	row := rankingArtifactFixture(t, true)
	if _, err := readRankingArtifact(row); err != nil {
		t.Fatal(err)
	}
	var payload rankingArtifactPayload
	if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	payload.Report.FeatureVersion = "of2 / fv7 / sq2"
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	row.Payload, row.ArtifactHash = string(b), rankingJSONHash(b)
	if _, err := readRankingArtifact(row); err == nil || !strings.Contains(err.Error(), "特征版本") {
		t.Fatalf("即使摘要自洽，也不能装载另一套特征上的权重：%v", err)
	}
}

func TestRankingPolicyExplicitActivationRollbackAndCAS(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	r, err := loadRecScoringRuntime(ctx, common.DB, model.RecTypeShortTerm, "momentum")
	if err != nil || r.Algorithm != recommendationScoringVersion {
		t.Fatal("默认质量规则应明确可读")
	}
	artifact := rankingArtifactFixture(t, false)
	if err := common.DB.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	req := RankingPolicyUpdate{RecType: model.RecTypeShortTerm, Profile: "momentum", Algorithm: rankingRidgeVersion, ArtifactID: artifact.ID}
	if _, err := UpdateRankingPolicy(ctx, common.DB, 1, req); err == nil {
		t.Fatal("研究用途的模型不能启用")
	}
	ready := rankingArtifactFixture(t, true) // 只验证通过门的程序分支，合成 fixture 不是上线证据。
	if err := common.DB.Create(&ready).Error; err != nil {
		t.Fatal(err)
	}
	req.ArtifactID = ready.ID
	policy, err := UpdateRankingPolicy(ctx, common.DB, 1, req)
	if err != nil || policy.Revision != 1 {
		t.Fatalf("受控启用失败: %+v %v", policy, err)
	}
	r, err = loadRecScoringRuntime(ctx, common.DB, model.RecTypeShortTerm, "momentum")
	if err != nil || r.Model == nil || r.ArtifactHash != ready.ArtifactHash {
		t.Fatal("执行必须使用已启用的确切模型")
	}
	if _, err := UpdateRankingPolicy(ctx, common.DB, 2, req); err == nil {
		t.Fatal("过期页面不能覆盖新政策")
	}
	req.Algorithm, req.ArtifactID, req.BaseRevision = recommendationAdditiveVersion, 0, 1
	policy, err = UpdateRankingPolicy(ctx, common.DB, 1, req)
	if err != nil || policy.Revision != 2 {
		t.Fatal("应能明确回退到加法评分对照")
	}
	r, err = loadRecScoringRuntime(ctx, common.DB, model.RecTypeShortTerm, "momentum")
	if err != nil || r.Model != nil || r.Algorithm != recommendationAdditiveVersion {
		t.Fatal("回退不能继续使用学习模型")
	}
	var audits int64
	if err := common.DB.Model(&model.RankingPolicyAudit{}).Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Fatal("只有成功的版本变更应进入审计")
	}
}

func TestRankingArtifactTamperAndInvalidModelRejected(t *testing.T) {
	artifact := rankingArtifactFixture(t, true)
	p, err := readRankingArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifact.Payload += " "
	if _, err := readRankingArtifact(artifact); err == nil {
		t.Fatal("工件摘要必须防止内容漂移")
	}
	p.Model.Scales[0] = 0
	if err := validateRankingRidge(p.Model); err == nil {
		t.Fatal("无效预处理尺度不得进入生成链路")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fitRankingRidge(researchSyntheticDataset(50).Samples, "net", 1, ctx); err == nil {
		t.Fatal("取消应中止模型拟合")
	}
}

func TestRankingQueuedJobFreezesAlgorithmAndBuiltinDefinition(t *testing.T) {
	const uid int64 = 9751
	seedReportEnv(t, uid, "http://127.0.0.1:1")
	svc := &RecommendationService{llm: NewLLMService()}
	req := RecommendRequest{Type: model.RecTypeShortTerm, Strategy: "momentum"}
	plan, err := svc.prepareGeneration(uid, true, req, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(recommendationJobRequestFromPlan(req, plan, true))
	if err != nil {
		t.Fatal(err)
	}
	originalName := shortStrategies[0].Name
	old := shortStrategies[0]
	t.Cleanup(func() { shortStrategies[0] = old })
	shortStrategies[0].Name = "后来编辑的名字"
	shortStrategies[0].guide = "后来编辑的导向"
	_, err = UpdateRankingPolicy(context.Background(), common.DB, 1, RankingPolicyUpdate{RecType: model.RecTypeShortTerm, Profile: "momentum", Algorithm: recommendationAdditiveVersion})
	if err != nil {
		t.Fatal(err)
	}
	job, err := decodeRecommendationJobRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachRecommendationJobRuntime(&job, *plan.newProcessingBatch()); err != nil {
		t.Fatal(err)
	}
	restored, err := svc.prepareGenerationWithSnapshot(uid, true, job.Request, true, job.PreferenceSnapshot)
	if err != nil || restored.strat.Name != originalName || restored.strat.guide != old.guide || restored.strat.scoringAlgorithm() != recommendationScoringVersion {
		t.Fatalf("排队任务漂移: %+v %v", restored, err)
	}
	if restored.runtime.Hash != plan.runtime.Hash || restored.newProcessingBatch().ScoringVersion != recommendationScoringVersion {
		t.Fatal("批次实际配置应等于提交时摘要")
	}
	newPlan, err := svc.prepareGeneration(uid, true, req, true)
	if err != nil || newPlan.strat.scoringAlgorithm() != recommendationAdditiveVersion {
		t.Fatal("回退只影响之后提交的任务")
	}
	for _, bad := range []string{`{"version":3,"request":{"type":"short_term"}}`, `{"version":999,"request":{"type":"short_term"}}`, `{"type":"short_term"}`} {
		if _, err := decodeRecommendationJobRequest([]byte(bad)); err == nil {
			t.Fatal("无法还原版本的旧任务不能悄悄按新算法执行")
		}
	}
}

func TestRankingFrozenScreenUsesSubmittedTree(t *testing.T) {
	setupTestDB(t)
	strat, err := resolveRecStrategy(17, model.RecTypeShortTerm, recStrategyScreenPrefix+builtinScreens[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	if strat == nil || strat.tree == nil {
		t.Fatal("内置选股目录缺失")
	}
	runtime, err := freezeRecommendationRuntime(17, model.RecTypeShortTerm, strat)
	if err != nil {
		t.Fatal(err)
	}
	req := RecommendRequest{Type: model.RecTypeShortTerm, Strategy: strat.Key}
	frozen, err := thawRecommendationRuntime(runtime, 17, req)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := NewScreenerService().resolveStrategy(17, frozen.scanRequest(20))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(resolved.Tree)
	b, _ := json.Marshal(runtime.Tree)
	if string(a) != string(b) || resolved.Name != strat.Name {
		t.Fatal("扫描不能重新读取已变化的内置目录")
	}
	if _, err := NewScreenerService().resolveStrategy(18, frozen.scanRequest(20)); err == nil {
		t.Fatal("冻结条件不能跨用户执行")
	}
}

func TestRankingActivatedScoreAndComparisonFactsAgree(t *testing.T) {
	fixture := rankingArtifactFixture(t, true)
	fixture.ID = 1
	p, err := readRankingArtifact(fixture)
	if err != nil {
		t.Fatal(err)
	}
	c, f, sc := qualityScenarioCandidate(qualityScenarioBars(false))
	c.ScoreDims = &scoreDims{Trend: sc.Trend, Momentum: sc.Momentum, Position: sc.Position, Volume: sc.Volume, Risk: sc.Risk}
	for _, algorithm := range []string{recommendationScoringVersion, recommendationAdditiveVersion, rankingRidgeVersion} {
		strat := shortStrategies[0]
		strat.scoring = recScoringRuntime{Algorithm: algorithm}
		if algorithm == rankingRidgeVersion {
			strat.scoring = recScoringRuntime{Algorithm: algorithm, ArtifactID: 1, ArtifactHash: fixture.ArtifactHash, Model: p.Model}
		}
		c.ScoreBreakdown, _ = scoreQualityCandidate(model.RecTypeShortTerm, &strat, c, f, sc)
		c.ScoringComparison = scoreComparison(model.RecTypeShortTerm, &strat, c, f, sc, c.ScoreBreakdown)
		score, _ := adjustedCandidateRankingScore(model.RecTypeShortTerm, &strat, c, f, sc)
		if !setCandidateRankingScore(&c, score) {
			t.Fatal("有效模型应产生合法排序")
		}
		c.Rank = 1
		at, _ := time.ParseInLocation("2006-01-02 15:04", c.QuoteAsOf, time.Local)
		at = at.Add(time.Minute)
		c.FactAsOf = &at
		batch := &model.RecommendationBatch{ScoringVersion: algorithm}
		raw, hash, err := marshalOptimizationFacts(batch, c)
		if err != nil {
			t.Fatal(err)
		}
		ev := model.RecommendationCandidateEvent{Symbol: c.Symbol, Market: c.Market, ScoreRank: c.Rank, RawScore: c.Score, RankingScore: c.RankingScore, RefPrice: c.Price, ScoringVersion: algorithm, FeatureVersion: recommendationOptimizationFactVersion, FeatureSnapshot: raw, FeatureHash: hash}
		if _, err := readOptimizationFacts(ev); err != nil {
			t.Fatalf("%s 事实不能重放: %v", algorithm, err)
		}
		blind, _ := json.Marshal(compactScoreBlindForLLM(model.RecTypeShortTerm, []candidate{c}))
		if strings.Contains(string(blind), "learned_score") || strings.Contains(string(blind), "model_hash") || strings.Contains(string(blind), "score_breakdown") {
			t.Fatal("盲化不能泄漏算法排序锚点")
		}
	}
}

func TestRankingJobStoresFullModelOnlyInBatchAndRetryUsesIt(t *testing.T) {
	const uid int64 = 9762
	seedReportEnv(t, uid, "http://127.0.0.1:1")
	submissionReviewRuntime(t)
	artifact := rankingArtifactFixture(t, true)
	if err := common.DB.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&artifact).Update("eligible", false).Error; err == nil {
		t.Fatal("模型工件应拒绝原地修改")
	}
	policy, err := UpdateRankingPolicy(context.Background(), common.DB, 1, RankingPolicyUpdate{RecType: model.RecTypeShortTerm, Profile: "momentum", Algorithm: rankingRidgeVersion, ArtifactID: artifact.ID})
	if err != nil {
		t.Fatal(err)
	}
	var leavesA, leavesB []CondNode
	for i := 0; i < 24; i++ {
		leavesA = append(leavesA, leafV("close", ">", float64(i)+0.123456))
		leavesB = append(leavesB, leafV("ma20", ">", float64(i)+0.654321))
	}
	tree := allOf(allOf(leavesA...), allOf(leavesB...))
	profile := "momentum"
	strategy, err := NewScreenerService().SaveStrategy(uid, SaveStrategyRequest{Name: "完整配置恢复测试", Desc: strings.Repeat("长说明", 70), Tree: &tree, ScoreProfile: &profile})
	if err != nil {
		t.Fatal(err)
	}
	req := RecommendRequest{Type: model.RecTypeShortTerm, Strategy: fmt.Sprintf("screen:u%d", strategy.ID)}
	svc := NewRecommendationService(nil, nil, NewLLMService())
	view, err := svc.Generate(context.Background(), uid, true, req)
	if err != nil {
		t.Fatal(err)
	}
	var run model.JobRun
	if err := common.DB.Where("user_id = ? AND result_id = ? AND result_type = ?", uid, view.ID, JobResultRecommendation).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if len(run.RequestSnapshot) > jobSnapshotMaxBytes {
		t.Fatal("复杂条件与学习模型不应撑破任务快照")
	}
	for _, forbidden := range []string{`"guide"`, `"tree"`, `"weights"`, `"means"`, `"api_key"`} {
		if strings.Contains(run.RequestSnapshot, forbidden) {
			t.Fatalf("任务快照不应复制业务正文 %s", forbidden)
		}
	}
	var batch model.RecommendationBatch
	if err := common.DB.First(&batch, view.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(batch.RuntimeSnapshot, `"weights"`) || !strings.Contains(batch.RuntimeSnapshot, `"tree"`) {
		t.Fatal("完整可重放配置必须保留在本人批次事实")
	}
	_, err = UpdateRankingPolicy(context.Background(), common.DB, 1, RankingPolicyUpdate{RecType: model.RecTypeShortTerm, Profile: "momentum", Algorithm: recommendationScoringVersion, BaseRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	raw := snapshotJSONRequest([]byte(run.RequestSnapshot))
	retry := model.JobRun{UserID: uid, ParentID: &run.ID}
	id, err := svc.recommendationJobBinding().create(common.DB, &retry, raw)
	if err != nil {
		t.Fatal(err)
	}
	var recovered model.RecommendationBatch
	if err := common.DB.First(&recovered, id).Error; err != nil {
		t.Fatal(err)
	}
	if recovered.ScoringVersion != rankingRidgeVersion || recovered.ScoringArtifactHash != batch.ScoringArtifactHash || recovered.RuntimeSnapshot != batch.RuntimeSnapshot {
		t.Fatal("重试必须恢复原模型及条件，不能读当前政策")
	}
}
