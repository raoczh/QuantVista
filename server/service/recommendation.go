package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// RecommendationService 短线/长线推荐编排。
// 反编造硬约束：候选池由真实数据（自选∪涨幅榜∪活跃榜）构建，AI 只能从池中选，
// 生成后逐一校验标的必须∈候选池，越池标的一律丢弃（不落库、不展示）。
type RecommendationService struct {
	market    *MarketService
	watchlist *WatchlistService
	llm       *LLMService
}

func NewRecommendationService(market *MarketService, watchlist *WatchlistService, llm *LLMService) *RecommendationService {
	return &RecommendationService{market: market, watchlist: watchlist, llm: llm}
}

const (
	recPromptVersion   = "p1"
	recStrategyVersion = "s1"
	maxCandidates      = 24 // 候选池上限，控制上下文预算
	recRepairAttempts  = 2
)

// strategyTemplate 策略模板。
type strategyTemplate struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	guide string // 注入 prompt 的选股导向（不外泄给前端）
}

var shortStrategies = []strategyTemplate{
	{Key: "momentum", Name: "动量突破", Desc: "顺势追强，关注突破与量价配合",
		guide: "优先选择处于上升趋势、价格站上均线、近日放量突破关键位、动能强的标的；回避明显滞涨或量价背离者。"},
	{Key: "pullback", Name: "强势回踩", Desc: "强势股回调至支撑的低吸机会",
		guide: "优先选择整体强势、近期健康回调至均线/前高支撑附近、缩量企稳的标的；给出更靠近支撑的买入观察区间。"},
	{Key: "active", Name: "热点活跃", Desc: "资金聚焦的高活跃标的",
		guide: "优先选择成交额显著放大、市场关注度高、处于热点板块的活跃标的；严格设置止损以控回撤。"},
}

var longStrategies = []strategyTemplate{
	{Key: "value", Name: "价值低估", Desc: "偏防御，关注估值与稳健",
		guide: "优先选择商业模式稳健、估值相对合理或偏低的标的，弱化短期涨幅；以中长期持有视角评估。"},
	{Key: "growth", Name: "成长趋势", Desc: "关注景气与成长持续性",
		guide: "优先选择处于景气赛道、成长趋势明确、中长期逻辑清晰的标的；说明关键的成长驱动与验证指标。"},
	{Key: "leader", Name: "龙头优选", Desc: "行业龙头与确定性",
		guide: "优先选择行业地位领先、确定性较高的龙头标的；强调竞争壁垒与长期跟踪要点。"},
}

// StrategiesFor 返回某类型的可选策略（供前端下拉，不含内部 guide）。
func StrategiesFor(recType string) []strategyTemplate {
	src := longStrategies
	if recType == model.RecTypeShortTerm {
		src = shortStrategies
	}
	out := make([]strategyTemplate, 0, len(src))
	for _, s := range src {
		out = append(out, strategyTemplate{Key: s.Key, Name: s.Name, Desc: s.Desc})
	}
	return out
}

func strategyByKey(recType, key string) *strategyTemplate {
	src := longStrategies
	if recType == model.RecTypeShortTerm {
		src = shortStrategies
	}
	for i := range src {
		if src[i].Key == key {
			return &src[i]
		}
	}
	return &src[0] // 缺省用第一个
}

