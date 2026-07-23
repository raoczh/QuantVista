package service

import (
	"fmt"
	"strings"

	"quantvista/model"
)

// P1-2 Claim/evidence/invalidator schema（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §5.2/§7.2）。
//
// 结论级声明（claim）把模块「关键结论」从散落的自然语言升级为结构化可追踪对象：
// 每条 claim 关联它引用的证据链 ID（P0-3 的 evidence_id 体系）、可标记
// resolved/unresolved/contradictory、并携带失效条件（invalidator）。
//
// 防伪造纪律（P0-3 的延续，ROADMAP §3 明令）：claim 与 evidence_id 的关联**必须程序推导**，
// 禁止让模型输出 evidence_ids 并照单采信——deriveClaims 从核验引擎（verifyEvidenceLabeled）
// 已产出的逐项明细里按段收集快照佐证命中项，模型无法伪造关联。模型侧只输出
// invalidator（失效条件）——它是预测性声明而非数据事实，模型输出合法，程序只做归一收集。
//
// status 判定（程序化，与 key_section 同口径——只有 origin 空的快照命中算佐证）：
//   - contradictory：该段存在 direction_mismatch 未命中项（引用数字与快照方向相反，
//     如快照 -5% 被写成「上涨 5%」）——最强告警，优先于 resolved；
//   - resolved：至少 1 个数字被数据快照佐证（evidence_ids 非空）；
//   - unresolved：无快照佐证（含：段内无可核验数字、全部命中是 plan/user/context 复述）。
//     unresolved 不是错误——定性结论可以没有数字；它如实声明「此结论未被快照数字佐证」。

// claim status 封闭枚举。
const (
	claimResolved      = "resolved"
	claimUnresolved    = "unresolved"
	claimContradictory = "contradictory"
)

// claim 归一上限：文本截断、失效条件条数与单条长度（入库/manifest 防撑爆；
// 完整原文在业务结果自身字段里，claim.text 只是索引摘要）。
const (
	claimTextMax        = 160
	claimInvalidatorMax = 4
	claimInvalidatorLen = 120
)

// llmClaim 结论级声明（evidenceCheck.claims 数组元素，ev5 起）。
// ClaimID/Section/EvidenceIDs/Status 全部程序推导；Text 为结论原文摘要（截断）；
// Invalidators 收集自模型输出的失效条件字段（kill_switches/invalidation/invalidators），
// 语义=「出现什么情况此结论作废」。
type llmClaim struct {
	ClaimID      string   `json:"claim_id"`               // cl-01…（按 spec 序分配）
	Section      string   `json:"section"`                // 来源结论段（与核验明细 Module 同名）
	Text         string   `json:"text"`                   // 结论原文摘要（≤claimTextMax rune）
	EvidenceIDs  []string `json:"evidence_ids,omitempty"` // 程序推导：该段被快照佐证数字的 evidence_id
	Status       string   `json:"status"`                 // resolved | unresolved | contradictory
	Invalidators []string `json:"invalidators,omitempty"` // 失效条件（模型声明，程序归一收集）
}

// claimSpec 一条待推导 claim 的输入：Section 必须与核验时 evidenceSection.Module 同名
// （deriveClaims 按它筛选明细项），Text 为结论原文，Invalidators 为模块失效条件字段原值。
type claimSpec struct {
	Section      string
	Text         string
	Invalidators []string
}

