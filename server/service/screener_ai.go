package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// P3c AI 白话建策略：自然语言 → 条件树 DSL（service/screener.go）。
//
// 设计要点：
//   - prompt 的因子字典由 factorDefs 程序生成（screenerFactorDictLines），
//     因子增删自动跟随，单测锁完整性——禁止手抄清单（会漂移）；
//   - unmatched 兜底纪律：宽表没有的数据（估值/资金流/财务/板块题材等）
//     必须如实进 unmatched 数组，禁止硬凑相近因子冒充；
//   - 输出 {tree, unmatched[], explain}：tree 过 validateCondTree（因子白名单/
//     深度/叶子上限现成校验）+ describeCondTree 人话回显；校验失败 repair 一次；
//   - AI 只负责「生成」：树落编辑器/保存/扫描全部由用户在前端确认后走既有链路，
//     本服务不落库不执行扫描。
//
// 配额：一次解析 = 1 次手动动作（manualAction=true），repair 不重复计次；
// LLM 走用户配置（ResolveForUse），无配置时回退管理员默认链路自然生效。

const (
	// screenerParsePromptVersion 解析 prompt 版本（独立于分析 p*/推荐 s* 序列）。
	screenerParsePromptVersion = "sp1"
	parseStrategyTextMax       = 300 // 白话输入长度上限（rune）
	parseStrategyRepairMax     = 1   // 结构/校验失败后的 repair 次数上限
)

// ScreenerAIService 白话建策略：LLM 解析自然语言为条件树。
type ScreenerAIService struct {
	llm *LLMService
}

func NewScreenerAIService(llm *LLMService) *ScreenerAIService {
	return &ScreenerAIService{llm: llm}
}

// ParseStrategyRequest 解析入参。
type ParseStrategyRequest struct {
	Text        string `json:"text"`
	LLMConfigID int64  `json:"llm_config_id"` // 0=默认配置（同分析/问答语义）
}

// ParseStrategyResult 解析结果：树 + 未映射表述 + 人话回显。
// Tree 可能为 nil（全部表述都无法映射时），此时 Unmatched 必非空。
type ParseStrategyResult struct {
	Tree          *CondNode `json:"tree"`
	Unmatched     []string  `json:"unmatched"`
	Explain       string    `json:"explain"`
	Conditions    []string  `json:"conditions"` // describeCondTree 人话条件清单（前端预览）
	PromptVersion string    `json:"prompt_version"`
	TotalTokens   int       `json:"total_tokens"`
}

// ParseStrategy 把白话选股描述解析为条件树。校验失败 repair 一次，仍不过则报错
//（交互式动作，不做降级半成品——用户重试成本低，脏树落编辑器危害大）。
func (s *ScreenerAIService) ParseStrategy(ctx context.Context, userID int64, allowPrivate bool, req ParseStrategyRequest) (*ParseStrategyResult, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, errors.New("请先用白话描述选股条件")
	}
	if len([]rune(text)) > parseStrategyTextMax {
		return nil, fmt.Errorf("描述过长（最多 %d 字）", parseStrategyTextMax)
	}

	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, err
	}
	allowPrivate = llmAllowPrivate(allowPrivate, cfg)
	if err := checkQuota(userID); err != nil {
		return nil, err
	}

	convo := []chatMessage{
		{Role: "system", Content: buildParseStrategySystemPrompt()},
		{Role: "user", Content: text},
	}
	var acc chatUsage
	var parsed *parsedStrategy
	var lastPerr error
	for attempt := 0; attempt <= parseStrategyRepairMax; attempt++ {
		res, callErr := chatCompletion(ctx, chatParams{
			BaseURL:      cfg.BaseURL,
			APIKey:       apiKey,
			Model:        cfg.Model,
			EndpointType: cfg.EndpointType,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
			Messages:     convo,
			JSONMode:     true,
			AllowPrivate: allowPrivate,
			Meta:         chatMeta{CallerUserID: userID, Module: "screener_parse", ConfigID: cfg.ID, Provider: cfg.Provider},
		})
		if callErr != nil {
			// 网络/鉴权类失败：已消耗的 token 照记（审计），动作照计次（与分析口径一致）。
			if acc.TotalTokens > 0 {
				consumeQuota(userID, acc.TotalTokens, true)
			}
			return nil, callErr
		}
		acc.PromptTokens += res.Usage.PromptTokens
		acc.CompletionTokens += res.Usage.CompletionTokens
		acc.TotalTokens += res.Usage.TotalTokens

		parsed, lastPerr = parseStrategyLLMOutput(res.Content)
		if lastPerr == nil {
			break
		}
		convo = append(convo,
			chatMessage{Role: "assistant", Content: res.Content},
			chatMessage{Role: "user", Content: "上一条输出不符合要求：" + lastPerr.Error() + "。" + parseStrategyRepairHint},
		)
	}
	if acc.TotalTokens > 0 {
		consumeQuota(userID, acc.TotalTokens, true)
	}
	if lastPerr != nil {
		return nil, fmt.Errorf("AI 输出无法解析为合法条件树（已重试一次）：%v", lastPerr)
	}

	out := &ParseStrategyResult{
		Tree:          parsed.Tree,
		Unmatched:     parsed.Unmatched,
		Explain:       truncateRunes(strings.TrimSpace(parsed.Explain), 200),
		PromptVersion: screenerParsePromptVersion,
		TotalTokens:   acc.TotalTokens,
	}
	if out.Unmatched == nil {
		out.Unmatched = []string{}
	}
	if out.Tree != nil {
		out.Conditions = describeCondTree(out.Tree)
	}
	return out, nil
}

