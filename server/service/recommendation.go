package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
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
	recPromptVersion   = "p7" // p7: 名单新增 T1 技术指标（RSI/MACD/BOLL/ATR）与筹码（获利盘/平均成本）字段说明；p6: senti 消息面字段；p5: 来源随策略组合；p4: 四阶段流水线；p3: 落选理由；p2: 估值富化
	recStrategyVersion = "s5" // s5: T1 指标加分项（水上金叉/RSI 分区/布林位置）+ 筹码超跌加分 + 五维动量/风险维升级（RSI 凹形、ATR）；s4: 消息面情绪因子；s3: 策略-来源映射 + 换手分位化；s2: 本地量化评分；s1: 纯 prompt 导向
	maxScanCandidates  = 48   // 进入量化评分的候选上限（约束日线拉取量：48 只 × 1 次 HTTP，并发 6 约 3~8s）
	maxLLMCandidates   = 16   // 量化排序后进入 LLM 精选的名单上限（控上下文与位置偏差面）
	factorBarLimit     = 90   // 五维评分/窗口因子的日线口径（MA60 需 ≥60，留余量）；实际拉取按 chipBarLimit=210，评分前截尾
	maxPoolIntake      = 240  // 建池总量护栏（自选无上限，防极端用户打爆估值批量请求）
	poolSnapshotMax    = 150  // 候选池快照落库条目上限（MySQL TEXT 64KB 容量保护，超出部分只记数量）
	recRepairAttempts  = 2
)

// poolFullPrefix 「评分名额已满」排除原因前缀（scorePool 补位时按它识别可回补的标的）。
const poolFullPrefix = "候选池已满"

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

// strategyByKey 按类型查策略模板：空 key 用第一个缺省；非空但查不到报错
// （旧版静默回退第一个，用户传跨类型 key 时会无感知地跑错策略）。
func strategyByKey(recType, key string) (*strategyTemplate, error) {
	src := longStrategies
	if recType == model.RecTypeShortTerm {
		src = shortStrategies
	}
	if key == "" {
		return &src[0], nil
	}
	for i := range src {
		if src[i].Key == key {
			return &src[i], nil
		}
	}
	return nil, fmt.Errorf("策略 %s 与推荐类型不匹配，请重新选择", key)
}