// deriveClaims 从核验结果按段推导结构化 claims（程序化，非模型自报）。
// check 为 verifyEvidenceLabeled 的产物（Items 已带 evidence_id/Module/Origin/Reason）；
// specs 由各模块调用方声明「哪些段是关键结论」。Text 为空的 spec 跳过（无结论不造 claim）。
// 注意：Items 有 50 上限截断（Truncated），claim 推导基于截断后的可见明细——与落库
// 明细一一对应（evidence_id 引用永远可在 items 里找到），不基于不可见数据。
func deriveClaims(check *evidenceCheck, specs []claimSpec) []llmClaim {
	if check == nil || len(specs) == 0 {
		return nil
	}
	out := make([]llmClaim, 0, len(specs))
	seq := 0
	for _, sp := range specs {
		text := strings.TrimSpace(sp.Text)
		if text == "" {
			continue
		}
		seq++
		cl := llmClaim{
			ClaimID:      fmt.Sprintf("cl-%02d", seq),
			Section:      sp.Section,
			Text:         truncateRunes(text, claimTextMax),
			Status:       claimUnresolved,
			Invalidators: normalizeInvalidators(sp.Invalidators),
		}
		contradictory := false
		for _, it := range check.Items {
			if it.Module != sp.Section {
				continue
			}
			// 只有 origin 空的快照命中算佐证（与 markKeySection 同口径）：
			// plan/user/context 复述命中不进 evidence_ids——「模型复述自己的计划价」
			// 不构成对结论的数据佐证。
			if it.Matched && it.Origin == "" && it.EvidenceID != "" {
				cl.EvidenceIDs = append(cl.EvidenceIDs, it.EvidenceID)
				continue
			}
			if !it.Matched && it.Reason == "direction_mismatch" {
				contradictory = true
			}
		}
		switch {
		case contradictory:
			cl.Status = claimContradictory
		case len(cl.EvidenceIDs) > 0:
			cl.Status = claimResolved
		}
		out = append(out, cl)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeInvalidators 失效条件归一：去空白、截长度、限条数。nil 安全。
func normalizeInvalidators(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, truncateRunes(s, claimInvalidatorLen))
		if len(out) >= claimInvalidatorMax {
			break
		}
	}
	return out
}

// analysisClaimSpecs 分析模块的关键结论段清单：总结（失效条件=模型 kill_switches）+
// 交易计划（失效条件=计划 invalidators，P1-2 起模型输出；无计划/no_plan 不建 claim）。
// Section 名与 fillAnalysisTrust 的 addSec 段名严格一致（推导按它匹配明细）。
func analysisClaimSpecs(result *AnalysisResult) []claimSpec {
	specs := []claimSpec{{Section: "总结", Text: result.Summary, Invalidators: result.KillSwitches}}
	if tp := result.TradePlan; tp != nil && !tp.NoPlan {
		specs = append(specs, claimSpec{Section: "交易计划", Text: tp.PlanNote, Invalidators: tp.Invalidators})
	}
	return specs
}

// recPickClaimText 单条推荐的结论正文（claim 文本与其核验段共用，审查修复批抽出）：
// 短线=首条理由、长线=投资逻辑（thesis）优先；全空时以动作本身为结论陈述兜底。
func recPickClaimText(p recPick) string {
	if strings.TrimSpace(p.Thesis) != "" {
		return p.Thesis
	}
	if len(p.Reason) > 0 && strings.TrimSpace(p.Reason[0]) != "" {
		return p.Reason[0]
	}
	return fmt.Sprintf("%s：%s", p.Symbol, recActionLabel(p.Action))
}

// recPickClaimSpec 单条推荐的 claim spec：text=recPickClaimText（thesis/首条理由）。
// Section=「推荐结论」——审查修复批起 claim 正文自成核验段（verifyEvidence 已把该文本
// 作为独立 section 核验）：claim 的 evidence_ids 只从**结论正文自身**的快照命中推导、
// 正文里的方向错误能标 contradictory。此前 Section=「推荐依据」时，一条与结论无关但
// 数字正确的 evidence 也会把 thesis 标成 resolved（核验的不是被声明的内容）——严禁改回。
func recPickClaimSpec(p recPick) claimSpec {
	var inv []string
	if strings.TrimSpace(p.Invalidation) != "" {
		inv = []string{p.Invalidation}
	}
	return claimSpec{Section: recClaimSection, Text: recPickClaimText(p), Invalidators: inv}
}

// recActionLabel action 人话（claim 兜底文本用）。
func recActionLabel(action string) string {
	if action == model.RecActionBuy {
		return "可考虑买入"
	}
	return "观察等待"
}

// dailyClaimSpecs 日报的关键结论段：总结 + 明日计划。日报 schema 无失效条件字段
// （复盘是对已发生事实的解读，非前瞻计划），invalidators 如实为空、不硬造。
func dailyClaimSpecs(rv *dailyReview) []claimSpec {
	return []claimSpec{
		{Section: "总结", Text: rv.Summary},
		{Section: "明日计划", Text: rv.TomorrowPlan},
	}
}
