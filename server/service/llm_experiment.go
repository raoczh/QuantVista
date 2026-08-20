package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"

	"gorm.io/gorm/clause"
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
	// llmExperimentPickSchemaVersion 是逐标的 champion/challenger 事实 JSON 口径。
	// 字段或归一化语义变化时递增；旧 run 空值表示无法恢复历史名单。
	llmExperimentPickSchemaVersion = "ep1"
	// scoreBlindInputSchemaVersion 是 recommendation 输入剥锚契约。字段、提示词净化
	// 或顺序/hash 语义变化时必须递增；输出逐标的事实仍复用 ep1。
	// sb2：discovery_history 剥除 best_score/best_rank/变化量/通道，sources 的
	// daily/recent_discovery 中性化为 daily_scan——sb1 经 4b667df 引入 discovery
	// 记忆后盲化已不彻底（泄漏同评分体系数值），sb1 样本不得与 sb2 混算。
	scoreBlindInputSchemaVersion = "sb2"
	// 上游推荐任务 deadline 为 6 分钟；超过此窗口的 running 占位视为进程中断遗留，
	// 新采样可清理，避免永久占满实验 target。
	llmExperimentRunClaimTTL = 15 * time.Minute
)

// llmExperimentSupportedModules 业务模块 → prompt 模块（PromptTemplate.module）。
var llmExperimentSupportedModules = map[string]string{
	"recommendation": "recommend",
}

const (
	llmExperimentBaselineVersionV1 = 1
	llmExperimentBaselineVersion   = 2
)

// experimentPromptBaseline 是某一时刻真实生效的 L3 champion 快照。Row 同时保留底层
// 模板行的 CAS 锚；默认态可有一条 disabled 行，但它不改变 Custom/Content/Hash。
type experimentPromptBaseline struct {
	Custom          bool
	Content         string
	Hash            string
	Version         string
	Revision        int
	Generation      int64
	GenerationKnown bool
	Row             *model.PromptTemplate
}

func loadExperimentPromptBaseline(db *gorm.DB, userID int64, promptModule string) (*experimentPromptBaseline, error) {
	return loadExperimentPromptBaselineMode(db, userID, promptModule, false)
}

func lockExperimentPromptBaseline(db *gorm.DB, userID int64, promptModule string) (*experimentPromptBaseline, error) {
	return loadExperimentPromptBaselineMode(db, userID, promptModule, true)
}

func loadExperimentPromptBaselineMode(db *gorm.DB, userID int64, promptModule string, lock bool) (*experimentPromptBaseline, error) {
	var generation int64
	var generationKnown bool
	if lock {
		state, err := lockPromptChampionState(db, userID, promptModule)
		if err != nil {
			return nil, err
		}
		generation, generationKnown = state.Generation, true
	} else {
		generation, generationKnown = promptChampionGeneration(db, userID, promptModule)
	}
	var row model.PromptTemplate
	err := db.Where("user_id = ? AND module = ?", userID, promptModule).First(&row).Error
	var rowPtr *model.PromptTemplate
	switch {
	case err == nil:
		rowPtr = &row
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 默认态允许没有模板行。
	default:
		return nil, err
	}

	b := &experimentPromptBaseline{Row: rowPtr, Generation: generation, GenerationKnown: generationKnown}
	if rowPtr != nil && rowPtr.Enabled {
		b.Custom = true
		b.Content = strings.TrimSpace(rowPtr.Content)
		b.Revision = rowPtr.Revision
	} else {
		content, ok := promptModuleDefaultTaskSegs[promptModule]
		if !ok || strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("模块 %s 缺少默认任务段，无法固化实验基线", promptModule)
		}
		b.Content = content
	}
	b.Hash = promptContentHash(b.Content)
	pr := promptRuntime{Module: promptModule, Custom: b.Custom, Raw: b.Content, Hash: b.Hash,
		Revision: b.Revision, Generation: b.Generation, GenerationKnown: b.GenerationKnown}
	b.Version = pr.Version(recPromptVersion)
	return b, nil
}

func experimentPromptBaselineFromRuntime(pr promptRuntime, promptModule string) (*experimentPromptBaseline, error) {
	b := &experimentPromptBaseline{Custom: pr.Custom, Revision: pr.Revision,
		Generation: pr.Generation, GenerationKnown: pr.GenerationKnown}
	if pr.Custom {
		b.Content = strings.TrimSpace(pr.Raw)
	} else {
		content, ok := promptModuleDefaultTaskSegs[promptModule]
		if !ok || strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("模块 %s 缺少默认任务段，无法校验实验基线", promptModule)
		}
		b.Content = content
	}
	b.Hash = promptContentHash(b.Content)
	b.Version = promptRuntime{Module: promptModule, Custom: b.Custom, Raw: b.Content,
		Hash: b.Hash, Revision: b.Revision}.Version(recPromptVersion)
	return b, nil
}

func experimentExpectedChampionCustom(exp *model.LLMExperiment) bool {
	if exp.BaselineVersion >= llmExperimentBaselineVersionV1 {
		return exp.ChampionCustom
	}
	// 旧行没有 ChampionCustom；历史版本串是唯一可靠的形态信号。
	return strings.Contains(exp.ChampionVersion, "-custom")
}

func experimentExpectedChampionHash(exp *model.LLMExperiment) string {
	if exp.ChampionHash != "" {
		return exp.ChampionHash
	}
	if strings.TrimSpace(exp.ChampionContent) != "" {
		return promptContentHash(exp.ChampionContent)
	}
	// 最老的默认态实验没有正文锚，只能按当前内置段兼容；新格式不再进入此分支。
	return promptContentHash(promptModuleDefaultTaskSegs[exp.PromptModule])
}

func experimentBaselineStaleReason(exp *model.LLMExperiment, current *experimentPromptBaseline) string {
	if strings.TrimSpace(exp.BaselineInvalidReason) != "" {
		return exp.BaselineInvalidReason
	}
	if exp.BaselineVersion < llmExperimentBaselineVersion {
		return fmt.Sprintf("实验基线格式 v%d 缺少单调 champion generation，无法证明运行期未发生 A→B→A；请重建实验",
			exp.BaselineVersion)
	}
	expectedCustom := experimentExpectedChampionCustom(exp)
	if current.Custom != expectedCustom {
		return fmt.Sprintf("champion 形态已从创建时的 custom=%v 变为 custom=%v", expectedCustom, current.Custom)
	}
	expectedHash := experimentExpectedChampionHash(exp)
	if expectedHash == "" || current.Hash != expectedHash {
		return fmt.Sprintf("champion 内容已偏离创建基线（期望 hash=%s，当前 hash=%s）", expectedHash, current.Hash)
	}
	if exp.BaselineVersion >= llmExperimentBaselineVersionV1 {
		if current.Version != exp.ChampionVersion {
			return fmt.Sprintf("champion 系统版本已变化（期望 %s，当前 %s）", exp.ChampionVersion, current.Version)
		}
		// 自定义模板 revision 是不可洗回的历史锚：A→B→A 虽内容 hash 恢复，revision 已前移。
		if expectedCustom && current.Revision != exp.ChampionRevision {
			return fmt.Sprintf("champion revision 已从 %d 前移到 %d（曾发生运行期编辑）",
				exp.ChampionRevision, current.Revision)
		}
	}
	if current.GenerationKnown && current.Generation != exp.ChampionGeneration {
		return fmt.Sprintf("champion generation 已从 %d 前移到 %d（曾发生内容、启停或删除变化）",
			exp.ChampionGeneration, current.Generation)
	}
	return ""
}

