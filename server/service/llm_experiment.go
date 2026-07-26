package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// P2-1 champion/challenger prompt 实验 + P2-2 hypothesis→experiment→feedback
// （docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.3、§9 评估协议、§10 发布/回滚）。
//
// 定位与边界（不可漂移）：
//   - **challenger 只影子运行**：采样挂推荐 runGeneration 主调成功之后——同一批次、
//     同一候选名单、同一模型配置，仅任务段 prompt 不同（单变量 §9.3）；challenger 输出
//     只落 llm_experiment_runs 供对照，**永不进入业务结果**（picks/batch 零改写，
//     测试锁定字节一致）。
//   - **champion 指针语义**（§10.1）：champion=启用中的 PromptTemplate（P0-6 体系）。
//     晋级=把 challenger 内容经 PromptService.Upsert 落为启用模板（生成新的不可变
//     revision 快照并切指针），不删旧工件；回滚=提示词页恢复上一 revision。
//   - **采样只命中实验创建者本人**的推荐生成：跨用户采样既污染单变量对照（各用户
//     champion 模板不同），也把别人批次的候选数据烧进实验者的实验——不做。
//   - **P1-9 质量门并入（硬检部分）**：promote 是本系统第一个真实的「资产晋级动作」，
//     发布门在 PromoteLLMExperiment 程序硬检（状态机/样本量/结构化有效率/结论=improved/
//     内容 lint），未过不得晋级——「未 PASS 不进入 synthesis」在此落地；LLM 复核缺口的
//     后半部分（审计员角色）留 P2-6 自动发布门。
//   - **P2-2 无增量不晋级**：Conclusion 非 improved（或未判定）一律拒绝 promote；
//     失败实验必须写 FailureReason 后 complete/abandon——失败原因是飞轮的资产不是垃圾。
//   - flag `llm_challenger` **缺省关**（每次命中额外一次 LLM 调用）；关闭只停采样，
//     实验与样本保留。
//
// 首版支持范围（如实声明）：仅 recommendation 模块（业务价值最高、输出可程序化对照
// ——picks 交集/coverage/结构化有效率都有既有解析器）。扩展新模块须在
// llmExperimentSupportedModules 登记并提供对应的影子采样钩子与对照口径。

const (
	// llmExperimentMinSamples promote 硬门槛：影子样本不足不得晋级（小样本对照是噪声）。
	llmExperimentMinSamples = 10
	// llmExperimentMinValidRate promote 硬门槛：challenger 结构化有效率下限（§8.1 精神：
	// 结构化最终成功率不得劣化；影子单次调用无 repair，门槛按 90% 定标）。
	llmExperimentMinValidRate = 0.9
	// 采样目标钳制。
	llmExperimentTargetDefault = 20
	llmExperimentTargetMin     = 5
	llmExperimentTargetMax     = 100
)

// llmExperimentSupportedModules 业务模块 → prompt 模块（PromptTemplate.module）。
var llmExperimentSupportedModules = map[string]string{
	"recommendation": "recommend",
}

// LLMExperimentInput 创建入参。
type LLMExperimentInput struct {
	Module              string `json:"module"`
	Name                string `json:"name"`
	Hypothesis          string `json:"hypothesis"`
	ExpectedImprovement string `json:"expected_improvement"`
	ChallengerContent   string `json:"challenger_content"`
	SampleTarget        int    `json:"sample_target"`
	ParentID            int64  `json:"parent_id"`
}

