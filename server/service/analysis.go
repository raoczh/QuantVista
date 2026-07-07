package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// FlexInt 容忍模型把整数字段输出成小数或带引号字符串（如 "confidence": 72.5 / "80"），
// 解析时四舍五入归一为整数，序列化仍输出普通数字（前端契约不变）。
type FlexInt int

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("期望数字，得到 %s", s)
	}
	*f = FlexInt(math.Round(v))
	return nil
}

// AnalysisService AI 分析编排：加载 LLM 配置 → 配额熔断 → 组装数据上下文 →
// 构造 prompt → 调用 LLM（JSON mode 优先）→ 结构化校验 + 有限次 repair → 优雅降级 → 落库历史 + 扣配额。
type AnalysisService struct {
	market    *MarketService
	watchlist *WatchlistService
	position  *PositionService
	llm       *LLMService
}

func NewAnalysisService(market *MarketService, watchlist *WatchlistService, position *PositionService, llm *LLMService) *AnalysisService {
	return &AnalysisService{market: market, watchlist: watchlist, position: position, llm: llm}
}

// 版本号：数据快照 + 这两个版本号共同保证「凭版本号复现」。改 prompt/策略时递增。
const (
	analysisPromptVersion   = "p8" // p8: risk_gate 风险闸门段（ST/退市 block、一字板/流动性/小市值提示+未接入数据声明）+ 持仓模块资金上下文与割/守/补三选一；p7: announcements 公告段；p6: news 舆情段；p5: 证据数字程序化核验威慑条款；p4: 五维量化评分锚点+强制引用数值/禁先验记忆；p3: 反方观点/失效条件/数据盲区
	analysisStrategyVersion = "s1"
	maxRepairAttempts       = 2 // 结构化校验失败后的额外重试次数（总调用 = 1 + maxRepairAttempts）
)

var validAnalysisModule = map[string]bool{
	model.AnalysisModuleMarket:    true,
	model.AnalysisModuleSector:    true,
	model.AnalysisModuleStock:     true,
	model.AnalysisModuleWatchlist: true,
	model.AnalysisModulePosition:  true,
}

// AnalyzeRequest 发起分析入参。
type AnalyzeRequest struct {
	Module      string `json:"module"`
	Market      string `json:"market"`
	Symbol      string `json:"symbol"`        // 个股模块必填
	Target      string `json:"target"`        // 板块名 / 自由标签（可选）
	LLMConfigID int64  `json:"llm_config_id"` // 0 = 用默认配置
	Question    string `json:"question"`      // 用户附加问题（可选）
	Mode        string `json:"mode"`          // ""=标准 / "panel"=多角色观点（仅 stock 模块）
	Verify      bool   `json:"verify"`        // AI 复核：额外一次「独立复核员」调用逐项挑刺（多耗一次 LLM 请求；panel/降级不复核）
}

// AnalysisResult 结构化分析结果（要求 LLM 严格按此 schema 输出 JSON）。
type AnalysisResult struct {
	Rating        string   `json:"rating"`        // bullish / neutral / bearish
	Confidence    FlexInt  `json:"confidence"`    // 0-100（容忍模型输出小数/字符串）
	Summary       string   `json:"summary"`       // 一句话总览
	Highlights    []string `json:"highlights"`    // 关键要点
	Risks         []string `json:"risks"`         // 风险
	Opportunities []string `json:"opportunities"` // 机会
	Suggestions   []string `json:"suggestions"`   // 关注点/操作参考（非投资建议）
	AntiThesis    []string `json:"anti_thesis"`   // 反方观点：为什么这个结论可能是错的
	KillSwitches  []string `json:"kill_switches"` // 结论失效条件：出现什么信号说明判断已不成立
	Unknowns      []string `json:"unknowns"`      // 数据盲区：本次数据看不到、但对结论重要的信息
	Disclaimer    string   `json:"disclaimer"`    // 免责声明

	// --- 服务端回填（信任层，非 LLM 输出）---
	EvidenceCheck    *evidenceCheck  `json:"evidence_check,omitempty"`     // 结论文本引用数字与数据快照的程序化核验
	SysConfidence    string          `json:"sys_confidence,omitempty"`     // 程序合成置信度 high/medium/low
	SysConfidenceWhy string          `json:"sys_confidence_why,omitempty"` // 置信度依据说明
	Review           *analysisReview `json:"review,omitempty"`             // AI 复核员结论（verify 模式）
}

// analysisReview AI 复核员对一份分析的结论。Confidence 用 FlexInt——模型常把整数字段
// 输出成小数或带引号字符串，裸 int 会让整个复核 JSON 反序列化失败、结论被静默丢弃。
type analysisReview struct {
	Verdict    string  `json:"verdict"` // pass / warn / reject
	Comment    string  `json:"comment"`
	Confidence FlexInt `json:"confidence"` // 复核后的建议置信度（0-100；0=不调整）
}

// PanelRole 多角色观点中的单个角色结论。
type PanelRole struct {
	Role    string `json:"role"`    // technical / momentum / risk / contrarian
	Rating  string `json:"rating"`  // bullish / neutral / bearish
	Summary string `json:"summary"` // 该角色核心观点
}

// PanelResult 多角色观点结果（mode=panel 时落库 result_json.panel）。
type PanelResult struct {
	Roles        []PanelRole `json:"roles"`
	Consensus    string      `json:"consensus"`    // 四角色共识
	Disagreement string      `json:"disagreement"` // 主要分歧与取决条件
}

// AnalysisView 返回给前端：记录 + 解析后的结构化结果。
type AnalysisView struct {
	model.AnalysisRecord
	Result    *AnalysisResult `json:"result"`               // 结构化结果；degraded/failed 时可能为 nil
	Panel     *PanelResult    `json:"panel"`                // 多角色观点（mode=panel 且 success 时非 nil）
	Raw       string          `json:"raw"`                  // 降级时的模型原文
	RiskFlags []riskFlag      `json:"risk_flags,omitempty"` // 快照 risk_gate 程序化风险标志（S1，个股模块）
}