func markExperimentBaselineInvalid(db *gorm.DB, exp *model.LLMExperiment, reason string) {
	reason = truncateRunes(strings.TrimSpace(reason), 500)
	if reason == "" {
		return
	}
	res := db.Model(&model.LLMExperiment{}).
		Where("id = ? AND (baseline_invalid_reason = '' OR baseline_invalid_reason IS NULL)", exp.ID).
		Update("baseline_invalid_reason", reason)
	if res.Error != nil {
		common.SysWarn("实验基线失效标记写入失败 exp=%d: %v", exp.ID, res.Error)
		return
	}
	exp.BaselineInvalidReason = reason
}

func validateExperimentCurrentBaseline(db *gorm.DB, exp *model.LLMExperiment) (*experimentPromptBaseline, string, error) {
	current, err := lockExperimentPromptBaseline(db, exp.UserID, exp.PromptModule)
	if err != nil {
		return nil, "", err
	}
	return current, experimentBaselineStaleReason(exp, current), nil
}

func lockExperimentRow(tx *gorm.DB, id int64, exp *model.LLMExperiment) error {
	q := tx
	if tx.Dialector.Name() != "sqlite" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return q.First(exp, id).Error
}

func lockExperimentModule(tx *gorm.DB, module string) error {
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.LLMExperimentModuleLock{
		Module: module,
	}).Error; err != nil {
		return err
	}
	var slot model.LLMExperimentModuleLock
	q := tx.Where("module = ?", module)
	if tx.Dialector.Name() != "sqlite" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return q.First(&slot).Error
}

type experimentBaselineStaleError struct{ reason string }

func (e *experimentBaselineStaleError) Error() string {
	return "实验 champion 基线已失效：" + e.reason + "；请基于当前 champion 重建实验"
}

// LLMExperimentInput 创建入参。
type LLMExperimentInput struct {
	Module              string                        `json:"module"`
	ExperimentType      string                        `json:"experiment_type"`
	Name                string                        `json:"name"`
	Hypothesis          string                        `json:"hypothesis"`
	ExpectedImprovement string                        `json:"expected_improvement"`
	ChallengerContent   string                        `json:"challenger_content"`
	SampleTarget        int                           `json:"sample_target"`
	ParentID            int64                         `json:"parent_id"`
	Protocol            *ScoreBlindEvaluationProtocol `json:"protocol"`
}

// ScoreBlindEvaluationProtocol 是启动前必须预注册并锁定的收益评价协议。严重亏损
// 定义沿用 so1 的 net_return_pct < -5%；MaxSevereLossRatePct 是允许的严重亏损率上限。
type ScoreBlindEvaluationProtocol struct {
	ShortHorizons           []int   `json:"short_horizons"`
	LongHorizons            []int   `json:"long_horizons"`
	MinEffectiveBatches     int     `json:"min_effective_batches"`
	MaxCoverageDropPct      float64 `json:"max_coverage_drop_pct"`
	MaxSevereLossRatePct    float64 `json:"max_severe_loss_rate_pct"`
	MultipleTestingMethod   string  `json:"multiple_testing_method"`
	SevereLossDefinitionPct float64 `json:"severe_loss_definition_pct"`
}

func normalizeExperimentType(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", model.LLMExperimentTypePrompt:
		return model.LLMExperimentTypePrompt, nil
	case model.LLMExperimentTypeScoreBlind:
		return model.LLMExperimentTypeScoreBlind, nil
	default:
		return "", errors.New("experiment_type 须为 prompt 或 score_blind")
	}
}

// experimentTypeOf 只在读取历史行时把空值兼容为 prompt，不写回数据库。未知类型
// 原样返回，由各入口 fail-closed，绝不能误入 prompt 发布/评估路径。
func experimentTypeOf(exp *model.LLMExperiment) string {
	if exp == nil {
		return model.LLMExperimentTypePrompt
	}
	typ := strings.TrimSpace(exp.ExperimentType)
	switch typ {
	case "", model.LLMExperimentTypePrompt:
		return model.LLMExperimentTypePrompt
	case model.LLMExperimentTypeScoreBlind:
		return model.LLMExperimentTypeScoreBlind
	default:
		return typ
	}
}

func normalizeScoreBlindProtocol(p *ScoreBlindEvaluationProtocol) (*ScoreBlindEvaluationProtocol, string, string, error) {
	if p == nil {
		return nil, "", "", errors.New("score_blind 启动前必须填写评价协议")
	}
	n := *p
	n.ShortHorizons = append([]int(nil), p.ShortHorizons...)
	n.LongHorizons = append([]int(nil), p.LongHorizons...)
	n.MultipleTestingMethod = strings.ToLower(strings.TrimSpace(p.MultipleTestingMethod))
	n.SevereLossDefinitionPct = -5
	if len(n.ShortHorizons) != 2 || n.ShortHorizons[0] != 5 || n.ShortHorizons[1] != 10 ||
		len(n.LongHorizons) != 2 || n.LongHorizons[0] != 20 || n.LongHorizons[1] != 60 {
		return nil, "", "", errors.New("评价窗口必须锁定为短线 [5,10]、长线 [20,60] 交易日")
	}
	if n.MinEffectiveBatches <= 0 || n.MinEffectiveBatches > 100000 {
		return nil, "", "", errors.New("min_effective_batches 必须为 1..100000")
	}
	if n.MaxCoverageDropPct <= 0 || n.MaxCoverageDropPct > 100 {
		return nil, "", "", errors.New("max_coverage_drop_pct 必须为 (0,100]")
	}
	if n.MaxSevereLossRatePct <= 0 || n.MaxSevereLossRatePct > 100 {
		return nil, "", "", errors.New("max_severe_loss_rate_pct 必须为 (0,100]")
	}
	switch n.MultipleTestingMethod {
	case "holm_bonferroni", "bonferroni", "benjamini_hochberg":
	default:
		return nil, "", "", errors.New("multiple_testing_method 须为 holm_bonferroni/bonferroni/benjamini_hochberg")
	}
	b, err := json.Marshal(&n)
	if err != nil {
		return nil, "", "", err
	}
	raw := string(b)
	return &n, raw, llmContentHash(raw), nil
}

