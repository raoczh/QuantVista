package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"quantvista/model"
	"quantvista/setting"
)

// P1-3 条件式独立 bull/bear/judge 辩论（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §4.5.5 蓝图 C、
// §6.2、§7.2）。
//
// 定位与边界（不可漂移）：
//   - 辩论是**条件式增强不是默认路径**：仅个股标准分析（非 panel/非 as_of/非历史解释）在
//     低置信度 / 证据冲突 / 风险闸门临界 时触发——高置信度默认单路，零额外成本（§6.2）。
//   - 辩论是**附加复核不是裁决替换**：judge verdict 不改写主分析 rating/summary/claims；
//     verdict 与主评级方向相反时压程序合成置信度（同 review reject 级联先例），并列展示
//     供用户自判。任一角色失败=辩论整体降级为既有单路（主结果原样），记 degraded_reason。
//   - 防伪造纪律（P0-3/P1-2 延续）：触发原因程序判定；bull/bear 引用的 evidence_id 必须
//     指向核验引擎已产出的证据白名单（越界剥除）；judge 的 *_claim_ids 必须指向双方实际
//     产出的 claim id（非法剥除）；claim id（bu-/be- 前缀）由程序重编号，不信模型自报；
//     模型自附的 result.debate 字段在 parseAnalysisResult 解析前剥除。
//   - 调用预算上限明确并声明进 manifest（bull run 携带 DebateManifestInfo）：
//     bull 1 + bear 1 + rebuttal ≤1（条件式）+ judge 1 = 最多 4 次调用，每次 repair ≤1
//     （llm_budget.go 预算表）；辩论轮上限 debateMaxRounds=3 是硬钳制，当前策略最多走到
//     第 2 轮（立论+条件式反驳）。
//   - rebuttal 触发条件程序化：bear 与 bull 的 claims 引用了**同一 evidence_id**（同一证据
//     得出对立解读=真正的交锋点）才给 bull 一次反驳机会；各说各话（证据集不相交）直接裁决。
//   - judge 纪律（蓝图 C）：按证据质量排序不按票数平均（prompt 纪律）；risk_gate=block 时
//     verdict 不得 bullish（程序校验触发 repair，打满=judge_invalid 降级——触发条件已排除
//     block，这是防调用顺序重构旁路的收口，同 validateTradePlanSemantics 先例）。
//
// flag `llm_conditional_debate`（缺省开）：关闭只回退触发判定，主分析链路不受影响。

// debateVersion 辩论编排版本（触发条件/轮次策略/prompt 措辞变更时递增）。
// db2（审查修复批）：程序收口三条——claim 必须引用合法 evidence_id（零引用剥除）、bear
// challenges 非空、judge 三列表互斥且至少一个有效引用；rebuttal 失败回退 Rounds=1 并记
// rebuttal_degraded；prompt 措辞同步。db1：初版条件式编排。
const debateVersion = "db2"

// 辩论轮次与规模上限。
const (
	debateMaxRounds   = 3 // 辩论轮硬上限（蓝图 C；当前策略最多走到第 2 轮）
	debateMaxClaims   = 4 // 每方 claims 上限
	debateCallBudget  = 4 // 调用预算上限：bull+bear+rebuttal(条件)+judge
	debateTextMax     = 200
	debateEvidenceMax = 30 // 喂给角色的证据索引条数上限（防 prompt 撑爆）
)

// 触发原因机读枚举（程序判定，进 manifest 与前端展示）。
const (
	debateTriggerLowConfidence = "low_confidence"       // 程序合成置信度 low（含复核 reject 级联后）
	debateTriggerContradictory = "contradictory_claims" // claims 存在与快照方向矛盾的结论
	debateTriggerRiskBorder    = "risk_gate_borderline" // 风险闸门 warn 级（一字板/流动性不足）
)

// DebateManifestInfo 辩论触发条件与调用预算声明（manifest 契约，bull run 携带）。
type DebateManifestInfo struct {
	TriggerReasons []string `json:"trigger_reasons"`
	Rounds         int      `json:"rounds"`      // 实际执行轮数（1=立论、2=含反驳轮）
	MaxRounds      int      `json:"max_rounds"`  // 轮次硬上限（debateMaxRounds）
	CallBudget     int      `json:"call_budget"` // 调用次数上限（debateCallBudget）
	Version        string   `json:"version"`
}

