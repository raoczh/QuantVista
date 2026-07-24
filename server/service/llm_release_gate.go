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

// releaseAuditChampionContent 审计输入的 champion 侧内容（当前启用模板；默认模板时声明）。
func releaseAuditChampionContent(userID int64, promptModule string) string {
	if row := userPromptTemplateRow(userID, promptModule); row != nil {
		return row.Content
	}
	return "（当前使用系统默认模板，无自定义任务段）"
}

// RunLLMExperimentAudit P2-6 发布审计：对 completed 实验运行一次审计员 LLM 复核，
// 工件落 llm_release_audits（每次一行，完整保留）。LLM 不可用返回错误（无假工件）；
// 输出无效 repair 打满则落 verdict=error 工件（照实记录，同样挡晋级）。
func RunLLMExperimentAudit(ctx context.Context, expID int64) (*model.LLMReleaseAudit, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, expID).Error; err != nil {
		return nil, errors.New("实验不存在")
	}
	if exp.Status != model.ExpStatusCompleted {
		return nil, fmt.Errorf("仅 completed 实验可运行发布审计（当前 %s）", exp.Status)
	}
	cfg, apiKey, adminID, err := resolveNewsLLM()
	if err != nil {
		return nil, fmt.Errorf("发布审计不可用（系统默认 LLM 未就绪）：%v", err)
	}

	input := map[string]any{
		"module":               exp.Module,
		"prompt_module":        exp.PromptModule,
		"hypothesis":           exp.Hypothesis,
		"expected_improvement": exp.ExpectedImprovement,
		"conclusion":           exp.Conclusion,
		"failure_reason":       exp.FailureReason,
		"actual_metrics":       json.RawMessage(orEmptyJSON(exp.ActualJSON)),
		"champion_content":     releaseAuditChampionContent(exp.UserID, exp.PromptModule),
		"challenger_content":   exp.ChallengerContent,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	convo := []chatMessage{
		{Role: "system", Content: releaseAuditSystem},
		{Role: "user", Content: "待审计的实验材料如下（JSON；champion_content 为当前启用任务段，challenger_content 为待晋级任务段）：\n" + string(inputJSON)},
	}
	run := newLLMRun(newLLMTraceID(), "", "release_audit", "release_audit.v1", releaseAuditPromptVersion)
	run.hashData(string(inputJSON))
	run.hashPrompt(convo)

	var usage chatUsage
	var result *releaseAuditResult
	var lastErr error
	for attempt := 0; attempt <= moduleRepairAttempts("release_audit"); attempt++ {
		res, cerr := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("release_audit", cfg.MaxTokens),
			Messages: convo, JSONMode: true, AllowPrivate: llmAllowPrivate(false, cfg),
			Repair: attempt > 0,
			Meta:   run.chatMeta(adminID, cfg, attempt+1),
		})
		run.record(res, cerr)
		if res != nil {
			usage.PromptTokens += res.Usage.PromptTokens
			usage.CompletionTokens += res.Usage.CompletionTokens
			usage.TotalTokens += res.Usage.TotalTokens
		}
		if cerr != nil {
			lastErr = cerr
			break // 调用层失败（网络/门禁拒收）：repair 救不了已失败的传输，直接落 error 工件
		}
		parsed, perr := parseReleaseAudit(res.Content)
		if perr == nil {
			result = parsed
			break
		}
		lastErr = perr
		convo = append(convo,
			chatMessage{Role: "assistant", Content: moduleRepairFeed("release_audit", res.Content)},
			chatMessage{Role: "user", Content: "上述输出不符合要求：" + perr.Error() + "。请严格按 JSON 结构重新输出。"},
		)
	}
	if usage.TotalTokens > 0 {
		// 手动管理员动作：token 记配置所有者并计一次动作（screener_parse 同款语义）。
		consumeQuota(adminID, usage.TotalTokens, true)
	}

	row := model.LLMReleaseAudit{
		ExperimentID: exp.ID, UserID: adminID,
		ChallengerHash: exp.ChallengerHash, TraceID: run.TraceID, TokensUsed: usage.TotalTokens,
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
	if err := common.DB.Create(&row).Error; err != nil {
		return nil, err
	}
	common.SysLog("实验 #%d 发布审计完成：verdict=%s tokens=%d trace=%s", exp.ID, row.Verdict, row.TokensUsed, row.TraceID)
	return &row, nil
}

// latestReleaseAudit 实验最新审计工件（promote 门 6 消费）。
func latestReleaseAudit(expID int64) *model.LLMReleaseAudit {
	var row model.LLMReleaseAudit
	if err := common.DB.Where("experiment_id = ?", expID).Order("id DESC").First(&row).Error; err != nil {
		return nil
	}
	return &row
}

// ListLLMReleaseAudits 实验全部审计工件（详情页展示，倒序）。
func ListLLMReleaseAudits(expID int64) []model.LLMReleaseAudit {
	var rows []model.LLMReleaseAudit
	if err := common.DB.Where("experiment_id = ?", expID).Order("id DESC").Limit(20).Find(&rows).Error; err != nil {
		return nil
	}
	return rows
}

// RollbackLLMExperiment P2-6 一键切回 champion：promoted→rolled_back（终态），
// 按晋级瞬间固化的 PrePromote 锚恢复模板状态（经 Upsert 生成新 revision，不删工件）。
func RollbackLLMExperiment(id int64) (*model.LLMExperiment, error) {
	var exp model.LLMExperiment
	if err := common.DB.First(&exp, id).Error; err != nil {
		return nil, errors.New("实验不存在")
	}
	if exp.Status != model.ExpStatusPromoted {
		return nil, fmt.Errorf("仅 promoted 实验可回滚（当前 %s）", exp.Status)
	}
	ps := NewPromptService()
	if exp.PrePromoteEnabled {
		// 晋级前已有启用中的自定义模板：把该内容重新落为启用模板（champion 指针切回）。
		if _, _, err := ps.Upsert(exp.UserID, PromptInput{
			Module: exp.PromptModule, Content: exp.PrePromoteContent, Enabled: true,
		}); err != nil {
			return nil, fmt.Errorf("回滚落模板失败: %v", err)
		}
	} else if row := userPromptTemplateRow(exp.UserID, exp.PromptModule); row != nil {
		// 晋级前用默认模板：停用当前自定义模板（内容保留、指针回默认——不删工件）。
		if _, _, err := ps.Upsert(exp.UserID, PromptInput{
			Module: exp.PromptModule, Content: row.Content, Enabled: false,
		}); err != nil {
			return nil, fmt.Errorf("回滚停用模板失败: %v", err)
		}
	}
	now := time.Now()
	exp.Status = model.ExpStatusRolledBack
	exp.RolledBackAt = &now
	if err := common.DB.Save(&exp).Error; err != nil {
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
