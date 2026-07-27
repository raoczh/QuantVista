package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		common.DB.Where("1=1").Delete(&model.LLMExperimentModuleLock{})
		common.DB.Where("module = ?", "recommend").Delete(&model.PromptTemplate{})
		common.DB.Where("module = ?", "recommend").Delete(&model.PromptChampionState{})
	}
	clean()
	t.Cleanup(clean)
}

func waitSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("等待 %s 超时", name)
	}
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

// TestLLMExperimentConcurrentStart 同模块两个 draft 并发启动时，模块锁槽把
// “检查 running + 状态迁移”串成一个事务，最终只能有一个 running。
func TestLLMExperimentConcurrentStart(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)
	a, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建 A: %v", err)
	}
	b, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建 B: %v", err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func(expID int64) {
			defer wg.Done()
			<-start
			_, err := StartLLMExperiment(expID)
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "单变量") {
			t.Fatalf("失败方应由单变量门拒绝: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("并发启动应恰好成功一个，got %d", successes)
	}
	var running int64
	common.DB.Model(&model.LLMExperiment{}).
		Where("module = ? AND status = ?", "recommendation", model.ExpStatusRunning).Count(&running)
	if running != 1 {
		t.Fatalf("库内 running 应恰好一个，got %d", running)
	}
}

// TestLLMExperimentChampionGenerationCannotWashBack generation 覆盖 revision 无法表达的
// 默认/启停/删除往返；旧格式非终态实验也 fail-closed。
func TestLLMExperimentChampionGenerationCannotWashBack(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)
	ps := NewPromptService()

	defaultExp, _, err := CreateLLMExperiment(11, expInput())
	if err != nil {
		t.Fatalf("创建默认态实验: %v", err)
	}
	tpl, _, err := ps.Upsert(11, PromptInput{Module: model.PromptModuleRecommend, Content: "临时 custom", Enabled: true})
	if err != nil {
		t.Fatalf("default→custom: %v", err)
	}
	if err := ps.Delete(11, tpl.ID); err != nil {
		t.Fatalf("custom→default: %v", err)
	}
	if _, err := StartLLMExperiment(defaultExp.ID); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("default→custom→default 不得洗回: %v", err)
	}

	if _, _, err := ps.Upsert(12, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-A", Enabled: true}); err != nil {
		t.Fatalf("预置 A: %v", err)
	}
	customExp, _, err := CreateLLMExperiment(12, expInput())
	if err != nil {
		t.Fatalf("创建 custom 实验: %v", err)
	}
	if _, _, err := ps.Upsert(12, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-A", Enabled: false}); err != nil {
		t.Fatalf("停用 A: %v", err)
	}
	if _, _, err := ps.Upsert(12, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-A", Enabled: true}); err != nil {
		t.Fatalf("恢复 A: %v", err)
	}
	if _, err := StartLLMExperiment(customExp.ID); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("custom A→disabled→A 不得洗回: %v", err)
	}

	legacy, _, err := CreateLLMExperiment(13, expInput())
	if err != nil {
		t.Fatalf("创建 legacy 探针: %v", err)
	}
	common.DB.Model(&model.LLMExperiment{}).Where("id = ?", legacy.ID).
		UpdateColumn("baseline_version", llmExperimentBaselineVersionV1)
	if _, err := StartLLMExperiment(legacy.ID); err == nil || !strings.Contains(err.Error(), "缺少单调") {
		t.Fatalf("旧非终态实验应 fail-closed: %v", err)
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
	if got.Status != model.ExpStatusPromoted || got.PromotedRevision <= 0 || got.PromotedGeneration <= 0 {
		t.Fatalf("晋级状态/revision/generation 不符: %+v", got)
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

// TestChallengerShadowInFlightCannotCrossComplete 外部调用开始后若实验先完成，迟到样本
// 不得落库；ActualJSON、SampleCount 与 run 集合保持同一稳定快照。
func TestChallengerShadowInFlightCannotCrossComplete(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[],\"rejected\":[]}"}}]}`))
	}))
	defer srv.Close()
	exp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("启动: %v", err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(1, model.PromptModuleRecommend)
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
			chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	}()
	waitSignal(t, started, "影子上游调用开始")
	completed, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeImproved, "")
	if err != nil {
		t.Fatalf("完成实验: %v", err)
	}
	close(release)
	waitSignal(t, done, "迟到影子调用返回")
	var runCount int64
	common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).Count(&runCount)
	var stored model.LLMExperiment
	common.DB.First(&stored, exp.ID)
	if runCount != 0 || stored.SampleCount != 0 || completed.SampleCount != 0 ||
		!strings.Contains(stored.ActualJSON, `"samples":0`) {
		t.Fatalf("完成后的迟到样本不得改变稳定集合: runs=%d stored=%+v completed=%+v", runCount, stored, completed)
	}
}

// TestChallengerShadowInFlightCannotCrossAbandon 废弃与样本最终提交使用同一实验行锁；
// 废弃先线性化时，迟到样本不能复活计数或写入 run。
func TestChallengerShadowInFlightCannotCrossAbandon(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[],\"rejected\":[]}"}}]}`))
	}))
	defer srv.Close()
	exp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("启动: %v", err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(1, model.PromptModuleRecommend)
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
			chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	}()
	waitSignal(t, started, "影子上游调用开始")
	abandoned, err := AbandonLLMExperiment(exp.ID, "并发废弃")
	if err != nil || abandoned.Status != model.ExpStatusAbandoned {
		t.Fatalf("废弃实验: %v %+v", err, abandoned)
	}
	close(release)
	waitSignal(t, done, "迟到影子调用返回")
	var runCount int64
	common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).Count(&runCount)
	var stored model.LLMExperiment
	common.DB.First(&stored, exp.ID)
	if runCount != 0 || stored.SampleCount != 0 || stored.Status != model.ExpStatusAbandoned {
		t.Fatalf("废弃后的迟到样本不得改写实验: runs=%d stored=%+v", runCount, stored)
	}
}