// CreateLLMExperiment 创建实验（draft）：challenger 内容当场固化快照 + champion 锚点
// 当场记录（创建者当时启用的模板版本），后续模板变更不影响实验对照基准的可追溯性。
// 返回 lint 警告（诊断不阻断，同 Prompts 保存语义）。
func CreateLLMExperiment(userID int64, in LLMExperimentInput) (*model.LLMExperiment, []string, error) {
	promptModule, ok := llmExperimentSupportedModules[in.Module]
	if !ok {
		return nil, nil, fmt.Errorf("模块 %q 暂不支持实验（首版仅 recommendation）", in.Module)
	}
	name := strings.TrimSpace(in.Name)
	hypo := strings.TrimSpace(in.Hypothesis)
	expect := strings.TrimSpace(in.ExpectedImprovement)
	content := strings.TrimSpace(in.ChallengerContent)
	if name == "" || hypo == "" || expect == "" {
		return nil, nil, errors.New("name/hypothesis/expected_improvement 必填（P2-2：没有假设与预期的实验不立项）")
	}
	if content == "" {
		return nil, nil, errors.New("challenger_content 必填")
	}
	if len([]rune(content)) > maxPromptContentRunes {
		return nil, nil, fmt.Errorf("challenger 内容超长（上限 %d 字符）", maxPromptContentRunes)
	}
	if in.ParentID > 0 {
		var parent model.LLMExperiment
		if err := common.DB.First(&parent, in.ParentID).Error; err != nil {
			return nil, nil, errors.New("父实验不存在")
		}
	}
	target := in.SampleTarget
	if target <= 0 {
		target = llmExperimentTargetDefault
	}
	if target < llmExperimentTargetMin {
		target = llmExperimentTargetMin
	}
	if target > llmExperimentTargetMax {
		target = llmExperimentTargetMax
	}

	champ := loadPromptRuntime(userID, promptModule)
	// champion 基线内容当场固化（P2-6 审查修复批）：自定义态=当时启用模板内容；默认态=
	// 实际的内置任务段（promptModuleDefaultTaskSegs，不用占位说明冒充）。发布审计与
	// promote 门恒对照此锚——创建后 champion 被编辑不改变实验的对照基准，只会被
	// 基线漂移校验拦下（单变量复核的稳定依据）。
	champContent := champ.Raw
	if !champ.Custom {
		champContent = promptModuleDefaultTaskSegs[promptModule]
	}
	exp := &model.LLMExperiment{
		UserID: userID, Module: in.Module, PromptModule: promptModule,
		Name: name, Hypothesis: hypo, ExpectedImprovement: expect,
		ChallengerContent: content, ChallengerHash: promptContentHash(content),
		ChampionVersion: champ.Version(recPromptVersion), ChampionHash: champ.Hash,
		ChampionContent: champContent,
		Status:          model.ExpStatusDraft, SampleTarget: target, ParentID: in.ParentID,
	}
	if err := common.DB.Create(exp).Error; err != nil {
		return nil, nil, err
	}
	return exp, lintPromptContent(promptModule, content), nil
}

// StartLLMExperiment draft→running。单变量纪律：同一模块全局同时只允许一个 running
// 实验（两个 challenger 并行会互抢影子流量且无法归因）。
func StartLLMExperiment(id int64) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, id).Error; err != nil {
		return nil, errors.New("实验不存在")
	}
	if exp.Status != model.ExpStatusDraft {
		return nil, fmt.Errorf("仅 draft 可启动（当前 %s）", exp.Status)
	}
	var running int64
	if err := common.DB.Model(&model.LLMExperiment{}).
		Where("module = ? AND status = ?", exp.Module, model.ExpStatusRunning).
		Count(&running).Error; err != nil {
		return nil, err
	}
	if running > 0 {
		return nil, errors.New("该模块已有 running 实验（单变量纪律：一个模块同时只跑一个 challenger）")
	}
	now := time.Now()
	exp.Status = model.ExpStatusRunning
	exp.StartedAt = &now
	if err := common.DB.Save(&exp).Error; err != nil {
		return nil, err
	}
	if !setting.LLMChallenger() {
		common.SysLog("实验 #%d 已启动，但 llm_challenger 开关为关——不会实际采样（管理后台可开）", exp.ID)
	}
	return &exp, nil
}