// debateClaim 辩论单方的一条论点：程序重编号 id（bu-01…/be-01…）、引用证据白名单内的
// evidence_id、失效条件（bull=看多失效条件 / bear=什么新证据推翻看空）。
type debateClaim struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Invalidator string   `json:"invalidator,omitempty"`
	// Confirmed 仅 bear 方使用：区分已证实风险（数据支持）与假设风险（蓝图 C）。
	Confirmed *bool `json:"confirmed,omitempty"`
}

// debateChallenge 针对对方某条 claim 的反驳（bear→bull 的 challenges 与 bull→bear 的
// rebuttals 同构）。
type debateChallenge struct {
	ClaimID string `json:"claim_id"`
	Text    string `json:"text"`
}

// debateJudge judge 裁决（蓝图 C 输出契约）。*_claim_ids 引用双方 claim id，程序校验剥除
// 非法引用；invalidators 复用 claim 归一（normalizeInvalidators）。
type debateJudge struct {
	Verdict            string   `json:"verdict"` // bullish / neutral / bearish
	DecisiveClaimIDs   []string `json:"decisive_claim_ids,omitempty"`
	RejectedClaimIDs   []string `json:"rejected_claim_ids,omitempty"`
	UnresolvedClaimIDs []string `json:"unresolved_claim_ids,omitempty"`
	ConfidenceReason   string   `json:"confidence_reason,omitempty"`
	Invalidators       []string `json:"invalidators,omitempty"`
	ConflictNote       string   `json:"conflict_note,omitempty"` // 估值/技术/事件结论冲突时的显式说明
}

// debateResult 辩论整体结果（AnalysisResult.Debate，服务端回填字段）。
// DegradedReason 非空=辩论未完成（bull_failed/bear_failed/judge_failed/judge_invalid），
// 主分析结果不受影响（既有单路语义）。
type debateResult struct {
	Triggered      bool              `json:"triggered"`
	TriggerReasons []string          `json:"trigger_reasons"`
	Rounds         int               `json:"rounds"`
	Bull           []debateClaim     `json:"bull,omitempty"`
	Bear           []debateClaim     `json:"bear,omitempty"`
	Challenges     []debateChallenge `json:"challenges,omitempty"`
	Rebuttals      []debateChallenge `json:"rebuttals,omitempty"`
	Judge          *debateJudge      `json:"judge,omitempty"`
	DegradedReason string            `json:"degraded_reason,omitempty"`
	// RebuttalDegraded 反驳轮失败标记（审查修复批）：rebuttal 是 best-effort，失败不
	// 降级整体（judge 照常裁决），但必须如实记录且 Rounds 回退 1——不许伪装成完整两轮。
	RebuttalDegraded bool   `json:"rebuttal_degraded,omitempty"`
	Version          string `json:"version"`
}

// debateTriggerReasons 程序判定触发条件（纯函数可测）。返回空=不触发（高置信默认单路）。
// 判定输入全部来自程序产出：SysConfidence（含复核 reject 级联后的终值）、claims 状态
// （P1-2 程序推导）、风险闸门 flags（S1 程序规则）。
//   - block 级风险**不触发**：语义校验已硬约束 block ⇒ 不得 bullish，辩论无从改变该结论，
//     跑辩论只是烧 token；warn 级（一字板/流动性不足）才是「临界」——评级仍有自由度，
//     多空对抗有信息增量。
func debateTriggerReasons(result *AnalysisResult, snapshot map[string]any) []string {
	if result == nil || !setting.LLMConditionalDebate() {
		return nil
	}
	if hasBlockRiskFlag(snapshot) {
		return nil
	}
	var reasons []string
	if result.SysConfidence == "low" {
		reasons = append(reasons, debateTriggerLowConfidence)
	}
	if ev := result.EvidenceCheck; ev != nil {
		for _, cl := range ev.Claims {
			if cl.Status == claimContradictory {
				reasons = append(reasons, debateTriggerContradictory)
				break
			}
		}
	}
	if hasWarnRiskFlag(snapshot) {
		reasons = append(reasons, debateTriggerRiskBorder)
	}
	return reasons
}

// hasWarnRiskFlag 快照风险闸门是否含 warn 级标志（兼容原生 []riskFlag 与 JSON 回灌形态，
// 同 hasBlockRiskFlag）。
func hasWarnRiskFlag(snapshot map[string]any) bool {
	blk, ok := snapshot["risk_gate"].(map[string]any)
	if !ok {
		return false
	}
	switch fl := blk["flags"].(type) {
	case []riskFlag:
		for _, f := range fl {
			if f.Level == "warn" {
				return true
			}
		}
	case []any:
		for _, v := range fl {
			if m, ok := v.(map[string]any); ok {
				if lv, _ := m["level"].(string); lv == "warn" {
					return true
				}
			}
		}
	}
	return false
}