// candidate 候选池条目（均为真实行情数据；估值字段为腾讯免费源 best-effort，缺失为 0）。
// 阶段②③会补充：Sources 来源、Excluded 被过滤原因、Factors 技术因子、Score/Rank 量化评分与排名、
// Bonus 策略加分项说明、SentToLLM 是否进入 LLM 精选名单。全量（含被过滤者）落库 candidate_pool——
// 池子完全透明，用户能看到每只股为什么进、为什么被筛掉、排第几。
type candidate struct {
	Symbol       string  `json:"symbol"`
	Market       string  `json:"market"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	ChangePct    float64 `json:"change_pct"`
	Amount       float64 `json:"amount"`                  // 成交额（元）
	PETTM        float64 `json:"pe_ttm,omitempty"`        // 市盈率 TTM（负=亏损）
	PB           float64 `json:"pb,omitempty"`            // 市净率
	TotalCap     float64 `json:"total_cap,omitempty"`     // 总市值（元）
	FloatCap     float64 `json:"float_cap,omitempty"`     // 流通市值（元）
	TurnoverRate float64 `json:"turnover_rate,omitempty"` // 换手率 %
	VolumeRatio  float64 `json:"volume_ratio,omitempty"`  // 量比（腾讯实时口径）
	Amplitude    float64 `json:"amplitude,omitempty"`     // 当日振幅 %
	LimitUp      float64 `json:"limit_up,omitempty"`      // 涨停价（判「已涨停买不进」）
	Source       string  `json:"source,omitempty"`        // 兼容旧记录的单来源字段（新记录用 Sources）

	Sources   []string     `json:"sources,omitempty"`    // watchlist / gainer / active / turnover（可多来源）
	Excluded  string       `json:"excluded,omitempty"`   // 非空=被用户筛选/风控排除的原因（透明可查）
	Factors   *candFactors `json:"factors,omitempty"`    // 技术因子快照（210 根日线派生：窗口因子尾窗口径、指标递推、筹码累积）
	ScoreDims *scoreDims   `json:"score_dims,omitempty"` // 五维评分明细
	SentiScore float64     `json:"senti_score,omitempty"` // N2 当日聚合情绪分 -1~1（新闻加权合成）
	SentiNews  int         `json:"senti_news,omitempty"`  // 参与聚合的新闻条数（0=当日无相关新闻）
	Score     float64      `json:"score,omitempty"`      // 量化综合分 0-100（五维基础分 + 策略加分）
	Rank      int          `json:"rank,omitempty"`       // 未被排除者中的排名（1=最高）
	Bonus     []string     `json:"bonus,omitempty"`      // 策略加分/扣分明细（可解释）
	SentToLLM bool         `json:"sent_to_llm,omitempty"`
}

// sourceLabelCN 候选来源的中文标签（落库英文 key，前端映射展示）。
var sourceLabelCN = map[string]string{
	"watchlist": "自选",
	"gainer":    "涨幅榜",
	"active":    "成交额榜",
	"turnover":  "换手率榜",
	"dipper":    "回调榜",
	"lowpb":     "低PB榜",
}

// sourceSpec 一路榜单来源的取数规格。
type sourceSpec struct {
	sort  string // 新浪榜单排序字段
	asc   bool   // true=升序方向（跌幅/低PB 等「不热」来源）
	limit int    // 取多少条（≤100）
	label string // 进池来源标签（sourceLabelCN 的 key）
	// keep 榜单行级本地过滤；nil=全收。升序榜的前排常有极端值（暴跌/负PB），在此滤掉。
	keep func(r datasource.StockRank) bool
}

// strategySources 按类型+策略给出榜单来源组合——来源与策略意图对齐，并显式补入
// 「不热」方向（回调榜/低PB榜/成交额深捞），对冲旧版三大热度榜全是「当天最热」、
// 与追高保护/涨停排除/死亡换手规则天然互斥、筛后存活极少导致推不满的结构性矛盾。
func strategySources(recType, stratKey string) []sourceSpec {
	// 当日温和回调（-7%~-0.5%）的活跃标的：强势回踩的主力来源。注意不能用跌幅榜
	// （changepercent 升序）——全市场跌超 2% 的家数常年远大于 100，升序前 100 名
	// 永远是深跌段（实测 [-21%,-7.6%]），过滤后只剩暴跌股与「温和回调」意图相反；
	// 正确做法是在成交额/换手率深捞结果里过滤当日回调票（活跃 × 回调）。
	dip := func(r datasource.StockRank) bool { return r.ChangePct <= -0.5 && r.ChangePct >= -7 }
	// 换手率榜深捞后取温和活跃带（3~15%）：榜单前排 20%+ 的极端换手大多会被
	// 30% 硬顶或高位判定剔除，直接取温和带存活率与票型都更好。
	mildTurnover := func(r datasource.StockRank) bool { return r.TurnoverRate >= 3 && r.TurnoverRate <= 15 }
	// 低PB榜升序前排的负 PB 为资不抵债/退市股；再用同响应自带的 per 剔除亏损与
	// 高估值（价值陷阱：PB 0.3 的亏损地产与盈利银行同池），皆为行级白捡过滤。
	posPB := func(r datasource.StockRank) bool { return r.PB > 0 && r.PE > 0 && r.PE <= 30 }
	if recType == model.RecTypeShortTerm {
		switch stratKey {
		case "pullback":
			return []sourceSpec{
				{sort: "amount", asc: false, limit: 100, label: "dipper", keep: dip},
				{sort: "turnoverratio", asc: false, limit: 80, label: "dipper", keep: dip},
				{sort: "amount", asc: false, limit: 40, label: "active"},
				{sort: "changepercent", asc: false, limit: 30, label: "gainer"},
			}
		case "active":
			return []sourceSpec{
				{sort: "amount", asc: false, limit: 60, label: "active"},
				{sort: "turnoverratio", asc: false, limit: 100, label: "turnover", keep: mildTurnover},
				{sort: "changepercent", asc: false, limit: 30, label: "gainer"},
			}
		default: // momentum
			return []sourceSpec{
				{sort: "changepercent", asc: false, limit: 60, label: "gainer"},
				{sort: "turnoverratio", asc: false, limit: 100, label: "turnover", keep: mildTurnover},
				{sort: "amount", asc: false, limit: 40, label: "active"},
			}
		}
	}
	switch stratKey {
	case "value":
		return []sourceSpec{
			{sort: "pb", asc: true, limit: 100, label: "lowpb", keep: posPB},
			{sort: "amount", asc: false, limit: 60, label: "active"},
		}
	case "leader":
		return []sourceSpec{
			{sort: "amount", asc: false, limit: 80, label: "active"},
			{sort: "changepercent", asc: false, limit: 20, label: "gainer"},
		}
	default: // growth
		return []sourceSpec{
			{sort: "changepercent", asc: false, limit: 50, label: "gainer"},
			{sort: "turnoverratio", asc: false, limit: 100, label: "turnover", keep: mildTurnover},
			{sort: "amount", asc: false, limit: 40, label: "active"},
		}
	}
}

// RecommendRequest 生成推荐入参。
type RecommendRequest struct {
	Type        string      `json:"type"` // short_term / long_term
	Market      string      `json:"market"`
	Strategy    string      `json:"strategy"`
	LLMConfigID int64       `json:"llm_config_id"`
	Count       int         `json:"count"`   // 期望 3-5
	Filters     *RecFilters `json:"filters"` // 候选筛选条件；nil = 用用户偏好（无偏好则按类型默认）
	Verify      bool        `json:"verify"`  // AI 复核：额外一次「风控复核员」调用逐条挑刺（多耗一次 LLM 请求）
}

// recPick LLM 输出的单条推荐（短线/长线字段并存，按类型取用）。
// QuantScore/Rank/LotCost/EvidenceCheck/SysConfidence/Review 为服务端生成后回填（非 LLM 输出）。
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

	// --- 服务端回填（信任层）---
	QuantScore       float64        `json:"quant_score,omitempty"`        // 量化综合分
	QuantRank        int            `json:"quant_rank,omitempty"`         // 池内排名
	PoolSize         int            `json:"pool_size,omitempty"`          // 参与排名的标的数
	LotCost          float64        `json:"lot_cost,omitempty"`           // 一手(100股)成本（元）
	EvidenceCheck    *evidenceCheck `json:"evidence_check,omitempty"`     // 证据数字核验结果
	SysConfidence    string         `json:"sys_confidence,omitempty"`     // 程序合成置信度 high/medium/low
	SysConfidenceWhy string         `json:"sys_confidence_why,omitempty"` // 置信度依据说明
	Review           *pickReview    `json:"review,omitempty"`             // AI 复核员结论（verify 模式）
}

// pickReview AI 复核员对单条推荐的结论。Confidence 用 FlexInt——模型常把整数字段
// 输出成 72.5 或 "80"，裸 int 会让整个复核 JSON 反序列化失败、结论被静默丢弃。
type pickReview struct {
	Symbol     string  `json:"symbol"`
	Verdict    string  `json:"verdict"` // pass / warn / reject
	Comment    string  `json:"comment"`
	Confidence FlexInt `json:"confidence"` // 复核后的建议置信度（0-100；0=不调整）
}

// recReject 池内落选标的的一句话理由（批次G「为什么没选它」，帮助复盘筛选逻辑）。
type recReject struct {
	Symbol string `json:"symbol"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

// RecommendationView 返回给前端：批次 + 条目（明细已解析）。
type RecommendationView struct {
	model.RecommendationBatch
	Items []RecommendationItemView `json:"items"`
}

// RecPositionLink 推荐对应的实际持仓（血缘：一键建仓时写入 positions.recommendation_id）。
// 供前端展示「已建仓」与 推荐参考价 vs 实际买入价 对比。
type RecPositionLink struct {
	PositionID int64   `json:"position_id"`
	BuyPrice   float64 `json:"buy_price"`
	BuyDate    string  `json:"buy_date"`
	Quantity   float64 `json:"quantity"`
	Status     string  `json:"status"` // holding / closed
}

// RecommendationItemView 条目 + 解析后的明细 + 追踪状态（若已评估）+ 对应持仓（若已建仓）。
type RecommendationItemView struct {
	model.Recommendation
	Detail   *recPick                    `json:"detail"`
	Status   *model.RecommendationStatus `json:"status"`
	Position *RecPositionLink            `json:"position"`
}

// Generate 生成一批推荐（用户手动发起，计 1 次配额）。allowPrivate 由调用方按角色决定（管理员可访问内网自建模型）。
func (s *RecommendationService) Generate(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
	return s.generate(ctx, userID, allowPrivate, req, true)
}

// GenerateAuto 后台任务（收盘日报）代用户生成：token 照记审计，但不消耗次数配额。
func (s *RecommendationService) GenerateAuto(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
	return s.generate(ctx, userID, allowPrivate, req, false)
}

func (s *RecommendationService) generate(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest, manualAction bool) (*RecommendationView, error) {
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	if req.Type != model.RecTypeShortTerm && req.Type != model.RecTypeLongTerm {
		return nil, errors.New("推荐类型须为 short_term 或 long_term")
	}
	market := normalizeMarketOnly(req.Market)
	strat, serr := strategyByKey(req.Type, strings.TrimSpace(req.Strategy))
	if serr != nil {
		return nil, serr
	}
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
	if err := checkQuota(userID); err != nil {
		return nil, err
	}

	// 3) 筛选条件：请求携带 > 用户偏好 > 类型默认。全程落库可回显。
	var filters RecFilters
	if req.Filters != nil {
		filters = sanitizeRecFilters(*req.Filters)
	} else {
		filters = loadUserRecFilters(userID, req.Type)
	}

	// 4) 阶段①②：多源建池（来源随策略组合）+ 基础准入 + 用户筛选（被筛掉的保留原因）。
	pool, err := s.buildPool(ctx, userID, market, req.Type, strat, filters)
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		if market != "cn" {
			return nil, errors.New("该市场暂无行情数据源支持，无法构建候选池（当前仅支持 A 股）")
		}
		return nil, errors.New("候选池为空：请先添加自选股，或稍后重试（榜单数据源繁忙）")
	}

	// 5) 阶段③：量化评分排序（拉日线算因子，追高保护在此判定），Top N 进入 LLM 名单。
	s.scorePool(ctx, req.Type, strat, pool, filters)
	kept, llmCands := 0, make([]candidate, 0, maxLLMCandidates)
	for _, c := range pool {
		if c.Excluded == "" {
			kept++
		}
		if c.SentToLLM {
			llmCands = append(llmCands, c)
		}
	}
	if len(llmCands) == 0 {
		excluded := len(pool) - kept
		return nil, fmt.Errorf("筛选后候选池为空（共扫描 %d 只，%d 只被筛掉）：请放宽股价/市值/换手等筛选条件后重试", len(pool), excluded)
	}
	poolBySymbol := make(map[string]candidate, len(llmCands))
	for _, c := range llmCands {
		poolBySymbol[c.Symbol] = c
	}
	poolJSON, poolOmitted := marshalPoolSnapshot(pool)
	filtersPayload := map[string]any{"filters": filters, "applied": filters.Describe()}
	if poolOmitted > 0 {
		filtersPayload["pool_omitted"] = poolOmitted
	}
	filtersJSON, _ := json.Marshal(filtersPayload)

	// 6) 阶段④：LLM 精选 + 反编造校验 + repair。
	mktCtx := s.buildMarketContext(ctx, market)
	messages := s.buildMessages(req.Type, strat, market, count, llmCands, filters, mktCtx)
	llmInput, _ := json.Marshal(map[string]any{"market_context": mktCtx, "candidates": compactForLLM(req.Type, llmCands)})
	batch := &model.RecommendationBatch{
		UserID: userID, Type: req.Type, Market: market, Strategy: strat.Key,
		Title:          composeBatchTitle(req.Type, strat, filters, count),
		CandidateCount: kept, CandidatePool: poolJSON,
		DataSnapshot: string(llmInput), FiltersJSON: string(filtersJSON),
		LLMConfigID: cfg.ID, Provider: cfg.Provider, Model: cfg.Model,
		PromptVersion: recPromptVersion, StrategyVersion: recStrategyVersion,
	}

	picks, rejected, usage, latency, callErr := s.callWithRepair(ctx, cfg, apiKey, allowPrivate, messages, poolBySymbol, count)

	// 7) 可选 AI 复核（verify）：风控复核员逐条挑刺，reject 降级为观察。
	if callErr == nil && len(picks) > 0 && req.Verify {
		reviews, overall, rvUsage := s.reviewPicks(ctx, cfg, apiKey, allowPrivate, req.Type, picks, poolBySymbol)
		usage.PromptTokens += rvUsage.PromptTokens
		usage.CompletionTokens += rvUsage.CompletionTokens
		usage.TotalTokens += rvUsage.TotalTokens
		if len(reviews) > 0 {
			picks = applyReviews(picks, reviews)
			if b, err := json.Marshal(map[string]any{"reviews": reviews, "overall": overall}); err == nil {
				batch.ReviewJSON = string(b)
			}
		}
	}

	batch.PromptTokens = usage.PromptTokens
	batch.CompletionTokens = usage.CompletionTokens
	batch.TotalTokens = usage.TotalTokens
	batch.LatencyMs = latency
	if usage.TotalTokens > 0 {
		consumeQuota(userID, usage.TotalTokens, manualAction)
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
		// 两种情形都不落脏数据：①模型行使「宁缺毋滥」拒选（合法，落选理由照存）；
		// ②repair 后仍无有效输出。均降级展示，文案区分开，不把正确拒选误报为故障。
		batch.Status = model.RecStatusDegraded
		if len(rejected) > 0 {
			if b, err := json.Marshal(rejected); err == nil {
				batch.RejectedJSON = string(b)
			}
			batch.Error = "模型判断当前无足够合适的标的，未强行凑数（宁缺毋滥）；各候选的落选理由见「为什么没选它」"
		} else {
			batch.Error = "模型未给出候选池内的有效推荐，请调整策略或稍后重试"
		}
		if err := common.DB.Create(batch).Error; err != nil {
			return nil, err
		}
		return &RecommendationView{RecommendationBatch: *batch, Items: []RecommendationItemView{}}, nil
	}

	// 8) 信任层回填：量化分/排名/一手成本/证据核验/程序合成置信度。
	for i := range picks {
		c := poolBySymbol[picks[i].Symbol]
		picks[i].QuantScore = c.Score
		picks[i].QuantRank = c.Rank
		picks[i].PoolSize = kept
		picks[i].LotCost = round2(c.Price * 100)
		// 计划价与用户筛选阈值并入证据核验值域：模型在 evidence 里复述自己给出的
		// 止盈/止损/买入区间、或用户设定的价格/换手/市值/追高阈值，均为合法引用而非幻觉。
		picks[i].EvidenceCheck = verifyEvidence(picks[i].Evidence, c,
			picks[i].BuyZoneLow, picks[i].BuyZoneHigh, picks[i].TakeProfit, picks[i].StopLoss,
			filters.PriceMin, filters.PriceMax, filters.MaxGain5dPct,
			filters.TurnoverMin, filters.TurnoverMax, filters.FloatCapMinYi, filters.FloatCapMaxYi)
		picks[i].SysConfidence, picks[i].SysConfidenceWhy = systemConfidence(c, picks[i].EvidenceCheck, kept)
	}

	// 落选理由（池内未入选标的的一句话说明；best-effort，模型未给则为空）。
	if len(rejected) > 0 {
		if b, err := json.Marshal(rejected); err == nil {
			batch.RejectedJSON = string(b)
		}
	}

	// 9) 事务落库批次 + 条目。
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

	return s.assembleView(*batch, items, nil, nil), nil
}