// llmExperimentActual 完成时的聚合指标（ActualJSON）。纯影子口径：结构化有效率/
// token/延迟/与 champion 的名单重合度。**决策质量（净收益/alpha）不在此自动聚合**——
// challenger 未真实落单没有标签，影子阶段只能对照结构与行为差异；收益对照属晋级后
// P2-5 联合评估。
type llmExperimentActual struct {
	Samples          int      `json:"samples"`
	ValidCount       int      `json:"valid_count"`
	ValidRatePct     float64  `json:"valid_rate_pct"`
	AvgChampTokens   float64  `json:"avg_champion_tokens"`
	AvgChalTokens    float64  `json:"avg_challenger_tokens"`
	AvgChampMs       float64  `json:"avg_champion_ms"`
	AvgChalMs        float64  `json:"avg_challenger_ms"`
	AvgOverlapPct    float64  `json:"avg_overlap_pct"` // challenger∩champion / champion picks
	AvgChampionPicks float64  `json:"avg_champion_picks"`
	AvgPicks         float64  `json:"avg_challenger_picks"`
	Errors           []string `json:"errors,omitempty"` // 失败样本原因（去重 ≤5 条）
}

func aggregateExperimentRuns(runs []model.LLMExperimentRun) llmExperimentActual {
	a := llmExperimentActual{Samples: len(runs)}
	if len(runs) == 0 {
		return a
	}
	var champTok, chalTok, champMs, chalMs, overlap, champPicks, picks float64
	errSeen := map[string]bool{}
	for _, r := range runs {
		if r.Valid {
			a.ValidCount++
		}
		champTok += float64(r.ChampionTokens)
		chalTok += float64(r.ChallengerTokens)
		champMs += float64(r.ChampionMs)
		chalMs += float64(r.ChallengerMs)
		champPicks += float64(r.ChampionPicks)
		picks += float64(r.PicksCount)
		if r.ChampionPicks > 0 {
			overlap += float64(r.OverlapCount) / float64(r.ChampionPicks) * 100
		}
		if r.Error != "" && !errSeen[r.Error] && len(a.Errors) < 5 {
			errSeen[r.Error] = true
			a.Errors = append(a.Errors, r.Error)
		}
	}
	n := float64(len(runs))
	a.ValidRatePct = round2(float64(a.ValidCount) / n * 100)
	a.AvgChampTokens = round2(champTok / n)
	a.AvgChalTokens = round2(chalTok / n)
	a.AvgChampMs = round2(champMs / n)
	a.AvgChalMs = round2(chalMs / n)
	a.AvgOverlapPct = round2(overlap / n)
	a.AvgChampionPicks = round2(champPicks / n)
	a.AvgPicks = round2(picks / n)
	return a
}

// CompleteLLMExperiment running→completed（P2-2 反馈闭环）：聚合影子样本进 ActualJSON，
// 管理员按报表判定结论；非 improved 必须写失败原因（失败原因是飞轮资产）。
func CompleteLLMExperiment(id int64, conclusion, failureReason string) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, id).Error; err != nil {
		return nil, errors.New("实验不存在")
	}
	if exp.Status != model.ExpStatusRunning {
		return nil, fmt.Errorf("仅 running 可完成（当前 %s）", exp.Status)
	}
	switch conclusion {
	case model.ExpConcludeImproved, model.ExpConcludeNoGain, model.ExpConcludeWorse:
	default:
		return nil, errors.New("conclusion 须为 improved/no_improvement/worse")
	}
	failureReason = strings.TrimSpace(failureReason)
	if conclusion != model.ExpConcludeImproved && failureReason == "" {
		return nil, errors.New("未达预期的实验必须记录失败原因（P2-2：失败原因是飞轮资产）")
	}
	var runs []model.LLMExperimentRun
	if err := common.DB.Where("experiment_id = ?", exp.ID).Find(&runs).Error; err != nil {
		return nil, err
	}
	actual := aggregateExperimentRuns(runs)
	b, _ := json.Marshal(actual)
	now := time.Now()
	exp.Status = model.ExpStatusCompleted
	exp.ActualJSON = string(b)
	exp.Conclusion = conclusion
	exp.FailureReason = failureReason
	exp.SampleCount = len(runs)
	exp.CompletedAt = &now
	if err := common.DB.Save(&exp).Error; err != nil {
		return nil, err
	}
	return &exp, nil
}