// Analyze 执行一次分析。allowPrivate 由调用方按角色决定（管理员可访问内网自建模型）。
func (s *AnalysisService) Analyze(ctx context.Context, userID int64, allowPrivate bool, req AnalyzeRequest) (*AnalysisView, error) {
	req.Module = strings.ToLower(strings.TrimSpace(req.Module))
	if !validAnalysisModule[req.Module] {
		return nil, errors.New("不支持的分析模块")
	}
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.Mode != model.AnalysisModeStandard && req.Mode != model.AnalysisModePanel {
		return nil, errors.New("不支持的分析模式")
	}
	if req.Mode == model.AnalysisModePanel && req.Module != model.AnalysisModuleStock {
		return nil, errors.New("多角色观点仅支持个股模块")
	}
	if len([]rune(req.Question)) > 500 {
		return nil, errors.New("附加问题过长（最多 500 字）")
	}

	// 1) LLM 配置（含解密密钥）。
	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, err
	}

	// 2) 配额熔断：次数额度用尽直接拒绝（不发起调用）。
	if err := checkQuota(userID); err != nil {
		return nil, err
	}

	// 3) 组装数据上下文（失败即返回，不产生空记录）。
	actx, err := s.buildContext(ctx, userID, req)
	if err != nil {
		return nil, err
	}
	snapshotJSON, _ := json.Marshal(actx.Snapshot)

	// 4) 构造消息并调用 + 结构化校验/repair。
	messages := s.buildMessages(userID, req, actx, string(snapshotJSON))
	promptVersion := analysisPromptVersion
	if req.Mode == model.AnalysisModeStandard && userPromptOverride(userID, req.Module) != "" {
		// 标记本次使用了用户自定义模板——否则历史记录声称 p1 却无法按 p1 复现。
		// （模板内容可被用户随后编辑，完整快照留待 ai_call_logs，当前仅标记。）
		promptVersion = analysisPromptVersion + "-custom"
	}
	rec := &model.AnalysisRecord{
		UserID:          userID,
		Module:          req.Module,
		Mode:            req.Mode,
		Market:          normalizeMarketOnly(req.Market),
		Symbol:          strings.TrimSpace(req.Symbol),
		Target:          actx.Label,
		Title:           s.title(req.Module, req.Mode, actx.Label),
		LLMConfigID:     cfg.ID,
		Provider:        cfg.Provider,
		Model:           cfg.Model,
		PromptVersion:   promptVersion,
		StrategyVersion: analysisStrategyVersion,
		DataSnapshot:    string(snapshotJSON),
	}

	// 解析器按模式分流：标准 → AnalysisResult；panel → PanelResult。
	var result *AnalysisResult
	var panel *PanelResult
	parse := func(content string) error {
		if req.Mode == model.AnalysisModePanel {
			p, perr := parsePanelResult(content)
			if perr != nil {
				return perr
			}
			panel = p
			return nil
		}
		r, perr := parseAnalysisResult(content)
		if perr != nil {
			return perr
		}
		result = r
		return nil
	}
	repairHint := analysisRepairHint
	if req.Mode == model.AnalysisModePanel {
		repairHint = panelRepairHint
	}

	raw, usage, latency, callErr := s.callWithRepair(ctx, cfg, apiKey, allowPrivate, messages, parse, repairHint)

	// 信任层：仅当成功解析出标准结构化结果时做证据核验 + 程序合成置信度 + 可选 AI 复核
	// （panel 无标准结论字段、degraded 无合法结构，都不做）。复核在配额记账前完成，
	// 其 token 折入本次动作一起累计与扣费（一次分析动作仍只计 1 次配额）。
	if callErr == nil && result != nil {
		rvUsage := s.fillAnalysisTrust(ctx, cfg, apiKey, allowPrivate, req, actx.Snapshot, result)
		usage.PromptTokens += rvUsage.PromptTokens
		usage.CompletionTokens += rvUsage.CompletionTokens
		usage.TotalTokens += rvUsage.TotalTokens
	}

	rec.PromptTokens = usage.PromptTokens
	rec.CompletionTokens = usage.CompletionTokens
	rec.TotalTokens = usage.TotalTokens
	rec.LatencyMs = latency

	// 消耗了 token 就记账（无论成功/降级）；一次分析动作计 1 次配额。
	if usage.TotalTokens > 0 {
		consumeQuota(userID, usage.TotalTokens, true)
	}

	switch {
	case callErr != nil:
		// 调用彻底失败：无有效结果，记 failed（不写脏结构化数据）。
		rec.Status = model.AnalysisStatusFailed
		rec.Error = truncateRunes(callErr.Error(), 500)
		rec.Summary = "分析失败"
		if err := common.DB.Create(rec).Error; err != nil {
			return nil, err
		}
		return nil, errors.New(callErr.Error())
	case result != nil:
		rec.Status = model.AnalysisStatusSuccess
		rec.Rating = result.Rating
		rec.Confidence = int(result.Confidence)
		rec.Summary = truncateRunes(result.Summary, 1024)
		rec.Title = truncateRunes(rec.Title+" · "+ratingCN(rec.Rating), 128)
		b, _ := json.Marshal(result)
		rec.ResultJSON = string(b)
	case panel != nil:
		// panel：rating 取多数投票（平票中性），summary 存共识。
		rec.Status = model.AnalysisStatusSuccess
		rec.Rating = panelMajorityRating(panel.Roles)
		rec.Summary = truncateRunes(panel.Consensus, 1024)
		rec.Title = truncateRunes(rec.Title+" · "+ratingCN(rec.Rating), 128)
		b, _ := json.Marshal(map[string]any{"panel": panel})
		rec.ResultJSON = string(b)
	default:
		// 有原文但结构化校验始终不过：优雅降级，保留原文供人看，不伪造结构化字段。
		rec.Status = model.AnalysisStatusDegraded
		rec.Rating = model.AnalysisRatingNeutral
		rec.Summary = truncateRunes(firstLine(raw), 1024)
		rec.Error = "结构化输出校验失败，已降级为原文展示"
		rec.ResultJSON = raw // 存原文（非合法 schema）
	}

	if err := common.DB.Create(rec).Error; err != nil {
		return nil, err
	}
	return s.toView(*rec), nil
}

// analysisRepairHint / panelRepairHint 结构化校验失败后的修复指令。
const analysisRepairHint = "请只输出一个合法 JSON 对象，严格包含字段 rating(bullish/neutral/bearish)、confidence(0-100 整数)、summary、highlights、risks、opportunities、suggestions、anti_thesis、kill_switches、unknowns、disclaimer，不要任何解释或代码块标记。"