// callWithRepair 调用 LLM，反编造校验（picks 必须∈候选池），失败有限次 repair，累计 token。
// 同时收集池内落选标的的一句话理由（rejected，best-effort 不参与合格判定）。
func (s *RecommendationService) callWithRepair(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, messages []chatMessage, pool map[string]candidate, count int) ([]recPick, []recReject, chatUsage, int64, error) {
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
			return nil, nil, acc, lastLatency, err
		}
		acc.PromptTokens += res.Usage.PromptTokens
		acc.CompletionTokens += res.Usage.CompletionTokens
		acc.TotalTokens += res.Usage.TotalTokens
		lastLatency = res.LatencyMs

		picks, rejected, perr := parseAndFilterPicks(res.Content, pool, count)
		if perr == nil {
			return picks, rejected, acc, lastLatency, nil
		}
		// 校验失败：追加 repair，明确告知只能用候选池中的 symbol。
		symbols := poolSymbolList(pool)
		convo = append(convo,
			chatMessage{Role: "assistant", Content: res.Content},
			chatMessage{Role: "user", Content: "上一条输出不合格：" + perr.Error() +
				"。只能从以下候选池 symbol 中选，严禁使用池外或杜撰的代码：" + symbols +
				"。请重新输出 JSON：{\"picks\":[...],\"rejected\":[...]}，每个 pick 含 symbol、action、confidence、reason、risks、evidence 等字段，rejected 为池内未入选标的的 {symbol,reason} 一句话理由，不要任何解释或代码块标记。"},
		)
	}
	return nil, nil, acc, lastLatency, nil // 降级
}