func validateScoreBlindProtocol(exp *model.LLMExperiment, requireLocked bool) error {
	typ := experimentTypeOf(exp)
	if typ != model.LLMExperimentTypePrompt && typ != model.LLMExperimentTypeScoreBlind {
		return fmt.Errorf("未知 experiment_type=%q，禁止进入实验生命周期", typ)
	}
	if typ != model.LLMExperimentTypeScoreBlind {
		return nil
	}
	if experimentExpectedChampionCustom(exp) {
		return errors.New("score_blind 仅支持默认推荐任务段；自定义 champion 会引入第二个实验变量")
	}
	if exp.InputSchemaVersion != scoreBlindInputSchemaVersion {
		return fmt.Errorf("score_blind 输入 schema 必须为 %s", scoreBlindInputSchemaVersion)
	}
	var p ScoreBlindEvaluationProtocol
	if err := json.Unmarshal([]byte(exp.ProtocolJSON), &p); err != nil {
		return errors.New("score_blind 评价协议 JSON 已损坏")
	}
	_, canonical, hash, err := normalizeScoreBlindProtocol(&p)
	if err != nil {
		return err
	}
	if exp.SampleTarget < 2*p.MinEffectiveBatches {
		return fmt.Errorf("score_blind 采样总目标至少应为每类最小有效批次数的 2 倍：当前 %d，至少 %d",
			exp.SampleTarget, 2*p.MinEffectiveBatches)
	}
	if canonical != exp.ProtocolJSON || hash == "" || hash != exp.ProtocolHash {
		return errors.New("score_blind 评价协议与创建时 hash 不一致，禁止启动或继续采样")
	}
	if requireLocked && exp.ProtocolLockedAt == nil {
		return errors.New("score_blind 评价协议尚未锁定")
	}
	return nil
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
	experimentType, typeErr := normalizeExperimentType(in.ExperimentType)
	if typeErr != nil {
		return nil, nil, typeErr
	}
	if name == "" || hypo == "" || expect == "" {
		return nil, nil, errors.New("name/hypothesis/expected_improvement 必填（P2-2：没有假设与预期的实验不立项）")
	}
	var protocolJSON, protocolHash string
	if experimentType == model.LLMExperimentTypePrompt {
		if content == "" {
			return nil, nil, errors.New("prompt 实验的 challenger_content 必填")
		}
		if len([]rune(content)) > maxPromptContentRunes {
			return nil, nil, fmt.Errorf("challenger 内容超长（上限 %d 字符）", maxPromptContentRunes)
		}
		if in.Protocol != nil {
			return nil, nil, errors.New("prompt 实验不能携带 score_blind 评价协议")
		}
	} else {
		if content != "" {
			return nil, nil, errors.New("score_blind 是输入实验，不接受 challenger_content")
		}
		if in.ParentID != 0 {
			return nil, nil, errors.New("score_blind 不进入 prompt 版本谱系，parent_id 必须为 0")
		}
		_, protocolJSON, protocolHash, typeErr = normalizeScoreBlindProtocol(in.Protocol)
		if typeErr != nil {
			return nil, nil, typeErr
		}
	}
	if in.ParentID > 0 {
		var parent model.LLMExperiment
		if err := common.DB.First(&parent, in.ParentID).Error; err != nil {
			return nil, nil, errors.New("父实验不存在")
		}
		if parent.UserID != userID || parent.Module != in.Module ||
			experimentTypeOf(&parent) != model.LLMExperimentTypePrompt {
			return nil, nil, errors.New("父实验必须是同一用户、同一模块的 prompt 实验")
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
	if experimentType == model.LLMExperimentTypeScoreBlind && target < 2*in.Protocol.MinEffectiveBatches {
		return nil, nil, fmt.Errorf("score_blind 采样总目标至少应为每类最小有效批次数的 2 倍：当前 %d，至少 %d",
			target, 2*in.Protocol.MinEffectiveBatches)
	}

	promptExperimentStateMu.Lock()
	defer promptExperimentStateMu.Unlock()
	var exp *model.LLMExperiment
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		champ, err := lockExperimentPromptBaseline(tx, userID, promptModule)
		if err != nil {
			return fmt.Errorf("读取 champion 基线失败: %v", err)
		}
		if experimentType == model.LLMExperimentTypeScoreBlind && champ.Custom {
			return errors.New("score_blind 仅支持默认推荐任务段；请先停用自定义 champion，避免引入第二个实验变量")
		}
		exp = &model.LLMExperiment{
			UserID: userID, Module: in.Module, PromptModule: promptModule,
			ExperimentType: experimentType,
			Name:           name, Hypothesis: hypo, ExpectedImprovement: expect,
			ChallengerContent: content,
			ChampionVersion:   champ.Version, ChampionHash: champ.Hash,
			ChampionCustom: champ.Custom, ChampionRevision: champ.Revision,
			ChampionGeneration: champ.Generation,
			ChampionContent:    champ.Content, BaselineVersion: llmExperimentBaselineVersion,
			Status: model.ExpStatusDraft, SampleTarget: target, ParentID: in.ParentID,
		}
		if experimentType == model.LLMExperimentTypePrompt {
			exp.ChallengerHash = promptContentHash(content)
		} else {
			lockedAt := time.Now()
			exp.InputSchemaVersion = scoreBlindInputSchemaVersion
			exp.ProtocolJSON, exp.ProtocolHash = protocolJSON, protocolHash
			exp.ProtocolLockedAt = &lockedAt
		}
		return tx.Create(exp).Error
	})
	if err != nil {
		return nil, nil, err
	}
	if experimentType == model.LLMExperimentTypePrompt {
		return exp, lintPromptContent(promptModule, content), nil
	}
	return exp, nil, nil
}