const panelRepairHint = "请只输出一个合法 JSON 对象，严格包含字段 roles（恰好 4 个元素的数组，每个元素含 role(technical/momentum/risk/contrarian)、rating(bullish/neutral/bearish)、summary）、consensus、disagreement，不要任何解释或代码块标记。"

// callWithRepair 调用 LLM 并在结构化校验失败时有限次 repair（parse 由调用方按模式提供）。
// 累计各次调用的 token。返回：最后一次原文、累计 usage、最后一次延迟、调用错误（网络/鉴权等，非校验失败）。
// parse 返回 nil 即视为成功（解析结果由闭包带出）。
func (s *AnalysisService) callWithRepair(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, messages []chatMessage, parse func(string) error, repairHint string) (string, chatUsage, int64, error) {
	var acc chatUsage
	var lastRaw string
	var lastLatency int64

	convo := append([]chatMessage(nil), messages...)
	for attempt := 0; attempt <= maxRepairAttempts; attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL:      cfg.BaseURL,
			APIKey:       apiKey,
			Model:        cfg.Model,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
			Messages:     convo,
			JSONMode:     true,
			AllowPrivate: allowPrivate,
		})
		if err != nil {
			// 网络/鉴权/服务端错误：若已有过成功响应文本则不再重试，直接返回错误。
			return lastRaw, acc, lastLatency, err
		}
		acc.PromptTokens += res.Usage.PromptTokens
		acc.CompletionTokens += res.Usage.CompletionTokens
		acc.TotalTokens += res.Usage.TotalTokens
		lastRaw = res.Content
		lastLatency = res.LatencyMs

		perr := parse(res.Content)
		if perr == nil {
			return lastRaw, acc, lastLatency, nil
		}
		// 校验失败：追加 repair 指令再试。
		convo = append(convo,
			chatMessage{Role: "assistant", Content: res.Content},
			chatMessage{Role: "user", Content: "上一条输出不符合要求：" + perr.Error() + "。" + repairHint},
		)
	}
	return lastRaw, acc, lastLatency, nil // 降级
}

// fillAnalysisTrust 回填一份成功分析的信任层字段（就地修改 result），返回 AI 复核消耗的 token。
// 证据核验值域排除 recent_bars（30 根 OHLCV 密度过大，纳入后几乎任何数字都能撞上容差）。
// unknowns 是「缺什么数据」的陈述、disclaimer 是套话，均不参与核验。
func (s *AnalysisService) fillAnalysisTrust(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, req AnalyzeRequest, snapshot map[string]any, result *AnalysisResult) chatUsage {
	texts := make([]string, 0, 8)
	texts = append(texts, result.Summary)
	texts = append(texts, result.Highlights...)
	texts = append(texts, result.Risks...)
	texts = append(texts, result.Opportunities...)
	texts = append(texts, result.Suggestions...)
	texts = append(texts, result.AntiThesis...)
	texts = append(texts, result.KillSwitches...)
	vals := snapshotValueSet(snapshot, "recent_bars")
	// 新闻标题是喂给模型的文本型合法来源（N2 舆情段）：标题里的小数并入值域，
	// 忠实引用不算幻觉（同日报 Alerts 前例）。公告标题（F1）与风险闸门提示文本
	//（S1，含 9.5/3000 等阈值数字）同理。
	vals = append(vals, decimalNumbersIn(newsTitleTexts(snapshot))...)
	vals = append(vals, decimalNumbersIn(announcementTitleTexts(snapshot))...)
	vals = append(vals, decimalNumbersIn(riskGateTexts(snapshot))...)
	result.EvidenceCheck = verifyEvidenceValues(texts, vals)
	result.SysConfidence, result.SysConfidenceWhy = analysisSystemConfidence(result.EvidenceCheck, snapshot)

	if !req.Verify {
		return chatUsage{}
	}
	review, usage := s.reviewAnalysis(ctx, cfg, apiKey, allowPrivate, snapshot, result)
	if review != nil {
		result.Review = review
		// reject 级联：程序置信度强制压低，与「AI 复核员否决」徽章保持一致，不并排展示高置信度。
		if review.Verdict == "reject" {
			result.SysConfidence = "low"
			if result.SysConfidenceWhy != "" {
				result.SysConfidenceWhy += "；AI 复核员否决"
			} else {
				result.SysConfidenceWhy = "AI 复核员否决"
			}
		}
		if review.Confidence > 0 {
			result.Confidence = review.Confidence
		}
	}
	return usage
}

// analysisSystemConfidence 程序合成置信度（high/medium/low）：由证据核验吻合率、
// 快照数据完备度与新鲜度合成，与 LLM 口头 confidence 并排展示而非替换。
// 起始「中」；证据高吻合 +1、低吻合 -1；个股快照缺技术因子/量化评分 -1；数据偏旧 -1。
func analysisSystemConfidence(ev *evidenceCheck, snapshot map[string]any) (string, string) {
	var reasons []string
	level := 1 // 0=低 1=中 2=高

	if ev != nil && ev.Total > 0 {
		ratio := float64(ev.Matched) / float64(ev.Total)
		switch {
		case ratio >= 0.7:
			level++
			reasons = append(reasons, fmt.Sprintf("证据核验 %d/%d 吻合", ev.Matched, ev.Total))
		case ratio < 0.4:
			level--
			reasons = append(reasons, fmt.Sprintf("证据核验仅 %d/%d 吻合", ev.Matched, ev.Total))
		default:
			reasons = append(reasons, fmt.Sprintf("证据核验 %d/%d 吻合", ev.Matched, ev.Total))
		}
	}
	// 个股快照（含 quote 块）才有技术因子/量化评分锚点；缺失则定性判断精度受限。
	// 全市场/板块/自选/持仓快照本就没有这些键，不属缺失，不扣分。
	if _, isStock := snapshot["quote"]; isStock {
		_, hasTech := snapshot["technicals"]
		_, hasQuant := snapshot["quant_score"]
		if !hasTech || !hasQuant {
			level--
			reasons = append(reasons, "技术指标或量化评分缺失，个股判断精度受限")
		}
		// N2 舆情段数据完备度：有相关新闻是加分信息（不升档——覆盖面有限），
		// 无新闻属常态不扣分，只在依据里如实说明。
		if n := len(newsTitleTexts(snapshot)); n > 0 {
			reasons = append(reasons, fmt.Sprintf("含 %d 条相关新闻舆情", n))
		}
	}
	if _, ok := snapshot["freshness_note"]; ok {
		level--
		reasons = append(reasons, "行情快照偏旧，非实时盘面")
	}

	if level < 0 {
		level = 0
	}
	if level > 2 {
		level = 2
	}
	labels := [3]string{"low", "medium", "high"}
	return labels[level], strings.Join(reasons, "；")
}