// candidate 候选池条目（均为真实行情数据）。
type candidate struct {
	Symbol    string  `json:"symbol"`
	Market    string  `json:"market"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	ChangePct float64 `json:"change_pct"`
	Amount    float64 `json:"amount"` // 成交额（元）
	Source    string  `json:"source"` // watchlist / gainer / active
}

// RecommendRequest 生成推荐入参。
type RecommendRequest struct {
	Type        string `json:"type"` // short_term / long_term
	Market      string `json:"market"`
	Strategy    string `json:"strategy"`
	LLMConfigID int64  `json:"llm_config_id"`
	Count       int    `json:"count"` // 期望 3-5
}

// recPick LLM 输出的单条推荐（短线/长线字段并存，按类型取用）。
type recPick struct {
	Symbol     string   `json:"symbol"`
	Action     string   `json:"action"`
	Confidence FlexInt  `json:"confidence"`
	Reason     []string `json:"reason"`
	Risks      []string `json:"risks"`
	Evidence   []string `json:"evidence"`
	// 短线
	BuyZoneLow   float64 `json:"buy_zone_low"`
	BuyZoneHigh  float64 `json:"buy_zone_high"`
	TakeProfit   float64 `json:"take_profit"`
	StopLoss     float64 `json:"stop_loss"`
	ValidDays    int     `json:"valid_days"`
	Invalidation string  `json:"invalidation"`
	// 长线
	Thesis        string   `json:"thesis"`
	ValuationLow  float64  `json:"valuation_low"`
	ValuationHigh float64  `json:"valuation_high"`
	KeyMetrics    []string `json:"key_metrics"`
	ReviewCycle   string   `json:"review_cycle"`
	Disclaimer    string   `json:"disclaimer"`
}

// RecommendationView 返回给前端：批次 + 条目（明细已解析）。
type RecommendationView struct {
	model.RecommendationBatch
	Items []RecommendationItemView `json:"items"`
}

// RecommendationItemView 条目 + 解析后的明细 + 追踪状态（若已评估）。
type RecommendationItemView struct {
	model.Recommendation
	Detail *recPick                    `json:"detail"`
	Status *model.RecommendationStatus `json:"status"`
}

// Generate 生成一批推荐。allowPrivate 由调用方按角色决定（管理员可访问内网自建模型）。
func (s *RecommendationService) Generate(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	if req.Type != model.RecTypeShortTerm && req.Type != model.RecTypeLongTerm {
		return nil, errors.New("推荐类型须为 short_term 或 long_term")
	}
	market := normalizeMarketOnly(req.Market)
	strat := strategyByKey(req.Type, strings.TrimSpace(req.Strategy))
	count := req.Count
	if count < 3 {
		count = 3
	}
	if count > 5 {
		count = 5
	}

	// 1) LLM 配置。
	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, err
	}

	// 2) 配额熔断。
	quota, err := s.getQuota(userID)
	if err != nil {
		return nil, err
	}
	if quota.TokenLimit > 0 && quota.TokenUsed >= quota.TokenLimit {
		return nil, errors.New("AI 配额已用尽，请联系管理员调整额度")
	}

	// 3) 候选池（真实数据；空池直接拒绝，避免无依据编造）。
	pool, err := s.buildCandidatePool(ctx, userID, market, req.Type)
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		if market != "cn" {
			return nil, errors.New("该市场暂无行情数据源支持，无法构建候选池（当前仅支持 A 股）")
		}
		return nil, errors.New("候选池为空：请先添加自选股，或稍后重试（榜单数据源繁忙）")
	}
	poolBySymbol := make(map[string]candidate, len(pool))
	for _, c := range pool {
		poolBySymbol[c.Symbol] = c
	}
	poolJSON, _ := json.Marshal(pool)

	// 4) 调用 + 反编造校验 + repair。
	messages := s.buildMessages(req.Type, strat, market, count, string(poolJSON))
	batch := &model.RecommendationBatch{
		UserID: userID, Type: req.Type, Market: market, Strategy: strat.Key,
		CandidateCount: len(pool), CandidatePool: string(poolJSON),
		DataSnapshot: string(poolJSON),
		LLMConfigID:  cfg.ID, Provider: cfg.Provider, Model: cfg.Model,
		PromptVersion: recPromptVersion, StrategyVersion: recStrategyVersion,
	}

	picks, usage, latency, callErr := s.callWithRepair(ctx, cfg, apiKey, allowPrivate, messages, poolBySymbol, count)
	batch.PromptTokens = usage.PromptTokens
	batch.CompletionTokens = usage.CompletionTokens
	batch.TotalTokens = usage.TotalTokens
	batch.LatencyMs = latency
	if usage.TotalTokens > 0 {
		s.addUsage(userID, usage.TotalTokens)
	}

	if callErr != nil {
		batch.Status = model.RecStatusFailed
		batch.Error = truncateRunes(callErr.Error(), 500)
		if err := common.DB.Create(batch).Error; err != nil {
			return nil, err
		}
		return nil, errors.New(callErr.Error())
	}
	if len(picks) == 0 {
		// 有响应但无一标的通过校验/在池中：降级（不落脏数据）。
		batch.Status = model.RecStatusDegraded
		batch.Error = "模型未给出候选池内的有效推荐，请调整策略或稍后重试"
		if err := common.DB.Create(batch).Error; err != nil {
			return nil, err
		}
		return &RecommendationView{RecommendationBatch: *batch, Items: []RecommendationItemView{}}, nil
	}

	// 5) 事务落库批次 + 条目。
	batch.Status = model.RecStatusSuccess
	items := make([]model.Recommendation, 0, len(picks))
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(batch).Error; err != nil {
			return err
		}
		for i, p := range picks {
			c := poolBySymbol[p.Symbol]
			detail, _ := json.Marshal(p)
			rec := model.Recommendation{
				BatchID: batch.ID, UserID: userID, Symbol: p.Symbol, Market: market,
				Name: c.Name, Action: p.Action, Confidence: int(p.Confidence),
				Summary: truncateRunes(firstReason(p), 512), RefPrice: c.Price,
				DetailJSON: string(detail), SortOrder: i,
			}
			if err := tx.Create(&rec).Error; err != nil {
				return err
			}
			items = append(items, rec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.assembleView(*batch, items, nil), nil
}

// callWithRepair 调用 LLM，反编造校验（picks 必须∈候选池），失败有限次 repair，累计 token。
func (s *RecommendationService) callWithRepair(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, messages []chatMessage, pool map[string]candidate, count int) ([]recPick, chatUsage, int64, error) {
	var acc chatUsage
	var lastLatency int64
	convo := append([]chatMessage(nil), messages...)

	for attempt := 0; attempt <= recRepairAttempts; attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model,
			Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
		})
		if err != nil {
			return nil, acc, lastLatency, err
		}
		acc.PromptTokens += res.Usage.PromptTokens
		acc.CompletionTokens += res.Usage.CompletionTokens
		acc.TotalTokens += res.Usage.TotalTokens
		lastLatency = res.LatencyMs

		picks, perr := parseAndFilterPicks(res.Content, pool, count)
		if perr == nil {
			return picks, acc, lastLatency, nil
		}
		// 校验失败：追加 repair，明确告知只能用候选池中的 symbol。
		symbols := poolSymbolList(pool)
		convo = append(convo,
			chatMessage{Role: "assistant", Content: res.Content},
			chatMessage{Role: "user", Content: "上一条输出不合格：" + perr.Error() +
				"。只能从以下候选池 symbol 中选，严禁使用池外或杜撰的代码：" + symbols +
				"。请重新输出 JSON：{\"picks\":[...]}，每个 pick 含 symbol、action、confidence、reason、risks、evidence 等字段，不要任何解释或代码块标记。"},
		)
	}
	return nil, acc, lastLatency, nil // 降级
}

// parseAndFilterPicks 解析 picks 并执行反编造过滤（只保留∈候选池的标的），
// 输出条数不超过 maxCount（PRD 3.6：用户可选 3~5 个，模型多给的截断丢弃）。
// 返回 error（触发 repair）当：JSON 解析失败、无 picks、或过滤后无任何有效标的。
func parseAndFilterPicks(content string, pool map[string]candidate, maxCount int) ([]recPick, error) {
	if maxCount <= 0 || maxCount > 5 {
		maxCount = 5
	}
	jsonStr := extractJSONObject(content)
	if jsonStr == "" {
		return nil, errors.New("未找到 JSON 对象")
	}
	var parsed struct {
		Picks []recPick `json:"picks"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	if len(parsed.Picks) == 0 {
		return nil, errors.New("picks 为空")
	}

	out := make([]recPick, 0, len(parsed.Picks))
	seen := map[string]bool{}
	for _, p := range parsed.Picks {
		sym := strings.TrimSpace(p.Symbol)
		if sym == "" || seen[sym] {
			continue
		}
		if _, ok := pool[sym]; !ok {
			continue // 反编造：越池标的直接丢弃
		}
		seen[sym] = true
		out = append(out, normalizePick(p, sym, pool[sym]))
		if len(out) >= maxCount {
			break
		}
	}
	if len(out) == 0 {
		return nil, errors.New("推荐的标的均不在候选池内")
	}
	return out, nil
}