// reviewPicks AI 复核（verify 模式）：以「风控复核员」独立视角逐条挑刺——核对证据与数据
// 是否一致、风险是否被低估、价位是否合理，输出 pass/warn/reject 与建议置信度。
// best-effort：失败只是没有复核结论，不影响主结果。1 次 repair。
func (s *RecommendationService) reviewPicks(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, recType string, picks []recPick, pool map[string]candidate) ([]pickReview, string, chatUsage) {
	var usage chatUsage
	rows := make([]map[string]any, 0, len(picks))
	for _, p := range picks {
		c := pool[p.Symbol]
		rows = append(rows, map[string]any{
			"pick": p,
			"data": map[string]any{
				"price": c.Price, "change_pct": c.ChangePct, "turnover_rate": c.TurnoverRate,
				"volume_ratio": c.VolumeRatio, "pe_ttm": c.PETTM, "pb": c.PB,
				"float_cap_yi": round2(c.FloatCap / 1e8), "score": c.Score, "rank": c.Rank,
				"factors": c.Factors, "score_dims": c.ScoreDims,
			},
		})
	}
	inputJSON, err := json.Marshal(rows)
	if err != nil {
		return nil, "", usage
	}

	sys := `你是一名独立的风控复核员，逐条审查另一位研究员给出的股票推荐。你不重新选股，只挑刺。审查维度：
1. 证据核对：推荐理由/evidence 引用的数字与 data 中的真实数据是否一致，有没有夸大或想象的成分；
2. 风险完整性：主要风险是否被低估或遗漏（乖离过高、爆量、亏损股、流动性、大盘环境）；
3. 价位合理性（短线）：买入区间是否贴近现价、止损止盈是否合理、盈亏比是否 ≥1.5；
4. 置信度校准：原 confidence 是否过度自信。
每条给出 verdict：pass（无实质问题）/ warn（有值得注意的问题但不至于否决）/ reject（证据与数据明显不符或风险被严重低估，应降级为观察）。
只输出 JSON：{"reviews":[{"symbol":"...","verdict":"pass|warn|reject","comment":"一两句中文说明","confidence":0-100 的复核后建议置信度}],"overall":"整体点评一两句"}，不要任何解释或代码块标记。confidence 给具体数值（reject 时给你认为的真实低值，如 5~25）；填 0 表示维持原置信度不调整。`

	convo := []chatMessage{
		{Role: "system", Content: sys},
		{Role: "user", Content: "推荐与对应真实数据如下（JSON 数组，每项含 pick 与 data）：\n" + string(inputJSON)},
	}
	type reviewOut struct {
		Reviews []pickReview `json:"reviews"`
		Overall string       `json:"overall"`
	}
	for attempt := 0; attempt <= 1; attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model,
			Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
		})
		if err != nil {
			return nil, "", usage
		}
		usage.PromptTokens += res.Usage.PromptTokens
		usage.CompletionTokens += res.Usage.CompletionTokens
		usage.TotalTokens += res.Usage.TotalTokens

		var out reviewOut
		if jerr := json.Unmarshal([]byte(extractJSONObject(res.Content)), &out); jerr == nil && len(out.Reviews) > 0 {
			valid := make([]pickReview, 0, len(out.Reviews))
			seen := map[string]bool{}
			for _, r := range out.Reviews {
				r.Symbol = strings.TrimSpace(r.Symbol)
				r.Verdict = strings.ToLower(strings.TrimSpace(r.Verdict))
				if r.Verdict != "pass" && r.Verdict != "warn" && r.Verdict != "reject" {
					continue
				}
				if _, ok := pool[r.Symbol]; !ok || seen[r.Symbol] {
					continue
				}
				if r.Confidence < 0 {
					r.Confidence = 0
				}
				if r.Confidence > 100 {
					r.Confidence = 100
				}
				r.Comment = truncateRunes(strings.TrimSpace(r.Comment), 300)
				seen[r.Symbol] = true
				valid = append(valid, r)
			}
			if len(valid) > 0 {
				return valid, truncateRunes(strings.TrimSpace(out.Overall), 500), usage
			}
		}
		convo = append(convo,
			chatMessage{Role: "assistant", Content: res.Content},
			chatMessage{Role: "user", Content: "上一条输出不合格。请只输出 JSON：{\"reviews\":[{\"symbol\",\"verdict\":\"pass|warn|reject\",\"comment\",\"confidence\"}],\"overall\":\"...\"}，symbol 必须来自被审推荐。"},
		)
	}
	return nil, "", usage
}

// applyReviews 把复核结论回填到推荐：reject 降级为观察并追加风险；复核置信度非 0 时覆盖。
// reject 无论复核员是否给出置信度，最终置信度强制压到 ≤25——否则「复核否决」徽章
// 与原有的高置信度（如 85）并排展示，自相矛盾误导用户。
func applyReviews(picks []recPick, reviews []pickReview) []recPick {
	bySym := make(map[string]pickReview, len(reviews))
	for _, r := range reviews {
		bySym[r.Symbol] = r
	}
	for i := range picks {
		r, ok := bySym[picks[i].Symbol]
		if !ok {
			continue
		}
		rc := r
		picks[i].Review = &rc
		if r.Confidence > 0 {
			picks[i].Confidence = r.Confidence
		}
		if r.Verdict == "reject" {
			picks[i].Action = model.RecActionWatch
			if picks[i].Confidence > 25 {
				picks[i].Confidence = 25
			}
			note := "AI 复核员否决：" + r.Comment
			if r.Comment == "" {
				note = "AI 复核员否决，建议仅观察"
			}
			picks[i].Risks = append(picks[i].Risks, note)
		}
	}
	return picks
}

