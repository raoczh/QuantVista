package service

import (
	"fmt"
	"strings"

	"quantvista/model"
)

// P2-3 多层上下文检索（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.3）——QA 侧。
//
// 现状问题：QA 每轮只带最近 qaHistoryLimit 条历史，更早轮次被静默丢弃——模型不知道
// 有更早讨论，用户也不知道模型没看到（不可见截断）。本文件把截断升级为程序化三层：
//   - Tier1 当前层：最近 qaHistoryLimit 条历史全文（消息流位置与截断行为不变）；
//   - Tier2 经验层：被裁剪轮次的程序化索引（每轮一行「Q 要点/A 要点」，预算内从近到远）；
//   - Tier3 按需层：本轮问题与被裁剪轮次做相关性匹配（rune bigram 重合），命中的旧轮
//     注入更完整摘录——「按需历史」不靠模型自取，由程序按当前问题检索。
// token 与被裁剪字段可见：每轮回答落 context_json（各层条数/字符/粗估 token/完全不可见
// 轮数/Tier3 命中依据），前端展示——「模型看到了什么、没看到什么」可核查。
//
// 纪律：
//   - 纯程序化零额外 LLM 调用（摘要=截断索引而非 LLM 生成摘要——摘要质量换确定性与零成本）；
//   - Tier2/Tier3 是数据段注入（历史会话摘录声明），不改任务段与契约段；
//   - 核验语义零变化：历史 user 消息本就全量进核验值域（origin=user）、assistant 历史
//     本就不进值域（复述旧回答数字仍须对照快照）——分层注入不改变这两条；
//   - flag llm_layered_context 关闭时消息序列与旧版逐字节一致（反例测试锁定）。

const (
	// qaCtxVersion 分层上下文结构版本（改分层规则/预算/匹配算法须递增）。
	qaCtxVersion = "qc1"
	// qaTier2CharBudget Tier2 索引层总字符预算（rune 数）；超出从最早轮开始丢弃并计数。
	qaTier2CharBudget = 1200
	// qaTier2QMax / qaTier2AMax 单轮索引行内 Q/A 摘录上限（rune）。
	qaTier2QMax = 60
	qaTier2AMax = 80
	// qaTier3MaxRounds 按需层最多注入的旧轮数。
	qaTier3MaxRounds = 2
	// qaTier3MinScore 相关性最低门槛（rune bigram 交集数）：低于此视为不相关不注入——
	// 单字/常用词撞车不构成「按需」依据。
	qaTier3MinScore = 3
	// qaTier3QMax / qaTier3AMax 按需层单轮摘录上限（rune）。
	qaTier3QMax = 200
	qaTier3AMax = 400
	// qaTokenPerRune token 粗估系数：approx_tokens = 总 rune 数 / 该值（中文 1 token ≈
	// 1.5~1.8 字符的经验中值；仅供量级观察，字段名 approx 已声明非精确）。
	qaTokenPerRune = 1.6
)

// QaTier3Match Tier3 命中依据（round 为 1 基轮号，按 user 消息序）。
type QaTier3Match struct {
	Round int `json:"round"`
	Score int `json:"score"` // rune bigram 交集数
}

// QaContextLayers 本轮上下文分层快照（落 assistant 消息 context_json，前端可见）。
type QaContextLayers struct {
	Version      string `json:"version"`
	HistoryMsgs  int    `json:"history_msgs"`  // 本轮之前的历史消息总数
	HistoryChars int    `json:"history_chars"` // 历史消息总 rune 数
	Tier1Msgs    int    `json:"tier1_msgs"`    // 全文窗口内消息数
	Tier1Chars   int    `json:"tier1_chars"`
	Tier2Rounds  int    `json:"tier2_rounds"` // 索引层列出的轮数
	Tier2Chars   int    `json:"tier2_chars"`
	// Tier2DroppedRounds 预算不够未列入索引的更早轮数（对模型不可见的部分之一）。
	Tier2DroppedRounds int            `json:"tier2_dropped_rounds"`
	Tier3Rounds        int            `json:"tier3_rounds"`
	Tier3Chars         int            `json:"tier3_chars"`
	Tier3Matched       []QaTier3Match `json:"tier3_matched,omitempty"`
	// InvisibleRounds 对模型完全不可见的轮数（未进 Tier1、被 Tier2 预算丢弃、也未被
	// Tier3 命中）——「被裁剪字段可见」的核心数字。
	InvisibleRounds int `json:"invisible_rounds"`
	// ApproxTokens 本轮全部注入内容（system+历史+问题）的粗估 token（rune/1.6）。
	ApproxTokens int `json:"approx_tokens"`
}