// StartLLMExperiment draft→running。单变量纪律：同一模块全局同时只允许一个 running
// 实验（两个 challenger 并行会互抢影子流量且无法归因）。
func StartLLMExperiment(id int64) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	var staleReason string
	err := withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockExperimentRow(tx, id, &exp); err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errors.New("实验不存在")
				}
				return err
			}
			if exp.Status != model.ExpStatusDraft {
				return fmt.Errorf("仅 draft 可启动（当前 %s）", exp.Status)
			}
			if err := validateScoreBlindProtocol(&exp, false); err != nil {
				return err
			}
			if err := lockExperimentModule(tx, exp.Module); err != nil {
				return err
			}
			if _, reason, err := validateExperimentCurrentBaseline(tx, &exp); err != nil {
				return fmt.Errorf("校验 champion 基线失败: %v", err)
			} else if reason != "" {
				staleReason = reason
				return &experimentBaselineStaleError{reason: reason}
			}
			var running int64
			if err := tx.Model(&model.LLMExperiment{}).
				Where("module = ? AND status = ?", exp.Module, model.ExpStatusRunning).
				Count(&running).Error; err != nil {
				return err
			}
			if running > 0 {
				return errors.New("该模块已有 running 实验（单变量纪律：一个模块同时只跑一个 challenger）")
			}
			now := time.Now()
			updates := map[string]any{"status": model.ExpStatusRunning, "started_at": now}
			if experimentTypeOf(&exp) == model.LLMExperimentTypeScoreBlind && exp.ProtocolLockedAt == nil {
				updates["protocol_locked_at"] = now
			}
			res := tx.Model(&model.LLMExperiment{}).
				Where("id = ? AND status = ? AND (baseline_invalid_reason = '' OR baseline_invalid_reason IS NULL)",
					exp.ID, model.ExpStatusDraft).
				Updates(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("实验状态或 champion 基线已被并发修改，请刷新后重试")
			}
			exp.Status, exp.StartedAt = model.ExpStatusRunning, &now
			if experimentTypeOf(&exp) == model.LLMExperimentTypeScoreBlind && exp.ProtocolLockedAt == nil {
				exp.ProtocolLockedAt = &now
			}
			return nil
		})
	})
	if err != nil {
		if staleReason != "" && exp.ID > 0 {
			markExperimentBaselineInvalid(common.DB, &exp, staleReason)
		}
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
	switch conclusion {
	case model.ExpConcludeImproved, model.ExpConcludeNoGain, model.ExpConcludeWorse:
	default:
		return nil, errors.New("conclusion 须为 improved/no_improvement/worse")
	}
	failureReason = strings.TrimSpace(failureReason)
	if conclusion != model.ExpConcludeImproved && failureReason == "" {
		return nil, errors.New("未达预期的实验必须记录失败原因（P2-2：失败原因是飞轮资产）")
	}
	var exp model.LLMExperiment
	err := withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockExperimentRow(tx, id, &exp); err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errors.New("实验不存在")
				}
				return err
			}
			if exp.Status != model.ExpStatusRunning {
				return fmt.Errorf("仅 running 可完成（当前 %s）", exp.Status)
			}
			if err := validateScoreBlindProtocol(&exp, true); err != nil {
				return err
			}
			if _, err := finalizeStaleExperimentClaims(tx, exp.ID); err != nil {
				return err
			}
			var running int64
			if err := tx.Model(&model.LLMExperimentRun{}).
				Where("experiment_id = ? AND run_status = ?", exp.ID, model.LLMExperimentRunRunning).
				Count(&running).Error; err != nil {
				return err
			}
			if running > 0 {
				return errors.New("仍有影子调用进行中，请等待运行事实终结后再完成实验")
			}
			var runs []model.LLMExperimentRun
			if err := tx.Where("experiment_id = ? AND (run_status IS NULL OR run_status = '' OR run_status <> ?)",
				exp.ID, model.LLMExperimentRunRunning).Find(&runs).Error; err != nil {
				return err
			}
			actual := aggregateExperimentRuns(runs)
			b, _ := json.Marshal(actual)
			now := time.Now()
			res := tx.Model(&model.LLMExperiment{}).Where("id = ? AND status = ?", exp.ID, model.ExpStatusRunning).
				Updates(map[string]any{"status": model.ExpStatusCompleted, "actual_json": string(b),
					"conclusion": conclusion, "failure_reason": failureReason, "sample_count": len(runs),
					"completed_at": now, "updated_at": now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("实验状态已被并发修改，完成操作已取消")
			}
			exp.Status, exp.ActualJSON, exp.Conclusion = model.ExpStatusCompleted, string(b), conclusion
			exp.FailureReason, exp.SampleCount, exp.CompletedAt = failureReason, len(runs), &now
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return &exp, nil
}

// PromoteLLMExperiment completed→promoted：**P1-9 发布质量门（代码硬检）**——全部通过
// 才把 challenger 内容落为启用模板（champion 指针切换，生成不可变 revision 快照）。
// 任何一条不过=拒绝晋级（保持 completed，可继续 abandon 或复制新实验迭代）。
func PromoteLLMExperiment(id int64) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	var staleReason string
	err := withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockExperimentRow(tx, id, &exp); err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errors.New("实验不存在")
				}
				return err
			}
			if experimentTypeOf(&exp) != model.LLMExperimentTypePrompt {
				return errors.New("score_blind 是纯影子输入实验，不进入 prompt promote 路径")
			}
			// 门 1：状态机——只有 completed（有聚合反馈）可晋级。
			if exp.Status != model.ExpStatusCompleted {
				return fmt.Errorf("仅 completed 可晋级（当前 %s）", exp.Status)
			}
			// 门 2：无增量不晋级（P2-2）。
			if exp.Conclusion != model.ExpConcludeImproved {
				return fmt.Errorf("结论为 %q：无增量不晋级", exp.Conclusion)
			}
			// 门 3/4：从样本重算样本量与结构化有效率，不信 ActualJSON 自报。
			var runs []model.LLMExperimentRun
			if err := tx.Where("experiment_id = ?", exp.ID).Find(&runs).Error; err != nil {
				return err
			}
			if len(runs) < llmExperimentMinSamples {
				return fmt.Errorf("影子样本 %d 不足 %d，不得晋级", len(runs), llmExperimentMinSamples)
			}
			valid := 0
			for _, r := range runs {
				if r.Valid {
					valid++
				}
			}
			if rate := float64(valid) / float64(len(runs)); rate < llmExperimentMinValidRate {
				return fmt.Errorf("challenger 结构化有效率 %.0f%% 低于 %.0f%%，不得晋级",
					rate*100, llmExperimentMinValidRate*100)
			}
			// 门 5：challenger 快照与 champion 基线均不可漂移。
			if promptContentHash(exp.ChallengerContent) != exp.ChallengerHash {
				return errors.New("challenger 内容与创建时 hash 不符（快照被篡改），拒绝晋级")
			}
			baseline, reason, err := validateExperimentCurrentBaseline(tx, &exp)
			if err != nil {
				return fmt.Errorf("校验 champion 基线失败: %v", err)
			}
			if reason != "" {
				staleReason = reason
				return &experimentBaselineStaleError{reason: reason}
			}

			// 门 6：最新审计工件必须 PASS，且同时绑定 challenger 与 champion 锚。
			audit := latestReleaseAuditDB(tx, exp.ID)
			if audit == nil {
				return errors.New("缺少发布审计工件：先在实验页运行「发布审计」（P2-6 门 6：未 PASS 不晋级）")
			}
			if audit.ChallengerHash != exp.ChallengerHash {
				return errors.New("最新审计工件的内容 hash 与当前 challenger 不符（内容变过须重审），拒绝晋级")
			}
			if audit.ChampionHash != promptContentHash(releaseAuditChampionContentDB(tx, &exp)) {
				return errors.New("最新审计工件未绑定实验的 champion 基线（旧版工件或基线被改动），须重新运行发布审计")
			}
			if audit.Verdict != model.ReleaseAuditPass {
				return fmt.Errorf("最新发布审计 verdict=%s（%s）：未 PASS 不晋级", audit.Verdict, audit.Summary)
			}

			// 模板写入与实验状态共用事务。expected Row 是基线校验读取的 CAS 锚；若其后有
			// 并发编辑，条件 UPDATE 不命中，整个事务（含 revision 快照）回滚。
			exp.PrePromoteEnabled = baseline.Custom
			exp.PrePromoteContent = ""
			if baseline.Custom {
				exp.PrePromoteContent = baseline.Content
			}
			module, content, hash, _, normErr := normalizePromptInput(PromptInput{
				Module: exp.PromptModule, Content: exp.ChallengerContent, Enabled: true,
			})
			if normErr != nil {
				return normErr
			}
			tpl, err := upsertPromptTemplateTx(tx, exp.UserID, module, content, hash, true, baseline.Row, true)
			if err != nil {
				return fmt.Errorf("晋级落模板失败: %v", err)
			}
			// 每次晋级都取得一个唯一的 champion 归属代际。通常模板内容/启用状态变化已由
			// Upsert 推进 generation；challenger 与当前内容相同时 Upsert 是幂等 no-op，仍须
			// 推进一步，避免后续同 hash 实验晋级后旧实验还能冒充当前回滚对象。
			promotedState, err := lockPromptChampionState(tx, exp.UserID, exp.PromptModule)
			if err != nil {
				return fmt.Errorf("读取晋级 champion generation 失败: %v", err)
			}
			if promotedState.Generation == baseline.Generation {
				if err := advancePromptChampionState(tx, promotedState); err != nil {
					return fmt.Errorf("推进晋级 champion generation 失败: %v", err)
				}
			}
			exp.PromotedGeneration = promotedState.Generation
			now := time.Now()
			res := tx.Model(&model.LLMExperiment{}).
				Where("id = ? AND status = ? AND (baseline_invalid_reason = '' OR baseline_invalid_reason IS NULL)",
					exp.ID, model.ExpStatusCompleted).
				Updates(map[string]any{
					"status": model.ExpStatusPromoted, "promoted_revision": tpl.Revision,
					"promoted_generation": exp.PromotedGeneration,
					"pre_promote_enabled": exp.PrePromoteEnabled, "pre_promote_content": exp.PrePromoteContent,
					"updated_at": now,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("实验状态或 champion 基线已被并发修改，晋级已取消")
			}
			exp.Status, exp.PromotedRevision = model.ExpStatusPromoted, tpl.Revision
			return nil
		})
	})
	if err != nil {
		// 事务内写失效原因会随拒绝一起回滚，因此在事务外粘性落库。
		if staleReason != "" && exp.ID > 0 {
			markExperimentBaselineInvalid(common.DB, &exp, staleReason)
		}
		return nil, err
	}
	common.SysLog("实验 #%d 晋级：module=%s revision=%d hash=%s（回滚=实验页「一键切回 champion」）",
		exp.ID, exp.PromptModule, exp.PromotedRevision, exp.ChallengerHash)
	return &exp, nil
}