// debateEvidenceRef 喂给辩论角色的证据索引条目（从核验命中项提取；evidence_id 白名单来源）。
type debateEvidenceRef struct {
	EvidenceID string  `json:"evidence_id"`
	Path       string  `json:"path"`
	Value      float64 `json:"value"`
	Unit       string  `json:"unit,omitempty"`
	AsOf       string  `json:"as_of,omitempty"`
	Origin     string  `json:"origin,omitempty"` // 空=快照佐证 | plan=模型计划价 | user/context=复述来源
}

// buildDebateEvidenceIndex 从核验结果提取证据索引与白名单（程序产出，模型只能引用不能新增）。
func buildDebateEvidenceIndex(ev *evidenceCheck) ([]debateEvidenceRef, map[string]bool) {
	if ev == nil {
		return nil, nil
	}
	refs := make([]debateEvidenceRef, 0, len(ev.Items))
	allow := make(map[string]bool, len(ev.Items))
	for _, it := range ev.Items {
		if !it.Matched || it.EvidenceID == "" {
			continue
		}
		refs = append(refs, debateEvidenceRef{
			EvidenceID: it.EvidenceID, Path: it.Path, Value: it.SnapValue,
			Unit: it.Unit, AsOf: it.AsOf, Origin: it.Origin,
		})
		allow[it.EvidenceID] = true
		if len(refs) >= debateEvidenceMax {
			break
		}
	}
	return refs, allow
}

// --- 角色 prompt（蓝图 C 中文化落地；数据段由程序构造，模型只能引用 EVIDENCE_INDEX） ---

const debateBullSystem = `你是独立的看多研究员（bull），只建立当前数据快照下最强的看多论证。你与主分析师、看空研究员相互独立。
规则：
1. 只依据【数据快照】与【证据索引】论证；每条论点的 evidence_ids **必须**引用至少一个证据索引中存在的 evidence_id（无证据引用的论点会被程序剥除），不得编造证据或引用快照外事实；
2. 主分析中的反方观点（anti_thesis）与矛盾结论是你要正面回应的对手观点，不得回避；
3. 必须至少为一条论点主动给出 invalidator（出现什么情况该看多论点失效）；
4. 不得淡化风险闸门提示，不得把数据缺失（unknown）当利好。
只输出 JSON：{"claims":[{"text":"论点（引用具体数字）","evidence_ids":["ev-001"],"invalidator":"失效条件"}]}，1~4 条，不要任何解释或代码块标记。`

const debateBearSystem = `你是独立的看空研究员（bear），只寻找会让看多论点失败的风险：财务、估值、行业、政策、流动性、事件与执行风险。你与主分析师、看多研究员相互独立。
规则：
1. 只依据【数据快照】与【证据索引】论证；每条看空论点的 evidence_ids **必须**引用至少一个证据索引中存在的 evidence_id（无证据引用的论点会被程序剥除）；
2. 逐条审视【看多论点】，challenges **至少给出一条**针对性反驳（claim_id 用对方论点 id）——空 challenges 视为未完成反驳职责；
3. 每条看空论点标注 confirmed：true=数据已证实的风险，false=假设性风险（数据不足以证实但值得警惕）；
4. 必须至少为一条论点写出 invalidator（什么新证据会推翻这条看空判断）；数据未提供的维度（如解禁减持）只能提示核查，严禁虚构。
只输出 JSON：{"claims":[{"text":"看空论点","evidence_ids":[],"confirmed":true,"invalidator":"推翻条件"}],"challenges":[{"claim_id":"bu-01","text":"针对性反驳（带具体数字/事实）"}]}，claims 1~4 条，不要任何解释或代码块标记。`

const debateRebuttalSystem = `你是看多研究员（bull），看空研究员针对你引用的同一证据给出了对立解读。本轮你只写反驳：
每条反驳针对看空论点的一条（claim_id 用对方 id），只写一条带具体数字/事实的反驳，并使用「但是/问题是/代价是」中的一种句式直面对方最强点。不得新增证据索引之外的事实，不得重复上一轮原话。
只输出 JSON：{"rebuttals":[{"claim_id":"be-01","text":"反驳"}]}，1~2 条，不要任何解释或代码块标记。`