// TestLLMExperimentDefaultBaselineDriftSticky 默认任务段也是完整基线：代码升级造成的
// 内置段变化须在 start/audit/promote 拒绝；内容恢复后已观测到的漂移仍不可洗回。
func TestLLMExperimentDefaultBaselineDriftSticky(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)
	original := promptModuleDefaultTaskSegs[model.PromptModuleRecommend]
	t.Cleanup(func() { promptModuleDefaultTaskSegs[model.PromptModuleRecommend] = original })

	startExp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建 start 探针: %v", err)
	}
	promptModuleDefaultTaskSegs[model.PromptModuleRecommend] = original + "\n默认任务段升级"
	if _, err := StartLLMExperiment(startExp.ID); err == nil || !strings.Contains(err.Error(), "基线已失效") {
		t.Fatalf("默认任务段变化后 start 应拒绝: %v", err)
	}

	// 另建一个 completed 实验，分别锁定 audit/promote 门。先恢复创建与采样时的基线。
	promptModuleDefaultTaskSegs[model.PromptModuleRecommend] = original
	exp := releaseGateFixture(t)
	promptModuleDefaultTaskSegs[model.PromptModuleRecommend] = original + "\n第二次默认任务段升级"
	if _, err := RunLLMExperimentAudit(context.Background(), exp.ID); err == nil ||
		!strings.Contains(err.Error(), "基线已失效") {
		t.Fatalf("默认任务段变化后 audit 应拒绝: %v", err)
	}
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "基线已失效") {
		t.Fatalf("默认任务段变化后 promote 应拒绝: %v", err)
	}

	// 恢复原文后仍由持久失效原因挡住，列表同步透出 baseline_stale。
	promptModuleDefaultTaskSegs[model.PromptModuleRecommend] = original
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "基线已失效") {
		t.Fatalf("默认任务段恢复后不得洗回: %v", err)
	}
	views, err := ListLLMExperiments()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := false
	for _, view := range views {
		if view.ID == exp.ID && view.BaselineStale != "" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("列表应返回 baseline_stale")
	}
}