// AbandonLLMExperiment 任何非 promoted 状态→abandoned（记录原因；样本保留可追溯）。
func AbandonLLMExperiment(id int64, reason string) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	err := withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockExperimentRow(tx, id, &exp); err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errors.New("实验不存在")
				}
				return err
			}
			if exp.Status == model.ExpStatusPromoted {
				return errors.New("已晋级实验不可废弃（回滚走「一键切回 champion」）")
			}
			if exp.Status == model.ExpStatusRolledBack {
				return errors.New("已回滚实验是终态，不可废弃")
			}
			if exp.Status == model.ExpStatusAbandoned {
				return nil
			}
			if exp.Status == model.ExpStatusRunning {
				if _, err := finalizeStaleExperimentClaims(tx, exp.ID); err != nil {
					return err
				}
				var running int64
				if err := tx.Model(&model.LLMExperimentRun{}).
					Where("experiment_id = ? AND run_status = ?", exp.ID, model.LLMExperimentRunRunning).
					Count(&running).Error; err != nil {
					return err
				}
				if running > 0 {
					return errors.New("仍有影子调用进行中，请等待运行事实终结后再废弃实验")
				}
			}
			updates := map[string]any{"status": model.ExpStatusAbandoned, "updated_at": time.Now()}
			if r := strings.TrimSpace(reason); r != "" {
				updates["failure_reason"] = r
				exp.FailureReason = r
			}
			res := tx.Model(&model.LLMExperiment{}).Where("id = ? AND status = ?", exp.ID, exp.Status).Updates(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("实验状态已被并发修改，废弃操作已取消")
			}
			exp.Status = model.ExpStatusAbandoned
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return &exp, nil
}

// LLMExperimentView 管理端实验视图：附回滚可用性判定（审查修复批——promoted 实验的
// 当前启用模板已不是其晋级产物时，回滚会覆盖更新的 champion，前端据此禁用按钮并说明）。
type LLMExperimentView struct {
	model.LLMExperiment
	RollbackStale string `json:"rollback_stale,omitempty"`
	BaselineStale string `json:"baseline_stale,omitempty"`
}

