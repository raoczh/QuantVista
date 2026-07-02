package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

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
	analysisPromptVersion   = "p2" // p2: 个股模块加入估值/盘面快照维度与数据新鲜度标注
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
	Disclaimer    string   `json:"disclaimer"`    // 免责声明
}

// AnalysisView 返回给前端：记录 + 解析后的结构化结果。
type AnalysisView struct {
	model.AnalysisRecord
	Result *AnalysisResult `json:"result"` // 结构化结果；degraded/failed 时可能为 nil
	Raw    string          `json:"raw"`    // 降级时的模型原文
}

// Analyze 执行一次分析。allowPrivate 由调用方按角色决定（管理员可访问内网自建模型）。
func (s *AnalysisService) Analyze(ctx context.Context, userID int64, allowPrivate bool, req AnalyzeRequest) (*AnalysisView, error) {
	req.Module = strings.ToLower(strings.TrimSpace(req.Module))
	if !validAnalysisModule[req.Module] {
		return nil, errors.New("不支持的分析模块")
	}
	if len([]rune(req.Question)) > 500 {
		return nil, errors.New("附加问题过长（最多 500 字）")
	}

	// 1) LLM 配置（含解密密钥）。
	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, err
	}

	// 2) 配额熔断：额度用尽直接拒绝（不发起调用）。
	quota, err := s.getQuota(userID)
	if err != nil {
		return nil, err
	}
	if quota.TokenLimit > 0 && quota.TokenUsed >= quota.TokenLimit {
		return nil, errors.New("AI 分析配额已用尽，请联系管理员调整额度")
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
	if userPromptOverride(userID, req.Module) != "" {
		// 标记本次使用了用户自定义模板——否则历史记录声称 p1 却无法按 p1 复现。
		// （模板内容可被用户随后编辑，完整快照留待 ai_call_logs，当前仅标记。）
		promptVersion = analysisPromptVersion + "-custom"
	}
	rec := &model.AnalysisRecord{
		UserID:          userID,
		Module:          req.Module,
		Market:          normalizeMarketOnly(req.Market),
		Symbol:          strings.TrimSpace(req.Symbol),
		Target:          actx.Label,
		Title:           s.title(req.Module, actx.Label),
		LLMConfigID:     cfg.ID,
		Provider:        cfg.Provider,
		Model:           cfg.Model,
		PromptVersion:   promptVersion,
		StrategyVersion: analysisStrategyVersion,
		DataSnapshot:    string(snapshotJSON),
	}

	result, raw, usage, latency, callErr := s.callWithRepair(ctx, cfg, apiKey, allowPrivate, messages)
	rec.PromptTokens = usage.PromptTokens
	rec.CompletionTokens = usage.CompletionTokens
	rec.TotalTokens = usage.TotalTokens
	rec.LatencyMs = latency

	// 消耗了 token 就扣配额（无论成功/降级）。
	if usage.TotalTokens > 0 {
		s.addUsage(userID, usage.TotalTokens)
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
		b, _ := json.Marshal(result)
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

// callWithRepair 调用 LLM 并在结构化校验失败时有限次 repair。累计各次调用的 token。
// 返回：解析成功的结果（失败为 nil）、最后一次原文、累计 usage、最后一次延迟、调用错误（网络/鉴权等，非校验失败）。
func (s *AnalysisService) callWithRepair(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, messages []chatMessage) (*AnalysisResult, string, chatUsage, int64, error) {
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
			return nil, lastRaw, acc, lastLatency, err
		}
		acc.PromptTokens += res.Usage.PromptTokens
		acc.CompletionTokens += res.Usage.CompletionTokens
		acc.TotalTokens += res.Usage.TotalTokens
		lastRaw = res.Content
		lastLatency = res.LatencyMs

		parsed, perr := parseAnalysisResult(res.Content)
		if perr == nil {
			return parsed, lastRaw, acc, lastLatency, nil
		}
		// 校验失败：追加 repair 指令再试。
		convo = append(convo,
			chatMessage{Role: "assistant", Content: res.Content},
			chatMessage{Role: "user", Content: "上一条输出不符合要求：" + perr.Error() +
				"。请只输出一个合法 JSON 对象，严格包含字段 rating(bullish/neutral/bearish)、confidence(0-100 整数)、summary、highlights、risks、opportunities、suggestions、disclaimer，不要任何解释或代码块标记。"},
		)
	}
	return nil, lastRaw, acc, lastLatency, nil // 降级
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
		"status", "rating", "confidence", "summary", "error",
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

// toView 把落库记录还原为前端视图（解析结构化结果 / 降级原文）。
func (s *AnalysisService) toView(rec model.AnalysisRecord) *AnalysisView {
	v := &AnalysisView{AnalysisRecord: rec}
	switch rec.Status {
	case model.AnalysisStatusSuccess:
		var r AnalysisResult
		if json.Unmarshal([]byte(rec.ResultJSON), &r) == nil {
			v.Result = &r
		}
	case model.AnalysisStatusDegraded:
		v.Raw = rec.ResultJSON
	}
	return v
}

func (s *AnalysisService) title(module, label string) string {
	names := map[string]string{
		model.AnalysisModuleMarket:    "全市场分析",
		model.AnalysisModuleSector:    "板块分析",
		model.AnalysisModuleStock:     "个股分析",
		model.AnalysisModuleWatchlist: "自选股分析",
		model.AnalysisModulePosition:  "持仓分析",
	}
	base := names[module]
	if label != "" && module != model.AnalysisModuleMarket {
		return base + " · " + label
	}
	return base
}

// getQuota / addUsage：配额读取与扣减。
func (s *AnalysisService) getQuota(userID int64) (*model.UserQuota, error) {
	var q model.UserQuota
	if err := common.DB.FirstOrCreate(&q, model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

func (s *AnalysisService) addUsage(userID int64, tokens int) {
	common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(map[string]any{
		"token_used":    gorm.Expr("token_used + ?", tokens),
		"request_count": gorm.Expr("request_count + 1"),
	})
}

// --- prompt 构造与结果校验 ---

// buildMessages 组装系统提示 + 用户消息（含数据快照 JSON）。系统提示按模块定制，且尊重用户自定义模板。
func (s *AnalysisService) buildMessages(userID int64, req AnalyzeRequest, actx *analysisContext, snapshotJSON string) []chatMessage {
	var b strings.Builder
	fmt.Fprintf(&b, "请对以下【%s】进行分析。\n\n", s.title(req.Module, actx.Label))
	if q := strings.TrimSpace(req.Question); q != "" {
		fmt.Fprintf(&b, "用户特别关注的问题（请在分析中优先回应）：%s\n\n", q)
	}
	b.WriteString("【数据】（JSON，数值为近似值，价格为货币单位，金额单位为元）：\n")
	b.WriteString(snapshotJSON)
	b.WriteString("\n\n数据时间：以快照内 data_as_of 为采集时刻、各字段 data_time/trade_date（如有）为准；" +
		"非交易时段采集的数据反映最近一个交易日，不代表实时状态，分析措辞须体现这一点。")
	b.WriteString("\n请严格按系统要求的 JSON schema 输出，只依据以上数据分析。")

	return []chatMessage{
		{Role: "system", Content: analysisSystemPrompt(userID, req.Module)},
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
2. 保持客观中立，机会与风险两面都要讲，不迎合用户、不夸大收益。
3. 全程使用简体中文。`

// moduleGuidance 各模块专属的分析维度与数据说明——必须与该模块实际注入的数据严格对应。
var moduleGuidance = map[string]string{
	model.AnalysisModuleStock: `本次分析对象是【单只个股】。可用数据：实时行情快照（现价、涨跌幅、开高低、昨收、成交额）、估值与盘面快照 valuation（PE-TTM/动态 PE、市净率、总市值/流通市值、换手率、振幅、量比、涨停/跌停价；该块缺失时表示估值数据暂不可得）、技术指标（MA5/MA10/MA20、区间最高/最低、近 5 日与近 20 日涨跌幅、日线根数）以及近期逐日 K 线明细。
请从以下维度展开：
- 趋势方向：结合现价与 MA5/MA10/MA20 的位置关系判断多空排列（多头/空头/纠缠）；
- 量价配合：结合近期成交量、换手率与量比判断是否放量/缩量、量价背离；
- 位置与空间：现价相对区间高低点的位置，可能的支撑位与压力位；
- 短中期动能：近 5 日、近 20 日涨跌幅反映的动能强弱与超买超卖倾向；
- 估值水位：若提供了 valuation，结合 PE/PB/市值说明估值的绝对水位与含义（无行业对比数据，须说明这是绝对水位而非行业相对水位；PE 为负表示亏损）；标注 is_st 的标的必须提示退市/风险警示。
重要限制：估值仅为快照指标，不含财务三表明细（营收/利润/负债）、机构持仓、个股资金流与新闻公告。若结论依赖这些，必须说明「数据缺失、无法判断」，绝不虚构。若快照带 freshness_note，措辞必须体现数据非实时。rating 以技术面为主、估值水位为辅给出。`,

	model.AnalysisModuleMarket: `本次分析对象是【整个市场】。可用数据：主要指数行情、涨跌家数与涨停/跌停数（市场情绪核心）、两市资金流（主力/超大/大/中/小单净流入）、涨幅榜、成交活跃榜、板块涨跌榜。
请从以下维度研判：
- 市场强弱：主要指数涨跌与相互印证（大盘 vs 创业板/科创，反映风格）；
- 赚钱效应：用涨跌家数对比、涨停/跌停数量判断市场情绪冷热与普涨/普跌/分化；
- 资金动向：主力与超大单净流入方向及规模，判断增量资金意愿；
- 领涨方向：涨幅榜、活跃榜、领涨板块反映的市场热点与风格切换。
若某些数据块标注为不可用(unavailable)，请指出该维度暂缺、结论相应保留。`,

	model.AnalysisModuleSector: `本次分析对象是【板块轮动】。可用数据：板块涨跌榜（含领涨股）、主要指数、市场涨跌家数情绪。
请从以下维度分析：
- 板块强弱排序：当日领涨与领跌板块，强弱分化程度；
- 轮动方向：结合板块涨跌与领涨股，判断资金在向哪些方向切换；
- 相对大盘：强势板块相对指数的超额表现；
- 持续性提示：仅凭单日数据能说到什么程度要如实说明，不夸大趋势延续性。
若用户指定了关注板块(focus)，请重点围绕它、并与榜单上其他板块横向对比；若榜单中没有该板块数据，请明确指出无法定位并只做大盘层面的板块结构分析。`,

	model.AnalysisModuleWatchlist: `本次分析对象是【用户的自选股清单】。数据：每个标的的名称、代码、所属分组、是否重点关注、实时现价与涨跌幅，以及用户填写的关注原因与备注。
请从以下维度分析：
- 整体表现：这批自选当日的涨跌分布与共性（是否集中在某类风格/板块）；
- 重点标的：结合 is_pinned 与关注原因，指出当前最值得留意的标的及理由；
- 风险提示：指出其中走弱或需要警惕的标的；
- 结合用户视角：呼应用户填写的关注原因给出研究性看法。
注意：数据仅含行情，不含个股财务与新闻，不要虚构基本面；无现价(缺 price)的标的说明数据缺失。`,

	model.AnalysisModulePosition: `本次分析对象是【用户的实际持仓】。数据：每笔持仓的标的、类型(短线/长线)、状态(持仓中/已卖出)、买入价、数量、成本、现价、盈亏额与收益率、买入理由，以及持仓中汇总盈亏。
请从以下维度分析：
- 组合整体：汇总盈亏与收益率，当前组合的盈亏结构（盈利仓 vs 亏损仓分布）；
- 集中度风险：是否过度集中于单一标的或单一市场/风格；
- 结构评估：短线与长线仓位的比例与各自表现是否符合其定位；
- 需复盘的仓位：结合买入理由与当前盈亏，指出哪些仓位偏离预期、值得重新审视。
所有涉及加仓/减仓/止盈止损的表述，一律作为研究参考与风险提示，不是操作指令。不要虚构未提供的成本或价格。`,
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
- disclaimer: 字符串，风险与免责提示`

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
	var r AnalysisResult
	if err := json.Unmarshal([]byte(jsonStr), &r); err != nil {
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
	if strings.TrimSpace(r.Disclaimer) == "" {
		r.Disclaimer = "本分析由 AI 基于公开数据生成，仅供研究参考，不构成投资建议，据此操作风险自负。"
	}
	return &r, nil
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
