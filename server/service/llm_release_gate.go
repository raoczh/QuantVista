package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// P2-6 自动发布门（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.3 P2-6）：
// 通过阈值（P2-1 已落地的 promote 门 1~5 程序硬检）+ **LLM 复核缺口**（本文件：P1-9
// 遗留的审计员角色，产出 PASS/FAIL 工件，promote 门 6 消费）+ 人工审批（promote 仍是
// 管理员显式动作，审计不自动晋级）+ 一键切回 champion（RollbackLLMExperiment）。
//
// 「LLM 只复核缺口」边界（不可漂移）：
//   - 程序硬检已覆盖的门槛（状态机/无增量不晋级/样本量/结构化有效率/内容 hash）
//     **不交给 LLM 复述**——审计员只看程序检不了的内容性缺口：
//       1) challenger 任务段是否试图削弱/覆盖系统契约与安全纪律（输出协议、宁缺毋滥、
//          不确定性声明——虽然 composeCustomTaskPrompt 会后置压制，意图本身是红旗）；
//       2) 是否夹带 hypothesis 未声明的额外变更（违反单变量纪律 §9.3）；
//       3) 是否含诱导模型伪造数据/绕过证据核验/无中生有引用的措辞；
//       4) 影子聚合指标与 improved 结论是否明显矛盾。
//   - **程序收口防口是心非**：verdict 封闭枚举 pass/fail；fail 必须附 findings；
//     出现 high 级 finding 时程序强制 verdict=fail（模型说 pass 也不算）。
//   - **不出假 PASS**：LLM 不可用如实报错（无调用无工件）；输出 repair 打满仍无效时
//     落 verdict=error 工件（error 不是 FAIL 判定，但同样挡晋级——未 PASS 不进 synthesis）。
//   - 工件逐次落 llm_release_audits（完整运行工件保留）；promote 门 6 只认最新工件，
//     且要求工件的 ChallengerHash 与实验当前内容一致（改内容须重审）。
//   - 回滚=晋级的对称操作：晋级瞬间固化 PrePromote 锚（当时启用中的模板状态），
//     RollbackLLMExperiment 一键恢复（经 Upsert 生成新 revision，不删工件），
//     状态 promoted→rolled_back（终态）。

const (
	// releaseAuditPromptVersion 审计员 prompt 版本（复核清单/输出结构变更时递增）。
	releaseAuditPromptVersion = "ra1"
	// releaseAuditMaxFindings findings 数量上限（归一截断）。
	releaseAuditMaxFindings = 8
)

// releaseAuditSystem 发布审计员系统提示：只复核程序硬检覆盖不了的缺口。
const releaseAuditSystem = `你是 prompt 实验的发布审计员。程序已硬检过样本量、结构化有效率、内容 hash 与状态机——这些不用你管。你只复核以下程序检不了的内容性缺口：
1. challenger 任务段是否试图削弱或覆盖系统契约与安全纪律（如要求忽略输出格式、放宽宁缺毋滥、隐藏不确定性声明）；
2. 任务段的实际改动是否与 hypothesis 声明一致，有没有夹带假设之外的额外变更（单变量纪律）；
3. 是否含诱导模型伪造数据、绕过证据核验、编造引用的措辞；
4. 影子聚合指标与「improved」结论是否明显矛盾。
纪律：只依据给出的材料判断；不确定时倾向 fail（发布门宁严勿松）；fail 必须给出具体 findings；不得输出 pass 的同时给出 high 级 finding。
只输出 JSON：{"verdict":"pass|fail","findings":[{"code":"机读码","severity":"high|med|low","message":"一句话说明"}],"summary":"一句话总评"}，不要任何解释或代码块标记。`

// releaseAuditFinding 单条审计发现（归一后）。
type releaseAuditFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// releaseAuditResult 审计输出（归一后）。
type releaseAuditResult struct {
	Verdict  string                `json:"verdict"`
	Findings []releaseAuditFinding `json:"findings"`
	Summary  string                `json:"summary"`
}