// ---------- LLM 输出解析 ----------

// parsedStrategy LLM 应输出的 JSON 结构。
type parsedStrategy struct {
	Tree      *CondNode `json:"tree"`
	Unmatched []string  `json:"unmatched"`
	Explain   string    `json:"explain"`
}

const parseStrategyRepairHint = "请只输出一个合法 JSON 对象：{\"tree\":<条件树或 null>,\"unmatched\":[\"无法映射的表述\"],\"explain\":\"一句话\"}；" +
	"条件树只能使用因子字典中列出的 key，op 只能是 >/>=/</<=/between/is_true/is_false，" +
	"无法映射的表述放进 unmatched 而不是硬凑因子。不要任何解释或代码块标记。"

// parseStrategyLLMOutput 解析并校验 LLM 输出。tree 为 null 仅当 unmatched 非空时合法
//（「一个条件都没映射出来」必须给出理由，防止模型空手交差）。
func parseStrategyLLMOutput(content string) (*parsedStrategy, error) {
	raw := extractJSONObject(content)
	if raw == "" {
		return nil, errors.New("输出中未找到 JSON 对象")
	}
	var p parsedStrategy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	// 规整 unmatched：去空白项。
	clean := p.Unmatched[:0]
	for _, u := range p.Unmatched {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, truncateRunes(u, 100))
		}
	}
	p.Unmatched = clean
	if p.Tree == nil {
		if len(p.Unmatched) == 0 {
			return nil, errors.New("tree 为 null 且 unmatched 为空——请给出条件树，或把无法映射的表述放进 unmatched")
		}
		return &p, nil
	}
	if _, err := validateCondTree(p.Tree, 1); err != nil {
		return nil, err
	}
	return &p, nil
}

// ---------- prompt 构造 ----------

// parseFactorHints 少数因子的典型阈值提示（供模型把「温和放量」这类模糊表述
// 合理量化）。只做提示不做清单——因子清单本体永远来自 factorDefs。
var parseFactorHints = map[string]string{
	"vol_boost":     "缩量 <0.8、温和放量 1.5~3、异常放量 >5",
	"vol_5v20":      ">1 量能走强，<1 走弱",
	"rsi_14":        "超卖 <30、强势区 55~70、超买 >70",
	"chip_profit":   "低获利盘 <15、高获利盘 >80",
	"pos_60":        "低位 <30、高位 >70",
	"pos_250":       "年内低位 <30、高位 >70",
	"turnover_rate": "温和 3~15、过热 >20",
	"bias_20":       "贴近 20 日线约 -3~3",
	"bias_250":      "贴近年线约 -5~5",
	"boll_pos":      "0=下轨、50=中轨、100=上轨",
	"ma_spread_pct": "<3 视为均线粘合",
	"volatility_20": "低波动 <2、高波动 >5",
	"drawdown_20":   "回撤温和 <8、深回撤 >15",
}

// factorUnitText 因子单位（按展示类型推导）。
func factorUnitText(k factorKind) string {
	switch k {
	case fkPrice:
		return "元"
	case fkPct:
		return "%"
	case fkInt:
		return "整数"
	case fkBool:
		return "布尔"
	default:
		return "倍数/无量纲"
	}
}

// screenerFactorDictLines 程序生成因子字典行（键｜中文名｜单位｜说明[｜典型阈值]）。
// 单测锁 factorDefs 每个 key 都在其中——新增因子自动进 prompt，无需改这里。
func screenerFactorDictLines() []string {
	lines := make([]string, 0, len(factorDefs))
	for _, d := range factorDefs {
		line := fmt.Sprintf("- %s｜%s｜%s｜%s", d.Key, d.Name, factorUnitText(d.Kind), d.Desc)
		if hint, ok := parseFactorHints[d.Key]; ok {
			line += "｜典型阈值：" + hint
		}
		lines = append(lines, line)
	}
	return lines
}