// reviewAnalysis AI 复核（verify 模式）：以独立复核员视角对照数据快照审查一份分析，
// 只挑刺不重写，输出 pass/warn/reject 与建议置信度。best-effort：失败只是没有复核结论，
// 不影响主结果。1 次 repair。
func (s *AnalysisService) reviewAnalysis(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, snapshot map[string]any, result *AnalysisResult) (*analysisReview, chatUsage) {
	var usage chatUsage
	snapJSON, _ := json.Marshal(snapshot)
	resJSON, _ := json.Marshal(result)

	sys := `你是一名独立的分析复核员，对照数据快照审查另一位研究员给出的分析。你不重新分析，只挑刺。审查维度：
1. 数字一致：分析中引用的数字是否与数据快照一致，有没有夸大、张冠李戴或凭空捏造；
2. 风险完整：主要风险是否说够，有没有被淡化或遗漏；
3. 评级自洽：rating（偏多/中性/偏空）与快照里的数据是否自洽，有没有明显相悖；
4. 置信校准：结论的置信度是否过度自信。
给出 verdict：pass（无实质问题）/ warn（有值得注意的问题但不至于否决）/ reject（数字与数据明显不符或风险被严重低估，应降级）。
只输出 JSON：{"verdict":"pass|warn|reject","comment":"一两句中文说明","confidence":0-100 的复核后建议置信度整数}，不要任何解释或代码块标记。confidence 给具体数值（reject 时给你认为的真实低值）；填 0 表示维持原置信度不调整。`

	convo := []chatMessage{
		{Role: "system", Content: sys},
		// 快照必须与主分析同源同量（buildMessages 注入的就是完整 snapJSON）：
		// 若在此截断，位于被截尾部的真实数字在复核员视角会变成「凭空捏造」，
		// 触发错误 reject 并级联压低置信度——与程序化核验徽章自相矛盾。
		{Role: "user", Content: "【数据快照】（JSON）：\n" + string(snapJSON) +
			"\n\n【待复核的分析结果】（JSON）：\n" + string(resJSON)},
	}
	for attempt := 0; attempt <= 1; attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model,
			Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
		})
		if err != nil {
			return nil, usage
		}
		usage.PromptTokens += res.Usage.PromptTokens
		usage.CompletionTokens += res.Usage.CompletionTokens
		usage.TotalTokens += res.Usage.TotalTokens

		var rv analysisReview
		if json.Unmarshal([]byte(extractJSONObject(res.Content)), &rv) == nil {
			v := strings.ToLower(strings.TrimSpace(rv.Verdict))
			if v == "pass" || v == "warn" || v == "reject" {
				rv.Verdict = v
				if rv.Confidence < 0 {
					rv.Confidence = 0
				}
				if rv.Confidence > 100 {
					rv.Confidence = 100
				}
				rv.Comment = truncateRunes(strings.TrimSpace(rv.Comment), 300)
				return &rv, usage
			}
		}
		convo = append(convo,
			chatMessage{Role: "assistant", Content: res.Content},
			chatMessage{Role: "user", Content: `上一条输出不合格。请只输出 JSON：{"verdict":"pass|warn|reject","comment":"...","confidence":0-100 整数}。`},
		)
	}
	return nil, usage
}

// History 列出分析历史（不返回重字段 result_json/data_snapshot）。
func (s *AnalysisService) History(userID int64, module string, limit int) ([]model.AnalysisRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	q := common.DB.Where("user_id = ?", userID)
	if module != "" && module != "all" {
		if !validAnalysisModule[module] {
			return nil, errors.New("非法的模块筛选")
		}
		q = q.Where("module = ?", module)
	}
	var rows []model.AnalysisRecord
	// 显式选列，避免把大字段 result_json/data_snapshot 拉进列表。
	err := q.Select("id", "user_id", "module", "market", "symbol", "target", "title",
		"status", "mode", "rating", "confidence", "summary", "error",
		"llm_config_id", "provider", "model", "prompt_version", "strategy_version",
		"prompt_tokens", "completion_tokens", "total_tokens", "latency_ms",
		"created_at", "updated_at").
		Order("id DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Get 取单条分析详情（含结构化结果与数据快照，供复现）。仅本人。
func (s *AnalysisService) Get(userID, id int64) (*AnalysisView, error) {
	var rec model.AnalysisRecord
	err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("分析记录不存在")
	}
	if err != nil {
		return nil, err
	}
	return s.toView(rec), nil
}

// Delete 删除分析记录（仅本人）。
func (s *AnalysisService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.AnalysisRecord{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("分析记录不存在")
	}
	return nil
}

// AnalysisDiff 与上一份同对象成功分析的差异（变化检测）。
type AnalysisDiff struct {
	PrevID            int64     `json:"prev_id"`
	PrevAt            time.Time `json:"prev_at"`
	PrevTitle         string    `json:"prev_title"`
	RatingFrom        string    `json:"rating_from"`
	RatingTo          string    `json:"rating_to"`
	ConfidenceFrom    int       `json:"confidence_from"`
	ConfidenceTo      int       `json:"confidence_to"`
	ConfidenceDelta   int       `json:"confidence_delta"`
	SummaryPrev       string    `json:"summary_prev"`
	SummaryNow        string    `json:"summary_now"`
	HighlightsAdded   []string  `json:"highlights_added"`   // 本次新增的要点
	HighlightsRemoved []string  `json:"highlights_removed"` // 上次有、本次消失的要点
	RisksAdded        []string  `json:"risks_added"`
	RisksRemoved      []string  `json:"risks_removed"`
}

// Diff 变化检测：找同 user+module+market+symbol（sector 模块再限定同 target）的上一份
// 成功分析，对比 rating/confidence/summary/highlights/risks。多角色（panel）记录无标准
// 字段，不参与对比（两侧都限定标准模式）。
func (s *AnalysisService) Diff(userID, id int64) (*AnalysisDiff, error) {
	var cur model.AnalysisRecord
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&cur).Error; err != nil {
		return nil, errors.New("分析记录不存在")
	}
	if cur.Status != model.AnalysisStatusSuccess {
		return nil, errors.New("仅成功的分析可对比")
	}
	if cur.Mode == model.AnalysisModePanel {
		return nil, errors.New("多角色观点暂不支持对比")
	}

	q := common.DB.Where(
		"user_id = ? AND module = ? AND market = ? AND symbol = ? AND status = ? AND mode = ? AND id < ?",
		userID, cur.Module, cur.Market, cur.Symbol, model.AnalysisStatusSuccess, model.AnalysisModeStandard, cur.ID,
	)
	if cur.Module == model.AnalysisModuleSector {
		q = q.Where("target = ?", cur.Target)
	}
	var prev model.AnalysisRecord
	if err := q.Order("id DESC").First(&prev).Error; err != nil {
		return nil, errors.New("没有更早的同对象成功分析可对比")
	}

	var curRes, prevRes AnalysisResult
	_ = json.Unmarshal([]byte(cur.ResultJSON), &curRes)
	_ = json.Unmarshal([]byte(prev.ResultJSON), &prevRes)

	d := &AnalysisDiff{
		PrevID:          prev.ID,
		PrevAt:          prev.CreatedAt,
		PrevTitle:       prev.Title,
		RatingFrom:      prev.Rating,
		RatingTo:        cur.Rating,
		ConfidenceFrom:  prev.Confidence,
		ConfidenceTo:    cur.Confidence,
		ConfidenceDelta: cur.Confidence - prev.Confidence,
		SummaryPrev:     prev.Summary,
		SummaryNow:      cur.Summary,
	}
	d.HighlightsAdded, d.HighlightsRemoved = diffStringSets(prevRes.Highlights, curRes.Highlights)
	d.RisksAdded, d.RisksRemoved = diffStringSets(prevRes.Risks, curRes.Risks)
	return d, nil
}