// ListLLMExperiments 全部实验（管理端；按 id 倒序）。
func ListLLMExperiments() ([]LLMExperimentView, error) {
	var rows []model.LLMExperiment
	if err := common.DB.Order("id DESC").Limit(200).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]LLMExperimentView, 0, len(rows))
	for i := range rows {
		baselineStale := rows[i].BaselineInvalidReason
		if baselineStale == "" && (rows[i].Status == model.ExpStatusDraft ||
			rows[i].Status == model.ExpStatusRunning || rows[i].Status == model.ExpStatusCompleted) {
			_, reason, err := validateExperimentCurrentBaseline(common.DB, &rows[i])
			if err != nil {
				return nil, err
			}
			baselineStale = reason
		}
		out = append(out, LLMExperimentView{
			LLMExperiment: rows[i], RollbackStale: experimentRollbackStale(&rows[i]),
			BaselineStale: baselineStale,
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

// llmExperimentPickFact 只固化决策质量评估所需的规范化字段，不复制理由、风险与
// 交易计划大文本。Order 是模型原始输出顺序（1-based）。
type llmExperimentPickFact struct {
	Symbol     string `json:"symbol"`
	Order      int    `json:"order"`
	Action     string `json:"action"`
	Confidence int    `json:"confidence"`
}

type scoreBlindInputSnapshot struct {
	ExperimentType      string          `json:"experiment_type"`
	InputSchemaVersion  string          `json:"input_schema_version"`
	Seed                int64           `json:"seed"`
	CandidateOrder      []string        `json:"candidate_order"`
	SchemaVersion       string          `json:"schema_version"`
	ConfigID            int64           `json:"config_id"`
	Provider            string          `json:"provider"`
	Model               string          `json:"model"`
	EndpointType        string          `json:"endpoint_type"`
	Temperature         float64         `json:"temperature"`
	MaxTokens           int             `json:"max_tokens"`
	JSONMode            bool            `json:"json_mode"`
	AccuracyContract    bool            `json:"accuracy_contract"`
	TemperatureOmitted  bool            `json:"temperature_omitted"`
	MaxCompletionTokens bool            `json:"max_completion_tokens"`
	Route               LLMRouteApplied `json:"route"`
	Messages            []chatMessage   `json:"messages"`
	// 思考档位两字段一律 omitempty：未配档位（历史与存量批次的常态）时序列化字节与
	// InputHash 完全不变，sb2 样本继续可比，无需递增 scoreBlindInputSchemaVersion。
	// 一旦某配置配上档位，其后续批次快照随之变化——那是真实的单变量变更，本就该体现。
	ReasoningEffort        string `json:"reasoning_effort,omitempty"`
	ReasoningEffortOmitted bool   `json:"reasoning_effort_omitted,omitempty"`
}

func marshalLLMExperimentPicks(picks []recPick) string {
	facts := make([]llmExperimentPickFact, 0, len(picks))
	for i, p := range picks {
		facts = append(facts, llmExperimentPickFact{
			Symbol: p.Symbol, Order: i + 1, Action: p.Action, Confidence: int(p.Confidence),
		})
	}
	b, _ := json.Marshal(facts)
	return string(b)
}

// activeExperimentFor 查该用户该模块可采样的 running 实验（采样只命中创建者本人）。
func activeExperimentFor(module string, userID int64) *model.LLMExperiment {
	var exp model.LLMExperiment
	err := common.DB.Where("module = ? AND user_id = ? AND status = ? AND sample_count < sample_target",
		module, userID, model.ExpStatusRunning).
		Where("baseline_invalid_reason = '' OR baseline_invalid_reason IS NULL").First(&exp).Error
	if err != nil {
		return nil
	}
	return &exp
}

var (
	errExperimentSampleClosed    = errors.New("实验已停止采样")
	errExperimentSampleDuplicate = errors.New("同一批次已存在实验样本")
)

func scoreBlindSeed(experimentID, batchID int64) int64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", scoreBlindInputSchemaVersion, experimentID, batchID)))
	// 管理 API 由浏览器消费；限制在 JavaScript Number 的精确整数范围内，页面展示
	// 与持久化 seed 必须逐位一致，不能因 64 位 JSON 数字被舍入而失去可复现性。
	seed := int64(binary.BigEndian.Uint64(sum[:8]) & ((uint64(1) << 53) - 1))
	if seed == 0 {
		return 1
	}
	return seed
}

// shuffledScoreBlindCandidates 先按 symbol 建立与上游偶然顺序无关的规范起点，再用
// 固定 seed 打乱。返回的 order 就是随后 JSON 数组的实际顺序，不允许事后重建。
func shuffledScoreBlindCandidates(cands []candidate, seed int64) ([]candidate, []string, error) {
	shuffled := append([]candidate(nil), cands...)
	sort.SliceStable(shuffled, func(i, j int) bool { return shuffled[i].Symbol < shuffled[j].Symbol })
	seen := make(map[string]bool, len(shuffled))
	for _, cand := range shuffled {
		if cand.Symbol == "" || seen[cand.Symbol] {
			return nil, nil, errors.New("score_blind 候选集合含空 symbol 或重复 symbol")
		}
		seen[cand.Symbol] = true
	}
	rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	order := make([]string, 0, len(shuffled))
	for _, cand := range shuffled {
		if !seen[cand.Symbol] {
			return nil, nil, errors.New("score_blind 打乱后候选集合发生变化")
		}
		order = append(order, cand.Symbol)
	}
	if len(order) != len(cands) {
		return nil, nil, errors.New("score_blind 打乱后候选数量发生变化")
	}
	return shuffled, order, nil
}

// finalizeStaleExperimentClaims 把进程中断遗留的 running 占位固化为失败事实。
// 结果未知时绝不删除或重试：这既保留精确输入，又让 batch_id 去重锚永久有效。
func finalizeStaleExperimentClaims(tx *gorm.DB, experimentID int64) (int64, error) {
	stale := tx.Model(&model.LLMExperimentRun{}).
		Where("experiment_id = ? AND run_status = ? AND created_at < ?", experimentID,
			model.LLMExperimentRunRunning, time.Now().Add(-llmExperimentRunClaimTTL)).
		Updates(map[string]any{
			"run_status":   model.LLMExperimentRunFailed,
			"finish_state": "call_failed",
			"error":        "影子调用占位超时，结果未知；为保证同批最多一次不重试",
		})
	if stale.Error != nil || stale.RowsAffected == 0 {
		return stale.RowsAffected, stale.Error
	}
	if err := tx.Model(&model.LLMExperiment{}).Where("id = ?", experimentID).
		UpdateColumn("sample_count", gorm.Expr("sample_count + ?", stale.RowsAffected)).Error; err != nil {
		return 0, err
	}
	return stale.RowsAffected, nil
}

// claimExperimentRun 在外部调用前持久化 running 工件。模块槽锁串行化同批全局去重，
// 实验行锁保护 target 余量和状态，因此同一批所有影子类型合计最多一次调用。
func claimExperimentRun(exp *model.LLMExperiment, row *model.LLMExperimentRun) (string, error) {
	var staleReason string
	err := withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockExperimentModule(tx, exp.Module); err != nil {
				return err
			}
			var latest model.LLMExperiment
			if err := lockExperimentRow(tx, exp.ID, &latest); err != nil {
				return err
			}
			if latest.Module != exp.Module || latest.Status != model.ExpStatusRunning || latest.UserID != row.UserID ||
				strings.TrimSpace(latest.BaselineInvalidReason) != "" {
				return errExperimentSampleClosed
			}
			latestType := experimentTypeOf(&latest)
			if (latestType != model.LLMExperimentTypePrompt &&
				latestType != model.LLMExperimentTypeScoreBlind) || latestType != row.ExperimentType {
				return errExperimentSampleClosed
			}
			if row.ExperimentType == model.LLMExperimentTypeScoreBlind {
				if err := validateScoreBlindProtocol(&latest, true); err != nil {
					return err
				}
			} else if promptContentHash(latest.ChallengerContent) != latest.ChallengerHash ||
				latest.ChallengerHash != exp.ChallengerHash {
				return errExperimentSampleClosed
			}
			if _, reason, err := validateExperimentCurrentBaseline(tx, &latest); err != nil {
				return err
			} else if reason != "" {
				staleReason = reason
				return &experimentBaselineStaleError{reason: reason}
			}

			// TTL 大于业务 deadline。进程中断后的结果已不可知，但调用可能已经发出。
			staleCount, err := finalizeStaleExperimentClaims(tx, latest.ID)
			if err != nil {
				return err
			}
			latest.SampleCount += int(staleCount)
			var existing int64
			if err := tx.Model(&model.LLMExperimentRun{}).
				Where("batch_id = ?", row.BatchID).
				Count(&existing).Error; err != nil {
				return err
			}
			if existing > 0 {
				return errExperimentSampleDuplicate
			}
			var running int64
			if err := tx.Model(&model.LLMExperimentRun{}).
				Where("experiment_id = ? AND run_status = ?", latest.ID, model.LLMExperimentRunRunning).
				Count(&running).Error; err != nil {
				return err
			}
			if latest.SampleCount+int(running) >= latest.SampleTarget {
				return errExperimentSampleClosed
			}
			return tx.Create(row).Error
		})
	})
	return staleReason, err
}