// normalizePick 规整单条推荐的字段（action/confidence/数值/免责兜底）。
func normalizePick(p recPick, sym string, c candidate) recPick {
	p.Symbol = sym
	p.Action = strings.ToLower(strings.TrimSpace(p.Action))
	if p.Action != model.RecActionBuy && p.Action != model.RecActionWatch {
		p.Action = model.RecActionWatch
	}
	if p.Confidence < 0 {
		p.Confidence = 0
	}
	if p.Confidence > 100 {
		p.Confidence = 100
	}
	// 价格类字段负值归零。
	for _, f := range []*float64{&p.BuyZoneLow, &p.BuyZoneHigh, &p.TakeProfit, &p.StopLoss, &p.ValuationLow, &p.ValuationHigh} {
		if *f < 0 {
			*f = 0
		}
		*f = round2(*f)
	}
	if p.BuyZoneLow > 0 && p.BuyZoneHigh > 0 && p.BuyZoneLow > p.BuyZoneHigh {
		p.BuyZoneLow, p.BuyZoneHigh = p.BuyZoneHigh, p.BuyZoneLow
	}
	if p.ValidDays < 0 {
		p.ValidDays = 0
	}
	if hasShortPlan(p) {
		if p.ValidDays == 0 {
			p.ValidDays = 5
		}
		if p.ValidDays < 3 {
			p.ValidDays = 3
		}
		if p.ValidDays > 10 {
			p.ValidDays = 10
		}
		if !shortPlanPricesValid(p, c.Price) {
			p.Action = model.RecActionWatch
			p.Risks = append(p.Risks, "短线价位关系不满足止盈>买入区间上沿>下沿>止损（或与现价明显脱节），已降级为观察")
			// 清零价位：无效价位不得驱动阶段6 追踪的止盈/止损判定（tracking 视为未设价位）。
			p.BuyZoneLow, p.BuyZoneHigh, p.TakeProfit, p.StopLoss = 0, 0, 0, 0
		}
		if c.Price > 0 {
			p.Evidence = append(p.Evidence, "短线计划按交易日跟踪，A股需考虑T+1、涨跌停与100股一手约束")
		}
	}
	p.Reason = orEmpty(p.Reason)
	p.Risks = orEmpty(p.Risks)
	p.Evidence = orEmpty(p.Evidence)
	p.KeyMetrics = orEmpty(p.KeyMetrics)
	if strings.TrimSpace(p.Disclaimer) == "" {
		p.Disclaimer = "本推荐由 AI 基于候选池内公开行情生成，仅供研究参考，不构成投资建议，据此操作风险自负。"
	}
	return p
}