// qaRound 一轮问答（user 消息开轮，其后连续 assistant 消息并入该轮）。
type qaRound struct {
	Idx       int // 1 基轮号
	User      string
	Assistant string
}

// qaSplitRounds 把消息序列切成轮次。开头的孤儿 assistant 消息（无 user 引导，理论
// 不出现）并入首个虚拟轮。
func qaSplitRounds(msgs []model.AiConversationMessage) []qaRound {
	var rounds []qaRound
	for _, m := range msgs {
		switch m.Role {
		case model.QaRoleUser:
			rounds = append(rounds, qaRound{Idx: len(rounds) + 1, User: m.Content})
		default:
			if len(rounds) == 0 {
				rounds = append(rounds, qaRound{Idx: 1})
			}
			r := &rounds[len(rounds)-1]
			if r.Assistant != "" {
				r.Assistant += "\n"
			}
			r.Assistant += m.Content
		}
	}
	return rounds
}

// qaBigrams rune bigram 集合（跳过含空白的对；大小写归一）。
func qaBigrams(text string) map[string]bool {
	rs := []rune(strings.ToLower(text))
	out := map[string]bool{}
	for i := 0; i+1 < len(rs); i++ {
		if rs[i] == ' ' || rs[i] == '\n' || rs[i] == '\t' || rs[i+1] == ' ' || rs[i+1] == '\n' || rs[i+1] == '\t' {
			continue
		}
		out[string(rs[i:i+2])] = true
	}
	return out
}

// qaRelevanceScore 本轮问题与旧轮文本的相关性（bigram 交集数）。
func qaRelevanceScore(qGrams map[string]bool, round qaRound) int {
	rGrams := qaBigrams(round.User + " " + round.Assistant)
	n := 0
	for g := range qGrams {
		if rGrams[g] {
			n++
		}
	}
	return n
}

