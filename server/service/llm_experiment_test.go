package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// ---------- P2-1/P2-2 champion/challenger prompt 实验 ----------

func setChallengerFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMChallenger(v); err != nil {
		t.Fatalf("切换 challenger 开关失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMChallenger(false) }) // 缺省关
}

func cleanExperimentTables(t *testing.T) {
	t.Helper()
	clean := func() {
		common.DB.Where("1=1").Delete(&model.LLMReleaseAudit{})
		common.DB.Where("1=1").Delete(&model.LLMExperimentRun{})
		common.DB.Where("1=1").Delete(&model.LLMExperiment{})
		common.DB.Where("module = ?", "recommend").Delete(&model.PromptTemplate{})
	}
	clean()
	t.Cleanup(clean)
}

func expInput() LLMExperimentInput {
	return LLMExperimentInput{
		Module: "recommendation", Name: "更保守的精选口吻",
		Hypothesis:          "更强调否决与宁缺毋滥可降低低质 buy",
		ExpectedImprovement: "越池/结构化失败率不升，picks 更少但重合核心标的",
		ChallengerContent:   "你是一名极度保守的证券研究员，只精选证据最扎实的标的。",
		SampleTarget:        20,
	}
}

// TestLLMExperimentLifecycle 创建校验 + 状态机 + 单变量纪律。
func TestLLMExperimentLifecycle(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)

	// P2-2：没有假设/预期不立项。
	bad := expInput()
	bad.Hypothesis = ""
	if _, _, err := CreateLLMExperiment(1, bad); err == nil {
		t.Fatal("缺 hypothesis 应拒绝创建")
	}
	badMod := expInput()
	badMod.Module = "analysis"
	if _, _, err := CreateLLMExperiment(1, badMod); err == nil {
		t.Fatal("未支持模块应拒绝（首版仅 recommendation）")
	}

	in := expInput()
	in.SampleTarget = 1000 // 越界钳制
	exp, _, err := CreateLLMExperiment(1, in)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if exp.Status != model.ExpStatusDraft || exp.SampleTarget != llmExperimentTargetMax {
		t.Fatalf("draft/钳制不符: %+v", exp)
	}
	if exp.ChallengerHash == "" || exp.ChampionVersion == "" || exp.PromptModule != "recommend" {
		t.Fatalf("challenger 快照/champion 锚未固化: %+v", exp)
	}

	// running 前不可 complete/promote。
	if _, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeImproved, ""); err == nil {
		t.Fatal("draft 不可 complete")
	}
	if _, err := PromoteLLMExperiment(exp.ID); err == nil {
		t.Fatal("draft 不可 promote")
	}

	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 单变量纪律：同模块第二个 running 拒绝。
	exp2, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建第二实验失败: %v", err)
	}
	if _, err := StartLLMExperiment(exp2.ID); err == nil || !strings.Contains(err.Error(), "单变量") {
		t.Fatalf("同模块并行 running 应拒绝: %v", err)
	}

	// complete：非 improved 必须写失败原因。
	if _, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeNoGain, ""); err == nil {
		t.Fatal("无失败原因的 no_improvement 应拒绝")
	}
	done, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeNoGain, "picks 数无差异且 token 上升")
	if err != nil || done.Status != model.ExpStatusCompleted || done.ActualJSON == "" {
		t.Fatalf("complete 失败: %v %+v", err, done)
	}
	// P2-2 无增量不晋级。
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "无增量") {
		t.Fatalf("no_improvement 不得晋级: %v", err)
	}
	// abandon 落原因；再次 start 已完成实验拒绝。
	ab, err := AbandonLLMExperiment(exp2.ID, "让位新一轮迭代")
	if err != nil || ab.Status != model.ExpStatusAbandoned {
		t.Fatalf("abandon: %v %+v", err, ab)
	}
}

// seedExperimentRuns 批量落影子样本。
func seedExperimentRuns(t *testing.T, expID int64, valid, invalid int) {
	t.Helper()
	for i := 0; i < valid; i++ {
		common.DB.Create(&model.LLMExperimentRun{ExperimentID: expID, Valid: true,
			PicksCount: 2, ChampionPicks: 3, OverlapCount: 2, ChampionTokens: 900, ChallengerTokens: 700})
	}
	for i := 0; i < invalid; i++ {
		common.DB.Create(&model.LLMExperimentRun{ExperimentID: expID, Valid: false, Error: "JSON 解析失败"})
	}
}