// PromoteLLMExperiment completed→promoted：**P1-9 发布质量门（代码硬检）**——全部通过
// 才把 challenger 内容落为启用模板（champion 指针切换，生成不可变 revision 快照）。
// 任何一条不过=拒绝晋级（保持 completed，可继续 abandon 或复制新实验迭代）。
func PromoteLLMExperiment(id int64) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, id).Error; err != nil {
		return nil, errors.New("实验不存在")
	}
	// 门 1：状态机——只有 completed（有聚合反馈）可晋级。
	if exp.Status != model.ExpStatusCompleted {
		return nil, fmt.Errorf("仅 completed 可晋级（当前 %s）", exp.Status)
	}
	// 门 2：无增量不晋级（P2-2）。
	if exp.Conclusion != model.ExpConcludeImproved {
		return nil, fmt.Errorf("结论为 %q：无增量不晋级", exp.Conclusion)
	}
	// 门 3：样本量。
	var runs []model.LLMExperimentRun
	if err := common.DB.Where("experiment_id = ?", exp.ID).Find(&runs).Error; err != nil {
		return nil, err
	}
	if len(runs) < llmExperimentMinSamples {
		return nil, fmt.Errorf("影子样本 %d 不足 %d，不得晋级", len(runs), llmExperimentMinSamples)
	}
	// 门 4：结构化有效率（从样本重算，不信 ActualJSON 自报）。
	valid := 0
	for _, r := range runs {
		if r.Valid {
			valid++
		}
	}
	if rate := float64(valid) / float64(len(runs)); rate < llmExperimentMinValidRate {
		return nil, fmt.Errorf("challenger 结构化有效率 %.0f%% 低于 %.0f%%，不得晋级",
			rate*100, llmExperimentMinValidRate*100)
	}
	// 门 5：内容完整性（防实验期间被外部改库）。
	if promptContentHash(exp.ChallengerContent) != exp.ChallengerHash {
		return nil, errors.New("challenger 内容与创建时 hash 不符（快照被篡改），拒绝晋级")
	}
	// 门 5b（审查修复批）：champion 基线未漂移——实验对照的是创建时的 champion 锚，
	// 创建后 champion 被编辑（或别的实验晋级）时影子对照与当前线上形态脱节，实验
	// 结论不再能回答「相对当前 champion 是否有增量」，须基于新基线重建实验。
	if h := promptTemplateRowHash(userPromptTemplateRow(exp.UserID, exp.PromptModule)); h != exp.ChampionHash {
		return nil, errors.New("当前 champion 已不是实验创建时的对照基线（创建后模板被编辑或有其他实验晋级），实验对照失效——请基于当前 champion 重建实验")
	}
	// 门 6（P2-6 自动发布门）：LLM 发布审计工件必须 PASS 且审计对象与当前内容一致
	// ——「未 PASS 不进入 synthesis」（P1-9）在此收口；审计只复核程序硬检覆盖不了的
	// 内容性缺口（llm_release_gate.go），promote 本身仍是管理员显式动作（人工审批）。
	audit := latestReleaseAudit(exp.ID)
	if audit == nil {
		return nil, errors.New("缺少发布审计工件：先在实验页运行「发布审计」（P2-6 门 6：未 PASS 不晋级）")
	}
	if audit.ChallengerHash != exp.ChallengerHash {
		return nil, errors.New("最新审计工件的内容 hash 与当前 challenger 不符（内容变过须重审），拒绝晋级")
	}
	// 门 6b（审查修复批）：工件必须绑定实验的 champion 基线——审计消费的对照与实验锚
	// 不一致（修复前的旧工件无绑定、或锚内容被改库）时旧 PASS 不可复用，须重审。
	if audit.ChampionHash != promptContentHash(releaseAuditChampionContent(&exp)) {
		return nil, errors.New("最新审计工件未绑定实验的 champion 基线（旧版工件或基线被改动），须重新运行发布审计")
	}
	if audit.Verdict != model.ReleaseAuditPass {
		return nil, fmt.Errorf("最新发布审计 verdict=%s（%s）：未 PASS 不晋级", audit.Verdict, audit.Summary)
	}

	// 回滚锚（P2-6）：晋级瞬间固化「当时启用中的模板状态」——一键切回 champion 的依据。
	if row := userPromptTemplateRow(exp.UserID, exp.PromptModule); row != nil {
		exp.PrePromoteEnabled = true
		exp.PrePromoteContent = row.Content
	} else {
		exp.PrePromoteEnabled = false
		exp.PrePromoteContent = ""
	}

	// 晋级=champion 指针切换：经既有 Upsert 落为启用模板（内容 hash/revision/不可变
	// revision 快照全走 P0-6 既有机制；契约段由 composeCustomTaskPrompt 恒追加）。
	ps := NewPromptService()
	tpl, _, err := ps.Upsert(exp.UserID, PromptInput{Module: exp.PromptModule, Content: exp.ChallengerContent, Enabled: true})
	if err != nil {
		return nil, fmt.Errorf("晋级落模板失败: %v", err)
	}
	exp.Status = model.ExpStatusPromoted
	exp.PromotedRevision = tpl.Revision
	if err := common.DB.Save(&exp).Error; err != nil {
		return nil, err
	}
	common.SysLog("实验 #%d 晋级：module=%s revision=%d hash=%s（回滚=实验页「一键切回 champion」）",
		exp.ID, exp.PromptModule, tpl.Revision, exp.ChallengerHash)
	return &exp, nil
}