// parseAndFilterPicks 解析 picks 并执行反编造过滤（只保留∈候选池的标的），
// 输出条数不超过 maxCount（PRD 3.6：用户可选 3~5 个，模型多给的截断丢弃）。
// 同时解析 rejected（池内落选理由）：仅保留∈候选池且未入选的标的，best-effort、
// 缺失不构成不合格。返回 error（触发 repair）当：JSON 解析失败、缺 picks 字段、
// 或非空 picks 过滤后无任何有效标的。**显式空数组是合法结果**——p4 prompt 明示
// 「宁缺毋滥、picks 可为空」，模型行使拒选权时不得触发 repair 强迫其硬凑标的。
func parseAndFilterPicks(content string, pool map[string]candidate, maxCount int) ([]recPick, []recReject, error) {
	if maxCount <= 0 || maxCount > 5 {
		maxCount = 5
	}
	jsonStr := extractJSONObject(content)
	if jsonStr == "" {
		return nil, nil, errors.New("未找到 JSON 对象")
	}
	var parsed struct {
		Picks    *[]recPick  `json:"picks"` // 指针区分「字段缺失」（repair）与「显式空数组」（合法拒选）
		Rejected []recReject `json:"rejected"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil, nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	if parsed.Picks == nil {
		return nil, nil, errors.New("缺少 picks 字段")
	}

	out := make([]recPick, 0, len(*parsed.Picks))
	seen := map[string]bool{}
	for _, p := range *parsed.Picks {
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
	if len(out) == 0 && len(*parsed.Picks) > 0 {
		return nil, nil, errors.New("推荐的标的均不在候选池内")
	}

	// 落选理由：同样只认池内标的（防杜撰），已入选的不算落选，去重、理由截断。
	rejected := make([]recReject, 0, len(parsed.Rejected))
	seenRej := map[string]bool{}
	for _, r := range parsed.Rejected {
		sym := strings.TrimSpace(r.Symbol)
		if sym == "" || seen[sym] || seenRej[sym] {
			continue
		}
		c, ok := pool[sym]
		if !ok {
			continue
		}
		reason := truncateRunes(strings.TrimSpace(r.Reason), 200)
		if reason == "" {
			continue
		}
		seenRej[sym] = true
		rejected = append(rejected, recReject{Symbol: sym, Name: c.Name, Reason: reason})
	}
	return out, rejected, nil
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
		p.Disclaimer = "本推荐由 AI 模型基于候选池内公开行情与量化因子自动生成，可能存在数字与事实偏差，仅供研究参考，不构成投资建议，历史表现不代表未来，据此操作风险自负。"
	}
	return p
}

// minCandidateAmount PRD 3.6 流动性前置筛选默认值：日成交额不足 1 亿元的标的剔除
// （仅对带成交额的榜单项生效；自选项无成交额数据，以「能取到实时行情」为准入）。
// 用户可在偏好中自定义门槛（min_candidate_amount，0=不过滤）。
const minCandidateAmount = 1e8

// candidateFilter 候选池回避规则（批次G：用户黑名单 + 流动性门槛，来自用户偏好）。
type candidateFilter struct {
	blacklist map[string]bool // key: market:symbol
	minAmount float64         // 最低日成交额（元）；0=不过滤
}

// defaultCandidateFilter 未配置偏好时的缺省规则（与历史行为一致：1 亿元门槛、无黑名单）。
func defaultCandidateFilter() candidateFilter {
	return candidateFilter{minAmount: minCandidateAmount}
}

// loadCandidateFilter 读取用户偏好中的回避规则；偏好缺失/解析失败回退默认。
func loadCandidateFilter(userID int64) candidateFilter {
	f := defaultCandidateFilter()
	if common.DB == nil {
		return f
	}
	var pref model.UserPreference
	if err := common.DB.Where("user_id = ?", userID).First(&pref).Error; err != nil {
		return f
	}
	f.minAmount = pref.MinCandidateAmount
	if strings.TrimSpace(pref.BlacklistJSON) != "" {
		var entries []BlacklistEntry
		if json.Unmarshal([]byte(pref.BlacklistJSON), &entries) == nil {
			f.blacklist = make(map[string]bool, len(entries))
			for _, e := range entries {
				if e.Symbol != "" {
					f.blacklist[e.Market+":"+e.Symbol] = true
				}
			}
		}
	}
	return f
}

// candidateEligible PRD 3.6 推荐前置筛选：排除退市风险（ST/*ST/退市整理）、
// 北交所（数据源不支持，日线/估值均取不到只会挤占评分名额）、停牌或取不到行情、
// 流动性不足、用户黑名单中的标的。候选池是反编造的根基——
// 无行情数据的标的只会诱导 LLM 编造依据，一并拒之门外。
func candidateEligible(c candidate, f candidateFilter) bool {
	name := strings.ToUpper(c.Name)
	if strings.Contains(name, "ST") || strings.Contains(name, "退") {
		return false
	}
	// 北交所前缀（与 limitUpPctFor 口径一致）：43/83/87 及 920 新段。
	if strings.HasPrefix(c.Symbol, "4") || strings.HasPrefix(c.Symbol, "8") || strings.HasPrefix(c.Symbol, "92") {
		return false
	}
	if c.Price <= 0 {
		return false
	}
	if f.minAmount > 0 && c.Amount > 0 && c.Amount < f.minAmount {
		return false
	}
	if f.blacklist[c.Market+":"+c.Symbol] {
		return false
	}
	return true
}

// buildPool 阶段①②：多源建池（自选 ∪ 按策略组合的榜单来源，来源可叠加）→
// 基础准入（candidateEligible：ST/退市/北交所/无行情/流动性/黑名单，不合格不进池）→
// 估值富化 → 用户筛选（applyQuoteFilters，不合格保留并标注 excluded 原因）。
// 返回全量池（含被筛掉者），未被筛掉的数量超过 maxScanCandidates 时后来者标注「池满」。
func (s *RecommendationService) buildPool(ctx context.Context, userID int64, market, recType string, strat *strategyTemplate, filters RecFilters) ([]candidate, error) {
	base := loadCandidateFilter(userID)
	byKey := map[string]int{} // symbol → pool 下标
	pool := make([]candidate, 0, 64)

	add := func(c candidate, source string) {
		if c.Symbol == "" {
			return
		}
		if i, ok := byKey[c.Symbol]; ok {
			// 已在池中：叠加来源，补齐榜单独有的字段（如换手率/估值兜底）。
			if !hasSource(pool[i].Sources, source) {
				pool[i].Sources = append(pool[i].Sources, source)
			}
			if pool[i].Amount == 0 && c.Amount > 0 {
				pool[i].Amount = c.Amount
			}
			if pool[i].TurnoverRate == 0 && c.TurnoverRate > 0 {
				pool[i].TurnoverRate = c.TurnoverRate
			}
			if pool[i].PB == 0 && c.PB > 0 {
				pool[i].PB = c.PB
			}
			if pool[i].FloatCap == 0 && c.FloatCap > 0 {
				pool[i].FloatCap = c.FloatCap
			}
			return
		}
		if len(pool) >= maxPoolIntake {
			return // 总量护栏：极端规模的自选不再扩池（估值批量请求与快照体积都有界）
		}
		if !candidateEligible(c, base) {
			return // 基础准入不合格（ST/退市/北交所/停牌/流动性/黑名单）：噪声，不进池快照
		}
		c.Sources = []string{source}
		byKey[c.Symbol] = len(pool)
		pool = append(pool, c)
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
					Price: round2(it.Price), ChangePct: round2(it.ChangePct)}, "watchlist")
			}
		}
	}

	// 榜单来源按策略组合（strategySources）：热度榜之外补「不热」方向（回调榜/低PB榜），
	// 升序榜前排的极端值由 keep 行级过滤。单榜失败降级不阻断。
	for _, src := range strategySources(recType, strat.Key) {
		rows, err := s.market.GetRanking(ctx, market, src.sort, src.asc, src.limit)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if src.keep != nil && !src.keep(r) {
				continue
			}
			add(candidate{Symbol: r.Symbol, Market: market, Name: r.Name,
				Price: round2(r.Price), ChangePct: round2(r.ChangePct),
				Amount: round2(r.Amount), TurnoverRate: round2(r.TurnoverRate),
				PB: round2(r.PB), FloatCap: round2(r.FloatCap)}, src.label)
		}
	}
	if len(pool) == 0 {
		return pool, nil
	}

	// 估值富化（腾讯免费源 best-effort）：PE/PB/市值/换手/量比/涨停价，
	// 既供筛选与评分，也是「已涨停买不进」的判定依据。单只取不到不阻断；
	// PB/流通市值腾讯缺失时保留新浪榜单兜底值（口径略有差异但优于缺失）。
	refs := make([]QuoteRef, 0, len(pool))
	for _, c := range pool {
		refs = append(refs, QuoteRef{Market: c.Market, Symbol: c.Symbol})
	}
	vals := s.market.ValuationsFor(ctx, refs)
	for i := range pool {
		if v := vals[QuoteKey(pool[i].Market, pool[i].Symbol)]; v != nil {
			pool[i].PETTM = round2(v.PETTM)
			if v.PB > 0 {
				pool[i].PB = round2(v.PB)
			}
			pool[i].TotalCap = round2(v.TotalCap)
			if v.FloatCap > 0 {
				pool[i].FloatCap = round2(v.FloatCap)
			}
			if v.TurnoverRate > 0 {
				pool[i].TurnoverRate = round2(v.TurnoverRate)
			}
			pool[i].VolumeRatio = round2(v.VolumeRatio)
			pool[i].Amplitude = round2(v.Amplitude)
			pool[i].LimitUp = round2(v.LimitUp)
		}
	}

	// 用户筛选（透明标记式排除），随后按来源轮转发放评分名额。
	for i := range pool {
		if reason := applyQuoteFilters(pool[i], filters); reason != "" {
			pool[i].Excluded = reason
		}
	}
	assignScanQuota(pool)
	return pool, nil
}

// assignScanQuota 评分名额分配：通过用户筛选的候选按「首来源」分组（保池序），
// 自选整组优先（用户已研究的标的不参与竞争），其余组逐轮各出一只轮转发放，
// 直到发满 maxScanCandidates，落选者标「池满」（scorePool 补位轮按前缀识别回补）。
// 不轮转的话名额按进池顺序先到先得，第一路榜单会垄断整个量化窗口（如 pullback
// 的回调票 50+ 只吃掉全部 48 席，成交额/涨幅路的强势票全部沦为「池满」）。
func assignScanQuota(pool []candidate) {
	var order []string
	groups := map[string][]int{}
	for i := range pool {
		if pool[i].Excluded != "" {
			continue
		}
		src := ""
		if len(pool[i].Sources) > 0 {
			src = pool[i].Sources[0]
		}
		if _, ok := groups[src]; !ok {
			order = append(order, src)
		}
		groups[src] = append(groups[src], i)
	}

	seq := append([]int(nil), groups["watchlist"]...)
	rest := make([][]int, 0, len(order))
	for _, src := range order {
		if src != "watchlist" {
			rest = append(rest, groups[src])
		}
	}
	for added := true; added; {
		added = false
		for gi := range rest {
			if len(rest[gi]) > 0 {
				seq = append(seq, rest[gi][0])
				rest[gi] = rest[gi][1:]
				added = true
			}
		}
	}
	for n, i := range seq {
		if n >= maxScanCandidates {
			pool[i].Excluded = fmt.Sprintf("%s（评分名额 %d），未进入量化评分", poolFullPrefix, maxScanCandidates)
		}
	}
}

func hasSource(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// marshalPoolSnapshot 候选池快照序列化：条目超上限时优先保留参与排名/评分的标的，
// 被排除者按序截断（MySQL TEXT 列 64KB 容量保护——大自选用户全量落库会使批次
// INSERT 失败，而 token 已消耗）。返回 JSON 与被省略的条目数（记入 filters_json 供前端提示）。
func marshalPoolSnapshot(pool []candidate) (string, int) {
	if len(pool) <= poolSnapshotMax {
		b, _ := json.Marshal(pool)
		return string(b), 0
	}
	keep := make([]candidate, 0, poolSnapshotMax)
	var excluded []candidate
	for _, c := range pool {
		if c.Excluded == "" {
			keep = append(keep, c)
		} else {
			excluded = append(excluded, c)
		}
	}
	omitted := 0
	for _, c := range excluded {
		if len(keep) >= poolSnapshotMax {
			omitted++
			continue
		}
		keep = append(keep, c)
	}
	b, _ := json.Marshal(keep)
	return string(b), omitted
}

// scorePool 阶段③：对未被排除的候选拉日线算技术因子，五维评分 + 策略加分合成量化分，
// 追高保护在此判定（需要近 5 日涨幅），最后按分排名、Top maxLLMCandidates 标记进入 LLM。
// 日线拉取失败的标的**透明排除**（"日线数据获取失败"）而非按中性 50 分混入排名——
// 否则既能挤占名单名额、又静默绕过近 5 日涨幅追高保护。追高/无日线剔除释放的名额
// 会从「池满」标的中按进池顺序补评一轮（只补一轮，拉取总量仍有界）。
func (s *RecommendationService) scorePool(ctx context.Context, recType string, strat *strategyTemplate, pool []candidate, filters RecFilters) {
	sentiDate := time.Now().Format("2006-01-02")
	// scoreRound 对给定下标拉日线并评分；返回本轮存活（未被排除）的数量。
	scoreRound := func(idxs []int) int {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6) // 并发上限：48 只 × 210 根日线约 3~8s，免费源经不起更狠的并发
		var mu sync.Mutex
		barsBy := map[int][]datasource.Bar{}
		for _, i := range idxs {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				// 拉 chipBarLimit=210 根：同一个上游请求（仅行数差异），一次取数
				// 同时满足筹码累积窗口（210）与技术因子窗口（尾部 90）。
				bars, err := s.market.GetDailyBars(ctx, pool[i].Market, pool[i].Symbol, chipBarLimit)
				if err != nil {
					return
				}
				mu.Lock()
				barsBy[i] = bars
				mu.Unlock()
			}(i)
		}
		wg.Wait()

		alive := 0
		for _, i := range idxs {
			bars := barsBy[i]
			if len(bars) == 0 {
				pool[i].Excluded = "日线数据获取失败，未参与量化评分（避免以中性分混入排名并绕过追高保护）"
				continue
			}
			// 五维评分必须用尾部 factorBarLimit=90 根：positionScore 是全窗口径，
			// 直接喂 210 根会把「90 日区间位置」悄悄漂成 210 日（Pos60 同类前科）。
			// computeCandFactors 内部全部是尾窗/递推口径，吃全长只会让 RSI/MACD
			// 的递推 seed 误差更小，不漂移。
			barsScore := bars
			if len(barsScore) > factorBarLimit {
				barsScore = barsScore[len(barsScore)-factorBarLimit:]
			}
			sc := computeScore(pool[i].Price, barsScore)
			pool[i].ScoreDims = &scoreDims{Trend: sc.Trend, Momentum: sc.Momentum, Position: sc.Position, Volume: sc.Volume, Risk: sc.Risk}
			pool[i].Factors = computeCandFactors(pool[i].Price, bars)

			// T1 筹码分布（零上游成本本地复算）：获利盘进因子与评分。
			// 失败（次新股不足 120 根/换手缺失）静默缺席——ChipBars=0 即「未算」。
			if chip, err := computeChipDistribution(bars, 0); err == nil {
				pool[i].Factors.ChipProfit = chip.Profit
				pool[i].Factors.ChipAvgCost = chip.AvgCost
				pool[i].Factors.ChipBars = chip.BarCount
			}

			// 追高保护（依赖近 5 日涨幅因子）。
			if reason := applyGainFilter(pool[i], pool[i].Factors, filters); reason != "" {
				pool[i].Excluded = reason
				continue
			}

			// 换手 20~30% 区间的位置判定（依赖 pos_60 因子）：高位=死亡换手排除；
			// 低位保留但下方统一扣分标注（放量启动与对倒出货并存，透明交给用户权衡）。
			if reason := applyTurnoverPosFilter(pool[i], pool[i].Factors); reason != "" {
				pool[i].Excluded = reason
				continue
			}

			// N2 消息面因子：当日聚合情绪分（缓存表命中即读，缺则由当日新闻合成一次）。
			if sc, cnt, ok := stockDailySentiment(pool[i].Symbol, sentiDate); ok {
				pool[i].SentiScore, pool[i].SentiNews = sc, cnt
			}

			delta, notes := strategyAdjust(recType, strat.Key, pool[i], pool[i].Factors)
			if pool[i].TurnoverRate > deadTurnoverPct {
				// 低位高换手分级扣分：25~30% 比 20~25% 更接近极端换手，扣更重，
				// 抵消五维量能维对爆量的加分（否则净效应近乎中性，惩罚失真）。
				penalty, band := 5.0, "20~25%"
				if pool[i].TurnoverRate > 25 {
					penalty, band = 8, "25~30%"
				}
				delta -= penalty
				notes = append(notes, fmt.Sprintf("低位高换手 %.1f%%（%s 档）：放量启动与对倒出货并存，需谨慎（-%.0f）", pool[i].TurnoverRate, band, penalty))
			}
			pool[i].Score = round2(clamp0100(sc.Total + delta))
			pool[i].Bonus = notes
			alive++
		}
		return alive
	}

	first := make([]int, 0, maxScanCandidates)
	for i := range pool {
		if pool[i].Excluded == "" {
			first = append(first, i)
		}
	}
	alive := scoreRound(first)

	// 名额补位：被追高/无日线剔除的名额还给「池满」标的（按原进池顺序），补评一轮。
	if freed := maxScanCandidates - alive; freed > 0 {
		refill := make([]int, 0, freed)
		for i := range pool {
			if strings.HasPrefix(pool[i].Excluded, poolFullPrefix) {
				pool[i].Excluded = ""
				refill = append(refill, i)
				if len(refill) >= freed {
					break
				}
			}
		}
		if len(refill) > 0 {
			scoreRound(refill)
		}
	}

	type scored struct {
		idx   int
		score float64
	}
	ranked := make([]scored, 0, len(pool))
	for i := range pool {
		if pool[i].Excluded == "" && pool[i].ScoreDims != nil {
			ranked = append(ranked, scored{idx: i, score: pool[i].Score})
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })
	for rank, r := range ranked {
		pool[r.idx].Rank = rank + 1
		if rank < maxLLMCandidates {
			pool[r.idx].SentToLLM = true
		}
	}
}

// recMarketContext LLM 精选时的市场环境锚点（给模型「今天是什么行情」的客观参照，
// 防止脱离大盘环境给出激进判断）。全部真实数据，缺失块置空。
type recMarketContext struct {
	Indices    []map[string]any `json:"indices,omitempty"`
	Breadth    map[string]any   `json:"breadth,omitempty"`
	MainNetYi  float64          `json:"main_fund_net_yi,omitempty"` // 两市主力净流入（亿元）
	BenchTrend string           `json:"bench_trend,omitempty"`      // 基准指数与长均线的位置关系
}

func (s *RecommendationService) buildMarketContext(ctx context.Context, market string) *recMarketContext {
	mc := &recMarketContext{}
	if ov := s.market.GetOverview(ctx, market); ov != nil {
		for i, ix := range ov.Indices {
			if i >= 3 {
				break
			}
			mc.Indices = append(mc.Indices, map[string]any{"name": ix.Name, "change_pct": round2(ix.ChangePct)})
		}
		if ov.Breadth != nil {
			mc.Breadth = map[string]any{
				"advances": ov.Breadth.Advances, "declines": ov.Breadth.Declines,
				"limit_up": ov.Breadth.LimitUp, "limit_down": ov.Breadth.LimitDown,
			}
		}
		if ov.FundFlow != nil {
			mc.MainNetYi = round2(ov.FundFlow.MainNet / 1e8)
		}
	}
	// 基准趋势（上证 vs MA60/MA200）：大盘弱势时模型应更保守。失败静默缺席。
	if _, bars, err := s.market.GetBenchmarkBars(ctx, market, 250); err == nil && len(bars) > 0 {
		closes := make([]float64, len(bars))
		for i, b := range bars {
			closes[i] = b.Close
		}
		last := closes[len(closes)-1]
		parts := []string{}
		if ma60, ok := movingAverage(closes, 60); ok {
			if last >= ma60 {
				parts = append(parts, "上证收于MA60上方")
			} else {
				parts = append(parts, "上证收于MA60下方（中期趋势偏弱）")
			}
		}
		if ma200, ok := movingAverage(closes, 200); ok {
			if last >= ma200 {
				parts = append(parts, "MA200上方")
			} else {
				parts = append(parts, "MA200下方（长期弱势，建议整体保守）")
			}
		}
		mc.BenchTrend = strings.Join(parts, "，")
	}
	return mc
}

// compactForLLM 生成喂给 LLM 的候选行（仅入选名单、按 rank 升序、字段紧凑）。
func compactForLLM(recType string, cands []candidate) []map[string]any {
	sorted := append([]candidate(nil), cands...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].Rank < sorted[b].Rank })
	rows := make([]map[string]any, 0, len(sorted))
	for _, c := range sorted {
		row := map[string]any{
			"symbol": c.Symbol, "name": c.Name, "price": c.Price, "change_pct": c.ChangePct,
			"score": c.Score, "rank": c.Rank, "sources": c.Sources,
		}
		if c.Amount > 0 {
			row["amount_yi"] = round2(c.Amount / 1e8)
		}
		if c.TurnoverRate > 0 {
			row["turnover_rate"] = c.TurnoverRate
		}
		if c.VolumeRatio > 0 {
			row["volume_ratio"] = c.VolumeRatio
		}
		if c.FloatCap > 0 {
			row["float_cap_yi"] = round2(c.FloatCap / 1e8)
		}
		if c.PETTM != 0 {
			row["pe_ttm"] = c.PETTM
		}
		if c.PB != 0 {
			row["pb"] = c.PB
		}
		if c.SentiNews > 0 {
			row["senti_score"] = c.SentiScore
			row["senti_news"] = c.SentiNews
		}
		if c.ScoreDims != nil {
			row["score_dims"] = c.ScoreDims
		}
		if c.Factors != nil {
			row["factors"] = c.Factors
		}
		if len(c.Bonus) > 0 {
			row["strategy_notes"] = c.Bonus
		}
		rows = append(rows, row)
	}
	return rows
}

// composeBatchTitle 用筛选条件组合生成批次标题（落库固化，历史列表稳定展示）。
// 形如「短线·动量突破 · ≤30元 · 3只」；条件多时只取最关键的一项（价格 > 市值）。
func composeBatchTitle(recType string, strat *strategyTemplate, filters RecFilters, count int) string {
	parts := []string{recTypeLabel(recType) + "·" + strat.Name}
	desc := filters.Describe()
	for _, d := range desc {
		if strings.HasPrefix(d, "股价") {
			parts = append(parts, strings.TrimPrefix(d, "股价"))
			break
		}
	}
	if len(parts) == 1 {
		for _, d := range desc {
			if strings.HasPrefix(d, "流通市值") {
				parts = append(parts, strings.TrimPrefix(d, "流通市值"))
				break
			}
		}
	}
	parts = append(parts, fmt.Sprintf("%d只", count))
	return truncateRunes(strings.Join(parts, " · "), 128)
}

// buildMessages 组装系统提示 + 用户消息。p4：候选已由量化系统筛选评分排序，
// LLM 的角色是「精选 + 解读 + 否决」而非海选；强制引用字段数值、禁用先验记忆、允许少选。
func (s *RecommendationService) buildMessages(recType string, strat *strategyTemplate, market string, count int, llmCands []candidate, filters RecFilters, mktCtx *recMarketContext) []chatMessage {
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
	if mktCtx != nil {
		if b, err := json.Marshal(mktCtx); err == nil {
			u.WriteString("【市场环境】（真实数据，作为整体判断的锚点；大盘弱势时应整体保守、可少选）：\n")
			u.Write(b)
			u.WriteString("\n\n")
		}
	}
	if desc := filters.Describe(); len(desc) > 0 {
		u.WriteString("【用户约束】" + strings.Join(desc, "；") + "。名单已按这些条件过滤。")
		if filters.PriceMax > 0 {
			fmt.Fprintf(&u, " 用户资金有限（A股一手=100股，一手成本=价格×100），价位越贴近其预算越实用。")
		}
		u.WriteString("\n\n")
	}
	fmt.Fprintf(&u, "请从以下【量化初选名单】中，按「%s」策略精选至多 %d 个%s标的。\n", strat.Name, count, recTypeLabel(recType))
	u.WriteString("名单已按量化综合分（score，0-100）降序排列，rank=1 为最高分。score 由五维技术评分（趋势/动量/位置/量能/风险，score_dims）加策略加分项（strategy_notes）合成，仅有排序意义、不代表预期收益。\n")
	fmt.Fprintf(&u, "硬性要求：只能从名单里选，symbol 必须与名单完全一致，严禁名单外或虚构的标的；名单中符合策略的合格标的充足时应给足 %d 个，确实不足时宁可少选甚至不选（picks 可为空数组），绝不硬凑。你可以不同意量化排序（例如否决 rank 靠前者），但必须用名单中的数据说明理由。\n", count)
	u.WriteString("同时请在 rejected 数组中，对名单内未入选的标的各给一句话落选理由，只解释名单内标的。\n\n")
	u.WriteString("【量化初选名单】（JSON；price 现价、change_pct 当日涨跌%、amount_yi 成交额亿元、turnover_rate 换手%、volume_ratio 量比、float_cap_yi 流通市值亿元、pe_ttm 市盈率TTM（负=亏损）、pb 市净率、senti_score 当日新闻聚合情绪分-1~1（senti_news 为条数，字段缺失=当日无相关新闻，不得臆测消息面）；factors：ma5/ma10/ma20/ma60 均线、chg_5d/chg_20d 近5/20日涨跌%、high_20d 创20日新高、bull_align 多头排列、vol_boost 今日量/5日均量、bias_20 MA20乖离%、volatility_20 波动率%、drawdown_20 近20日最大回撤%、pos_60 60日区间位置、rsi_14 RSI(14,Wilder)、macd_dif/macd_dea/macd_hist MACD(12,26,9)（柱=2×(DIF−DEA)）、macd_gold DIF在DEA上方、macd_cross_up 近3日金叉、boll_up/boll_mid/boll_low 布林带(20,2σ)、boll_pos 布林带内位置%、atr_14/atr_pct 真实波幅及其占现价%、chip_profit 获利盘%（收盘价下方筹码占比）、chip_avg_cost 筹码平均成本（chip_bars 为筹码窗口根数，缺失=未计算，不得臆测筹码面）；指标/估值字段缺失表示该数据暂不可得，不得臆测）：\n")
	if b, err := json.Marshal(compactForLLM(recType, llmCands)); err == nil {
		u.Write(b)
	}
	u.WriteString("\n\n请只输出 JSON：{\"picks\":[...],\"rejected\":[{\"symbol\":\"...\",\"reason\":\"一句话\"}]}。")

	return []chatMessage{
		{Role: "system", Content: sys.String()},
		{Role: "user", Content: u.String()},
	}
}

const recRoleIntro = `你是一名严谨的证券研究员，服务于个人投资研究工具。量化系统已完成候选筛选、技术因子计算与评分排序，你的任务是在此基础上精选、解读与否决——不是重新海选。你的输出仅供研究参考，不构成任何投资建议或买卖指令。

铁律：
1. 只能从【量化初选名单】中挑选，symbol 必须与名单完全一致；严禁推荐名单外标的或杜撰任何代码/数据。
2. 只依据名单与市场环境中给出的数据分析。禁止使用你记忆中关于任何公司的信息——名气、行业地位、历史印象、新闻记忆都不算数据，只看本次给出的数字。数据不足处如实说明局限，不臆测未提供的财务/消息。
3. 每个标的必须给出：理由(reason)、风险(risks)、数据依据(evidence)。evidence 每条都要引用具体字段名与数值（如「score=78.5 池内第1」「换手率6.3%、量比2.1 温和放量」「bias_20=+4.2% 未超买」）。系统会程序化核对你引用的数字，与数据不符的证据会被标记出来展示给用户。
4. 宁缺毋滥但不无故少给：名单中符合策略且证据充分的标的足够时，应给足要求的数量；证据不足或与策略不符时明确不选，picks 可以少于要求数量甚至为空。不得为凑满数量而降低证据标准——第 N 只的证据强度不应明显弱于第 1 只；对勉强符合策略的边际标的，用 action="watch" 并如实给低 confidence，而不是凑成 buy。落选与入选都要有数据依据。
5. 名单内未入选的标的，在 rejected 数组中各给一句话落选理由（{"symbol","reason"}）。
6. 全程简体中文。只输出一个 JSON 对象 {"picks":[...],"rejected":[...]}，不要任何解释文字或 Markdown 代码块标记。`

const shortTermSpec = `本次为【短线推荐】。每个 pick 需包含字段：
- symbol: 名单中的代码
- action: "buy"(可考虑买入) 或 "watch"(观察等待)
- confidence: 0-100 整数
- reason: 字符串数组，选择理由（技术面/量价/热点）
- risks: 字符串数组，主要风险
- evidence: 字符串数组，数据依据（引用名单中的具体字段与数值）
- buy_zone_low / buy_zone_high: 买入观察区间（下沿/上沿价格）
- take_profit: 止盈目标价
- stop_loss: 止损价
- valid_days: 该短线机会的有效天数（交易日，通常 3-10）
- invalidation: 失效条件（一句话，如"跌破止损价或放量破位"）
- disclaimer: 风险与免责提示
交易规则硬约束：当前数据源仅支持 A 股；A 股当日买入不可当日卖出(T+1)，止盈/止损最早次一交易日生效；必须考虑涨跌停限制，涨停可能买不进、跌停可能卖不出；最小交易单位为 100 股一手；有效期和持有周期都按交易日计算，不按自然日。
价位纪律：要求止盈>买入区间上沿>买入区间下沿>止损，价格贴近现价合理设置；止损建议参考 MA20 附近或现价-5%~-7%（可用名单中 factors.ma20 锚定）；止盈到止损的距离比（盈亏比）至少 1.5，不足时降为 watch 并说明。`

const longTermSpec = `本次为【长线推荐】。名单含实时行情、估值快照（PE-TTM/PB/市值）与技术因子，但缺少财务三表明细（营收/利润/负债/现金流），请基于可得信息给出中长期视角，并明确指出财务明细的缺失。每个 pick 需包含字段：
- symbol: 名单中的代码
- action: "buy"(可考虑逢低布局) 或 "watch"(观察等待)
- confidence: 0-100 整数
- reason: 字符串数组，长期看好/关注的理由
- risks: 字符串数组，主要风险
- evidence: 字符串数组，数据依据（引用名单中的具体字段与数值，含 PE/PB/市值等估值依据）
- thesis: 基本面/投资逻辑（一段话；估值判断只能基于名单给出的 PE/PB/市值绝对水位，不得虚构行业对比或财务明细）
- valuation_low / valuation_high: 合理估值区间（若估值数据缺失无法给出可填 0 并在 thesis 说明）
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
	err := q.Select("id", "user_id", "type", "market", "strategy", "title", "status", "error",
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
	// 附对应持仓（血缘：一键建仓写入 recommendation_id；同一推荐多笔建仓取最早一笔）。
	posLinks := map[int64]RecPositionLink{}
	if len(items) > 0 {
		recIDs := make([]int64, 0, len(items))
		for _, it := range items {
			recIDs = append(recIDs, it.ID)
		}
		var prows []model.Position
		common.DB.Where("user_id = ? AND recommendation_id IN ?", userID, recIDs).Order("id").Find(&prows)
		for _, p := range prows {
			if _, ok := posLinks[p.RecommendationID]; !ok {
				posLinks[p.RecommendationID] = RecPositionLink{
					PositionID: p.ID, BuyPrice: p.BuyPrice, BuyDate: p.BuyDate,
					Quantity: p.Quantity, Status: p.Status,
				}
			}
		}
	}
	return s.assembleView(batch, items, statuses, posLinks), nil
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

// assembleView 组装批次视图（解析条目明细，附可选追踪状态与持仓血缘）。
func (s *RecommendationService) assembleView(batch model.RecommendationBatch, items []model.Recommendation, statuses map[int64]model.RecommendationStatus, posLinks map[int64]RecPositionLink) *RecommendationView {
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
		if pl, ok := posLinks[it.ID]; ok {
			p := pl
			iv.Position = &p
		}
		views = append(views, iv)
	}
	return &RecommendationView{RecommendationBatch: batch, Items: views}
}