// TestLLMExperimentPromoteGate P1-9 发布质量门硬检：样本量/有效率/结论/内容 hash 逐门
// 拒绝；全过后 challenger 落为启用模板（champion 指针切换，revision 快照可回放）。
func TestLLMExperimentPromoteGate(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)

	exp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 样本不足（5 条 < 10）。
	seedExperimentRuns(t, exp.ID, 5, 0)
	if _, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeImproved, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "不足") {
		t.Fatalf("样本不足应拒绝晋级: %v", err)
	}
	// 有效率不足（8 valid / 12 = 67% < 90%）。
	seedExperimentRuns(t, exp.ID, 3, 4)
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "有效率") {
		t.Fatalf("有效率不足应拒绝晋级: %v", err)
	}
	// 补足 valid：8+28=36 valid / 40 = 90% 达标。
	seedExperimentRuns(t, exp.ID, 28, 0)
	// 内容被篡改：hash 校验拒绝。
	common.DB.Model(&model.LLMExperiment{}).Where("id = ?", exp.ID).
		UpdateColumn("challenger_content", "被外部改过的内容")
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("内容篡改应拒绝晋级: %v", err)
	}
	common.DB.Model(&model.LLMExperiment{}).Where("id = ?", exp.ID).
		UpdateColumn("challenger_content", expInput().ChallengerContent)

	// 门 6（P2-6）：无发布审计工件不得晋级；补 PASS 工件（e2e 见 TestLLMExperimentAuditGate）。
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "审计") {
		t.Fatalf("无审计工件应拒绝晋级: %v", err)
	}
	// 门 6b 反例（审查修复批）：未绑定 champion 基线的旧版工件（ChampionHash 空）不可用。
	common.DB.Create(&model.LLMReleaseAudit{ExperimentID: exp.ID, Verdict: model.ReleaseAuditPass,
		ChallengerHash: exp.ChallengerHash, Summary: "legacy-seed"})
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "champion 基线") {
		t.Fatalf("未绑定基线的工件应拒绝晋级: %v", err)
	}
	var expRow model.LLMExperiment
	common.DB.First(&expRow, exp.ID)
	common.DB.Create(&model.LLMReleaseAudit{ExperimentID: exp.ID, Verdict: model.ReleaseAuditPass,
		ChallengerHash: exp.ChallengerHash,
		ChampionHash:   promptContentHash(releaseAuditChampionContent(&expRow)), Summary: "seed"})

	got, err := PromoteLLMExperiment(exp.ID)
	if err != nil {
		t.Fatalf("全门通过应晋级: %v", err)
	}
	if got.Status != model.ExpStatusPromoted || got.PromotedRevision <= 0 {
		t.Fatalf("晋级状态/revision 不符: %+v", got)
	}
	// champion 指针已切换：启用中的 recommend 模板即 challenger 内容。
	var tpl model.PromptTemplate
	if err := common.DB.Where("user_id = 1 AND module = ? AND enabled = ?", "recommend", true).
		First(&tpl).Error; err != nil {
		t.Fatalf("晋级后应存在启用模板: %v", err)
	}
	if strings.TrimSpace(tpl.Content) != expInput().ChallengerContent || tpl.Revision != got.PromotedRevision {
		t.Fatalf("模板内容/revision 不符: rev=%d", tpl.Revision)
	}
	// 已晋级不可 abandon（回滚走提示词页）。
	if _, err := AbandonLLMExperiment(exp.ID, "x"); err == nil {
		t.Fatal("promoted 不可 abandon")
	}
}

// challengerShadowFixture 构造影子采样入参（最小闭环：两候选、champion 一条 pick）。
func challengerShadowFixture(srvURL string) (*recGenPlan, *model.RecommendationBatch, *llmRun, []recPick, map[string]candidate, []candidate) {
	cands := []candidate{
		{Symbol: "600100", Name: "甲", Price: 10, Score: 90, Rank: 1},
		{Symbol: "600200", Name: "乙", Price: 20, Score: 80, Rank: 2},
	}
	pool := map[string]candidate{}
	for _, c := range cands {
		pool[c.Symbol] = c
	}
	plan := &recGenPlan{userID: 1, recType: model.RecTypeShortTerm, market: "cn",
		cfg:    &model.LLMConfig{BaseURL: srvURL, Model: "m", MaxTokens: 8000},
		apiKey: "sk", allowPrivate: true}
	batch := &model.RecommendationBatch{TraceID: "t-exp", UserID: 1, ID: 42}
	mainRun := newLLMRun("t-exp", "", "recommendation", "recommendation.v2", "p13")
	champion := []recPick{{Symbol: "600100", Action: model.RecActionBuy}}
	return plan, batch, mainRun, champion, pool, cands
}