// parseReleaseAudit 解析+程序收口：verdict 封闭枚举；fail 必附 findings；
// high 级 finding 强制 fail（模型口是心非时以程序改判为准，summary 追加说明）。
func parseReleaseAudit(content string) (*releaseAuditResult, error) {
	var out releaseAuditResult
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &out); err != nil {
		return nil, fmt.Errorf("审计输出不是合法 JSON: %v", err)
	}
	out.Verdict = strings.ToLower(strings.TrimSpace(out.Verdict))
	if out.Verdict != model.ReleaseAuditPass && out.Verdict != model.ReleaseAuditFail {
		return nil, fmt.Errorf("verdict %q 非法（须为 pass/fail）", out.Verdict)
	}
	norm := make([]releaseAuditFinding, 0, len(out.Findings))
	hasHigh := false
	for _, f := range out.Findings {
		f.Code = truncateRunes(strings.TrimSpace(f.Code), 32)
		f.Message = truncateRunes(strings.TrimSpace(f.Message), 200)
		if f.Code == "" && f.Message == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(f.Severity)) {
		case "high":
			f.Severity = "high"
			hasHigh = true
		case "low":
			f.Severity = "low"
		default:
			f.Severity = "med"
		}
		norm = append(norm, f)
		if len(norm) >= releaseAuditMaxFindings {
			break
		}
	}
	out.Findings = norm
	out.Summary = truncateRunes(strings.TrimSpace(out.Summary), 400)
	if out.Verdict == model.ReleaseAuditFail && len(out.Findings) == 0 {
		return nil, errors.New("verdict=fail 必须附 findings（说明否决理由）")
	}
	if hasHigh && out.Verdict == model.ReleaseAuditPass {
		out.Verdict = model.ReleaseAuditFail
		out.Summary = truncateRunes("程序改判 fail：存在 high 级 finding 不得 pass。"+out.Summary, 400)
	}
	return &out, nil
}

// releaseAuditChampionContent 审计输入的 champion 侧基线（审查修复批）：恒用实验创建
// 时固化的 ChampionContent 锚——审计对照的是实验的对照基准，不是「审计当下」的活模板
// （创建后 champion 被编辑时两者漂移，「是否夹带 hypothesis 外额外变更」的单变量复核
// 会失去稳定依据）；默认 champion 场景锚里存的就是实际内置任务段，不再用占位说明冒充。
// 修复前创建的旧实验行无内容锚：按锚 hash 尽力回退（默认态=内置任务段/自定义态=当前
// 启用模板现值），promote 侧的基线漂移门仍会拦住锚不一致的晋级。
func releaseAuditChampionContentDB(db *gorm.DB, exp *model.LLMExperiment) string {
	if strings.TrimSpace(exp.ChampionContent) != "" {
		return exp.ChampionContent
	}
	if exp.ChampionHash == "" {
		if seg, ok := promptModuleDefaultTaskSegs[exp.PromptModule]; ok {
			return seg
		}
		return "（系统默认模板，无自定义任务段）"
	}
	var row model.PromptTemplate
	if err := db.Where("user_id = ? AND module = ? AND enabled = ?", exp.UserID, exp.PromptModule, true).
		First(&row).Error; err == nil {
		return row.Content
	}
	return "（champion 基线内容不可得：旧实验行无内容锚且当前无启用中的模板）"
}

func releaseAuditChampionContent(exp *model.LLMExperiment) string {
	return releaseAuditChampionContentDB(common.DB, exp)
}