// AbandonLLMExperiment 任何非 promoted 状态→abandoned（记录原因；样本保留可追溯）。
func AbandonLLMExperiment(id int64, reason string) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, id).Error; err != nil {
		return nil, errors.New("实验不存在")
	}
	if exp.Status == model.ExpStatusPromoted {
		return nil, errors.New("已晋级实验不可废弃（回滚走「一键切回 champion」）")
	}
	if exp.Status == model.ExpStatusRolledBack {
		return nil, errors.New("已回滚实验是终态，不可废弃")
	}
	if exp.Status == model.ExpStatusAbandoned {
		return &exp, nil
	}
	exp.Status = model.ExpStatusAbandoned
	if r := strings.TrimSpace(reason); r != "" {
		exp.FailureReason = r
	}
	if err := common.DB.Save(&exp).Error; err != nil {
		return nil, err
	}
	return &exp, nil
}

// LLMExperimentView 管理端实验视图：附回滚可用性判定（审查修复批——promoted 实验的
// 当前启用模板已不是其晋级产物时，回滚会覆盖更新的 champion，前端据此禁用按钮并说明）。
type LLMExperimentView struct {
	model.LLMExperiment
	RollbackStale string `json:"rollback_stale,omitempty"`
}

// ListLLMExperiments 全部实验（管理端；按 id 倒序）。
func ListLLMExperiments() ([]LLMExperimentView, error) {
	var rows []model.LLMExperiment
	if err := common.DB.Order("id DESC").Limit(200).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]LLMExperimentView, 0, len(rows))
	for i := range rows {
		out = append(out, LLMExperimentView{
			LLMExperiment: rows[i], RollbackStale: experimentRollbackStale(&rows[i]),
		})
	}
	return out, nil
}

// LLMExperimentDetail 实验 + 影子样本明细。
func LLMExperimentDetail(id int64) (*model.LLMExperiment, []model.LLMExperimentRun, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, id).Error; err != nil {
		return nil, nil, errors.New("实验不存在")
	}
	var runs []model.LLMExperimentRun
	if err := common.DB.Where("experiment_id = ?", exp.ID).Order("id DESC").Limit(200).Find(&runs).Error; err != nil {
		return nil, nil, err
	}
	return &exp, runs, nil
}

// ---------- 影子采样钩子（推荐 runGeneration 主调成功后调用） ----------

// activeExperimentFor 查该用户该模块可采样的 running 实验（采样只命中创建者本人）。
func activeExperimentFor(module string, userID int64) *model.LLMExperiment {
	var exp model.LLMExperiment
	err := common.DB.Where("module = ? AND user_id = ? AND status = ? AND sample_count < sample_target",
		module, userID, model.ExpStatusRunning).First(&exp).Error
	if err != nil {
		return nil
	}
	return &exp
}