// TestChallengerShadowNotMutateBusiness 影子采样端到端（假 LLM）：flag 关/无实验零调用；
// 命中时恰一次调用、样本行与计数落库、challenger 输出不改业务 picks；达标停采。
func TestChallengerShadowNotMutateBusiness(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// challenger 选 600100（与 champion 重合）+ 600200。
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[{\"symbol\":\"600100\",\"action\":\"watch\",\"confidence\":50,\"reason\":[\"r\"],\"risks\":[\"k\"],\"evidence\":[\"e\"]},{\"symbol\":\"600200\",\"action\":\"watch\",\"confidence\":50,\"reason\":[\"r\"],\"risks\":[\"k\"],\"evidence\":[\"e\"]}],\"rejected\":[]}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	svc := &RecommendationService{}
	strat, serr := strategyByKey(model.RecTypeShortTerm, "")
	if serr != nil {
		t.Fatalf("strategyByKey: %v", serr)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	championBefore := champion[0]

	// 无实验：零调用。
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 100, chatUsage{TotalTokens: 30},
		champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("无 running 实验不应调用: %d", calls.Load())
	}

	in := expInput()
	in.SampleTarget = llmExperimentTargetMin
	exp, _, err := CreateLLMExperiment(1, in)
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// flag 关：不采样。
	_ = setting.SetLLMChallenger(false)
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 100, chatUsage{TotalTokens: 30},
		champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("flag 关不应调用: %d", calls.Load())
	}
	_ = setting.SetLLMChallenger(true)

	// 别人（用户 2）的批次：不采样（只命中创建者本人）。
	plan2 := *plan
	plan2.userID = 2
	svc.maybeChallengerShadow(context.Background(), &plan2, batch, mainRun, 100, chatUsage{TotalTokens: 30},
		champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("跨用户不应采样: %d", calls.Load())
	}

	// 命中：一次调用 + 样本行 + 计数。
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 100, chatUsage{TotalTokens: 30},
		champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 1 {
		t.Fatalf("应恰一次影子调用: %d", calls.Load())
	}
	var row model.LLMExperimentRun
	if err := common.DB.Where("experiment_id = ?", exp.ID).First(&row).Error; err != nil {
		t.Fatalf("样本行应落库: %v", err)
	}
	if !row.Valid || row.PicksCount != 2 || row.OverlapCount != 1 || row.ChampionPicks != 1 {
		t.Fatalf("对照指标不符: %+v", row)
	}
	if row.TraceID != "t-exp" || row.ChampionRun != mainRun.RunID || row.ChallengerTokens != 15 || row.ChampionTokens != 30 {
		t.Fatalf("关联/成本记录不符: %+v", row)
	}
	if champion[0].Symbol != championBefore.Symbol || champion[0].Action != championBefore.Action {
		t.Fatalf("影子采样不得改业务 picks: %+v", champion[0])
	}
	var after model.LLMExperiment
	common.DB.First(&after, exp.ID)
	if after.SampleCount != 1 {
		t.Fatalf("sample_count 应 +1: %d", after.SampleCount)
	}

	// 达标停采：补满 target 后不再调用。
	common.DB.Model(&model.LLMExperiment{}).Where("id = ?", exp.ID).
		UpdateColumn("sample_count", after.SampleTarget)
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 100, chatUsage{TotalTokens: 30},
		champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 1 {
		t.Fatalf("达标后不应继续采样: %d", calls.Load())
	}
}

// TestExperimentBudgetRegistered 影子模块预算登记：无 repair、与推荐主调同 token 预算。
func TestExperimentBudgetRegistered(t *testing.T) {
	if moduleTokenCap("experiment", 0) != 2500 || moduleRepairAttempts("experiment") != 0 {
		t.Fatalf("experiment 预算应 2500/0: cap=%d repair=%d",
			moduleTokenCap("experiment", 0), moduleRepairAttempts("experiment"))
	}
}