// finishExperimentRun 只终结仍属于 running 实验的对应 claim。Complete/Abandon 会在
// 存在 claim 时拒绝；若调用期间协议/基线异常，claim 也必须固化为 failed，禁止删除
// 已经发出的外部调用事实。
func finishExperimentRun(exp *model.LLMExperiment, row *model.LLMExperimentRun) (bool, string, error) {
	var finalized bool
	var staleReason string
	var rejectedErr error
	err := withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			var latest model.LLMExperiment
			if err := lockExperimentRow(tx, exp.ID, &latest); err != nil {
				return err
			}
			var claim model.LLMExperimentRun
			if err := tx.Where("id = ? AND experiment_id = ? AND run_status = ?", row.ID, exp.ID,
				model.LLMExperimentRunRunning).First(&claim).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			runUpdates := func() map[string]any {
				return map[string]any{
					"run_status": row.RunStatus, "valid": row.Valid, "picks_count": row.PicksCount,
					"overlap_count": row.OverlapCount, "coverage_json": row.CoverageJSON,
					"challenger_picks_json": row.ChallengerPicksJSON,
					"challenger_tokens":     row.ChallengerTokens, "challenger_ms": row.ChallengerMs,
					"finish_state": row.FinishState, "error": row.Error,
				}
			}
			advanceSampleCount := func(required bool) error {
				res := tx.Model(&model.LLMExperiment{}).
					Where("id = ? AND status = ? AND sample_count < sample_target", latest.ID, model.ExpStatusRunning).
					UpdateColumn("sample_count", gorm.Expr("sample_count + 1"))
				if res.Error != nil {
					return res.Error
				}
				if required && res.RowsAffected != 1 {
					return errExperimentSampleClosed
				}
				return nil
			}
			rejectClaim := func(reason error) error {
				rejectedErr = reason
				updates := runUpdates()
				updates["run_status"] = model.LLMExperimentRunFailed
				updates["valid"] = false
				detail := "影子调用完成后实验状态校验失败：" + reason.Error()
				if strings.TrimSpace(row.Error) != "" {
					detail = row.Error + "；" + detail
				}
				updates["error"] = truncateRunes(detail, 500)
				res := tx.Model(&model.LLMExperimentRun{}).
					Where("id = ? AND run_status = ?", claim.ID, model.LLMExperimentRunRunning).Updates(updates)
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					return errExperimentSampleClosed
				}
				if err := advanceSampleCount(false); err != nil {
					return err
				}
				finalized = true
				return nil
			}
			if latest.Status != model.ExpStatusRunning || strings.TrimSpace(latest.BaselineInvalidReason) != "" ||
				latest.SampleCount >= latest.SampleTarget {
				return rejectClaim(errExperimentSampleClosed)
			}
			latestType := experimentTypeOf(&latest)
			if (latestType != model.LLMExperimentTypePrompt &&
				latestType != model.LLMExperimentTypeScoreBlind) || latestType != row.ExperimentType {
				return rejectClaim(errExperimentSampleClosed)
			}
			if row.ExperimentType == model.LLMExperimentTypeScoreBlind {
				if err := validateScoreBlindProtocol(&latest, true); err != nil {
					return rejectClaim(err)
				}
			} else if promptContentHash(latest.ChallengerContent) != latest.ChallengerHash ||
				latest.ChallengerHash != exp.ChallengerHash {
				return rejectClaim(errExperimentSampleClosed)
			}
			if _, reason, err := validateExperimentCurrentBaseline(tx, &latest); err != nil {
				return err
			} else if reason != "" {
				staleReason = reason
				return rejectClaim(&experimentBaselineStaleError{reason: reason})
			}
			res := tx.Model(&model.LLMExperimentRun{}).
				Where("id = ? AND run_status = ?", row.ID, model.LLMExperimentRunRunning).Updates(runUpdates())
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errExperimentSampleClosed
			}
			if err := advanceSampleCount(true); err != nil {
				return err
			}
			finalized = true
			return nil
		})
	})
	if err != nil {
		return false, staleReason, err
	}
	return finalized, staleReason, rejectedErr
}