// diffStringSets 返回（cur 相对 prev 新增的、prev 有而 cur 没有的），按原顺序，精确匹配。
func diffStringSets(prev, cur []string) (added, removed []string) {
	added, removed = []string{}, []string{}
	prevSet := make(map[string]bool, len(prev))
	for _, s := range prev {
		prevSet[strings.TrimSpace(s)] = true
	}
	curSet := make(map[string]bool, len(cur))
	for _, s := range cur {
		curSet[strings.TrimSpace(s)] = true
	}
	for _, s := range cur {
		if !prevSet[strings.TrimSpace(s)] {
			added = append(added, s)
		}
	}
	for _, s := range prev {
		if !curSet[strings.TrimSpace(s)] {
			removed = append(removed, s)
		}
	}
	return added, removed
}

// toView 把落库记录还原为前端视图（解析结构化结果 / 多角色结果 / 降级原文）。
func (s *AnalysisService) toView(rec model.AnalysisRecord) *AnalysisView {
	v := &AnalysisView{AnalysisRecord: rec}
	if rec.Module == model.AnalysisModuleStock {
		v.RiskFlags = parseRiskFlagsFromSnapshot(rec.DataSnapshot)
	}
	switch rec.Status {
	case model.AnalysisStatusSuccess:
		if rec.Mode == model.AnalysisModePanel {
			var wrap struct {
				Panel *PanelResult `json:"panel"`
			}
			if json.Unmarshal([]byte(rec.ResultJSON), &wrap) == nil {
				v.Panel = wrap.Panel
			}
			return v
		}
		var r AnalysisResult
		if json.Unmarshal([]byte(rec.ResultJSON), &r) == nil {
			v.Result = &r
		}
	case model.AnalysisStatusDegraded:
		v.Raw = rec.ResultJSON
	}
	return v
}

func (s *AnalysisService) title(module, mode, label string) string {
	names := map[string]string{
		model.AnalysisModuleMarket:    "全市场分析",
		model.AnalysisModuleSector:    "板块分析",
		model.AnalysisModuleStock:     "个股分析",
		model.AnalysisModuleWatchlist: "自选股分析",
		model.AnalysisModulePosition:  "持仓分析",
	}
	base := names[module]
	if mode == model.AnalysisModePanel {
		base = "个股多角色观点"
	}
	// 自选/持仓的 label 恒为「自选股」「持仓」，与模块名语义重复（曾拼出「自选股分析 · 自选股」）。
	if label != "" && module != model.AnalysisModuleMarket &&
		module != model.AnalysisModuleWatchlist && module != model.AnalysisModulePosition {
		return base + " · " + label
	}
	return base
}

// ratingCN 评级中文标签（成功后拼进标题，历史列表可区分同对象的多次分析结论）。
func ratingCN(rating string) string {
	switch rating {
	case model.AnalysisRatingBullish:
		return "偏多"
	case model.AnalysisRatingBearish:
		return "偏空"
	default:
		return "中性"
	}
}

// --- prompt 构造与结果校验 ---

// buildMessages 组装系统提示 + 用户消息（含数据快照 JSON）。系统提示按模块定制，且尊重用户自定义模板；
// panel 模式使用专属多角色系统提示（不套用用户模板——panel 是固定编排）。
func (s *AnalysisService) buildMessages(userID int64, req AnalyzeRequest, actx *analysisContext, snapshotJSON string) []chatMessage {
	var b strings.Builder
	fmt.Fprintf(&b, "请对以下【%s】进行分析。\n\n", s.title(req.Module, req.Mode, actx.Label))
	if q := strings.TrimSpace(req.Question); q != "" {
		fmt.Fprintf(&b, "用户特别关注的问题（请在分析中优先回应）：%s\n\n", q)
	}
	b.WriteString("【数据】（JSON，数值为近似值，价格为货币单位，金额单位为元）：\n")
	b.WriteString(snapshotJSON)
	b.WriteString("\n\n数据时间：以快照内 data_as_of 为采集时刻、各字段 data_time/trade_date（如有）为准；" +
		"非交易时段采集的数据反映最近一个交易日，不代表实时状态，分析措辞须体现这一点。")
	b.WriteString("\n请严格按系统要求的 JSON schema 输出，只依据以上数据分析。")

	sysPrompt := analysisSystemPrompt(userID, req.Module)
	if req.Mode == model.AnalysisModePanel {
		sysPrompt = analysisRoleIntro + "\n\n" + panelGuidance + "\n\n" + panelOutputSpec
	}
	return []chatMessage{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: b.String()},
	}
}

