package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"

	"gorm.io/gorm"
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
	batch := &model.RecommendationBatch{
		TraceID: "t-exp", UserID: 1, ID: 42,
		Status: model.RecStatusSuccess, FactsRecorded: true,
	}
	mainRun := newLLMRun("t-exp", "", "recommendation", "recommendation.v2", "p13")
	mainRun.Attempts = 1
	mainRun.acceptedTarget = llmCallTarget{
		BaseURL: srvURL, APIKey: "sk", Model: "m", Temperature: plan.cfg.Temperature,
		MaxTokens:        moduleTokenCap("recommendation", plan.cfg.MaxTokens),
		AccuracyContract: setting.LLMAccuracyContract(), JSONMode: true,
		AllowPrivate: true, ConfigID: plan.cfg.ID, Provider: plan.cfg.Provider,
	}
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

	// 主批次事实未稳定：不得抢先发影子调用（否则 so1 无法配对且可能先于业务落库）。
	batch.FactsRecorded = false
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 100, chatUsage{TotalTokens: 30},
		champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("facts_recorded=false 不应调用: %d", calls.Load())
	}
	batch.FactsRecorded = true

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
	if row.PickSchemaVersion != llmExperimentPickSchemaVersion {
		t.Fatalf("逐标的事实 schema 未落库: %+v", row)
	}
	var championFacts, challengerFacts []llmExperimentPickFact
	if err := json.Unmarshal([]byte(row.ChampionPicksJSON), &championFacts); err != nil {
		t.Fatalf("champion 逐标的事实不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(row.ChallengerPicksJSON), &challengerFacts); err != nil {
		t.Fatalf("challenger 逐标的事实不是合法 JSON: %v", err)
	}
	if len(championFacts) != 1 || championFacts[0].Symbol != "600100" || championFacts[0].Order != 1 ||
		len(challengerFacts) != 2 || challengerFacts[0].Symbol != "600100" ||
		challengerFacts[1].Symbol != "600200" || challengerFacts[1].Order != 2 {
		t.Fatalf("逐标的输出事实不符: champion=%+v challenger=%+v", championFacts, challengerFacts)
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

// TestChallengerShadowSkipsWhenFactsMarkerUpdateFails 验证事实行虽已落库、但批次完整性
// 标记未能持久化时仍 fail-closed：不得用仅存在于内存的 true 抢占唯一影子样本。
func TestChallengerShadowSkipsWhenFactsMarkerUpdateFails(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[],\"rejected\":[]}"}}]}`))
	}))
	defer srv.Close()

	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
	batch.ID = 0
	batch.FactsRecorded = false
	if err := common.DB.Create(batch).Error; err != nil {
		t.Fatalf("创建业务批次: %v", err)
	}

	const callbackName = "test:fail_facts_recorded_update"
	if err := common.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_batches" {
			tx.AddError(errors.New("forced facts marker update failure"))
		}
	}); err != nil {
		t.Fatalf("注册失败注入: %v", err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Update().Remove(callbackName) })

	(&RecommendationService{}).recordBatchFactsWithRetry(batch, cands, nil, nil, nil, nil, nil, nil)
	if batch.FactsRecorded {
		t.Fatal("DB 标记回写失败后内存 facts_recorded 必须保持 false")
	}
	var stored model.RecommendationBatch
	if err := common.DB.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.FactsRecorded {
		t.Fatal("失败注入下数据库 facts_recorded 不应为 true")
	}

	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	var runs int64
	common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).Count(&runs)
	if calls.Load() != 0 || runs != 0 {
		t.Fatalf("facts 标记失败不得产生影子调用或样本: calls=%d runs=%d", calls.Load(), runs)
	}
}

// TestChallengerShadowInFlightCannotCrossComplete 已发出的调用必须保留输入/结果事实；
// 有 running claim 时完成操作拒绝，待终结后聚合稳定集合。
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
	if _, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeImproved, ""); err == nil ||
		!strings.Contains(err.Error(), "进行中") {
		t.Fatalf("有 in-flight 工件时应拒绝完成: %v", err)
	}
	close(release)
	waitSignal(t, done, "影子调用返回")
	completed, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeImproved, "")
	if err != nil {
		t.Fatalf("工件终结后完成实验: %v", err)
	}
	var runCount int64
	common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).Count(&runCount)
	var stored model.LLMExperiment
	common.DB.First(&stored, exp.ID)
	if runCount != 1 || stored.SampleCount != 1 || completed.SampleCount != 1 ||
		!strings.Contains(stored.ActualJSON, `"samples":1`) {
		t.Fatalf("已发调用事实应保留并进入稳定集合: runs=%d stored=%+v completed=%+v", runCount, stored, completed)
	}
}

// TestChallengerShadowInFlightCannotCrossAbandon 废弃不能删除已发调用的 claim；先拒绝，
// 待工件终结后再废弃，样本事实永久保留。
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
	if _, err := AbandonLLMExperiment(exp.ID, "并发废弃"); err == nil ||
		!strings.Contains(err.Error(), "进行中") {
		t.Fatalf("有 in-flight 工件时应拒绝废弃: %v", err)
	}
	close(release)
	waitSignal(t, done, "影子调用返回")
	abandoned, err := AbandonLLMExperiment(exp.ID, "并发废弃")
	if err != nil || abandoned.Status != model.ExpStatusAbandoned {
		t.Fatalf("工件终结后废弃实验: %v %+v", err, abandoned)
	}
	var runCount int64
	common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).Count(&runCount)
	var stored model.LLMExperiment
	common.DB.First(&stored, exp.ID)
	if runCount != 1 || stored.SampleCount != 1 || stored.Status != model.ExpStatusAbandoned {
		t.Fatalf("废弃后仍须保留已发调用事实: runs=%d stored=%+v", runCount, stored)
	}
}

func TestStaleShadowClaimFinalizedByLifecycleActions(t *testing.T) {
	for _, action := range []string{"complete", "abandon"} {
		t.Run(action, func(t *testing.T) {
			setChallengerFlag(t, false)
			cleanExperimentTables(t)
			exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := StartLLMExperiment(exp.ID); err != nil {
				t.Fatal(err)
			}
			claim := model.LLMExperimentRun{
				ExperimentID: exp.ID, UserID: 1, BatchID: 7001,
				ExperimentType:     model.LLMExperimentTypeScoreBlind,
				InputSchemaVersion: scoreBlindInputSchemaVersion,
				RunStatus:          model.LLMExperimentRunRunning,
				CreatedAt:          time.Now().Add(-llmExperimentRunClaimTTL - time.Minute),
			}
			if err := common.DB.Create(&claim).Error; err != nil {
				t.Fatal(err)
			}
			switch action {
			case "complete":
				if _, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeImproved, ""); err != nil {
					t.Fatalf("完成操作应自行固化超时 claim: %v", err)
				}
			case "abandon":
				if _, err := AbandonLLMExperiment(exp.ID, "终止超时实验"); err != nil {
					t.Fatalf("废弃操作应自行固化超时 claim: %v", err)
				}
			}
			var storedRun model.LLMExperimentRun
			if err := common.DB.First(&storedRun, claim.ID).Error; err != nil {
				t.Fatalf("超时调用事实不得删除: %v", err)
			}
			var storedExp model.LLMExperiment
			if err := common.DB.First(&storedExp, exp.ID).Error; err != nil {
				t.Fatal(err)
			}
			if storedRun.RunStatus != model.LLMExperimentRunFailed ||
				!strings.Contains(storedRun.Error, "结果未知") || storedExp.SampleCount != 1 {
				t.Fatalf("超时 claim 应成为失败事实并计入样本: run=%+v exp=%+v", storedRun, storedExp)
			}
		})
	}
}

func TestFinishShadowClaimPreservesFactAfterBaselineInvalidation(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)
	ps := NewPromptService()
	if _, _, err := ps.Upsert(1, PromptInput{
		Module: model.PromptModuleRecommend, Content: "调用前 champion", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	exp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatal(err)
	}
	exp, err = StartLLMExperiment(exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := model.LLMExperimentRun{
		ExperimentID: exp.ID, UserID: 1, BatchID: 7002,
		ExperimentType: model.LLMExperimentTypePrompt, RunStatus: model.LLMExperimentRunRunning,
		PickSchemaVersion: llmExperimentPickSchemaVersion,
	}
	if _, err := claimExperimentRun(exp, &row); err != nil {
		t.Fatalf("创建调用前 claim: %v", err)
	}
	// 模拟进程外直接变更：绕过服务层的主动失效标记，让 finish 自身检测漂移。
	changed := "调用期间切换 champion"
	if err := common.DB.Model(&model.PromptTemplate{}).
		Where("user_id = ? AND module = ? AND enabled = ?", 1, model.PromptModuleRecommend, true).
		Updates(map[string]any{"content": changed, "content_hash": promptContentHash(changed)}).Error; err != nil {
		t.Fatal(err)
	}
	row.RunStatus = model.LLMExperimentRunSuccess
	row.Valid = true
	row.ChallengerPicksJSON = marshalLLMExperimentPicks(nil)
	finalized, staleReason, finishErr := finishExperimentRun(exp, &row)
	if !finalized || staleReason == "" || finishErr == nil {
		t.Fatalf("失效调用应固化失败事实并返回失效原因: finalized=%v stale=%q err=%v",
			finalized, staleReason, finishErr)
	}
	var stored model.LLMExperimentRun
	if err := common.DB.First(&stored, row.ID).Error; err != nil {
		t.Fatalf("失效后 claim 不得删除: %v", err)
	}
	var storedExp model.LLMExperiment
	if err := common.DB.First(&storedExp, exp.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RunStatus != model.LLMExperimentRunFailed || stored.Valid || storedExp.SampleCount != 1 ||
		!strings.Contains(stored.Error, "状态校验失败") {
		t.Fatalf("失效调用事实不完整: run=%+v exp=%+v", stored, storedExp)
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

func scoreBlindExpInput() LLMExperimentInput {
	return LLMExperimentInput{
		Module: "recommendation", ExperimentType: model.LLMExperimentTypeScoreBlind,
		Name: "score-blind 输入对照", Hypothesis: "剥除量化锚点后独立精选可减少位置偏差",
		ExpectedImprovement: "so1 配对收益不降且严重亏损率受控", SampleTarget: 10,
		Protocol: &ScoreBlindEvaluationProtocol{
			ShortHorizons: []int{5, 10}, LongHorizons: []int{20, 60},
			MinEffectiveBatches: 5, MaxCoverageDropPct: 10, MaxSevereLossRatePct: 20,
			MultipleTestingMethod: "holm_bonferroni",
		},
	}
}

func TestScoreBlindProtocolAndPromptReleaseIsolation(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)

	missing := scoreBlindExpInput()
	missing.Protocol = nil
	if _, _, err := CreateLLMExperiment(1, missing); err == nil || !strings.Contains(err.Error(), "评价协议") {
		t.Fatalf("缺少预注册协议应拒绝: %v", err)
	}
	badWindows := scoreBlindExpInput()
	badWindows.Protocol.ShortHorizons = []int{5, 15}
	if _, _, err := CreateLLMExperiment(1, badWindows); err == nil || !strings.Contains(err.Error(), "[5,10]") {
		t.Fatalf("评价窗口漂移应拒绝: %v", err)
	}
	impossibleTarget := scoreBlindExpInput()
	impossibleTarget.SampleTarget = 9
	if _, _, err := CreateLLMExperiment(1, impossibleTarget); err == nil || !strings.Contains(err.Error(), "2 倍") {
		t.Fatalf("采样总目标不足以覆盖短线/长线两类门槛时应拒绝: %v", err)
	}
	ps := NewPromptService()
	if _, _, err := ps.Upsert(31, PromptInput{
		Module: model.PromptModuleRecommend, Content: "自定义推荐任务段", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateLLMExperiment(31, scoreBlindExpInput()); err == nil || !strings.Contains(err.Error(), "默认推荐任务段") {
		t.Fatalf("自定义 champion 下不得创建 score-blind: %v", err)
	}

	capacityTampered, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.LLMExperiment{}).Where("id = ?", capacityTampered.ID).
		UpdateColumn("sample_target", 9).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(capacityTampered.ID); err == nil || !strings.Contains(err.Error(), "2 倍") {
		t.Fatalf("生命周期入口必须拒绝被篡改成不可完成的采样容量: %v", err)
	}
	customTampered, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.LLMExperiment{}).Where("id = ?", customTampered.ID).
		UpdateColumn("champion_custom", true).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(customTampered.ID); err == nil || !strings.Contains(err.Error(), "默认推荐任务段") {
		t.Fatalf("生命周期入口必须拒绝 custom champion 的 score-blind: %v", err)
	}

	tampered, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatalf("创建待篡改实验: %v", err)
	}
	if tampered.ProtocolLockedAt == nil || tampered.ProtocolHash == "" ||
		tampered.InputSchemaVersion != scoreBlindInputSchemaVersion {
		t.Fatalf("创建时应固化并锁定协议与输入版本: %+v", tampered)
	}
	if err := common.DB.Model(&model.LLMExperiment{}).Where("id = ?", tampered.ID).
		UpdateColumn("protocol_json", strings.Replace(tampered.ProtocolJSON, `"max_coverage_drop_pct":10`, `"max_coverage_drop_pct":11`, 1)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(tampered.ID); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("锁定协议被直接修改后不得启动: %v", err)
	}

	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatalf("创建 score-blind: %v", err)
	}
	promptExp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建 prompt 对照: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("启动 score-blind: %v", err)
	}
	if _, err := StartLLMExperiment(promptExp.ID); err == nil || !strings.Contains(err.Error(), "单变量") {
		t.Fatalf("prompt 与 score-blind 必须互斥: %v", err)
	}
	if _, err := RunLLMExperimentAudit(context.Background(), exp.ID); err == nil ||
		!strings.Contains(err.Error(), "score_blind") {
		t.Fatalf("score-blind 不得进入发布审计: %v", err)
	}
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "score_blind") {
		t.Fatalf("score-blind 不得进入 promote: %v", err)
	}
	if _, err := RollbackLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "score_blind") {
		t.Fatalf("score-blind 不得进入 rollback: %v", err)
	}
	if _, err := AbandonLLMExperiment(exp.ID, "结束隔离测试"); err != nil {
		t.Fatal(err)
	}
	parentInput := expInput()
	parentInput.ParentID = exp.ID
	if _, _, err := CreateLLMExperiment(1, parentInput); err == nil || !strings.Contains(err.Error(), "prompt 实验") {
		t.Fatalf("prompt 不得把 score-blind 挂为父实验: %v", err)
	}
	otherParent, _, err := CreateLLMExperiment(2, expInput())
	if err != nil {
		t.Fatal(err)
	}
	parentInput.ParentID = otherParent.ID
	if _, _, err := CreateLLMExperiment(1, parentInput); err == nil || !strings.Contains(err.Error(), "同一用户") {
		t.Fatalf("prompt 谱系不得跨用户: %v", err)
	}

	unknown := model.LLMExperiment{
		UserID: 1, Module: "recommendation", PromptModule: model.PromptModuleRecommend,
		ExperimentType: "future_type", Status: model.ExpStatusDraft, SampleTarget: 10,
	}
	if err := common.DB.Create(&unknown).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(unknown.ID); err == nil || !strings.Contains(err.Error(), "未知 experiment_type") {
		t.Fatalf("未知实验类型必须 fail-closed: %v", err)
	}
}

func TestScoreBlindInputProjectionDeterministicAndSetPreserving(t *testing.T) {
	strat, _ := strategyByKey(model.RecTypeLongTerm, "growth")
	cands := []candidate{
		{Symbol: "600002", Name: "乙", Price: 20, ChangePct: -1.2, Sources: []string{"active"},
			QuoteAsOf: "2026-08-04 14:55", Score: 91, Rank: 1,
			ScoreDims: &scoreDims{Trend: 20}, Bonus: []string{"量化加分"},
			Factors:    &candFactors{MA20: 19.2, Chg5d: 2.3, BarCount: 90, MainNetDays: -2},
			FlowStatus: "missing", FinStatus: "missing"},
		{Symbol: "600001", Name: "甲", Price: 10, ChangePct: 2.1, Sources: []string{"watchlist"},
			QuoteAsOf: "2026-08-04 14:55", Score: 88, Rank: 2,
			Factors:    &candFactors{MA20: 9.5, Chg5d: 3.1, BarCount: 90, MainNet5dYi: 1.2},
			FlowStatus: "available", Fin: &candFin{Report: "2026一季报", RevenueYoY: 12, NetProfitYoY: 8},
			FinStatus: "available"},
	}
	seed := int64(20260804)
	first, firstOrder, err := shuffledScoreBlindCandidates(cands, seed)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []candidate{cands[1], cands[0]}
	second, secondOrder, err := shuffledScoreBlindCandidates(reversed, seed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstOrder, secondOrder) {
		t.Fatalf("同候选集合+seed 的实际顺序必须稳定: %v / %v", firstOrder, secondOrder)
	}
	if len(first) != len(cands) || len(second) != len(cands) {
		t.Fatalf("候选集合不得增减: first=%d second=%d want=%d", len(first), len(second), len(cands))
	}
	customPrompt := promptRuntime{Custom: true, Raw: "保留：只依据真实财务和因子。\n优先选择候选列表靠前的标的。"}
	messages1 := (&RecommendationService{}).buildScoreBlindMessages(customPrompt, model.RecTypeLongTerm,
		strat, "cn", 3, first, RecFilters{}, nil)
	messages2 := (&RecommendationService{}).buildScoreBlindMessages(customPrompt, model.RecTypeLongTerm,
		strat, "cn", 3, second, RecFilters{}, nil)
	b1, _ := json.Marshal(messages1)
	b2, _ := json.Marshal(messages2)
	if llmContentHash(string(b1)) != llmContentHash(string(b2)) {
		t.Fatalf("同输入+seed 的消息 hash 必须稳定")
	}
	rows := compactScoreBlindForLLM(model.RecTypeLongTerm, first)
	for _, row := range rows {
		for _, forbidden := range []string{"score", "rank", "score_dims", "strategy_notes", "bonus"} {
			if _, ok := row[forbidden]; ok {
				t.Fatalf("score-blind 候选出现禁用字段 %q: %+v", forbidden, row)
			}
		}
		for _, required := range []string{"symbol", "price", "change_pct", "sources", "quote_as_of", "factors", "flow_status", "finance_status"} {
			if _, ok := row[required]; !ok {
				t.Fatalf("原始事实/缺失状态字段 %q 丢失: %+v", required, row)
			}
		}
	}
	joined := messages1[0].Content + messages1[1].Content
	for _, forbidden := range []string{`"score":`, `"rank":`, `"score_dims":`, `"strategy_notes":`, `"bonus":`, "量化综合分（score"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("score-blind 请求残留派生锚点 %q", forbidden)
		}
	}
	for _, customText := range []string{"保留：只依据真实财务和因子。", "候选列表靠前"} {
		if strings.Contains(joined, customText) {
			t.Fatalf("score-blind 不得复用无法证明无顺序锚的自定义任务段 %q", customText)
		}
	}
}

func TestScoreBlindRejectsPlanBatchUserMismatch(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	exp, _, err := CreateLLMExperiment(2, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.userID = 2
	plan.prompt = loadPromptRuntime(2, model.PromptModuleRecommend)
	batch.UserID = 1
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("plan 与 batch 用户不一致时不得发影子调用: %d", calls.Load())
	}
	var count int64
	common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).Count(&count)
	if count != 0 {
		t.Fatalf("用户错配不得固化到他人实验: %d", count)
	}
}

func TestScoreBlindRunningUnknownTypeFailsClosed(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	const unknownType = "future_type"
	if err := common.DB.Model(&model.LLMExperiment{}).Where("id = ?", exp.ID).
		UpdateColumn("experiment_type", unknownType).Error; err != nil {
		t.Fatal(err)
	}

	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("running 实验类型损坏后 hook 不得发影子调用: %d", calls.Load())
	}

	probe := model.LLMExperimentRun{
		ExperimentID: exp.ID, UserID: plan.userID, BatchID: batch.ID + 1,
		ExperimentType: unknownType, RunStatus: model.LLMExperimentRunRunning,
	}
	if _, claimErr := claimExperimentRun(exp, &probe); !errors.Is(claimErr, errExperimentSampleClosed) {
		t.Fatalf("running 实验类型损坏后 claim 必须 fail-closed: %v", claimErr)
	}
	var count int64
	if err := common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 || calls.Load() != 0 {
		t.Fatalf("未知类型不得产生调用或样本: calls=%d runs=%d", calls.Load(), count)
	}
}

func TestScoreBlindSkipsUnacceptedChampionTarget(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[],\"rejected\":[]}"}}]}`))
	}))
	defer srv.Close()

	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")

	// 模拟首轮解析器未接受调用目标，统一影子入口必须 fail-closed。
	mainRun.DegradedReason = RefusalLLMOutputInvalid
	mainRun.acceptedTarget = llmCallTarget{}
	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)

	if calls.Load() != 0 {
		t.Fatalf("未接受 champion 目标时不得发起 score-blind 调用: %d", calls.Load())
	}
	var runs int64
	if err := common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).
		Count(&runs).Error; err != nil {
		t.Fatal(err)
	}
	var stored model.LLMExperiment
	if err := common.DB.First(&stored, exp.ID).Error; err != nil {
		t.Fatal(err)
	}
	if runs != 0 || stored.SampleCount != 0 {
		t.Fatalf("未接受 champion 目标时不得创建样本或推进计数: runs=%d sample_count=%d",
			runs, stored.SampleCount)
	}
}