// RunLLMExperimentAudit P2-6 发布审计：对 completed 实验运行一次审计员 LLM 复核，
// 工件落 llm_release_audits（每次一行，完整保留）。LLM 不可用返回错误（无假工件）；
// 输出无效 repair 打满则落 verdict=error 工件（照实记录，同样挡晋级）。
func RunLLMExperimentAudit(ctx context.Context, expID int64) (*model.LLMReleaseAudit, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, expID).Error; err != nil {
		return nil, errors.New("实验不存在")
	}
	if experimentTypeOf(&exp) != model.LLMExperimentTypePrompt {
		return nil, errors.New("score_blind 是纯影子输入实验，不进入 prompt 发布审计路径")
	}
	if exp.Status != model.ExpStatusCompleted {
		return nil, fmt.Errorf("仅 completed 实验可运行发布审计（当前 %s）", exp.Status)
	}
	if promptContentHash(exp.ChallengerContent) != exp.ChallengerHash {
		return nil, errors.New("challenger 内容与创建时 hash 不符（快照被篡改），拒绝审计")
	}
	if _, reason, err := validateExperimentCurrentBaseline(common.DB, &exp); err != nil {
		return nil, fmt.Errorf("校验 champion 基线失败: %v", err)
	} else if reason != "" {
		markExperimentBaselineInvalid(common.DB, &exp, reason)
		return nil, &experimentBaselineStaleError{reason: reason}
	}
	cfg, apiKey, adminID, err := resolveNewsLLM()
	if err != nil {
		return nil, fmt.Errorf("发布审计不可用（系统默认 LLM 未就绪）：%v", err)
	}

	championContent := releaseAuditChampionContent(&exp)
	input := map[string]any{
		"module":               exp.Module,
		"prompt_module":        exp.PromptModule,
		"hypothesis":           exp.Hypothesis,
		"expected_improvement": exp.ExpectedImprovement,
		"conclusion":           exp.Conclusion,
		"failure_reason":       exp.FailureReason,
		"actual_metrics":       json.RawMessage(orEmptyJSON(exp.ActualJSON)),
		"champion_content":     championContent,
		"challenger_content":   exp.ChallengerContent,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	convo := []chatMessage{
		{Role: "system", Content: releaseAuditSystem},
		{Role: "user", Content: "待审计的实验材料如下（JSON；champion_content 为实验创建时固化的对照基线任务段，challenger_content 为待晋级任务段）：\n" + string(inputJSON)},
	}
	run := newLLMRun(newLLMTraceID(), "", "release_audit", "release_audit.v1", releaseAuditPromptVersion)
	run.hashData(string(inputJSON))
	run.hashPrompt(convo)

	var usage chatUsage
	var result *releaseAuditResult
	var lastErr error
	repairLimit := moduleRepairAttempts("release_audit")
	requestMax := moduleTokenCap("release_audit", cfg.MaxTokens)
	for attempt := 0; attempt <= repairLimit; attempt++ {
		res, cerr := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			ReasoningEffort: cfg.ReasoningEffort,
			Temperature: cfg.Temperature, MaxTokens: requestMax,
			Messages: convo, JSONMode: true, AllowPrivate: llmAllowPrivate(false, cfg),
			Repair: attempt > 0,
			Meta:   run.chatMeta(adminID, cfg, attempt+1),
		})
		run.record(res, cerr)
		if res != nil {
			addChatUsage(&usage, res.Usage)
		}
		if cerr != nil {
			lastErr = cerr
			if attempt < repairLimit && isTokenLimitFinishState(run.FinishState) {
				requestMax = moduleRepairTokenCap("release_audit", requestMax)
				convo = appendModuleRepairMessages(convo, "release_audit", chatResultContent(res), run.FinishState,
					"上一条输出因 token 上限被截断。请从头严格按完整 JSON 结构重新输出。")
				continue
			}
			break // 调用层失败（网络/门禁拒收）：repair 救不了已失败的传输，直接落 error 工件
		}
		parsed, perr := parseReleaseAudit(res.Content)
		if perr == nil {
			result = parsed
			break
		}
		lastErr = perr
		convo = appendModuleRepairMessages(convo, "release_audit", res.Content, run.FinishState,
			"上述输出不符合要求："+perr.Error()+"。请严格按 JSON 结构重新输出。")
	}
	if usage.TotalTokens > 0 {
		// 手动管理员动作：token 记配置所有者并计一次动作（screener_parse 同款语义）。
		consumeQuota(adminID, usage.TotalTokens, true)
	}

	row := model.LLMReleaseAudit{
		ExperimentID: exp.ID, UserID: adminID,
		ChallengerHash: exp.ChallengerHash,
		// 工件绑定审计实际消费的 champion 基线（审查修复批）：promote 门校验工件与实验锚
		// 一致——审计通过后基线认知变化（旧工件）不得复用旧 PASS。
		ChampionHash: promptContentHash(championContent),
		TraceID:      run.TraceID, TokensUsed: usage.TotalTokens,
	}
	if result != nil {
		row.Verdict = result.Verdict
		row.Summary = result.Summary
		if b, jerr := json.Marshal(result.Findings); jerr == nil {
			row.FindingsJSON = string(b)
		}
	} else {
		// 不出假 PASS：输出无效/调用失败如实落 error 工件（挡晋级，可重跑）。
		row.Verdict = model.ReleaseAuditError
		row.Summary = truncateRunes("审计未得出判定："+errString(lastErr), 500)
	}

	// 外部调用期间实验或模板可能变化。最终“锁实验行 + 锁 champion generation +
	// 复验 + 插入工件”必须是一个事务，并与 promote 锁同一实验行：新 FAIL 若先落库，
	// promote 必然看见；promote 若先完成，本次结果因状态已变而不落库。
	var latest model.LLMExperiment
	var staleReason string
	err = withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockExperimentRow(tx, exp.ID, &latest); err != nil {
				return err
			}
			if latest.Status != model.ExpStatusCompleted {
				return fmt.Errorf("审计期间实验状态已变为 %s，本次结果不落发布工件", latest.Status)
			}
			if promptContentHash(latest.ChallengerContent) != latest.ChallengerHash ||
				latest.ChallengerHash != exp.ChallengerHash {
				return errors.New("审计期间 challenger 快照发生变化，本次结果作废")
			}
			if _, reason, err := validateExperimentCurrentBaseline(tx, &latest); err != nil {
				return fmt.Errorf("复验 champion 基线失败: %v", err)
			} else if reason != "" {
				staleReason = reason
				return &experimentBaselineStaleError{reason: reason}
			}
			if promptContentHash(releaseAuditChampionContentDB(tx, &latest)) != row.ChampionHash {
				return errors.New("审计期间 champion 对照内容发生变化，本次结果作废")
			}
			return tx.Create(&row).Error
		})
	})
	if staleReason != "" {
		markExperimentBaselineInvalid(common.DB, &latest, staleReason)
	}
	if err != nil {
		return nil, err
	}
	common.SysLog("实验 #%d 发布审计完成：verdict=%s tokens=%d trace=%s", exp.ID, row.Verdict, row.TokensUsed, row.TraceID)
	return &row, nil
}