// minCandidateAmount PRD 3.6 流动性前置筛选：日成交额不足 1 亿元的标的剔除
// （仅对带成交额的榜单项生效；自选项无成交额数据，以「能取到实时行情」为准入）。
const minCandidateAmount = 1e8

// candidateEligible PRD 3.6 推荐前置筛选：排除退市风险（ST/*ST/退市整理）、
// 停牌或取不到行情、流动性不足的标的。候选池是反编造的根基——无行情数据的
// 标的只会诱导 LLM 编造依据，一并拒之门外。
func candidateEligible(c candidate) bool {
	name := strings.ToUpper(c.Name)
	if strings.Contains(name, "ST") || strings.Contains(name, "退") {
		return false
	}
	if c.Price <= 0 {
		return false
	}
	if c.Amount > 0 && c.Amount < minCandidateAmount {
		return false
	}
	return true
}

// buildCandidatePool 从真实数据构建候选池：自选∪涨幅榜∪活跃榜，经 PRD 3.6
// 前置筛选（candidateEligible）后去重富化，上限 maxCandidates。
func (s *RecommendationService) buildCandidatePool(ctx context.Context, userID int64, market, recType string) ([]candidate, error) {
	byKey := map[string]candidate{}
	order := []string{} // 保序：自选优先，其次榜单

	add := func(c candidate) {
		if c.Symbol == "" || !candidateEligible(c) {
			return
		}
		if _, ok := byKey[c.Symbol]; ok {
			return
		}
		byKey[c.Symbol] = c
		order = append(order, c.Symbol)
	}

	// 自选股（用户已研究的标的，优先纳入）——仅限当前 market，且必须有实时行情
	// （取不到行情可能是停牌，且无数据会诱导 LLM 编造依据）。
	if groups, err := s.watchlist.List(ctx, userID); err == nil {
		for _, g := range groups {
			for _, it := range g.Items {
				if it.Market != market || !it.QuoteOK {
					continue
				}
				add(candidate{Symbol: it.Symbol, Market: market, Name: it.Name,
					Price: round2(it.Price), ChangePct: round2(it.ChangePct), Source: "watchlist"})
			}
		}
	}

	// 榜单（真实行情）。短线更看重活跃/涨幅，长线也纳入以扩池，靠 prompt 导向区分。
	ov := s.market.GetOverview(ctx, market)
	for _, r := range ov.Actives {
		add(candidate{Symbol: r.Symbol, Market: market, Name: r.Name,
			Price: round2(r.Price), ChangePct: round2(r.ChangePct), Amount: round2(r.Amount), Source: "active"})
	}
	for _, r := range ov.Gainers {
		add(candidate{Symbol: r.Symbol, Market: market, Name: r.Name,
			Price: round2(r.Price), ChangePct: round2(r.ChangePct), Amount: round2(r.Amount), Source: "gainer"})
	}

	pool := make([]candidate, 0, len(order))
	for _, sym := range order {
		pool = append(pool, byKey[sym])
		if len(pool) >= maxCandidates {
			break
		}
	}
	return pool, nil
}