// qaFirstLine 文本首个非空行（索引行的 A 要点取回答开头结论）。
func qaFirstLine(text string) string {
	for _, ln := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// qaLayeredContext 分层结果：注入 system 的数据段 + 可观测快照。
type qaLayeredContext struct {
	Segment string           // system 注入段（空=无被裁剪历史，零注入零噪声）
	Layers  *QaContextLayers // 恒非 nil（无分层时也落 Tier1 计数，可观测一致）
}

// buildQaLayeredContext 对「窗口外历史」构建 Tier2/Tier3（Tier1=窗口内消息，由调用方
// 照旧拼进消息流）。older 为被截断的更早消息（升序）、recent 为窗口内消息（升序）。
func buildQaLayeredContext(older, recent []model.AiConversationMessage, question string) qaLayeredContext {
	layers := &QaContextLayers{Version: qaCtxVersion}
	layers.HistoryMsgs = len(older) + len(recent)
	for _, m := range older {
		layers.HistoryChars += len([]rune(m.Content))
	}
	layers.Tier1Msgs = len(recent)
	for _, m := range recent {
		c := len([]rune(m.Content))
		layers.HistoryChars += c
		layers.Tier1Chars += c
	}
	if len(older) == 0 {
		return qaLayeredContext{Segment: "", Layers: layers}
	}

	rounds := qaSplitRounds(older)

	// Tier3 按需检索：相关性达门槛的旧轮，按分数降序取 top（同分取更近的轮）。
	qGrams := qaBigrams(question)
	type scored struct {
		round qaRound
		score int
	}
	var cands []scored
	for _, r := range rounds {
		if sc := qaRelevanceScore(qGrams, r); sc >= qaTier3MinScore {
			cands = append(cands, scored{round: r, score: sc})
		}
	}
	// 稳定排序：分数降序，同分轮号大（更近）优先。
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].score > cands[i].score ||
				(cands[j].score == cands[i].score && cands[j].round.Idx > cands[i].round.Idx) {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	if len(cands) > qaTier3MaxRounds {
		cands = cands[:qaTier3MaxRounds]
	}
	tier3Hit := map[int]bool{}
	var tier3 strings.Builder
	for _, c := range cands {
		tier3Hit[c.round.Idx] = true
		entry := fmt.Sprintf("- 第 %d 轮（相关度 %d）｜问：%s", c.round.Idx, c.score, truncateRunes(c.round.User, qaTier3QMax))
		if a := strings.TrimSpace(c.round.Assistant); a != "" {
			entry += "｜答：" + truncateRunes(a, qaTier3AMax)
		}
		tier3.WriteString(entry + "\n")
		layers.Tier3Rounds++
		layers.Tier3Chars += len([]rune(entry))
		layers.Tier3Matched = append(layers.Tier3Matched, QaTier3Match{Round: c.round.Idx, Score: c.score})
	}

	// Tier2 索引：从最近往前列到预算耗尽（Tier3 已全文摘录的轮不重复占索引预算；
	// 预算不够即停——跳过长行继续装短行会产生「跳轮索引」，误导模型以为中间轮不存在）。
	var tier2Lines []string
	budget := qaTier2CharBudget
	dropped := 0
	for i := len(rounds) - 1; i >= 0; i-- {
		r := rounds[i]
		if tier3Hit[r.Idx] {
			continue
		}
		line := fmt.Sprintf("- 第 %d 轮｜问：%s", r.Idx, truncateRunes(r.User, qaTier2QMax))
		if a := qaFirstLine(r.Assistant); a != "" {
			line += "｜答要点：" + truncateRunes(a, qaTier2AMax)
		}
		n := len([]rune(line))
		if n > budget {
			// 预算尽：本轮及所有更早的未列轮（含未被 Tier3 命中者）全部计入 dropped。
			for j := i; j >= 0; j-- {
				if !tier3Hit[rounds[j].Idx] {
					dropped++
				}
			}
			break
		}
		budget -= n
		tier2Lines = append(tier2Lines, line)
		layers.Tier2Rounds++
		layers.Tier2Chars += n
	}
	// tier2Lines 是从近到远 append 的，反转成轮号升序展示。
	for i, j := 0, len(tier2Lines)-1; i < j; i, j = i+1, j-1 {
		tier2Lines[i], tier2Lines[j] = tier2Lines[j], tier2Lines[i]
	}
	layers.Tier2DroppedRounds = dropped
	layers.InvisibleRounds = dropped // Tier3 命中的轮不会进 dropped（上面 continue 在预算判断前）

	var seg strings.Builder
	seg.WriteString("【历史会话分层上下文】本会话更早轮次未随消息全文带入，以下为程序化摘录（截断索引与按相关性检索的原文摘录，非当前数据快照事实；「答」为当时回答，其结论可能已过时，引用其中数字仍须与数据快照核对）：\n")
	if len(tier2Lines) > 0 {
		seg.WriteString(fmt.Sprintf("更早轮次索引（共 %d 轮", len(rounds)))
		if dropped > 0 {
			seg.WriteString(fmt.Sprintf("，最早 %d 轮因篇幅未列出", dropped))
		}
		seg.WriteString("）：\n")
		seg.WriteString(strings.Join(tier2Lines, "\n"))
		seg.WriteString("\n")
	} else if dropped > 0 {
		seg.WriteString(fmt.Sprintf("更早 %d 轮因篇幅未列出。\n", dropped))
	}
	if tier3.Len() > 0 {
		seg.WriteString("与本轮问题相关的旧轮摘录（按相关性检索）：\n")
		seg.WriteString(tier3.String())
	}
	return qaLayeredContext{Segment: strings.TrimRight(seg.String(), "\n"), Layers: layers}
}

// qaApproxTokens 粗估 token（全部消息内容 rune 数 / qaTokenPerRune）。
func qaApproxTokens(msgs []chatMessage) int {
	total := 0
	for _, m := range msgs {
		total += len([]rune(m.Content))
	}
	return int(float64(total)/qaTokenPerRune + 0.5)
}