// buildParseStrategySystemPrompt 解析系统提示词（版本 sp1）。
func buildParseStrategySystemPrompt() string {
	var b strings.Builder
	b.WriteString("你是选股条件翻译器：把用户的白话选股描述转换成条件树 JSON。你只做翻译，不评价策略好坏，不添加用户没提的条件。\n\n")

	b.WriteString("【因子字典】（唯一合法因子清单，格式：key｜中文名｜单位｜说明）\n")
	for _, l := range screenerFactorDictLines() {
		b.WriteString(l)
		b.WriteString("\n")
	}

	b.WriteString("\n【条件树 DSL】\n")
	b.WriteString("- 组节点：{\"all\":[...]}（全部满足）或 {\"any\":[...]}（满足其一）；同一节点只能有 all 或 any，可互相嵌套（≤6 层，叶子总数 ≤48）。\n")
	b.WriteString("- 叶子：{\"factor\":\"<key>\",\"op\":\"<op>\",\"value\":<数值>}；op 只能是 > / >= / < / <= / between / is_true / is_false。\n")
	b.WriteString("- between 需要 value（下限）与 value2（上限）；布尔因子只能用 is_true/is_false 且不带 value。\n")
	b.WriteString("- 与另一因子比较用 ref：{\"factor\":\"close\",\"op\":\">\",\"ref\":\"ma20\"}（value 与 ref 互斥）。\n")
	b.WriteString("- 数值单位与因子一致：百分比因子直接写数字（15 表示 15%）。\n")

	b.WriteString("\n【输出格式】只输出一个 JSON 对象，不要解释、不要代码块标记：\n")
	b.WriteString("{\"tree\":<条件树，全部表述都无法映射时为 null>,\"unmatched\":[\"无法映射的表述（可括注原因）\"],\"explain\":\"一句话复述你理解的选股意图\"}\n")

	b.WriteString("\n【纪律】\n")
	b.WriteString("1. 只能使用因子字典中的 key，绝不发明、拼写变体或改写。\n")
	b.WriteString("2. 无法映射的表述必须原样放进 unmatched，禁止硬凑相近因子冒充——宁可 unmatched 也不错配。\n")
	b.WriteString("3. 因子字典没有估值（市盈率/市净率/市值）、资金流（主力/北向）、财务（营收/利润）、板块题材、龙虎榜等数据，这类表述一律进 unmatched。\n")
	b.WriteString("4. 用户没给具体数值的模糊表述（如「温和放量」），按典型阈值合理量化，并在 explain 里体现。\n")
	b.WriteString("5. 「回踩/贴近某均线」用对应 bias 因子（如 bias_20 介于小区间），「站上/跌破某均线」用 close 与均线的 ref 比较或 above_* 布尔因子。\n")

	b.WriteString("\n【示例】\n")
	b.WriteString("输入：量比 2 倍以上放量突破 20 日新高，换手别超过 20%\n")
	b.WriteString(`输出：{"tree":{"all":[{"factor":"vol_boost","op":">=","value":2},{"factor":"high_20d","op":"is_true"},{"factor":"turnover_rate","op":"<=","value":20}]},"unmatched":[],"explain":"量比≥2 放量创 20 日新高，且换手率不过热（≤20%）"}` + "\n\n")
	b.WriteString("输入：缩量回踩 20 日线不破，或者 RSI 超卖\n")
	b.WriteString(`输出：{"tree":{"any":[{"all":[{"factor":"vol_boost","op":"<","value":0.8},{"factor":"bias_20","op":"between","value":-3,"value2":2},{"factor":"close","op":">=","ref":"ma20"}]},{"factor":"rsi_14","op":"<","value":30}]},"unmatched":[],"explain":"缩量回踩 20 日线附近且收盘不破，或 RSI 进入超卖区"}` + "\n\n")
	b.WriteString("输入：市盈率低于 20、北向资金加仓、获利盘低于 15%\n")
	b.WriteString(`输出：{"tree":{"all":[{"factor":"chip_profit","op":"<","value":15}]},"unmatched":["市盈率低于 20（因子库无估值数据）","北向资金加仓（因子库无北向资金数据）"],"explain":"仅获利盘条件可映射；估值与北向资金不在因子库"}` + "\n")

	return b.String()
}