// buildMessages 组装系统提示 + 用户消息（含候选池 JSON）。分短线/长线定制。
func (s *RecommendationService) buildMessages(recType string, strat *strategyTemplate, market string, count int, poolJSON string) []chatMessage {
	var sys strings.Builder
	sys.WriteString(recRoleIntro)
	sys.WriteString("\n\n")
	if recType == model.RecTypeShortTerm {
		sys.WriteString(shortTermSpec)
	} else {
		sys.WriteString(longTermSpec)
	}
	sys.WriteString("\n\n【本次策略】" + strat.Name + "：" + strat.guide)

	var u strings.Builder
	fmt.Fprintf(&u, "请从以下【候选池】中，按「%s」策略精选 %d 个%s标的。\n", strat.Name, count, recTypeLabel(recType))
	u.WriteString("硬性要求：只能从候选池里选，symbol 必须与候选池完全一致，严禁推荐池外或虚构的标的；若候选池中合适标的不足，可少于 " + fmt.Sprintf("%d", count) + " 个，但绝不编造。\n\n")
	u.WriteString("【候选池】（JSON，price 为现价，amount 为成交额/元，change_pct 为当日涨跌幅%）：\n")
	u.WriteString(poolJSON)
	u.WriteString("\n\n请只输出 JSON：{\"picks\":[...]}。")

	return []chatMessage{
		{Role: "system", Content: sys.String()},
		{Role: "user", Content: u.String()},
	}
}