const debateJudgeSystem = `你是辩论裁判（judge），对看多/看空双方的论点做证据裁决。
规则：
1. 按证据质量排序：来源状态、时效、直接性与独立来源数——不按角色票数平均，不和稀泥；
2. verdict 只能是 bullish/neutral/bearish；neutral 只能用于证据真正平衡或不足，不得用于回避判断；
3. 风险闸门为禁止买入级（block）时 verdict 不得为 bullish；
4. decisive_claim_ids=决定裁决方向的关键论点、rejected_claim_ids=被证据否定的论点、unresolved_claim_ids=证据不足无法裁决的论点——只能引用双方实际给出的论点 id（bu-*/be-*），三个列表合计**必须至少引用一个**（无引用的裁决无效），同一 id 不得重复出现在多个列表，不得新增双方未提出的事实；
5. 估值、技术与事件结论方向冲突时必须写 conflict_note 点名冲突，不能用一句「综合来看」抹平；
6. confidence_reason 写清裁决的置信依据；invalidators 写出会推翻本裁决的条件。
只输出 JSON：{"verdict":"bullish|neutral|bearish","decisive_claim_ids":[],"rejected_claim_ids":[],"unresolved_claim_ids":[],"confidence_reason":"...","invalidators":[],"conflict_note":""}，不要任何解释或代码块标记。`

// debateCallOne 辩论单角色调用（共用小循环：结构化 JSON + repair ≤1 按预算表）。
// parse 返回 error 触发 repair；打满返回最后错误。usage 由调用方累计。
func (s *AnalysisService) debateCallOne(ctx context.Context, userID int64, run *llmRun, cfg *model.LLMConfig, apiKey string, allowPrivate bool, messages []chatMessage, parse func(string) error, repairHint string) (chatUsage, error) {
	var usage chatUsage
	convo := messages
	run.hashPrompt(convo)
	var lastErr error
	for attempt := 0; attempt <= moduleRepairAttempts(run.Module); attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			Temperature: cfg.Temperature, MaxTokens: moduleTokenCap(run.Module, cfg.MaxTokens),
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
			Repair: attempt > 0, // repair 轮：契约开启时温度固定 0（llm_contract.go）
			Meta:   run.chatMeta(userID, cfg, attempt+1),
		})
		run.record(res, err)
		if err != nil {
			// audit outcome：拒收调用的真实 token 消耗照常累计（res 可能非 nil）。
			if res != nil {
				usage.PromptTokens += res.Usage.PromptTokens
				usage.CompletionTokens += res.Usage.CompletionTokens
				usage.TotalTokens += res.Usage.TotalTokens
			}
			return usage, err
		}
		usage.PromptTokens += res.Usage.PromptTokens
		usage.CompletionTokens += res.Usage.CompletionTokens
		usage.TotalTokens += res.Usage.TotalTokens
		if perr := parse(res.Content); perr == nil {
			return usage, nil
		} else {
			lastErr = perr
			convo = append(convo,
				chatMessage{Role: "assistant", Content: moduleRepairFeed(run.Module, res.Content)},
				chatMessage{Role: "user", Content: "上一条输出不合格：" + perr.Error() + "。" + repairHint},
			)
		}
	}
	run.DegradedReason = "llm_output_invalid"
	return usage, lastErr
}

// normalizeDebateClaims 归一一方 claims：程序重编号 id（prefix-01…）、剥除证据白名单外的
// evidence_id、截断文本、限条数。程序收口两条（审查修复批，蓝图 C「每方逐条引用证据」）：
// ①剥除后零条合法 evidence 引用的 claim 直接丢弃——空口论点不进辩论记录；②全部条目
// 都无 invalidator 时返回错误。两类失败均作为解析错误触发 repair。
func normalizeDebateClaims(in []debateClaim, prefix string, allow map[string]bool) ([]debateClaim, error) {
	out := make([]debateClaim, 0, debateMaxClaims)
	hasInvalidator := false
	dropped := 0
	for _, c := range in {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		var evs []string
		for _, id := range c.EvidenceIDs {
			id = strings.TrimSpace(id)
			if allow[id] {
				evs = append(evs, id)
			}
		}
		if len(evs) == 0 {
			dropped++ // 无合法证据引用：丢弃（引用越界被剥空的与压根没引用的同罪）
			continue
		}
		inv := strings.TrimSpace(c.Invalidator)
		if inv != "" {
			hasInvalidator = true
		}
		out = append(out, debateClaim{
			ID:          fmt.Sprintf("%s-%02d", prefix, len(out)+1),
			Text:        truncateRunes(text, debateTextMax),
			EvidenceIDs: evs,
			Invalidator: truncateRunes(inv, claimInvalidatorLen),
			Confirmed:   c.Confirmed,
		})
		if len(out) >= debateMaxClaims {
			break
		}
	}
	if len(out) == 0 {
		if dropped > 0 {
			return nil, fmt.Errorf("全部 %d 条论点均未引用证据索引中的合法 evidence_id（每条论点必须引用至少一个）", dropped)
		}
		return nil, errors.New("claims 为空或全部无效")
	}
	if !hasInvalidator {
		return nil, errors.New("必须至少为一条论点给出 invalidator（失效条件）")
	}
	return out, nil
}