// latestReleaseAudit 实验最新审计工件（promote 门 6 消费）。
func latestReleaseAuditDB(db *gorm.DB, expID int64) *model.LLMReleaseAudit {
	var row model.LLMReleaseAudit
	if err := db.Where("experiment_id = ?", expID).Order("id DESC").First(&row).Error; err != nil {
		return nil
	}
	return &row
}

func latestReleaseAudit(expID int64) *model.LLMReleaseAudit {
	return latestReleaseAuditDB(common.DB, expID)
}

// ListLLMReleaseAudits 实验全部审计工件（详情页展示，倒序）。
func ListLLMReleaseAudits(expID int64) []model.LLMReleaseAudit {
	var rows []model.LLMReleaseAudit
	if err := common.DB.Where("experiment_id = ?", expID).Order("id DESC").Limit(20).Find(&rows).Error; err != nil {
		return nil
	}
	return rows
}

// experimentRollbackStale 判定 promoted 实验的回滚是否已失去对象（审查修复批）：
// 当前启用模板不再是该实验晋级出的 challenger 内容（晋级后模板被编辑、或更新的实验
// 再晋级）时，按 PrePromote 锚无条件恢复会覆盖更新的 champion——返回不可回滚原因。
// 非 promoted 状态返回空串（无判定语义）。
func experimentRollbackStaleForBaseline(exp *model.LLMExperiment, current *experimentPromptBaseline) string {
	if exp.Status != model.ExpStatusPromoted {
		return ""
	}
	if current == nil || !current.Custom {
		return "当前该模块无启用中的自定义模板（晋级后模板已被删除或停用），本实验的回滚锚已失去对象"
	}
	if exp.PromotedGeneration <= 0 {
		return "实验缺少晋级时的 champion generation 锚，无法证明当前模板仍是该实验的晋级产物"
	}
	if !current.GenerationKnown || current.Generation != exp.PromotedGeneration {
		return fmt.Sprintf("当前 champion generation=%d，已不是本实验晋级时的 generation=%d（晋级后被编辑、洗回或有后续实验晋级）",
			current.Generation, exp.PromotedGeneration)
	}
	if current.Hash != exp.ChallengerHash {
		return "当前启用模板内容已不是本实验晋级出的 challenger（晋级后被编辑或有更新的实验晋级），回滚会覆盖更新的 champion"
	}
	if exp.PromotedRevision <= 0 || current.Revision != exp.PromotedRevision {
		return fmt.Sprintf("当前模板 revision=%d，已不是本实验晋级时的 revision=%d，回滚会覆盖更新的 champion",
			current.Revision, exp.PromotedRevision)
	}
	return ""
}