const recRoleIntro = `你是一名严谨的证券研究助理，服务于个人投资研究工具。你的输出仅供研究参考，不构成任何投资建议或买卖指令。

总则：
1. 只能从用户提供的【候选池】中挑选标的，symbol 必须与候选池完全一致；严禁推荐池外标的或杜撰任何代码/数据。
2. 只依据候选池给出的真实行情数据分析，数据不足处要如实说明局限，不臆测未提供的财务/新闻。
3. 每个标的必须给出：理由(reason)、风险(risks)、数据依据(evidence，引用候选池中的具体数值)、免责声明(disclaimer)。
4. 全程简体中文。只输出一个 JSON 对象 {"picks":[...]}，不要任何解释文字或 Markdown 代码块标记。`

const shortTermSpec = `本次为【短线推荐】。每个 pick 需包含字段：
- symbol: 候选池中的代码
- action: "buy"(可考虑买入) 或 "watch"(观察等待)
- confidence: 0-100 整数
- reason: 字符串数组，选择理由（技术面/量价/热点）
- risks: 字符串数组，主要风险
- evidence: 字符串数组，数据依据（引用候选池中的 price/change_pct/amount 等）
- buy_zone_low / buy_zone_high: 买入观察区间（下沿/上沿价格）
- take_profit: 止盈目标价
- stop_loss: 止损价
- valid_days: 该短线机会的有效天数（交易日，通常 3-10）
- invalidation: 失效条件（一句话，如"跌破止损价或放量破位"）
- disclaimer: 风险与免责提示
交易规则硬约束：当前数据源仅支持 A 股；A 股当日买入不可当日卖出(T+1)，止盈/止损最早次一交易日生效；必须考虑涨跌停限制，涨停可能买不进、跌停可能卖不出；最小交易单位为 100 股一手；有效期和持有周期都按交易日计算，不按自然日。
要求止盈>买入区间上沿>买入区间下沿>止损，价格贴近现价合理设置。`

const longTermSpec = `本次为【长线推荐】。当前数据以实时行情为主，缺少完整财务报表，请基于可得信息给出中长期视角，并明确指出财务数据的缺失。每个 pick 需包含字段：
- symbol: 候选池中的代码
- action: "buy"(可考虑逢低布局) 或 "watch"(观察等待)
- confidence: 0-100 整数
- reason: 字符串数组，长期看好/关注的理由
- risks: 字符串数组，主要风险
- evidence: 字符串数组，数据依据（引用候选池中的具体数值）
- thesis: 基本面/投资逻辑（一段话）
- valuation_low / valuation_high: 合理估值区间（若无财务数据无法给出可填 0 并在 thesis 说明）
- key_metrics: 字符串数组，需持续跟踪的关键指标（如营收增速、毛利率、市占率）
- review_cycle: 复盘周期（如"每季度财报后"）
- disclaimer: 风险与免责提示`

func hasShortPlan(p recPick) bool {
	return p.BuyZoneLow > 0 || p.BuyZoneHigh > 0 || p.TakeProfit > 0 || p.StopLoss > 0 || p.ValidDays > 0
}

// shortPlanPricesValid 校验短线计划价位：四价关系 止盈>区间上沿>下沿>止损，
// 且与现价锚定——现价必须落在 (止损, 止盈) 之间，否则整套计划悬空于现价
// 之外（如现价 10 而止损 12），首日即会误触发追踪判定。
func shortPlanPricesValid(p recPick, price float64) bool {
	if p.BuyZoneLow <= 0 || p.BuyZoneHigh <= 0 || p.TakeProfit <= 0 || p.StopLoss <= 0 {
		return false
	}
	if !(p.TakeProfit > p.BuyZoneHigh && p.BuyZoneHigh > p.BuyZoneLow && p.BuyZoneLow > p.StopLoss) {
		return false
	}
	if price > 0 && (p.StopLoss >= price || p.TakeProfit <= price) {
		return false
	}
	return true
}
func recTypeLabel(t string) string {
	if t == model.RecTypeShortTerm {
		return "短线"
	}
	return "长线"
}