// analysisSystemPrompt 按模块拼接系统提示：通用身份 + 模块专属分析维度 + 通用输出/合规约束。
// 若用户为该模块启用了自定义模板，则用其替换默认分析维度指引。
func analysisSystemPrompt(userID int64, module string) string {
	guidance := moduleGuidance[module]
	if custom := userPromptOverride(userID, module); custom != "" {
		guidance = custom
	}
	return analysisRoleIntro + "\n\n" + guidance + "\n\n" + analysisOutputSpec
}

// analysisRoleIntro 通用身份与总纲。
const analysisRoleIntro = `你是一名严谨的证券研究助理，服务于个人投资研究工具。你的输出仅供研究参考，不构成任何投资建议或买卖指令。

总则：
1. 只依据【数据】中提供的事实分析，严禁编造未给出的数据、财务报表、消息、评级或价格。数据不足时必须明确指出局限，不得臆测。
2. 禁止使用你记忆中关于具体公司/板块的信息（名气、行业地位、历史印象都不算本次数据）；对知名标的尤其要克制，只看本次给出的数字。
3. 关键判断必须引用【数据】中的具体字段与数值佐证（如「现价 12.34 站上 MA20=11.98」「主力净流入 23.5 亿」），让用户可以逐条核对。系统会程序化核对你引用的数字，与数据快照不符的会被标记出来展示给用户，因此严禁编造或凭印象填写数字。
4. 保持客观中立，机会与风险两面都要讲，不迎合用户、不夸大收益。
5. 全程使用简体中文。`

// moduleGuidance 各模块专属的分析维度与数据说明——必须与该模块实际注入的数据严格对应。
var moduleGuidance = map[string]string{
	model.AnalysisModuleStock: `本次分析对象是【单只个股】。可用数据：实时行情快照（现价、涨跌幅、开高低、昨收、成交额）、估值与盘面快照 valuation（PE-TTM/动态 PE、市净率、总市值/流通市值、换手率、振幅、量比、涨停/跌停价；该块缺失时表示估值数据暂不可得）、技术指标（MA5/MA10/MA20、区间最高/最低、近 5 日与近 20 日涨跌幅、日线根数）、五维量化评分 quant_score（趋势/动量/位置/量能/风险加权 0-100，与本站个股评分页同口径）以及近期逐日 K 线明细。
请从以下维度展开：
- 趋势方向：结合现价与 MA5/MA10/MA20 的位置关系判断多空排列（多头/空头/纠缠）；
- 量价配合：结合近期成交量、换手率与量比判断是否放量/缩量、量价背离；
- 位置与空间：现价相对区间高低点的位置，可能的支撑位与压力位；
- 短中期动能：近 5 日、近 20 日涨跌幅反映的动能强弱与超买超卖倾向；
- 估值水位：若提供了 valuation，结合 PE/PB/市值说明估值的绝对水位与含义（无行业对比数据，须说明这是绝对水位而非行业相对水位；PE 为负表示亏损）；标注 is_st 的标的必须提示退市/风险警示；
- 量化锚点：quant_score 是确定性算出的技术面综合分，你的定性判断若与其明显相悖（如评分 30 弱势而你看多），必须专门解释分歧原因，不可无视。
- 消息面（若快照含 news 块）：news.items 是该股最近相关新闻的标题与情绪标签（利好/利空/中性为程序化预判），结合技术面判断消息驱动的持续性；权重纪律：公告>政策>报道>传闻，旧闻与已充分定价的消息不加分；引用新闻只能复述给出的标题，不得展开臆测正文细节。若 news 标注「暂无直接相关新闻」，请依据 market_signals（涨跌五档/量能三档/换手率）判断，并在措辞中明示消息面数据缺失。
- 公告（若快照含 announcements 块）：announcements.items 是该股最近的交易所公告（标题/类型/日期），证据权重高于新闻报道；关注业绩类、股权变动类、重大合同类公告对结论的影响；引用公告只能复述给出的标题与类型，不得臆测公告正文细节；无 announcements 块表示暂未采集到该股公告，不代表没有公告。
- 风险闸门（快照 risk_gate 块，程序化前置判定，必须遵守）：flags 中 level=block 的条目（ST/退市风险警示）为硬约束——rating 不得为 bullish、不得给出任何买入倾向的表述，并把该风险放在 risks 首条；level=warn（一字板/流动性不足）必须在 risks 中原样提示并约束相关结论（一字板不得按可正常成交分析）；level=info（小市值）在风险中带一句提示。risk_gate.note 声明了未接入的数据维度（质押/解禁等），涉及时照实说「未接入数据，请自行核查」，严禁装作已核查。
重要限制：估值仅为快照指标，不含财务三表明细（营收/利润/负债）、机构持仓与个股资金流。news 块的覆盖面有限（快讯与个股新闻采集），没有新闻不代表没有消息。若结论依赖未提供的数据，必须说明「数据缺失、无法判断」，绝不虚构。若快照带 freshness_note，措辞必须体现数据非实时。rating 以技术面为主、估值水位与消息面为辅给出。
反方视角（必填）：anti_thesis 针对你给出的 rating 论证相反情形（看多时论证为什么现在买入可能是错的，看空/中性亦然）；kill_switches 给出可观察的失效信号（价格/均线/量能等具体条件）；unknowns 列出财务、新闻、资金流等本次数据看不到但影响结论的盲区。`,

	model.AnalysisModuleMarket: `本次分析对象是【整个市场】。可用数据：主要指数行情、涨跌家数与涨停/跌停数（市场情绪核心）、两市资金流（主力/超大/大/中/小单净流入）、涨幅榜、成交活跃榜、板块涨跌榜。
请从以下维度研判：
- 市场强弱：主要指数涨跌与相互印证（大盘 vs 创业板/科创，反映风格）；
- 赚钱效应：用涨跌家数对比、涨停/跌停数量判断市场情绪冷热与普涨/普跌/分化；
- 资金动向：主力与超大单净流入方向及规模，判断增量资金意愿；
- 领涨方向：涨幅榜、活跃榜、领涨板块反映的市场热点与风格切换。
若某些数据块标注为不可用(unavailable)，请指出该维度暂缺、结论相应保留。
反方视角（必填）：anti_thesis 论证与你主结论相反的市场情形（如判断偏暖时论证情绪可能只是脉冲）；kill_switches 用指数关键位、涨跌家数逆转、资金流转向等可观察信号描述结论何时失效；unknowns 指出宏观政策、外盘、增量资金来源等数据盲区。`,

	model.AnalysisModuleSector: `本次分析对象是【板块轮动】。可用数据：板块涨跌榜（含领涨股）、主要指数、市场涨跌家数情绪。
请从以下维度分析：
- 板块强弱排序：当日领涨与领跌板块，强弱分化程度；
- 轮动方向：结合板块涨跌与领涨股，判断资金在向哪些方向切换；
- 相对大盘：强势板块相对指数的超额表现；
- 持续性提示：仅凭单日数据能说到什么程度要如实说明，不夸大趋势延续性。
若用户指定了关注板块(focus)，请重点围绕它、并与榜单上其他板块横向对比；若榜单中没有该板块数据，请明确指出无法定位并只做大盘层面的板块结构分析。
反方视角（必填）：anti_thesis 论证当日强势板块为何可能只是一日游、轮动判断可能错在哪；kill_switches 给出板块相对大盘走弱等失效信号；unknowns 指出板块内个股资金明细、板块基本面与消息面等盲区。`,

	model.AnalysisModuleWatchlist: `本次分析对象是【用户的自选股清单】。数据：每个标的的名称、代码、所属分组、是否重点关注、实时现价与涨跌幅，以及用户填写的关注原因与备注。
请从以下维度分析：
- 整体表现：这批自选当日的涨跌分布与共性（是否集中在某类风格/板块）；
- 重点标的：结合 is_pinned 与关注原因，指出当前最值得留意的标的及理由；
- 风险提示：指出其中走弱或需要警惕的标的；
- 结合用户视角：呼应用户填写的关注原因给出研究性看法。
注意：数据仅含行情，不含个股财务与新闻，不要虚构基本面；无现价(缺 price)的标的说明数据缺失。
反方视角（必填）：anti_thesis 指出对这批自选的整体判断可能错在哪（如共性走强可能只是板块 beta）；kill_switches 给出应移出关注或警惕的信号；unknowns 指出个股基本面、消息面等盲区。`,

	model.AnalysisModulePosition: `本次分析对象是【用户的实际持仓】。数据：每笔持仓的标的、类型(短线/长线)、状态(持仓中/已卖出)、买入价、数量、成本、现价、盈亏额与收益率、买入理由，以及持仓中汇总盈亏；若含 capital_context 块，则 total_capital 为用户设定的总投资资金、holding_ratio_pct 为当前仓位占比。
请从以下维度分析：
- 组合整体：汇总盈亏与收益率，当前组合的盈亏结构（盈利仓 vs 亏损仓分布）；
- 集中度风险：是否过度集中于单一标的或单一市场/风格；
- 结构评估：短线与长线仓位的比例与各自表现是否符合其定位；
- 需复盘的仓位：结合买入理由与当前盈亏，指出哪些仓位偏离预期、值得重新审视；
- 逐仓倾向（强制三选一）：对每笔持仓中的仓位，在 suggestions 中明确给出「割（认错离场）/守（持有观察）/补（逢低加仓）」三选一的研究倾向及依据，格式如「XX(600000)：守——现价仍在 MA20 上方，亏损 3% 未破买入逻辑」；不允许「可割可守」的骑墙表述，拿不准就选守并说明拿不准的原因。若有 capital_context，判断须结合仓位占比（重仓浮亏与轻仓浮亏的容错不同；补仓建议必须核对剩余资金空间，holding_ratio_pct 已接近 100% 时不得建议补仓）；无 capital_context 则声明「未设置总资金，仓位占比未知」并趋保守。
所有涉及加仓/减仓/止盈止损的表述，一律作为研究参考与风险提示，不是操作指令。不要虚构未提供的成本或价格。
反方视角（必填）：anti_thesis 对你的组合评估给出相反论证（如认为结构合理时，论证它何时会变得脆弱）；kill_switches 给出组合层面的风险信号（回撤、集中度、亏损仓扩大等）；unknowns 指出持仓个股基本面、市场环境等本次数据看不到的盲区。`,
}