// maybeChallengerShadow 是 recommendation 的统一影子调度器（best-effort）：普通 prompt
// challenger 与 score-blind 输入实验互斥，共用一次调用上限。所有成功、空 picks、越池与
// 失败只写实验/LLM 审计事实，绝不改业务 picks、批次状态、候选池或 l2 标签。
func (s *RecommendationService) maybeChallengerShadow(ctx context.Context, plan *recGenPlan,
	batch *model.RecommendationBatch, mainRun *llmRun, championMs int64, championUsage chatUsage,
	championPicks []recPick, poolBySymbol map[string]candidate,
	recType string, strat *strategyTemplate, market string, count int,
	llmCands []candidate, filters RecFilters, mktCtx *recMarketContext) {
	// 只有业务解析器已接受的 champion attempt 才具备可配对的真实调用目标。
	// repair 打满后 recommendation 会走确定性降级并返回 nil error，但不会提交
	// acceptedTarget；此时继续影子调用既没有合法 champion 输出，也无法保证同一路由目标。
	if common.DB == nil || !setting.LLMChallenger() || plan == nil || batch == nil ||
		mainRun == nil || batch.UserID != plan.userID || !batch.FactsRecorded ||
		mainRun.Attempts != 1 || mainRun.acceptedTarget.Model == "" {
		return
	}
	exp := activeExperimentFor("recommendation", plan.userID)
	if exp == nil {
		return
	}
	// 对照主调用实际消费的 plan.prompt 快照。旧批次只跳过；live champion 已漂移则
	// 粘性失效，防 A→B→A 洗回污染样本。
	championBaseline, err := experimentPromptBaselineFromRuntime(plan.prompt, exp.PromptModule)
	if err != nil {
		common.SysWarn("实验基线快照校验失败 exp=%d: %v", exp.ID, err)
		return
	}
	if reason := experimentBaselineStaleReason(exp, championBaseline); reason != "" {
		_, liveReason, liveErr := validateExperimentCurrentBaseline(common.DB, exp)
		if liveErr != nil {
			common.SysWarn("实验 #%d 复验 live champion 失败，跳过本批影子采样：%v", exp.ID, liveErr)
			return
		}
		if liveReason == "" {
			common.SysLog("实验 #%d 跳过早于当前基线的旧批次：%s", exp.ID, reason)
			return
		}
		markExperimentBaselineInvalid(common.DB, exp, liveReason)
		common.SysWarn("实验 #%d 停止影子采样：live champion 基线失效：%s", exp.ID, liveReason)
		return
	}

	experimentType := experimentTypeOf(exp)
	if experimentType != model.LLMExperimentTypePrompt &&
		experimentType != model.LLMExperimentTypeScoreBlind {
		common.SysWarn("实验 #%d 类型 %q 非法，停止采样", exp.ID, experimentType)
		return
	}
	var messages []chatMessage
	var scoreBlindOrder []string
	promptVersion := ""
	row := model.LLMExperimentRun{
		ExperimentID: exp.ID, UserID: plan.userID, BatchID: batch.ID,
		TraceID: batch.TraceID, ChampionRun: mainRun.RunID, ExperimentType: experimentType,
		RunStatus:     model.LLMExperimentRunRunning,
		ChampionPicks: len(championPicks), PickSchemaVersion: llmExperimentPickSchemaVersion,
		ChampionPicksJSON: marshalLLMExperimentPicks(championPicks),
		ChampionTokens:    championUsage.TotalTokens, ChampionMs: championMs,
	}
	if experimentType == model.LLMExperimentTypeScoreBlind {
		if err := validateScoreBlindProtocol(exp, true); err != nil {
			common.SysWarn("score_blind 实验 #%d 协议校验失败，停止采样：%v", exp.ID, err)
			return
		}
		row.InputSchemaVersion = scoreBlindInputSchemaVersion
		row.Seed = scoreBlindSeed(exp.ID, batch.ID)
		shuffled, order, err := shuffledScoreBlindCandidates(llmCands, row.Seed)
		if err != nil {
			common.SysWarn("score_blind 实验 #%d 输入冻结失败：%v", exp.ID, err)
			return
		}
		messages = s.buildScoreBlindMessages(plan.prompt, recType, strat, market, count,
			shuffled, filters, mktCtx)
		orderJSON, orderErr := json.Marshal(order)
		if orderErr != nil {
			common.SysWarn("score_blind 实验 #%d 输入顺序序列化失败：%v", exp.ID, orderErr)
			return
		}
		scoreBlindOrder = order
		row.InputOrderJSON = string(orderJSON)
		promptVersion = scoreBlindInputSchemaVersion
	} else {
		chPr := promptRuntime{Module: exp.PromptModule, Custom: true,
			Raw: exp.ChallengerContent, Hash: exp.ChallengerHash}
		messages = s.buildMessages(chPr, recType, strat, market, count, llmCands, filters, mktCtx)
		promptVersion = chPr.Version(recPromptVersion)
	}

	run := newLLMRun(batch.TraceID, mainRun.RunID, "experiment", "recommendation.v2", promptVersion)
	run.hashPrompt(messages)
	cfg, apiKey := plan.cfg, plan.apiKey
	params := chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
		ReasoningEffort: cfg.ReasoningEffort,
		Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("experiment", cfg.MaxTokens),
		Messages: messages, JSONMode: true, AllowPrivate: plan.allowPrivate,
		Meta: run.chatMeta(plan.userID, cfg, 1),
	}
	// 不重新查询当前路由：必须逐值复用同批 champion 已接受 attempt 的真实目标，
	// 防路由配置/健康状态在主调与影子间变化而破坏单变量对照。
	target := mainRun.acceptedTarget
	run.routeApplied = mainRun.acceptedRouteApplied
	prepared := prepareChatCompletionForAcceptedTarget(params, target)
	if experimentType == model.LLMExperimentTypeScoreBlind {
		snapshot, snapshotErr := json.Marshal(scoreBlindInputSnapshot{
			ExperimentType: experimentType, InputSchemaVersion: scoreBlindInputSchemaVersion,
			Seed: row.Seed, CandidateOrder: append([]string(nil), scoreBlindOrder...),
			SchemaVersion: "recommendation.v2", ConfigID: prepared.Meta.ConfigID, Provider: prepared.Meta.Provider,
			Model: prepared.Model, EndpointType: prepared.EndpointType, Temperature: prepared.Temperature,
			MaxTokens: prepared.MaxTokens, JSONMode: prepared.JSONMode,
			AccuracyContract:    target.AccuracyContract,
			TemperatureOmitted:  prepared.temperatureOmitted(),
			MaxCompletionTokens: prepared.usesMaxCompletionTokens(), Route: run.routeApplied,
			ReasoningEffort:        prepared.ReasoningEffort,
			ReasoningEffortOmitted: prepared.reasoningEffortOmitted(),
			Messages:               append([]chatMessage(nil), prepared.Messages...),
		})
		if snapshotErr != nil {
			common.SysWarn("score_blind 实验 #%d 精确输入序列化失败：%v", exp.ID, snapshotErr)
			return
		}
		row.InputSnapshotJSON = string(snapshot)
		row.InputHash = llmContentHash(row.InputSnapshotJSON)
	}

	staleReason, claimErr := claimExperimentRun(exp, &row)
	if staleReason != "" {
		markExperimentBaselineInvalid(common.DB, exp, staleReason)
	}
	if claimErr != nil {
		if !errors.Is(claimErr, errExperimentSampleClosed) &&
			!errors.Is(claimErr, errExperimentSampleDuplicate) {
			common.SysWarn("实验 #%d 抢占影子样本失败：%v", exp.ID, claimErr)
		}
		return
	}

	start := time.Now()
	res, callErr := chatCompletionPrepared(ctx, prepared)
	run.record(res, callErr)
	row.ChallengerMs = time.Since(start).Milliseconds()
	row.FinishState = run.FinishState
	row.RunStatus = model.LLMExperimentRunFailed
	if res != nil {
		row.ChallengerTokens = res.Usage.TotalTokens
		if res.Usage.TotalTokens > 0 {
			consumeQuota(plan.userID, res.Usage.TotalTokens, false)
		}
	}
	if callErr != nil {
		row.Error = truncateRunes(callErr.Error(), 500)
	} else {
		picks, _, diag, parseErr := parseAndFilterPicks(res.Content, poolBySymbol, count)
		if diag != nil {
			if b, jerr := json.Marshal(diag); jerr == nil {
				row.CoverageJSON = string(b)
			}
		}
		if len(picks) > 0 || parseErr == nil {
			row.PicksCount = len(picks)
			row.ChallengerPicksJSON = marshalLLMExperimentPicks(picks)
			champSet := make(map[string]bool, len(championPicks))
			for _, pick := range championPicks {
				champSet[pick.Symbol] = true
			}
			for _, pick := range picks {
				if champSet[pick.Symbol] {
					row.OverlapCount++
				}
			}
		}
		outOfPool := diag != nil && (diag.OutOfPoolCount > 0 || diag.UnknownCount > 0)
		switch {
		case outOfPool:
			row.RunStatus = model.LLMExperimentRunOutOfPool
			if parseErr != nil {
				row.Error = truncateRunes(parseErr.Error(), 500)
			} else {
				row.Error = "模型输出包含候选集合外或无法识别的 symbol"
			}
		case parseErr != nil:
			row.Error = truncateRunes(parseErr.Error(), 500)
		case len(picks) == 0:
			row.Valid = true
			row.RunStatus = model.LLMExperimentRunEmpty
		default:
			row.Valid = true
			row.RunStatus = model.LLMExperimentRunSuccess
		}
	}

	finalized, staleReason, finishErr := finishExperimentRun(exp, &row)
	if staleReason != "" {
		markExperimentBaselineInvalid(common.DB, exp, staleReason)
	}
	if finishErr != nil {
		if !errors.Is(finishErr, errExperimentSampleClosed) {
			common.SysWarn("实验样本终结失败 exp=%d run=%d: %v", exp.ID, row.ID, finishErr)
		}
		return
	}
	if !finalized {
		return
	}
	common.SysLog("实验 #%d(%s) 影子采样：batch=%d status=%s valid=%v picks=%d overlap=%d/%d tokens=%d",
		exp.ID, experimentType, batch.ID, row.RunStatus, row.Valid, row.PicksCount,
		row.OverlapCount, row.ChampionPicks, row.ChallengerTokens)
}