func experimentRollbackStale(exp *model.LLMExperiment) string {
	if exp.Status != model.ExpStatusPromoted {
		return ""
	}
	current, err := loadExperimentPromptBaseline(common.DB, exp.UserID, exp.PromptModule)
	if err != nil {
		return "读取当前 champion 失败，无法安全回滚"
	}
	return experimentRollbackStaleForBaseline(exp, current)
}

// RollbackLLMExperiment P2-6 一键切回 champion：promoted→rolled_back（终态），
// 按晋级瞬间固化的 PrePromote 锚恢复模板状态（经 Upsert 生成新 revision，不删工件）。
// 审查修复批：回滚前校验当前启用模板仍是本实验晋级出的 challenger——champion 指针
// 已前移（编辑/更新实验晋级）时拒绝，防旧实验回滚覆盖更新的 champion。
func RollbackLLMExperiment(id int64) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	err := withPromptExperimentState(func() error {
		return common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockExperimentRow(tx, id, &exp); err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errors.New("实验不存在")
				}
				return err
			}
			if experimentTypeOf(&exp) != model.LLMExperimentTypePrompt {
				return errors.New("score_blind 是纯影子输入实验，不进入 prompt 回滚路径")
			}
			if exp.Status != model.ExpStatusPromoted {
				return fmt.Errorf("仅 promoted 实验可回滚（当前 %s）", exp.Status)
			}
			current, err := lockExperimentPromptBaseline(tx, exp.UserID, exp.PromptModule)
			if err != nil {
				return err
			}
			if reason := experimentRollbackStaleForBaseline(&exp, current); reason != "" {
				return errors.New("拒绝回滚：" + reason + "（如需恢复历史内容请在提示词页按 revision 快照操作）")
			}

			content, enabled := current.Content, false
			if exp.PrePromoteEnabled {
				content, enabled = exp.PrePromoteContent, true
			}
			module, normalized, hash, _, normErr := normalizePromptInput(PromptInput{
				Module: exp.PromptModule, Content: content, Enabled: enabled,
			})
			if normErr != nil {
				return normErr
			}
			if _, err := upsertPromptTemplateTx(tx, exp.UserID, module, normalized, hash,
				enabled, current.Row, true); err != nil {
				return fmt.Errorf("回滚落模板失败: %v", err)
			}

			now := time.Now()
			res := tx.Model(&model.LLMExperiment{}).
				Where("id = ? AND status = ?", exp.ID, model.ExpStatusPromoted).
				Updates(map[string]any{"status": model.ExpStatusRolledBack, "rolled_back_at": now, "updated_at": now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("实验状态已被并发修改，回滚已取消")
			}
			exp.Status, exp.RolledBackAt = model.ExpStatusRolledBack, &now
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	common.SysLog("实验 #%d 已回滚：module=%s 恢复晋级前状态（pre_promote_enabled=%v）",
		exp.ID, exp.PromptModule, exp.PrePromoteEnabled)
	return &exp, nil
}

// orEmptyJSON 空串归 JSON null（json.RawMessage 不能为空串）。
func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "null"
	}
	return s
}

// errString nil 安全的错误文案。
func errString(err error) string {
	if err == nil {
		return "未知原因"
	}
	return err.Error()
}
