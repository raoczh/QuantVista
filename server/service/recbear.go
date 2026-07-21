package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"quantvista/model"
)

// S2-2 反方研究员（RECOMMENDATION_ACCURACY_PLAN §5 S2-2）：对每只 buy 用一次独立
// LLM 调用构建**最强 bear case**（A 股 bear 论据框架：解禁/T+1 锁仓/高位放量/拥挤/
// 估值），输出 {symbol, bear_case, severity}。
//
// 影子纪律：bear_case 与 severity 只随明细展示，**不改写 action/置信度**；同时记录
// 「若按 severity=high→watch 执行会改写谁」进反事实事件表（gate_type=bear_shadow），
// 其影子收益由既有标签任务自然结算。证明否决的标的确实更差（影子对照报表）后，
// 才启用程序裁决（high→降 watch+置信度≤40；med→置信度 -15，见文档）。
//
// 调用预算：主调 1 + 复核（verify）1 + 反方 1 = 上限 3 次不变；反方开关默认关联
// verify（RecommendRequest.BearCheck 未显式指定时跟随 Verify）。
const (
	bearReviewVersion = "br1"
	recBearTokensCap  = 1500 // 反方研究员输出预算（与复核员同档：每只 buy 一段 bear case）
)

// pickBear 反方研究员对单条 buy 的结论（落 pick 明细，影子期仅展示）。
type pickBear struct {
	Symbol   string `json:"symbol"`
	BearCase string `json:"bear_case"`
	Severity string `json:"severity"` // high / med / low
}

// bearSystemPrompt 反方研究员系统提示：独立空头视角，注入 A 股 bear 论据检查框架。
// 数据诚实纪律与主调用同源——解禁/减持等本系统未提供的数据只能提示核查，禁止虚构。
const bearSystemPrompt = `你是一名独立的空头研究员（反方研究员），任务是对另一位研究员建议买入的每只 A 股标的，构建**最强反方论证（bear case）**。你不给买入建议，只找否决理由。

A 股空头论据检查框架（逐项对照名单数据检查，命中才写）：
1. 高位放量风险：近期涨幅过大（chg_5d/chg_20d/pos_60 高位）+ 放量（vol_boost/换手率高），警惕获利盘兑现与出货；
2. 拥挤交易：人气榜高位（pop_rank 靠前）、换手率极高、量比异常——情绪退潮时踩踏；
3. 估值风险：pe_ttm/pb 明显偏高或为负（亏损股）；长线标的财务恶化（fin 中增速/ROE 下滑）；
4. T+1 锁仓风险：当日买入无法当日卖出，若尾盘拉升买入次日低开即被动锁仓（tail30_chg 大幅拉升 + close_vs_vwap 偏离过高时要点名）；
5. 技术面背离：MACD 顶背离迹象、RSI 超买（rsi_14≥70）、乖离过大（bias_20 偏高）、跌破关键均线；
6. 解禁与减持：本系统未提供解禁/减持数据——如判断该风险值得核查，只能提示「需自行核查解禁与股东减持公告」，严禁虚构具体解禁日期、数量或股东行为。

铁律：只依据给出的数据论证，引用具体字段与数值；数据不足处如实说明，不得用记忆中的公司印象编造论据。severity 分级：high=反方证据足以否决买入（若采纳应降为观察）；med=风险显著需减仓或收紧止损；low=常规风险提示。
只输出 JSON：{"bears":[{"symbol":"...","bear_case":"3~5 句最强反方论证（引用数据）","severity":"high|med|low"}]}，覆盖全部给出的标的，不要任何解释或代码块标记。每条 bear_case ≤120 字。`