func firstReason(p recPick) string {
	if len(p.Reason) > 0 {
		return p.Reason[0]
	}
	if p.Thesis != "" {
		return p.Thesis
	}
	return ""
}

func poolSymbolList(pool map[string]candidate) string {
	syms := make([]string, 0, len(pool))
	for s := range pool {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	return strings.Join(syms, ", ")
}

// --- 查询 ---

// History 列出推荐批次（不返回重字段）。
func (s *RecommendationService) History(userID int64, recType string, limit int) ([]model.RecommendationBatch, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	q := common.DB.Where("user_id = ?", userID)
	if recType == model.RecTypeShortTerm || recType == model.RecTypeLongTerm {
		q = q.Where("type = ?", recType)
	}
	var rows []model.RecommendationBatch
	err := q.Select("id", "user_id", "type", "market", "strategy", "status", "error",
		"candidate_count", "llm_config_id", "provider", "model", "prompt_version", "strategy_version",
		"prompt_tokens", "completion_tokens", "total_tokens", "latency_ms", "created_at", "updated_at").
		Order("id DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Get 取单批推荐详情（含条目）。仅本人。
func (s *RecommendationService) Get(userID, id int64) (*RecommendationView, error) {
	var batch model.RecommendationBatch
	err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&batch).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("推荐记录不存在")
	}
	if err != nil {
		return nil, err
	}
	var items []model.Recommendation
	if err := common.DB.Where("batch_id = ? AND user_id = ?", id, userID).Order("sort_order, id").Find(&items).Error; err != nil {
		return nil, err
	}
	// 附追踪状态（若后台/手动已评估）。
	statuses := map[int64]model.RecommendationStatus{}
	var srows []model.RecommendationStatus
	common.DB.Where("batch_id = ? AND user_id = ?", id, userID).Find(&srows)
	for _, r := range srows {
		statuses[r.RecommendationID] = r
	}
	return s.assembleView(batch, items, statuses), nil
}

// Delete 删除推荐批次及其条目（仅本人，事务）。
func (s *RecommendationService) Delete(userID, id int64) error {
	var batch model.RecommendationBatch
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&batch).Error; err != nil {
		return errors.New("推荐记录不存在")
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("batch_id = ? AND user_id = ?", id, userID).Delete(&model.Recommendation{}).Error; err != nil {
			return err
		}
		if err := tx.Where("batch_id = ? AND user_id = ?", id, userID).Delete(&model.RecommendationStatus{}).Error; err != nil {
			return err
		}
		return tx.Delete(&batch).Error
	})
}

// assembleView 组装批次视图（解析条目明细，附可选追踪状态）。
func (s *RecommendationService) assembleView(batch model.RecommendationBatch, items []model.Recommendation, statuses map[int64]model.RecommendationStatus) *RecommendationView {
	views := make([]RecommendationItemView, 0, len(items))
	for _, it := range items {
		iv := RecommendationItemView{Recommendation: it}
		var d recPick
		if it.DetailJSON != "" && json.Unmarshal([]byte(it.DetailJSON), &d) == nil {
			iv.Detail = &d
		}
		if st, ok := statuses[it.ID]; ok {
			s := st
			iv.Status = &s
		}
		views = append(views, iv)
	}
	return &RecommendationView{RecommendationBatch: batch, Items: views}
}

// 配额（与分析共用 user_quota）。
func (s *RecommendationService) getQuota(userID int64) (*model.UserQuota, error) {
	var q model.UserQuota
	if err := common.DB.FirstOrCreate(&q, model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

func (s *RecommendationService) addUsage(userID int64, tokens int) {
	common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(map[string]any{
		"token_used":    gorm.Expr("token_used + ?", tokens),
		"request_count": gorm.Expr("request_count + 1"),
	})
}