// TestChallengerShadowOldBatchBeforeExperimentDoesNotInvalidate 新实验基于 live B 启动后，
// 启动前已固化 A 的旧业务批次迟到完成时只跳过，不得把有效的 B 基线实验永久标 stale。
func TestChallengerShadowOldBatchBeforeExperimentDoesNotInvalidate(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	ps := NewPromptService()
	if _, _, err := ps.Upsert(1, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-A", Enabled: true}); err != nil {
		t.Fatalf("预置 A: %v", err)
	}
	oldPrompt := loadPromptRuntime(1, model.PromptModuleRecommend) // 实验启动前批次已固化 A
	if _, _, err := ps.Upsert(1, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-B", Enabled: true}); err != nil {
		t.Fatalf("切到 B: %v", err)
	}
	exp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("基于 B 创建实验: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("启动 B 基线实验: %v", err)
	}

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[],\"rejected\":[]}"}}]}`))
	}))
	defer srv.Close()
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = oldPrompt
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	svc := &RecommendationService{}
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("旧批次 A 不属于 B 实验采样窗口，不应调用 challenger: %d", calls.Load())
	}
	var stored model.LLMExperiment
	if err := common.DB.First(&stored, exp.ID).Error; err != nil {
		t.Fatalf("读取实验: %v", err)
	}
	if stored.BaselineInvalidReason != "" || stored.SampleCount != 0 {
		t.Fatalf("旧批次只能跳过，不得污染或永久失效实验: %+v", stored)
	}

	// 后续真实使用 live B 的批次仍可正常采样，证明旧批次没有误杀实验。
	plan.prompt = loadPromptRuntime(1, model.PromptModuleRecommend)
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 1 {
		t.Fatalf("live B 批次应继续正常采样: %d", calls.Load())
	}
	if err := common.DB.First(&stored, exp.ID).Error; err != nil || stored.SampleCount != 1 || stored.BaselineInvalidReason != "" {
		t.Fatalf("live B 采样后实验状态不符: err=%v exp=%+v", err, stored)
	}
}

// TestChallengerShadowCustomBaselineCannotWashBack 自定义 A→B 时，使用 B 的主调用不得
// 进入 A 基线实验；即使随后恢复 A，revision 与粘性失效标记仍保留。
func TestChallengerShadowCustomBaselineCannotWashBack(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	ps := NewPromptService()
	if _, _, err := ps.Upsert(1, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-A", Enabled: true}); err != nil {
		t.Fatalf("预置 A: %v", err)
	}
	exp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, _, err := ps.Upsert(1, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-B", Enabled: true}); err != nil {
		t.Fatalf("切到 B: %v", err)
	}

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(1, model.PromptModuleRecommend) // 主调用实际使用 B
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("基线漂移后不得发影子调用: %d", calls.Load())
	}
	var runCount int64
	common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).Count(&runCount)
	if runCount != 0 {
		t.Fatalf("基线漂移不得落污染样本: %d", runCount)
	}

	if _, _, err := ps.Upsert(1, PromptInput{Module: model.PromptModuleRecommend, Content: "custom-A", Enabled: true}); err != nil {
		t.Fatalf("恢复 A: %v", err)
	}
	var row model.LLMExperiment
	common.DB.First(&row, exp.ID)
	if row.BaselineInvalidReason == "" {
		t.Fatal("A→B 采样尝试必须粘性记录基线失效")
	}
	views, err := ListLLMExperiments()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, view := range views {
		if view.ID == exp.ID {
			if view.BaselineStale == "" {
				t.Fatal("恢复 A 后列表仍应返回 baseline_stale")
			}
			return
		}
	}
	t.Fatal("列表缺少实验")
}

// TestExperimentBudgetRegistered 影子模块预算登记：无 repair、与推荐主调同 token 预算。
func TestExperimentBudgetRegistered(t *testing.T) {
	if moduleTokenCap("experiment", 0) != 2500 || moduleRepairAttempts("experiment") != 0 {
		t.Fatalf("experiment 预算应 2500/0: cap=%d repair=%d",
			moduleTokenCap("experiment", 0), moduleRepairAttempts("experiment"))
	}
}