// bearReview 对 buy 条目独立调用构建 bear case。best-effort：失败只是没有反方结论，
// 不影响主结果；1 次 repair。返回按 symbol 归并的有效结论与 token 用量。
func (s *RecommendationService) bearReview(ctx context.Context, userID int64, cfg *model.LLMConfig, apiKey string, allowPrivate bool, picks []recPick, pool map[string]candidate) ([]pickBear, chatUsage) {
	var usage chatUsage
	rows := make([]map[string]any, 0, len(picks))
	buySet := map[string]bool{}
	for _, p := range picks {
		if p.Action != model.RecActionBuy {
			continue
		}
		c := pool[p.Symbol]
		buySet[p.Symbol] = true
		rows = append(rows, map[string]any{
			"symbol": p.Symbol, "name": c.Name, "action": p.Action, "confidence": int(p.Confidence),
			"bull_reason": p.Reason,
			"data": map[string]any{
				"price": c.Price, "change_pct": c.ChangePct, "turnover_rate": c.TurnoverRate,
				"volume_ratio": c.VolumeRatio, "pe_ttm": c.PETTM, "pb": c.PB,
				"float_cap_yi": round2(c.FloatCap / 1e8), "pop_rank": c.PopRank,
				"factors": c.Factors, "fin": c.Fin,
			},
		})
	}
	if len(rows) == 0 {
		return nil, usage // 无 buy 条目：零调用零成本
	}
	inputJSON, err := json.Marshal(rows)
	if err != nil {
		return nil, usage
	}

	convo := []chatMessage{
		{Role: "system", Content: bearSystemPrompt},
		{Role: "user", Content: "买入建议与对应真实数据如下（JSON 数组，bull_reason 为正方理由，供你针对性反驳）：\n" + string(inputJSON)},
	}
	type bearOut struct {
		Bears []pickBear `json:"bears"`
	}
	for attempt := 0; attempt <= 1; attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			Temperature: cfg.Temperature, MaxTokens: capModuleTokens(cfg.MaxTokens, recBearTokensCap),
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
			Repair: attempt > 0, // repair 轮：契约开启时温度固定 0
			Meta:   chatMeta{CallerUserID: userID, Module: "rec_bear", ConfigID: cfg.ID, Provider: cfg.Provider},
		})
		if err != nil {
			return nil, usage
		}
		usage.PromptTokens += res.Usage.PromptTokens
		usage.CompletionTokens += res.Usage.CompletionTokens
		usage.TotalTokens += res.Usage.TotalTokens

		var out bearOut
		if jerr := json.Unmarshal([]byte(extractJSONObject(res.Content)), &out); jerr == nil && len(out.Bears) > 0 {
			valid := make([]pickBear, 0, len(out.Bears))
			seen := map[string]bool{}
			for _, b := range out.Bears {
				b.Symbol = strings.TrimSpace(b.Symbol)
				b.Severity = normalizeBearSeverity(b.Severity)
				b.BearCase = truncateRunes(strings.TrimSpace(b.BearCase), 300)
				if !buySet[b.Symbol] || seen[b.Symbol] || b.BearCase == "" || b.Severity == "" {
					continue // 只认 buy 条目上的结论：越界/重复/空结论丢弃
				}
				seen[b.Symbol] = true
				valid = append(valid, b)
			}
			if len(valid) > 0 {
				return valid, usage
			}
		}
		convo = append(convo,
			chatMessage{Role: "assistant", Content: truncateRunes(res.Content, 600)},
			chatMessage{Role: "user", Content: "上一条输出不合格。请只输出 JSON：{\"bears\":[{\"symbol\",\"bear_case\",\"severity\":\"high|med|low\"}]}，symbol 必须来自给出的买入标的。"},
		)
	}
	return nil, usage
}

// normalizeBearSeverity 归一 severity 枚举（模型常输出 medium/中/高 等变体）。
func normalizeBearSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high", "高":
		return "high"
	case "med", "medium", "mid", "中":
		return "med"
	case "low", "低":
		return "low"
	}
	return ""
}

// applyBearShadow 影子接线：bear 结论写入 pick 明细（只展示），**不改写 action/置信度**；
// severity=high 的 buy 条目产出 gateNote（would_be_action=watch）进反事实事件表——
// 「若按 high→watch 执行会改写谁」，影子收益由标签任务自然结算。
func applyBearShadow(picks []recPick, bears []pickBear) []gateNote {
	bySym := make(map[string]pickBear, len(bears))
	for _, b := range bears {
		bySym[b.Symbol] = b
	}
	var gates []gateNote
	for i := range picks {
		b, ok := bySym[picks[i].Symbol]
		if !ok {
			continue
		}
		bc := b
		picks[i].Bear = &bc
		if b.Severity == "high" && picks[i].Action == model.RecActionBuy {
			gates = append(gates, gateNote{
				Symbol:        picks[i].Symbol,
				GateType:      model.GateBearShadow,
				GateVersion:   bearReviewVersion,
				WouldBeAction: model.RecActionWatch,
				Reason:        fmt.Sprintf("反方研究员（影子）severity=high：若强制执行将 buy→watch（当前未改写）。%s", truncateRunes(b.BearCase, 120)),
			})
		}
	}
	return gates
}