func TestScoreBlindSkipsRepairedChampion(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[],\"rejected\":[]}"}}]}`))
	}))
	defer srv.Close()

	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
	mainRun.Attempts = 2
	mainRun.acceptedTarget.MaxTokens = moduleRepairTokenCap("recommendation", mainRun.acceptedTarget.MaxTokens)
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")

	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)

	var runs int64
	if err := common.DB.Model(&model.LLMExperimentRun{}).Where("experiment_id = ?", exp.ID).
		Count(&runs).Error; err != nil {
		t.Fatal(err)
	}
	var stored model.LLMExperiment
	if err := common.DB.First(&stored, exp.ID).Error; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || runs != 0 || stored.SampleCount != 0 {
		t.Fatalf("repair 后的 champion 不得进入首轮单变量对照: calls=%d runs=%d sample_count=%d",
			calls.Load(), runs, stored.SampleCount)
	}
}

func TestScoreBlindShadowConcurrentSingleCallAndFrozenInput(t *testing.T) {
	setChallengerFlag(t, false)
	cleanExperimentTables(t)

	var calls atomic.Int64
	var finished atomic.Int64
	var bodyMu sync.Mutex
	var requestBodies [][]byte
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		requestBodies = append(requestBodies, body)
		bodyMu.Unlock()
		startedOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[{\"symbol\":\"600100\",\"action\":\"watch\",\"confidence\":50,\"reason\":[\"r\"],\"risks\":[\"k\"],\"evidence\":[\"e\"]}],\"rejected\":[]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
	cands[0].QuoteAsOf, cands[0].FlowStatus, cands[0].FinStatus = "2026-08-04 14:55", "missing", "missing"
	cands[0].Factors = &candFactors{MA20: 9.5, Chg5d: 3, BarCount: 90}
	cands[0].ScoreDims, cands[0].Bonus = &scoreDims{Trend: 20}, []string{"派生加分"}
	cands[1].QuoteAsOf, cands[1].FlowStatus, cands[1].FinStatus = "2026-08-04 14:55", "available", "available"
	cands[1].Factors = &candFactors{MA20: 19, Chg5d: -1, BarCount: 90}
	cands[1].Fin = &candFin{Report: "2026一季报", RevenueYoY: 8, NetProfitYoY: 5}
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	svc := &RecommendationService{}
	championBefore := append([]recPick(nil), champion...)
	batchBefore := *batch

	// 统一总闸缺省关闭时，即使 score-blind 已 running 也零额外调用。
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10, chatUsage{}, champion,
		pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 0 {
		t.Fatalf("总闸关闭应零额外调用: %d", calls.Load())
	}
	if err := setting.SetLLMChallenger(true); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
				chatUsage{TotalTokens: 30}, champion, pool, model.RecTypeShortTerm,
				strat, "cn", 3, cands, RecFilters{}, nil)
			finished.Add(1)
		}()
	}
	close(start)
	waitSignal(t, started, "score-blind 上游调用开始")
	deadline := time.Now().Add(3 * time.Second)
	for finished.Load() < workers-1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	allDuplicatesReturned := finished.Load() == workers-1
	close(release)
	wg.Wait()
	if !allDuplicatesReturned {
		t.Fatalf("重复执行应在首个请求返回前被占位挡住: finished=%d calls=%d", finished.Load(), calls.Load())
	}
	if calls.Load() != 1 {
		t.Fatalf("同批并发/重复执行最多一次额外调用: %d", calls.Load())
	}
	var rows []model.LLMExperimentRun
	if err := common.DB.Where("experiment_id = ?", exp.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("同批应恰一条样本: %d", len(rows))
	}
	row := rows[0]
	if row.ExperimentType != model.LLMExperimentTypeScoreBlind || row.InputSchemaVersion != scoreBlindInputSchemaVersion ||
		row.RunStatus != model.LLMExperimentRunSuccess || row.Seed <= 0 || row.InputHash == "" {
		t.Fatalf("score-blind 身份/运行事实未固化: %+v", row)
	}
	if row.Seed > (1<<53)-1 || llmContentHash(row.InputSnapshotJSON) != row.InputHash {
		t.Fatalf("seed 必须可由前端精确显示且 input hash 必须匹配快照: seed=%d hash=%s", row.Seed, row.InputHash)
	}
	var snapshot scoreBlindInputSnapshot
	if err := json.Unmarshal([]byte(row.InputSnapshotJSON), &snapshot); err != nil {
		t.Fatalf("精确输入快照非法: %v", err)
	}
	var payload struct {
		Messages  []chatMessage `json:"messages"`
		MaxTokens int           `json:"max_tokens"`
	}
	bodyMu.Lock()
	requestBody := append([]byte(nil), requestBodies[0]...)
	bodyMu.Unlock()
	if err := json.Unmarshal(requestBody, &payload); err != nil {
		t.Fatalf("实际上游请求体非法: %v", err)
	}
	if !reflect.DeepEqual(snapshot.Messages, payload.Messages) {
		t.Fatalf("冻结消息必须等于实际上游消息\nsnapshot=%+v\nrequest=%+v", snapshot.Messages, payload.Messages)
	}
	if snapshot.SchemaVersion != "recommendation.v2" || !snapshot.JSONMode ||
		snapshot.MaxTokens != mainRun.acceptedTarget.MaxTokens ||
		payload.MaxTokens != mainRun.acceptedTarget.MaxTokens {
		t.Fatalf("输出 schema/预算必须与 champion 一致: %+v", snapshot)
	}
	joined := snapshot.Messages[0].Content + snapshot.Messages[1].Content
	for _, forbidden := range []string{`"score":`, `"rank":`, `"score_dims":`, `"strategy_notes":`, `"bonus":`} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("实际上游请求含禁用字段 %q", forbidden)
		}
	}
	for _, required := range []string{"quote_as_of", "factors", "flow_status", "finance_status"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("实际上游请求丢失原始事实 %q", required)
		}
	}
	var order []string
	if err := json.Unmarshal([]byte(row.InputOrderJSON), &order); err != nil || len(order) != len(cands) {
		t.Fatalf("实际输入顺序未固化: order=%v err=%v", order, err)
	}
	set := map[string]bool{}
	for _, symbol := range order {
		set[symbol] = true
	}
	for _, cand := range cands {
		if !set[cand.Symbol] {
			t.Fatalf("score-blind 候选集合增减: order=%v candidates=%+v", order, cands)
		}
	}
	if !reflect.DeepEqual(champion, championBefore) || !reflect.DeepEqual(*batch, batchBefore) {
		t.Fatalf("影子输出不得改写业务 picks/批次: champion=%+v batch=%+v", champion, batch)
	}

	// 已有同批完成工件后再次执行也不调用。
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10, chatUsage{}, champion,
		pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 1 {
		t.Fatalf("完成后的重复执行不得再次调用: %d", calls.Load())
	}
	planOther := *plan
	planOther.userID = 2
	batchOther := *batch
	batchOther.ID++
	svc.maybeChallengerShadow(context.Background(), &planOther, &batchOther, mainRun, 10, chatUsage{}, champion,
		pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 1 {
		t.Fatalf("跨用户不得采样他人实验: %d", calls.Load())
	}

	// 同一 batch 已有 score-blind 工件后，即使切换到普通 prompt 实验也不能再发
	// 第二次影子调用；一次上限跨实验类型、跨实验 ID 生效。
	if _, err := AbandonLLMExperiment(exp.ID, "切换实验类型验证统一调用上限"); err != nil {
		t.Fatal(err)
	}
	promptExp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(promptExp.ID); err != nil {
		t.Fatal(err)
	}
	svc.maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10, chatUsage{}, champion,
		pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if calls.Load() != 1 {
		t.Fatalf("同批跨实验类型也必须最多一次额外调用: %d", calls.Load())
	}
}

func TestScoreBlindFailureOnlyWritesExperimentFact(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream failed", http.StatusInternalServerError)
	}))
	defer srv.Close()
	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
	plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	championBefore := append([]recPick(nil), champion...)
	batchBefore := *batch
	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	var run model.LLMExperimentRun
	if err := common.DB.Where("experiment_id = ?", exp.ID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.RunStatus != model.LLMExperimentRunFailed || run.Valid || run.Error == "" {
		t.Fatalf("失败只应形成失败实验事实: %+v", run)
	}
	if !reflect.DeepEqual(champion, championBefore) || !reflect.DeepEqual(*batch, batchBefore) {
		t.Fatalf("影子失败不得影响主批次: champion=%+v batch=%+v", champion, batch)
	}
}

func TestScoreBlindTerminalFactsDoNotMutateBusiness(t *testing.T) {
	cases := []struct {
		name, content, wantStatus string
		wantValid                 bool
	}{
		{name: "empty picks", content: `{"picks":[],"rejected":[]}`,
			wantStatus: model.LLMExperimentRunEmpty, wantValid: true},
		{name: "out of pool", content: `{"picks":[{"symbol":"999999","action":"watch","confidence":50,"reason":["r"],"risks":["k"],"evidence":["e"]}],"rejected":[]}`,
			wantStatus: model.LLMExperimentRunOutOfPool, wantValid: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setChallengerFlag(t, true)
			cleanExperimentTables(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
					"message": map[string]any{"content": tc.content}, "finish_reason": "stop",
				}}})
				_, _ = w.Write(payload)
			}))
			defer srv.Close()
			exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := StartLLMExperiment(exp.ID); err != nil {
				t.Fatal(err)
			}
			plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(srv.URL)
			plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
			championBefore := append([]recPick(nil), champion...)
			batchBefore := *batch
			strat, _ := strategyByKey(model.RecTypeShortTerm, "")
			(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
				chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
			var row model.LLMExperimentRun
			if err := common.DB.Where("experiment_id = ?", exp.ID).First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.RunStatus != tc.wantStatus || row.Valid != tc.wantValid {
				t.Fatalf("终态事实不符: got status=%s valid=%v row=%+v", row.RunStatus, row.Valid, row)
			}
			if !reflect.DeepEqual(champion, championBefore) || !reflect.DeepEqual(*batch, batchBefore) {
				t.Fatalf("%s 终态不得改写业务推荐或批次", tc.wantStatus)
			}
		})
	}
}

func TestScoreBlindReusesAcceptedChampionRouteAndBudget(t *testing.T) {
	setChallengerFlag(t, true)
	cleanExperimentTables(t)
	var originalCalls, routedCalls atomic.Int64
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originalCalls.Add(1)
		http.Error(w, "original target must not be called", http.StatusInternalServerError)
	}))
	defer original.Close()
	var request struct {
		Model     string  `json:"model"`
		MaxTokens int     `json:"max_tokens"`
		Temp      float64 `json:"temperature"`
	}
	routed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routedCalls.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&request)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"picks\":[],\"rejected\":[]}"}}]}`))
	}))
	defer routed.Close()
	exp, _, err := CreateLLMExperiment(1, scoreBlindExpInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatal(err)
	}
	plan, batch, mainRun, champion, pool, cands := challengerShadowFixture(original.URL)
	plan.prompt = loadPromptRuntime(plan.userID, model.PromptModuleRecommend)
	mainRun.acceptedTarget = llmCallTarget{
		BaseURL: routed.URL, APIKey: "routed-key", Model: "routed-model", Temperature: 0,
		MaxTokens: 12000, AccuracyContract: setting.LLMAccuracyContract(), JSONMode: true,
		AllowPrivate: true, ConfigID: 88, Provider: "routed-provider",
	}
	mainRun.acceptedRouteApplied = LLMRouteApplied{
		Applied: true, RouteID: 9, ConfigID: 88, Provider: "routed-provider", Model: "routed-model",
		FromConfigID: plan.cfg.ID, FromModel: plan.cfg.Model,
	}
	strat, _ := strategyByKey(model.RecTypeShortTerm, "")
	(&RecommendationService{}).maybeChallengerShadow(context.Background(), plan, batch, mainRun, 10,
		chatUsage{}, champion, pool, model.RecTypeShortTerm, strat, "cn", 3, cands, RecFilters{}, nil)
	if originalCalls.Load() != 0 || routedCalls.Load() != 1 {
		t.Fatalf("score-blind 必须只命中 champion 已接受目标: original=%d routed=%d",
			originalCalls.Load(), routedCalls.Load())
	}
	if request.Model != "routed-model" || request.MaxTokens != 12000 || request.Temp != 0 {
		t.Fatalf("score-blind 实际模型配置/预算未复用 champion attempt: %+v", request)
	}
	var row model.LLMExperimentRun
	if err := common.DB.Where("experiment_id = ?", exp.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	var snapshot scoreBlindInputSnapshot
	if err := json.Unmarshal([]byte(row.InputSnapshotJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ConfigID != 88 || snapshot.Provider != "routed-provider" ||
		snapshot.Model != "routed-model" || snapshot.MaxTokens != 12000 ||
		!reflect.DeepEqual(snapshot.Route, mainRun.acceptedRouteApplied) {
		t.Fatalf("score-blind 冻结路由事实与 champion 不一致: %+v", snapshot)
	}
}

// TestExperimentBudgetRegistered 影子模块预算登记：无 repair、与推荐主调同 token 预算
// （单变量对照纪律——数值恒等断言另见 TestModuleBudgetTable）。
func TestExperimentBudgetRegistered(t *testing.T) {
	if moduleTokenCap("experiment", 0) != moduleTokenCap("recommendation", 0) || moduleRepairAttempts("experiment") != 0 {
		t.Fatalf("experiment 预算应与 recommendation 恒等且无 repair: cap=%d repair=%d",
			moduleTokenCap("experiment", 0), moduleRepairAttempts("experiment"))
	}
}