// filterChallenges 剥除引用非法 claim_id 的反驳（越界不 repair——剥除后其余仍可用）。
func filterChallenges(in []debateChallenge, validIDs map[string]bool, max int) []debateChallenge {
	var out []debateChallenge
	for _, ch := range in {
		id := strings.TrimSpace(ch.ClaimID)
		text := strings.TrimSpace(ch.Text)
		if !validIDs[id] || text == "" {
			continue
		}
		out = append(out, debateChallenge{ClaimID: id, Text: truncateRunes(text, debateTextMax)})
		if len(out) >= max {
			break
		}
	}
	return out
}

// hasSharedEvidence rebuttal 轮触发判定（程序化）：双方 claims 引用了同一 evidence_id
// （同一证据对立解读=真正的交锋点）。各说各话（证据集不相交）不值得追加反驳轮。
func hasSharedEvidence(bull, bear []debateClaim) bool {
	seen := map[string]bool{}
	for _, c := range bull {
		for _, id := range c.EvidenceIDs {
			seen[id] = true
		}
	}
	for _, c := range bear {
		for _, id := range c.EvidenceIDs {
			if seen[id] {
				return true
			}
		}
	}
	return false
}

// claimIDSet 双方 claim id 集合（judge 引用校验白名单）。
func claimIDSet(lists ...[]debateClaim) map[string]bool {
	set := map[string]bool{}
	for _, l := range lists {
		for _, c := range l {
			set[c.ID] = true
		}
	}
	return set
}