// maybeChallengerShadow challenger 影子采样（best-effort，任何失败只记日志/样本行，
// 不影响业务批次）：同一批次候选名单，仅换 challenger 任务段重建消息，一次调用
// （无 repair——影子对照要测的就是「一把过」的结构化质量），解析对照 champion picks。
// **纪律：本函数只读业务数据，写入仅限实验两表与审计；championPicks 传值副本。**
func (s *RecommendationService) maybeChallengerShadow(ctx context.Context, plan *recGenPlan,
	batch *model.RecommendationBatch, mainRun *llmRun, championMs int64, championUsage chatUsage,
	championPicks []recPick, poolBySymbol map[string]candidate,
	recType string, strat *strategyTemplate, market string, count int,
	llmCands []candidate, filters RecFilters, mktCtx *recMarketContext) {
	if common.DB == nil || !setting.LLMChallenger() {
		return
	}
	exp := activeExperimentFor("recommendation", plan.userID)
	if exp == nil {
		return
	}
	chPr := promptRuntime{Module: exp.PromptModule, Custom: true,
		Raw: exp.ChallengerContent, Hash: exp.ChallengerHash}
	messages := s.buildMessages(chPr, recType, strat, market, count, llmCands, filters, mktCtx)

	run := newLLMRun(batch.TraceID, mainRun.RunID, "experiment", "recommendation.v2", chPr.Version(recPromptVersion))
	run.hashPrompt(messages)
	cfg, apiKey := plan.cfg, plan.apiKey
	start := time.Now()
	res, err := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
		Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("experiment", cfg.MaxTokens),
		Messages: messages, JSONMode: true, AllowPrivate: plan.allowPrivate,
		Meta: run.chatMeta(plan.userID, cfg, 1),
	})
	run.record(res, err)
	elapsed := time.Since(start).Milliseconds()

	row := model.LLMExperimentRun{
		ExperimentID: exp.ID, UserID: plan.userID, BatchID: batch.ID,
		TraceID: batch.TraceID, ChampionRun: mainRun.RunID,
		ChampionPicks:  len(championPicks),
		ChampionTokens: championUsage.TotalTokens, ChampionMs: championMs,
		ChallengerMs: elapsed, FinishState: run.FinishState,
	}
	if res != nil {
		row.ChallengerTokens = res.Usage.TotalTokens
		if res.Usage.TotalTokens > 0 {
			consumeQuota(plan.userID, res.Usage.TotalTokens, false) // 只审计不扣次（后台影子）
		}
	}
	if err != nil {
		row.Error = truncateRunes(err.Error(), 500)
	} else {
		picks, _, diag, perr := parseAndFilterPicks(res.Content, poolBySymbol, count)
		if perr != nil {
			row.Error = truncateRunes(perr.Error(), 500)
		} else {
			row.Valid = true
			row.PicksCount = len(picks)
			champSet := map[string]bool{}
			for _, p := range championPicks {
				champSet[p.Symbol] = true
			}
			for _, p := range picks {
				if champSet[p.Symbol] {
					row.OverlapCount++
				}
			}
		}
		if diag != nil {
			if b, jerr := json.Marshal(diag); jerr == nil {
				row.CoverageJSON = string(b)
			}
		}
	}
	if dberr := common.DB.Create(&row).Error; dberr != nil {
		common.SysWarn("实验样本落库失败 exp=%d: %v", exp.ID, dberr)
		return
	}
	if dberr := common.DB.Model(&model.LLMExperiment{}).Where("id = ?", exp.ID).
		UpdateColumn("sample_count", gorm.Expr("sample_count + 1")).Error; dberr != nil {
		common.SysWarn("实验计数更新失败 exp=%d: %v", exp.ID, dberr)
	}
	common.SysLog("实验 #%d 影子采样：batch=%d valid=%v picks=%d overlap=%d/%d tokens=%d",
		exp.ID, batch.ID, row.Valid, row.PicksCount, row.OverlapCount, row.ChampionPicks, row.ChallengerTokens)
}