// analysisOutputSpec 通用输出格式与合规约束。
const analysisOutputSpec = `输出要求：
只输出一个 JSON 对象，不要任何解释文字，也不要 Markdown 代码块标记。字段：
- rating: 只能是 "bullish"(偏多) / "neutral"(中性) / "bearish"(偏空) 之一
- confidence: 0-100 的整数，表示结论置信度（数据越充分、信号越一致，置信度越高）
- summary: 一句话核心结论（不超过 80 字）
- highlights: 字符串数组，关键要点
- risks: 字符串数组，主要风险
- opportunities: 字符串数组，潜在机会
- suggestions: 字符串数组，需关注的点或研究方向（表述为研究参考，非操作指令）
- anti_thesis: 字符串数组，反方观点——针对你给出的结论论证「为什么它可能是错的」，至少 1 条，须言之有物
- kill_switches: 字符串数组，结论失效条件——出现什么可观察信号说明本次判断已不成立
- unknowns: 字符串数组，数据盲区——本次数据看不到、但对结论重要的信息
- disclaimer: 字符串，风险与免责提示`

// panelGuidance 多角色观点（mode=panel，仅个股模块）：一次调用同时输出四个立场角色的独立结论。
const panelGuidance = `本次任务是【个股多角色观点】：你需要同时扮演四位立场不同的研究员，对同一只个股各自独立给出观点，再总结共识与分歧。可用数据与个股分析相同：实时行情快照、估值与盘面快照 valuation（缺失表示估值数据暂不可得）、技术指标（MA5/MA10/MA20、区间高低、近 5/20 日涨跌幅）、近期日 K 明细，以及 news 舆情段（最近相关新闻标题+情绪标签，标注「暂无直接相关新闻」时按 market_signals 判断）；不含财务三表与个股资金流，任何角色都不得虚构这些数据，引用新闻只能复述给出的标题。

四个角色与视角（各自独立判断，不得互相妥协、允许结论矛盾）：
- technical（技术面研究员）：只看趋势、均线排列、支撑压力与量价配合；
- momentum（动量交易者）：只看短期动能、换手/量比与市场活跃度，偏好强者恒强，反感阴跌；
- risk（风控经理）：专挑风险——波动与回撤、估值透支、流动性、ST/涨跌停约束，宁可错过不可做错；
- contrarian（反方唱空者）：刻意站在主流叙事对立面，论证「为什么现在买入可能是错的」。

consensus 提炼四人都认可的事实与结论；disagreement 点明最大分歧及其取决条件。数据缺失时各角色都须如实说明。`