// filterClaimIDs 剥除非法 claim id 引用并去重（保持原序）。
func filterClaimIDs(in []string, valid map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range in {
		id = strings.TrimSpace(id)
		if !valid[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// debateOpposite 主评级与裁决是否方向相反（bullish vs bearish；neutral 不算相反）。
func debateOpposite(rating, verdict string) bool {
	return (rating == model.AnalysisRatingBullish && verdict == model.AnalysisRatingBearish) ||
		(rating == model.AnalysisRatingBearish && verdict == model.AnalysisRatingBullish)
}

// runDebate 执行条件式辩论（触发判定由调用方完成，reasons 非空才进来）。
// 返回辩论结果（含降级形态）、token 用量与实际发起的 run 列表（manifest 用；
// 未发起调用的角色不产生 run）。主分析 result 只读不改写——SysConfidence 联动由调用方
// 依据返回的 debate 结果处理。
func (s *AnalysisService) runDebate(ctx context.Context, userID int64, cfg *model.LLMConfig, apiKey string, allowPrivate bool, snapshot map[string]any, result *AnalysisResult, reasons []string, traceID, parentRunID string) (*debateResult, chatUsage, []*llmRun) {
	var usage chatUsage
	var runs []*llmRun
	deb := &debateResult{Triggered: true, TriggerReasons: reasons, Rounds: 1, Version: debateVersion}
	info := &DebateManifestInfo{
		TriggerReasons: reasons, Rounds: 1,
		MaxRounds: debateMaxRounds, CallBudget: debateCallBudget, Version: debateVersion,
	}

	snapJSON, _ := json.Marshal(snapshot)
	refs, allow := buildDebateEvidenceIndex(result.EvidenceCheck)
	refsJSON, _ := json.Marshal(refs)
	// 主分析摘要（双方的对手盘上下文）：评级/总结/反方观点/claims 状态。
	briefJSON, _ := json.Marshal(map[string]any{
		"rating": result.Rating, "summary": result.Summary,
		"anti_thesis": result.AntiThesis, "claims": claimsBrief(result.EvidenceCheck),
	})
	sharedData := "【数据快照】（JSON）：\n" + string(snapJSON) +
		"\n\n【证据索引】（EVIDENCE_INDEX，evidence_ids 只能从这里引用）：\n" + string(refsJSON) +
		"\n\n【主分析摘要】：\n" + string(briefJSON)

	// --- 第 1 轮：bull 立论 ---
	bullRun := newLLMRun(traceID, parentRunID, "debate_bull", "debate_bull.v1", debateVersion)
	bullRun.DebateInfo = info // 触发条件与预算声明挂辩论首 run
	bullRun.hashData(string(snapJSON))
	runs = append(runs, bullRun)
	var bullClaims []debateClaim
	bullUsage, err := s.debateCallOne(ctx, userID, bullRun, cfg, apiKey, allowPrivate,
		[]chatMessage{
			{Role: "system", Content: debateBullSystem},
			{Role: "user", Content: sharedData},
		},
		func(content string) error {
			var out struct {
				Claims []debateClaim `json:"claims"`
			}
			if jerr := json.Unmarshal([]byte(extractJSONObject(content)), &out); jerr != nil {
				return fmt.Errorf("JSON 解析失败: %v", jerr)
			}
			claims, nerr := normalizeDebateClaims(out.Claims, "bu", allow)
			if nerr != nil {
				return nerr
			}
			bullClaims = claims
			return nil
		},
		`请只输出 JSON：{"claims":[{"text","evidence_ids":[],"invalidator"}]}，evidence_ids 只能引用证据索引中的 id。`)
	addUsage(&usage, bullUsage)
	if err != nil {
		deb.DegradedReason = "bull_failed"
		return deb, usage, runs
	}

	// --- 第 1 轮：bear 反驳 + 立论（见 bull 论点，本身即一次交锋） ---
	bullJSON, _ := json.Marshal(bullClaims)
	bearRun := newLLMRun(traceID, parentRunID, "debate_bear", "debate_bear.v1", debateVersion)
	bearRun.hashData(string(snapJSON))
	runs = append(runs, bearRun)
	var bearClaims []debateClaim
	var challenges []debateChallenge
	bearUsage, err := s.debateCallOne(ctx, userID, bearRun, cfg, apiKey, allowPrivate,
		[]chatMessage{
			{Role: "system", Content: debateBearSystem},
			{Role: "user", Content: sharedData + "\n\n【看多论点】（BULL_CLAIMS，逐条审视）：\n" + string(bullJSON)},
		},
		func(content string) error {
			var out struct {
				Claims     []debateClaim     `json:"claims"`
				Challenges []debateChallenge `json:"challenges"`
			}
			if jerr := json.Unmarshal([]byte(extractJSONObject(content)), &out); jerr != nil {
				return fmt.Errorf("JSON 解析失败: %v", jerr)
			}
			claims, nerr := normalizeDebateClaims(out.Claims, "be", allow)
			if nerr != nil {
				return nerr
			}
			bearClaims = claims
			challenges = filterChallenges(out.Challenges, claimIDSet(bullClaims), debateMaxClaims)
			// 程序收口（审查修复批，蓝图 C「逐条回应」的最低要求）：bear 必须至少
			// 有效回应一条看多论点——空 challenges（或引用全部非法被剥空）不算完成
			// 反驳职责，触发 repair。全覆盖不强制（claims 条数不对等时不可达）。
			if len(challenges) == 0 {
				return errors.New("challenges 为空或 claim_id 全部非法：必须逐条审视看多论点，至少有效回应一条")
			}
			return nil
		},
		`请只输出 JSON：{"claims":[{"text","evidence_ids":[],"confirmed":true,"invalidator"}],"challenges":[{"claim_id","text"}]}，claim_id 只能引用看多论点的 id。`)
	addUsage(&usage, bearUsage)
	if err != nil {
		deb.DegradedReason = "bear_failed"
		return deb, usage, runs
	}
	deb.Bull, deb.Bear, deb.Challenges = bullClaims, bearClaims, challenges

	// --- 第 2 轮（条件式）：双方引用了同一证据（对立解读）时给 bull 一次反驳 ---
	var rebuttals []debateChallenge
	if hasSharedEvidence(bullClaims, bearClaims) && deb.Rounds < debateMaxRounds {
		deb.Rounds = 2
		info.Rounds = 2
		bearJSON, _ := json.Marshal(bearClaims)
		rbRun := newLLMRun(traceID, parentRunID, "debate_rebuttal", "debate_rebuttal.v1", debateVersion)
		rbRun.hashData(string(snapJSON))
		runs = append(runs, rbRun)
		rbUsage, rbErr := s.debateCallOne(ctx, userID, rbRun, cfg, apiKey, allowPrivate,
			[]chatMessage{
				{Role: "system", Content: debateRebuttalSystem},
				{Role: "user", Content: "【你上一轮的看多论点】：\n" + string(bullJSON) +
					"\n\n【看空论点】（BEAR_CLAIMS，反驳对象）：\n" + string(bearJSON) +
					"\n\n【证据索引】：\n" + string(refsJSON)},
			},
			func(content string) error {
				var out struct {
					Rebuttals []debateChallenge `json:"rebuttals"`
				}
				if jerr := json.Unmarshal([]byte(extractJSONObject(content)), &out); jerr != nil {
					return fmt.Errorf("JSON 解析失败: %v", jerr)
				}
				rb := filterChallenges(out.Rebuttals, claimIDSet(bearClaims), 2)
				if len(rb) == 0 {
					return errors.New("rebuttals 为空或 claim_id 全部非法")
				}
				rebuttals = rb
				return nil
			},
			`请只输出 JSON：{"rebuttals":[{"claim_id","text"}]}，claim_id 只能引用看空论点的 id。`)
		addUsage(&usage, rbUsage)
		if rbErr != nil {
			// 反驳轮 best-effort：失败不降级整体、继续裁决，但**如实回退轮数并记录**
			// （审查修复批）——Rounds=2 是「完成了两轮」的声明，失败后维持 2 会把
			// 单轮辩论伪装成完整两轮；RebuttalDegraded 供前端/manifest 归因。
			rebuttals = nil
			deb.Rounds = 1
			info.Rounds = 1
			deb.RebuttalDegraded = true
		}
	}
	deb.Rebuttals = rebuttals

	// --- judge 裁决 ---
	validIDs := claimIDSet(bullClaims, bearClaims)
	judgeRun := newLLMRun(traceID, parentRunID, "debate_judge", "debate_judge.v1", debateVersion)
	judgeRun.hashData(string(snapJSON))
	runs = append(runs, judgeRun)
	blockGate := hasBlockRiskFlag(snapshot)
	debJSON, _ := json.Marshal(map[string]any{
		"bull": bullClaims, "bear": bearClaims,
		"challenges": challenges, "rebuttals": rebuttals,
	})
	var judge *debateJudge
	judgeUsage, err := s.debateCallOne(ctx, userID, judgeRun, cfg, apiKey, allowPrivate,
		[]chatMessage{
			{Role: "system", Content: debateJudgeSystem},
			{Role: "user", Content: sharedData + "\n\n【辩论记录】（双方论点与交锋）：\n" + string(debJSON)},
		},
		func(content string) error {
			var out debateJudge
			if jerr := json.Unmarshal([]byte(extractJSONObject(content)), &out); jerr != nil {
				return fmt.Errorf("JSON 解析失败: %v", jerr)
			}
			out.Verdict = normalizeRating(out.Verdict)
			if !validRating[out.Verdict] {
				return errors.New("verdict 取值非法（须为 bullish/neutral/bearish）")
			}
			// 收口纪律（同 validateTradePlanSemantics 先例）：触发条件已排除 block，
			// 此处校验防未来触发判定重构后旁路——block 时 verdict 不得 bullish。
			if blockGate && out.Verdict == model.AnalysisRatingBullish {
				return errors.New("风险闸门为禁止买入级（block），verdict 不得为 bullish")
			}
			out.DecisiveClaimIDs = filterClaimIDs(out.DecisiveClaimIDs, validIDs)
			out.RejectedClaimIDs = filterClaimIDs(out.RejectedClaimIDs, validIDs)
			out.UnresolvedClaimIDs = filterClaimIDs(out.UnresolvedClaimIDs, validIDs)
			// 审查修复批：①三列表互斥——同一 claim 同时出现在多个列表时按
			// decisive>rejected>unresolved 优先级去重（裁决自相矛盾不作 repair，程序收口）；
			// ②剥除后必须至少保留一个有效 claim 引用——{"verdict":"bearish",
			// "decisive_claim_ids":["fake"]} 这类空引用裁决不得生效（会压低主分析置信度
			// 却没有任何论点支撑），触发 repair、打满走 judge_invalid。
			taken := map[string]bool{}
			dedupe := func(ids []string) []string {
				var kept []string
				for _, id := range ids {
					if !taken[id] {
						taken[id] = true
						kept = append(kept, id)
					}
				}
				return kept
			}
			out.DecisiveClaimIDs = dedupe(out.DecisiveClaimIDs)
			out.RejectedClaimIDs = dedupe(out.RejectedClaimIDs)
			out.UnresolvedClaimIDs = dedupe(out.UnresolvedClaimIDs)
			if len(out.DecisiveClaimIDs)+len(out.RejectedClaimIDs)+len(out.UnresolvedClaimIDs) == 0 {
				return errors.New("decisive/rejected/unresolved 必须引用至少一个双方实际产出的 claim id（bu-*/be-*）")
			}
			out.ConfidenceReason = truncateRunes(strings.TrimSpace(out.ConfidenceReason), debateTextMax)
			out.ConflictNote = truncateRunes(strings.TrimSpace(out.ConflictNote), debateTextMax)
			out.Invalidators = normalizeInvalidators(out.Invalidators)
			judge = &out
			return nil
		},
		`请只输出 JSON：{"verdict":"bullish|neutral|bearish","decisive_claim_ids":[],"rejected_claim_ids":[],"unresolved_claim_ids":[],"confidence_reason","invalidators":[],"conflict_note"}。`)
	addUsage(&usage, judgeUsage)
	if err != nil {
		// 双方对抗已完成、裁决缺席：保留双方论点供用户自判，如实记降级原因。
		if judgeRun.DegradedReason == "llm_output_invalid" {
			deb.DegradedReason = "judge_invalid"
		} else {
			deb.DegradedReason = "judge_failed"
		}
		return deb, usage, runs
	}
	deb.Judge = judge
	return deb, usage, runs
}

// claimsBrief 主分析 claims 的轻量摘要（喂辩论角色：id/文本/状态/证据引用）。
func claimsBrief(ev *evidenceCheck) []map[string]any {
	if ev == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(ev.Claims))
	for _, c := range ev.Claims {
		out = append(out, map[string]any{
			"claim_id": c.ClaimID, "section": c.Section, "text": c.Text,
			"status": c.Status, "evidence_ids": c.EvidenceIDs,
		})
	}
	return out
}

// addUsage usage 累加小工具。
func addUsage(dst *chatUsage, u chatUsage) {
	dst.PromptTokens += u.PromptTokens
	dst.CompletionTokens += u.CompletionTokens
	dst.TotalTokens += u.TotalTokens
}

// attachDebate 辩论编排入口（runAnalysis 在信任层回填之后调用）：判定触发条件 → 执行辩论 →
// 回填 result.Debate 与置信度联动。返回 token 用量与 run 列表（未触发时零值/nil）。
// 仅个股标准分析（调用方已限定 module=stock、mode=standard、非 as_of、非历史解释）。
//
// 置信度联动（增强不裁决）：judge verdict 与主评级方向相反时压 SysConfidence=low 并在
// 依据里点名（同 review reject 级联先例）；不改写 rating/summary/claims——辩论失败时
// 主结果就是「既有单路」，语义连续。
func (s *AnalysisService) attachDebate(ctx context.Context, userID int64, cfg *model.LLMConfig, apiKey string, allowPrivate bool, snapshot map[string]any, result *AnalysisResult, traceID, parentRunID string) (chatUsage, []*llmRun) {
	reasons := debateTriggerReasons(result, snapshot)
	if len(reasons) == 0 {
		return chatUsage{}, nil
	}
	sort.Strings(reasons) // 稳定序（manifest/测试可复现）
	deb, usage, runs := s.runDebate(ctx, userID, cfg, apiKey, allowPrivate, snapshot, result, reasons, traceID, parentRunID)
	result.Debate = deb
	if deb != nil && deb.Judge != nil && debateOpposite(result.Rating, deb.Judge.Verdict) {
		result.SysConfidence = "low"
		note := "辩论裁决（" + ratingCN(deb.Judge.Verdict) + "）与主评级方向相反"
		if result.SysConfidenceWhy != "" {
			result.SysConfidenceWhy += "；" + note
		} else {
			result.SysConfidenceWhy = note
		}
	}
	return usage, runs
}