// panelOutputSpec 多角色输出格式。
const panelOutputSpec = `输出要求：
只输出一个 JSON 对象，不要任何解释文字，也不要 Markdown 代码块标记。字段：
- roles: 数组，恰好 4 个元素，每个元素为 {"role": "technical"|"momentum"|"risk"|"contrarian", "rating": "bullish"|"neutral"|"bearish", "summary": "该角色核心观点（60 字以内）"}
- consensus: 字符串，四个角色的共识结论
- disagreement: 字符串，主要分歧点与取决条件`

var validRating = map[string]bool{
	model.AnalysisRatingBullish: true,
	model.AnalysisRatingNeutral: true,
	model.AnalysisRatingBearish: true,
}

// parseAnalysisResult 从模型输出中解析并校验结构化结果。容忍代码块包裹与中文枚举。
func parseAnalysisResult(content string) (*AnalysisResult, error) {
	jsonStr := extractJSONObject(content)
	if jsonStr == "" {
		return nil, errors.New("未找到 JSON 对象")
	}
	// 信任层字段（evidence_check/sys_confidence/sys_confidence_why/review）只能由服务端
	// 回填（fillAnalysisTrust），不可由被审对象伪造：p5 威慑句会诱导模型自附「复核通过」类
	// 自评字段，若照单全收，未发起复核的默认路径会展示伪造的复核徽章。走 map 中转剥除，
	// 顺带避免模型把这些字段输出成错误类型（如 "review":"pass"）污染整个反序列化触发无谓 repair。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	for _, k := range []string{"evidence_check", "sys_confidence", "sys_confidence_why", "review"} {
		delete(raw, k)
	}
	cleaned, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	var r AnalysisResult
	if err := json.Unmarshal(cleaned, &r); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	r.Rating = normalizeRating(r.Rating)
	if !validRating[r.Rating] {
		return nil, errors.New("rating 取值非法")
	}
	if strings.TrimSpace(r.Summary) == "" {
		return nil, errors.New("summary 不能为空")
	}
	if r.Confidence < 0 {
		r.Confidence = 0
	}
	if r.Confidence > 100 {
		r.Confidence = 100
	}
	// 数组字段兜底为非 nil，前端无需判空。
	r.Highlights = orEmpty(r.Highlights)
	r.Risks = orEmpty(r.Risks)
	r.Opportunities = orEmpty(r.Opportunities)
	r.Suggestions = orEmpty(r.Suggestions)
	r.AntiThesis = orEmpty(r.AntiThesis)
	r.KillSwitches = orEmpty(r.KillSwitches)
	r.Unknowns = orEmpty(r.Unknowns)
	if strings.TrimSpace(r.Disclaimer) == "" {
		r.Disclaimer = "本分析由 AI 基于公开数据生成，仅供研究参考，不构成投资建议，据此操作风险自负。"
	}
	return &r, nil
}

// 多角色观点的合法角色。
var validPanelRole = map[string]bool{
	"technical":  true,
	"momentum":   true,
	"risk":       true,
	"contrarian": true,
}

// parsePanelResult 解析并校验多角色观点输出。角色去重、rating 归一，至少 3 个合法角色才算通过
//（允许模型偶发漏一个角色，不因此整体降级）。
func parsePanelResult(content string) (*PanelResult, error) {
	jsonStr := extractJSONObject(content)
	if jsonStr == "" {
		return nil, errors.New("未找到 JSON 对象")
	}
	var p PanelResult
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	seen := map[string]bool{}
	roles := make([]PanelRole, 0, len(p.Roles))
	for _, r := range p.Roles {
		r.Role = strings.ToLower(strings.TrimSpace(r.Role))
		r.Rating = normalizeRating(r.Rating)
		if !validPanelRole[r.Role] || seen[r.Role] || !validRating[r.Rating] || strings.TrimSpace(r.Summary) == "" {
			continue
		}
		seen[r.Role] = true
		roles = append(roles, r)
	}
	if len(roles) < 3 {
		return nil, fmt.Errorf("合法角色不足（需≥3 个，得到 %d）", len(roles))
	}
	p.Roles = roles
	if strings.TrimSpace(p.Consensus) == "" {
		return nil, errors.New("consensus 不能为空")
	}
	return &p, nil
}

// panelMajorityRating 多角色评级的多数投票（平票取中性）。
func panelMajorityRating(roles []PanelRole) string {
	counts := map[string]int{}
	for _, r := range roles {
		counts[r.Rating]++
	}
	best, bestN, tie := model.AnalysisRatingNeutral, 0, false
	for rating, n := range counts {
		switch {
		case n > bestN:
			best, bestN, tie = rating, n, false
		case n == bestN:
			tie = true
		}
	}
	if tie {
		return model.AnalysisRatingNeutral
	}
	return best
}

// normalizeRating 把常见中文/英文变体归一到枚举。
func normalizeRating(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "bullish", "bull", "positive", "看多", "偏多", "买入", "增持", "乐观":
		return model.AnalysisRatingBullish
	case "bearish", "bear", "negative", "看空", "偏空", "卖出", "减持", "悲观":
		return model.AnalysisRatingBearish
	case "neutral", "hold", "中性", "观望", "持有":
		return model.AnalysisRatingNeutral
	}
	return s
}

// extractJSONObject 从文本里抽出第一个平衡的 JSON 对象（容忍 ```json 包裹与前后噪声）。
// 先尝试围栏内提取（fallback 模式下模型常用围栏包裹），围栏内无合法对象再回退
// 全文扫描——避免「JSON 出现在 ``` 之前」时把正文误吞。
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		inner := s[i+3:]
		if j := strings.Index(inner, "\n"); j >= 0 && strings.HasPrefix(strings.ToLower(strings.TrimSpace(inner[:j])), "json") {
			inner = inner[j+1:]
		}
		if k := strings.Index(inner, "```"); k >= 0 {
			inner = inner[:k]
		}
		if obj := scanBalancedObject(inner); obj != "" && json.Valid([]byte(obj)) {
			return obj
		}
	}
	return scanBalancedObject(s)
}

// scanBalancedObject 返回 s 中第一个花括号平衡的对象子串（字符串字面量内的花括号不计）。
func scanBalancedObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func orEmpty(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
