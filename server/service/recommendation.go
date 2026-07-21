package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	em        *datasource.EastMoneyAdapter // M3a 资金流历史按需补拉（缓存冷时预算内回上游）
}

func NewRecommendationService(market *MarketService, watchlist *WatchlistService, llm *LLMService) *RecommendationService {
	return &RecommendationService{market: market, watchlist: watchlist, llm: llm, em: datasource.NewEastMoneyAdapter()}
}

const (
	recPromptVersion   = "p11" // p11: 输出瘦身（reason/risks/evidence 条数与单条字数上限、落选理由 ≤20 字，压单次生成时长进上游 60s 窗口）+ LLM 名单 16→10；p10: M3b 盘中因子字段说明（尾盘涨幅/量占比/早盘/VWAP）；p9: M3a 龙虎榜/机构/人气榜/主力资金流字段说明；p8: F2 长线名单新增 fin 财务摘要（ROE/营收净利增速/毛利率）字段说明、longTermSpec 撤「缺财务明细」声明；p7: T1 技术指标与筹码字段说明；p6: senti 消息面字段；p5: 来源随策略组合；p4: 四阶段流水线；p3: 落选理由；p2: 估值富化
	recStrategyVersion = "s9"  // s9: S1-3 名单去相关（相关性去重+同行业≤2 只，被挤出者记反事实事件）；s8: M3b 盘中因子短线加分项（尾盘放量拉升/跳水/收盘vs VWAP/午后重心上移/早盘强势）；s7: M3a 龙虎榜净买/机构席位/人气跃升/主力连续净流入加分项 + 量能维融合主力资金分；s6: F2 财务加分项（value ROE/growth 双增速/leader 盈利质量 + 业绩恶化通用扣分）；s5: T1 指标加分项 + 筹码超跌 + 五维动量/风险维升级；s4: 消息面情绪因子；s3: 策略-来源映射 + 换手分位化；s2: 本地量化评分；s1: 纯 prompt 导向
	maxScanCandidates  = 48    // 进入量化评分的候选上限（约束日线拉取量：48 只 × 1 次 HTTP，并发 6 约 3~8s）
	maxLLMCandidates   = 10    // 量化排序后进入 LLM 精选的名单上限（2026-07-14 16→10：上游 60s 整包超时下压输入与落选理由输出量；控上下文与位置偏差面）
	factorBarLimit     = 90    // 五维评分/窗口因子的日线口径（MA60 需 ≥60，留余量）；实际拉取按 chipBarLimit=210，评分前截尾
	maxPoolIntake      = 240   // 建池总量护栏（自选无上限，防极端用户打爆估值批量请求）
	poolSnapshotMax    = 150   // 候选池快照落库条目上限（MySQL TEXT 64KB 容量保护，超出部分只记数量）

	// 异步任务化（2026-07-14）：手动生成立即返回 processing 批次，后台独立 Context 完成后回写。
	recJobTimeout      = 6 * time.Minute  // 后台生成任务总 deadline（建池+评分 3~8s + LLM 主调/repair/复核）
	recProcessingStale = 15 * time.Minute // processing 批次超过该时长视为死任务（进程重启遗留），惰性判 failed

	// 模块级输出预算与 repair 次数（P0-9）：已收口 llm_budget.go 模块预算表
	//（recommendation 2500/rec_review 1500/rec_bear 1500、repair 均 1 次、坏输出回灌 600 字）。
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

	Sources    []string     `json:"sources,omitempty"`     // watchlist / gainer / active / turnover（可多来源）
	Excluded   string       `json:"excluded,omitempty"`    // 非空=被用户筛选/风控排除的原因（透明可查）
	Factors    *candFactors `json:"factors,omitempty"`     // 技术因子快照（210 根日线派生：窗口因子尾窗口径、指标递推、筹码累积）
	Fin        *candFin     `json:"fin,omitempty"`         // F2 财务摘要（长线：最新一期 ROE/增速/毛利率；缺失=无缓存且预算耗尽）
	ScoreDims  *scoreDims   `json:"score_dims,omitempty"`  // 五维评分明细
	SentiScore float64      `json:"senti_score,omitempty"` // N2 当日聚合情绪分 -1~1（新闻加权合成）
	SentiNews  int          `json:"senti_news,omitempty"`  // 参与聚合的新闻条数（0=当日无相关新闻）
	// M3a 扩展信号（最近有数据交易日的口径，通常为 T-1 信息；缺失=未上榜/无快照）。
	LhbNetYi  float64  `json:"lhb_net_yi,omitempty"` // 龙虎榜净买额（亿元，负=净卖出）
	LhbReason string   `json:"lhb_reason,omitempty"` // 上榜原因
	OrgNetYi  float64  `json:"org_net_yi,omitempty"` // 机构席位净买额（亿元，负=净卖出）
	OrgBuys   int      `json:"org_buys,omitempty"`   // 机构席位买入次数
	PopRank   int      `json:"pop_rank,omitempty"`   // 股吧人气榜名次（1~100）
	PopPrev   int      `json:"pop_prev,omitempty"`   // 昨日名次（<=0=新上榜）
	PopNew    bool     `json:"pop_new,omitempty"`    // 人气榜新上榜
	Score     float64  `json:"score,omitempty"`      // 量化综合分 0-100（五维基础分 + 策略加分）
	Rank      int      `json:"rank,omitempty"`       // 未被排除者中的排名（1=最高）
	Bonus     []string `json:"bonus,omitempty"`      // 策略加分/扣分明细（可解释）
	SentToLLM bool     `json:"sent_to_llm,omitempty"`
	// QuoteAsOf 行情时效硬门通过时的数据源行情时刻（YYYY-MM-DD HH:MM）：终选名单喂
	// LLM 前逐只全源核验并刷新现价，此字段随池快照/pick 明细落库，标明推荐建立在
	// 哪个时点的行情上（字符串不进数值核验值域）。
	QuoteAsOf string `json:"quote_as_of,omitempty"`

	// 进程内工作字段（小写不序列化，不进池快照）：closes/closeDates 尾部收盘序列及
	// 其交易日（S1-3 相关性去重与 S1-2 相关性系数——按交易日交集对齐，任一股停牌
	// 时相同下标对应不同交易日，位置对齐会失真）；lastBar* 生成时点最近收盘日与
	// 收盘价（S0-4 价格版本，防前复权重锚——落库进 Recommendation.RefDate/RefClose
	// 与标签行）。
	closes       []float64
	closeDates   []string
	lastBarDate  string
	lastBarClose float64
}

// sourceLabelCN 候选来源的中文标签（落库英文 key，前端映射展示）。
var sourceLabelCN = map[string]string{
	"watchlist":       "自选",
	"gainer":          "涨幅榜",
	"active":          "成交额榜",
	"turnover":        "换手率榜",
	"dipper":          "回调榜",
	"lowpb":           "低PB榜",
	"strategy_signal": "策略信号",
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
	// BearCheck S2-2 反方研究员（影子）：对每只 buy 额外一次独立调用构建最强 bear case，
	// 只展示不改写。nil = 默认关联 Verify（复核开则反方也开）；显式 true/false 覆盖。
	BearCheck *bool `json:"bear_check"`
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
	// PositionPct S1-2 建议仓位（占总资金 %，目标波动模型程序计算，非 LLM 输出；
	// 0=无法给出，PositionWhy 说明公式因子与原因）。
	PositionPct      float64        `json:"position_pct,omitempty"`
	PositionWhy      string         `json:"position_why,omitempty"`
	QuantScore       float64        `json:"quant_score,omitempty"`        // 量化综合分
	QuantRank        int            `json:"quant_rank,omitempty"`         // 池内排名
	PoolSize         int            `json:"pool_size,omitempty"`          // 参与排名的标的数
	LotCost          float64        `json:"lot_cost,omitempty"`           // 一手(100股)成本（元）
	EvidenceCheck    *evidenceCheck `json:"evidence_check,omitempty"`     // 证据数字核验结果
	SysConfidence    string         `json:"sys_confidence,omitempty"`     // 程序合成置信度 high/medium/low
	SysConfidenceWhy string         `json:"sys_confidence_why,omitempty"` // 置信度依据说明
	Review           *pickReview    `json:"review,omitempty"`             // AI 复核员结论（verify 模式）
	// Bear S2-2 反方研究员结论（影子：只展示，不改写 action/置信度；severity=high 的
	// buy 另记 gate_type=bear_shadow 反事实事件供影子收益对照）。
	Bear *pickBear `json:"bear,omitempty"`
	// QualityGate S2-3 数据质量门控影子输出（只记录 would-be 封顶与缺失面，不实际封顶）。
	QualityGate *qualityGateResult `json:"quality_gate,omitempty"`
	// DegradedSource 非空 = 该条为降级生成（"quant_fallback"：AI 精选超时后按量化排名
	// 规则合成，计划价为 ATR 规则价、未经 AI 解读）。前端据此展示降级标签。
	DegradedSource string `json:"degraded_source,omitempty"`
	// QuoteAsOf 该条推荐所依据行情的数据源时刻（服务端从候选回填，模型无权自附）。
	QuoteAsOf string `json:"quote_as_of,omitempty"`
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

// Generate 生成一批推荐（用户手动发起，计 1 次配额）——异步任务化（2026-07-14）：
// 同步段只做参数校验/LLM 配置解析/配额熔断，落一条 processing 批次后立即返回；
// 建池/评分/LLM 精选在后台以独立 Context 执行，完成后回写批次。浏览器超时、反代
// 掐断、页面刷新都不再中断任务；前端轮询 GET /recommendations/:id 直到脱离 processing。
// allowPrivate 由调用方按角色决定（管理员可访问内网自建模型）。
func (s *RecommendationService) Generate(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
	plan, err := s.prepareGeneration(userID, allowPrivate, req, true)
	if err != nil {
		return nil, err
	}
	// 幂等防重：该用户仍有生成中的批次时直接复用（重复点击/刷新不重复建任务烧 token）。
	if v := s.reuseProcessingBatch(userID); v != nil {
		return v, nil
	}
	batch := plan.newProcessingBatch()
	if err := common.DB.Create(batch).Error; err != nil {
		return nil, err
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				common.SysWarn("推荐后台任务 panic batch=%d: %v", batch.ID, r)
				s.failBatch(batch, chatUsage{}, 0, fmt.Sprintf("生成任务异常终止: %v", r))
			}
		}()
		// 独立于 HTTP 请求的 Context：浏览器断开不取消任务；总 deadline 防永久挂起。
		bg, cancel := context.WithTimeout(context.Background(), recJobTimeout)
		defer cancel()
		if _, err := s.runGeneration(bg, batch, plan); err != nil {
			common.SysWarn("用户 %d 推荐生成失败 batch=%d: %v", userID, batch.ID, err)
		}
	}()
	return &RecommendationView{RecommendationBatch: *batch, Items: []RecommendationItemView{}}, nil
}

// GenerateAuto 后台任务（收盘日报）代用户生成：token 照记审计，但不消耗次数配额。
// 调用方已在自己的后台 goroutine 与 deadline 内，保持同步执行返回最终结果；
// 批次同样先落 processing 行再回写，与手动路径完全同链路。
func (s *RecommendationService) GenerateAuto(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
	plan, err := s.prepareGeneration(userID, allowPrivate, req, false)
	if err != nil {
		return nil, err
	}
	batch := plan.newProcessingBatch()
	if err := common.DB.Create(batch).Error; err != nil {
		return nil, err
	}
	return s.runGeneration(ctx, batch, plan)
}

// recGenPlan 同步段产出、后台段消费的生成计划（校验通过的参数 + 解析后的 LLM 配置）。
type recGenPlan struct {
	userID       int64
	allowPrivate bool
	manualAction bool
	recType      string
	market       string
	count        int
	strat        *strategyTemplate
	filters      RecFilters
	verify       bool
	bear         bool // S2-2 反方研究员（未显式指定时关联 verify）
	cfg          *model.LLMConfig
	apiKey       string
}

// prepareGeneration 同步段：参数校验、LLM 配置解析、配额熔断、筛选条件装载。
// 确定性错误（类型/策略非法、无 LLM、配额尽）立即返回给用户，不建任务。
func (s *RecommendationService) prepareGeneration(userID int64, allowPrivate bool, req RecommendRequest, manualAction bool) (*recGenPlan, error) {
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

	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, err
	}
	allowPrivate = llmAllowPrivate(allowPrivate, cfg) // 回退到管理员配置时按配置所有者放行内网

	if err := checkQuota(userID); err != nil {
		return nil, err
	}

	// 筛选条件：请求携带 > 用户偏好 > 类型默认。全程落库可回显。
	var filters RecFilters
	if req.Filters != nil {
		filters = sanitizeRecFilters(*req.Filters)
	} else {
		filters = loadUserRecFilters(userID, req.Type)
	}
	// S2-2 反方研究员开关：未显式指定时关联 verify（复核开则反方也开），
	// 调用预算 = 主调 1 + 复核 1 + 反方 1 ≤ 3 次上限。
	bear := req.Verify
	if req.BearCheck != nil {
		bear = *req.BearCheck
	}
	return &recGenPlan{
		userID: userID, allowPrivate: allowPrivate, manualAction: manualAction,
		recType: req.Type, market: market, count: count, strat: strat,
		filters: filters, verify: req.Verify, bear: bear, cfg: cfg, apiKey: apiKey,
	}, nil
}

// newProcessingBatch 由计划生成 processing 批次行：元数据（类型/策略/标题/模型/版本）
// 即时可见，池快照与结果由后台回写。
func (p *recGenPlan) newProcessingBatch() *model.RecommendationBatch {
	return &model.RecommendationBatch{
		UserID: p.userID, Type: p.recType, Market: p.market, Strategy: p.strat.Key,
		Title:       composeBatchTitle(p.recType, p.strat, p.filters, p.count),
		Status:      model.RecStatusProcessing,
		LLMConfigID: p.cfg.ID, Provider: p.cfg.Provider, Model: p.cfg.Model,
		// M3c：启用 recommend 自定义模板时版本号加 -custom 后缀（同分析域前例，历史可归因）。
		PromptVersion:   promptVersionFor(p.userID, model.PromptModuleRecommend, recPromptVersion),
		StrategyVersion: recStrategyVersion,
		// P0-2：批次级 trace_id 建任务即固化——processing 阶段轮询详情已可按它查审计。
		TraceID: newLLMTraceID(),
	}
}

// reuseProcessingBatch 幂等防重：该用户仍在生成中的批次直接复用；超过 recProcessingStale
// 未回写的 processing 批次视为死任务（进程重启/崩溃遗留）惰性判 failed，放行新任务。
func (s *RecommendationService) reuseProcessingBatch(userID int64) *RecommendationView {
	common.DB.Model(&model.RecommendationBatch{}).
		Where("user_id = ? AND status = ? AND updated_at < ?",
			userID, model.RecStatusProcessing, time.Now().Add(-recProcessingStale)).
		Updates(map[string]any{"status": model.RecStatusFailed, "error": "任务中断（服务重启或执行超时），请重新生成"})
	var b model.RecommendationBatch
	if err := common.DB.Where("user_id = ? AND status = ?", userID, model.RecStatusProcessing).
		Order("id DESC").First(&b).Error; err != nil {
		return nil
	}
	return &RecommendationView{RecommendationBatch: b, Items: []RecommendationItemView{}}
}

// failBatch 后台任务失败收尾：回写 failed 状态与错误（token 审计一并落）。
func (s *RecommendationService) failBatch(batch *model.RecommendationBatch, usage chatUsage, latency int64, msg string) {
	batch.Status = model.RecStatusFailed
	batch.Error = truncateRunes(msg, 500)
	batch.PromptTokens = usage.PromptTokens
	batch.CompletionTokens = usage.CompletionTokens
	batch.TotalTokens = usage.TotalTokens
	batch.LatencyMs = latency
	if err := common.DB.Save(batch).Error; err != nil {
		common.SysWarn("推荐批次失败状态回写失败 batch=%d: %v", batch.ID, err)
	}
}

// runGeneration 生成主体（同步执行；手动路径由 Generate 放进后台 goroutine）：
// 建池 → 量化评分 → LLM 精选 →（可选复核）→ 信任层回填 → 回写批次+落条目。
// 失败一律先回写批次状态再返回错误；AI 超时/网络类失败降级为量化推荐。
func (s *RecommendationService) runGeneration(ctx context.Context, batch *model.RecommendationBatch, plan *recGenPlan) (*RecommendationView, error) {
	userID := plan.userID
	recType, market, strat, filters, count := plan.recType, plan.market, plan.strat, plan.filters, plan.count
	cfg, apiKey, allowPrivate := plan.cfg, plan.apiKey, plan.allowPrivate

	// 阶段①②：多源建池（来源随策略组合）+ 基础准入 + 行情时效刷新（qf3 前置）+
	// 用户筛选（基于刷新后行情，被筛掉的保留原因）。
	pool, freshGates, err := s.buildPool(ctx, userID, market, recType, strat, filters)
	if err != nil {
		s.failBatch(batch, chatUsage{}, 0, err.Error())
		return nil, err
	}
	if len(pool) == 0 {
		msg := "候选池为空：请先添加自选股，或稍后重试（榜单数据源繁忙）"
		if market != "cn" {
			msg = "该市场暂无行情数据源支持，无法构建候选池（当前仅支持 A 股）"
		}
		s.failBatch(batch, chatUsage{}, 0, msg)
		return nil, errors.New(msg)
	}

	// 阶段③：量化评分排序（拉日线算因子，追高保护在此判定），去相关贪心选出 LLM 名单。
	// 行业归属来自宇宙快照（S0-3；未积累时行业约束自动缺席）。
	poolSyms := make([]string, 0, len(pool))
	for _, c := range pool {
		poolSyms = append(poolSyms, c.Symbol)
	}
	industryBy := industriesFor(poolSyms)
	gates := s.scorePool(ctx, recType, strat, pool, filters, industryBy)
	gates = append(freshGates, gates...)
	kept, llmCands := 0, make([]candidate, 0, maxLLMCandidates)
	for _, c := range pool {
		if c.Excluded == "" {
			kept++
		}
		if c.SentToLLM {
			llmCands = append(llmCands, c)
		}
	}
	// 终选兜底门：评分期间（数秒~数十秒）行情可能失效，喂 LLM 前再核验一次
	//（freshenPool 刚拉过、大概率命中缓存）。fresh 刷新现价/涨跌幅并记 quote_as_of；
	// stale/取不到透明排除。全部过期时宁可零推荐也不基于旧价精选。
	staleGates := s.applyQuoteFreshGate(ctx, market, pool, &llmCands)
	gates = append(gates, staleGates...)
	kept -= len(staleGates)
	if len(llmCands) == 0 {
		return nil, s.failEmptyShortlist(batch, pool, filters, gates, industryBy, kept,
			len(staleGates) > 0 || len(freshGates) > 0)
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

	// 阶段④：LLM 精选 + 反编造校验 + repair。批次行（processing）已在同步段创建，
	// 这里回填池快照与模型输入（生成中轮询详情即可见建池全景）。
	mktCtx, regime := s.buildMarketContext(ctx, market)
	// S1-1 regime 三档判定（影子模式）：照算照落库、前端展示，但**不注入 prompt 也
	// 不改写 action**——注入 prompt 会让 LLM 自行少发 buy，污染「若强制降级会改写谁」
	// 的影子对照（prompt 里既有的 BenchTrend 弱势提示保持现状不动）。
	batch.Regime = regime.Regime
	batch.RegimeJSON = marshalRegimeJSON(regime) // 拒选/降级路径也可见；成功路径追加仓位参数后覆盖
	messages := s.buildMessages(userID, recType, strat, market, count, llmCands, filters, mktCtx)
	llmInput, _ := json.Marshal(map[string]any{"market_context": mktCtx, "candidates": compactForLLM(recType, llmCands)})
	batch.CandidateCount = kept
	batch.CandidatePool = poolJSON
	batch.DataSnapshot = string(llmInput)
	batch.FiltersJSON = string(filtersJSON)

	// P0-2 调用关联：主调一个 run（repair 同 run 按 attempt 区分），复核/反方派生 run
	// 回指主调；manifest 数组随批次落库，llm_call_logs 凭 batch.TraceID 双向可查。
	mainRun := newLLMRun(batch.TraceID, "", "recommendation", "recommendation.v1", batch.PromptVersion)
	mainRun.hashData(string(llmInput))
	var rvRun, bearRun *llmRun
	fillBatchRunMeta := func() {
		batch.LlmRunJSON = marshalLLMRunManifests(cfg,
			runEntry(mainRun, true), runEntry(rvRun, true), runEntry(bearRun, true))
	}

	picks, rejected, usage, latency, callErr := s.callWithRepair(ctx, userID, mainRun, cfg, apiKey, allowPrivate, messages, poolBySymbol, count)

	// #11 原始 LLM 动作快照（复核前）：applyReviews 对 reject 会把 p.Action 强制改写为
	// watch，事件表 RawAction 须记复核前值才能与 PostGateAction 构成门控前后对照。此处
	// 在复核之前快照 Symbol→Action；降级兜底路径下方另行重建。
	rawActionBySym := make(map[string]string, len(picks))
	for _, p := range picks {
		rawActionBySym[p.Symbol] = p.Action
	}

	// 可选 AI 复核（verify）：风控复核员逐条挑刺，reject 降级为观察。
	if callErr == nil && len(picks) > 0 && plan.verify {
		reviews, overall, rvUsage, rvR := s.reviewPicks(ctx, userID, cfg, apiKey, allowPrivate, recType, picks, poolBySymbol, batch.TraceID, mainRun.RunID)
		rvRun = rvR
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

	// S2-2 反方研究员（影子，默认关联 verify）：对复核后仍为 buy 的条目独立构建最强
	// bear case。只展示不改写 action/置信度；severity=high 记 bear_shadow 反事实事件。
	// 放在复核之后——被复核否决降为 watch 的条目不再消耗反方论证篇幅。
	var bearGates []gateNote
	if callErr == nil && len(picks) > 0 && plan.bear {
		bears, bUsage, bearR := s.bearReview(ctx, userID, cfg, apiKey, allowPrivate, picks, poolBySymbol, batch.TraceID, mainRun.RunID)
		bearRun = bearR
		usage.PromptTokens += bUsage.PromptTokens
		usage.CompletionTokens += bUsage.CompletionTokens
		usage.TotalTokens += bUsage.TotalTokens
		bearGates = applyBearShadow(picks, bears)
	}

	batch.PromptTokens = usage.PromptTokens
	batch.CompletionTokens = usage.CompletionTokens
	batch.TotalTokens = usage.TotalTokens
	batch.LatencyMs = latency
	if usage.TotalTokens > 0 {
		consumeQuota(userID, usage.TotalTokens, plan.manualAction)
	}

	// AI 主调用失败：上游超时/网络类失败降级为量化推荐（量化系统已完成筛选与排序，
	// AI 挂掉不该让整个功能不可用）；鉴权/路径/配额类确定性错误直接失败，让用户修配置。
	degradedNote := ""
	if callErr != nil {
		if quantFallbackEligible(callErr) {
			if fb := buildQuantFallbackPicks(recType, llmCands, count); len(fb) > 0 {
				picks, rejected = fb, nil
				// 降级兜底 picks 全新构造（AI 未产出任何原始动作）：RawAction 快照按兜底
				// 动作重建，picked 事件 RawAction==PostGateAction（本就无门控前后差异）。
				rawActionBySym = make(map[string]string, len(fb))
				for _, p := range fb {
					rawActionBySym[p.Symbol] = p.Action
				}
				degradedNote = "AI 精选未完成（" + callErr.Error() + "）；已按量化排名生成降级推荐——规则生成，未经 AI 解读，请自行核查"
				mainRun.DegradedReason = "quant_fallback"
				callErr = nil
			}
		}
		if callErr != nil {
			fillBatchRunMeta()
			s.failBatch(batch, usage, latency, callErr.Error())
			// 保留中央客户端返回的 RefusalError 错误链，后台调用方/后续 API
			// 才能继续识别 llm_call_failed、llm_response_incomplete 等拒答码。
			return nil, callErr
		}
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
			mainRun.DegradedReason = "llm_output_invalid"
		}
		fillBatchRunMeta()
		if err := common.DB.Save(batch).Error; err != nil {
			return nil, err
		}
		// #9 零推荐批次同样落全池候选事件 + 影子标签：无 picks/items（picked 分支自动
		// 跳过，不伪造入选），只落 llm_list/filtered/scored/pool_full + 各候选影子标签，
		// 供「拒选是否正确」的错失机会/召回评估验证。gates 为名单阶段（相关性/同行业）
		// 门控；无 buy 故无 regime/bear/quality 影子门控。
		s.recordBatchFactsWithRetry(batch, pool, nil, nil, rejected, gates, industryBy, nil)
		return &RecommendationView{RecommendationBatch: *batch, Items: []RecommendationItemView{}}, nil
	}

	// 8) 信任层回填：量化分/排名/一手成本/证据核验/程序合成置信度。
	for i := range picks {
		c := poolBySymbol[picks[i].Symbol]
		picks[i].QuantScore = c.Score
		picks[i].QuantRank = c.Rank
		picks[i].PoolSize = kept
		picks[i].LotCost = round2(c.Price * 100)
		picks[i].QuoteAsOf = c.QuoteAsOf // 行情时效硬门核验过的数据源行情时刻
		// 计划价与用户筛选阈值并入证据核验值域：模型在 evidence 里复述自己给出的
		// 止盈/止损/买入区间、或用户设定的价格/换手/市值/追高阈值，均为合法引用而非幻觉。
		// Origin 标注（plan/user）让前端区分「被快照数据佐证」与「模型复述自身结论」。
		picks[i].EvidenceCheck = verifyEvidence(picks[i].Evidence, c,
			append(
				markValueOrigin(labeledVals("交易计划", picks[i].BuyZoneLow, picks[i].BuyZoneHigh, picks[i].TakeProfit, picks[i].StopLoss), "plan"),
				markValueOrigin(labeledVals("筛选阈值", filters.PriceMin, filters.PriceMax, filters.MaxGain5dPct,
					filters.TurnoverMin, filters.TurnoverMax, filters.FloatCapMinYi, filters.FloatCapMaxYi), "user")...,
			)...)
		if picks[i].DegradedSource != "" {
			// 量化降级条目：核验照跑（证明引用数字真实），但置信度必须如实标 low——
			// AI 未参与，「核验全绿」不等于研究结论可信。
			picks[i].SysConfidence = "low"
			picks[i].SysConfidenceWhy = "AI 精选未完成（上游超时），本条由量化规则自动生成，未经 AI 解读与复核"
		} else {
			picks[i].SysConfidence, picks[i].SysConfidenceWhy = systemConfidence(c, picks[i].EvidenceCheck, kept)
		}
	}

	// S1-1 大盘闸门：defense 档对 buy 条目记录「若强制降级会改写谁」（影子）；
	// 仅当 feature flag（recRegimeEnforce，默认关）打开才真正改写 action——转正须凭
	// 影子配对数据评审，防「少发 buy 做高表面胜率」。
	gates = append(gates, applyRegimeGate(picks, regime.Regime, recRegimeEnforce)...)
	// S2-2 反方研究员影子事件（severity=high 的 buy）。
	gates = append(gates, bearGates...)
	// S2-3 数据质量门控（影子）：would-be 封顶写入明细，构成约束时记 quality_shadow
	// 事件（不实际封顶——放在置信度终值确定之后，cap 与最终 confidence 比较才有意义）。
	gates = append(gates, applyQualityGateShadow(recType, picks, poolBySymbol)...)

	// S1-2 仓位建议（服务端目标波动模型，非 LLM 输出）：公式与参数快照随 RegimeJSON
	// 落库可回溯。相关性系数用同批次入选标的间的真实收益相关性。
	sizingParams := defaultSizingParams()
	applyBuyPositionSizing(picks, poolBySymbol, regime.Regime, sizingParams)
	regime.Sizing = &sizingParams
	batch.RegimeJSON = marshalRegimeJSON(regime)

	// 落选理由（池内未入选标的的一句话说明；best-effort，模型未给则为空）。
	if len(rejected) > 0 {
		if b, err := json.Marshal(rejected); err == nil {
			batch.RejectedJSON = string(b)
		}
	}

	// 9) 回写批次 + 事务落条目。降级批次（AI 超时量化兜底）记 degraded + 说明。
	if degradedNote != "" {
		batch.Status = model.RecStatusDegraded
		batch.Error = truncateRunes(degradedNote, 500)
	} else {
		batch.Status = model.RecStatusSuccess
		batch.Error = ""
	}
	fillBatchRunMeta()
	items := make([]model.Recommendation, 0, len(picks))
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(batch).Error; err != nil {
			return err
		}
		for i, p := range picks {
			c := poolBySymbol[p.Symbol]
			detail, _ := json.Marshal(p)
			rec := model.Recommendation{
				BatchID: batch.ID, UserID: userID, Symbol: p.Symbol, Market: market,
				Name: c.Name, Action: p.Action, Confidence: int(p.Confidence),
				Summary: truncateRunes(firstReason(p), 512), RefPrice: c.Price,
				// S0-4 价格版本：生成时点最近收盘日与收盘价，防前复权重锚后计划价错位。
				RefDate: c.lastBarDate, RefClose: c.lastBarClose,
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
		s.failBatch(batch, usage, latency, "结果落库失败: "+err.Error())
		return nil, err
	}

	// S0-5 反事实事件 + 标签事实（含影子标签）：进程内同步重试覆盖瞬时 DB 抖动，
	// 全部成功置 facts_recorded=true；用尽仍失败保持 false 供人工排查（不影响主结果返回）。
	s.recordBatchFactsWithRetry(batch, pool, items, picks, rejected, gates, industryBy, rawActionBySym)

	return s.assembleView(*batch, items, nil, nil), nil
}

// failEmptyShortlist 名单为空时的失败收尾（P0-3）：提前失败也必须落可复核快照与事实
// 账本——回填池快照/筛选参数 → failBatch → recordBatchFactsWithRetry。quote_stale
// 反事实事件、影子标签、池内透明排除原因都依赖 recordBatchFacts，不能在它之前直接
// return（否则「全 stale 拒绝生成」既无法回验误伤、也无从复核当时的池面貌）。
func (s *RecommendationService) failEmptyShortlist(batch *model.RecommendationBatch, pool []candidate,
	filters RecFilters, gates []gateNote, industryBy map[string]string, kept int, hasStale bool) error {
	poolJSON, poolOmitted := marshalPoolSnapshot(pool)
	filtersPayload := map[string]any{"filters": filters, "applied": filters.Describe()}
	if poolOmitted > 0 {
		filtersPayload["pool_omitted"] = poolOmitted
	}
	filtersJSON, _ := json.Marshal(filtersPayload)
	batch.CandidatePool = poolJSON
	batch.FiltersJSON = string(filtersJSON)
	batch.CandidateCount = kept
	var msg string
	if hasStale {
		msg = "候选行情已全部过期（可能停牌、休市异常或数据源故障）：为避免基于旧价推荐，本次不生成，请稍后重试"
	} else {
		excluded := len(pool) - kept
		msg = fmt.Sprintf("筛选后候选池为空（共扫描 %d 只，%d 只被筛掉）：请放宽股价/市值/换手等筛选条件后重试", len(pool), excluded)
	}
	s.failBatch(batch, chatUsage{}, 0, msg)
	s.recordBatchFactsWithRetry(batch, pool, nil, nil, nil, gates, industryBy, nil)
	return errors.New(msg)
}

// recFactsRetries 事实账本落库进程内同步重试次数（覆盖瞬时 DB 抖动；recordBatchFacts
// 已同事务原子化，重试重跑不会残留半成品）。
const recFactsRetries = 3

// recordBatchFactsWithRetry 事实账本落库 + 完整性标记（#10）：同步重试 recordBatchFacts，
// 全部成功后把批次 FactsRecorded 置 true；重试用尽仍失败保持 false 并 SysWarn——
// 无法可靠跨进程持久化补建（快照丢失价格版本锚、gates 未持久化、事件表无唯一索引
// 重跑会重复，见 reclabel.go recordBatchFacts 注释），失败批次交 tracking 扫描与人工排查。
func (s *RecommendationService) recordBatchFactsWithRetry(batch *model.RecommendationBatch, pool []candidate, items []model.Recommendation, picks []recPick, rejected []recReject, gates []gateNote, industryBy map[string]string, rawActionBySym map[string]string) {
	if common.DB == nil || len(pool) == 0 {
		return
	}
	var err error
	for attempt := 0; attempt < recFactsRetries; attempt++ {
		if err = recordBatchFacts(batch, pool, items, picks, rejected, gates, industryBy, rawActionBySym); err == nil {
			break
		}
	}
	if err != nil {
		common.SysWarn("批次事实账本落库失败（重试 %d 次仍失败），facts_recorded 保持 false 待人工排查 batch=%d: %v", recFactsRetries, batch.ID, err)
		return
	}
	batch.FactsRecorded = true
	if uerr := common.DB.Model(batch).Update("facts_recorded", true).Error; uerr != nil {
		common.SysWarn("批次 facts_recorded 标记回写失败 batch=%d: %v", batch.ID, uerr)
	}
}

// applyBuyPositionSizing S1-2 仓位回填（#21）：只对 action=buy 条目算仓位——watch 不参与
// 买入仓位预算，否则 watch 得非零建议仓位并计入 Σ、归一化时按比例挤占真实 buy 的仓位，
// 且前端对 watch 展示建议仓位语义矛盾。相关性也只在 buy 之间对齐（watch 不入组合）。
// 与「买入胜率只统计 buy」口径一致。picks 就地回填 PositionPct/PositionWhy。
func applyBuyPositionSizing(picks []recPick, poolBySymbol map[string]candidate, regime string, params positionSizingParams) {
	sizingIns := make([]sizingInput, 0, len(picks))
	sizingIdx := make([]int, 0, len(picks)) // sizingIns[k] → picks 下标
	for i := range picks {
		if picks[i].Action != model.RecActionBuy {
			picks[i].PositionPct = 0
			picks[i].PositionWhy = "观察标的不计入买入仓位预算，不给出建议仓位"
			continue
		}
		c := poolBySymbol[picks[i].Symbol]
		in := sizingInput{Symbol: picks[i].Symbol}
		if c.Factors != nil {
			in.Vol20Daily = c.Factors.Volatility20
		}
		for j := range picks {
			if j == i || picks[j].Action != model.RecActionBuy {
				continue
			}
			co := poolBySymbol[picks[j].Symbol]
			if corr := pairwiseCorrAligned(c.closeDates, c.closes, co.closeDates, co.closes); corr > in.MaxCorr {
				in.MaxCorr = corr
			}
		}
		sizingIns = append(sizingIns, in)
		sizingIdx = append(sizingIdx, i)
	}
	sizingOuts := computePositionPcts(sizingIns, regime, params)
	for k, out := range sizingOuts {
		picks[sizingIdx[k]].PositionPct = out.PositionPct
		picks[sizingIdx[k]].PositionWhy = out.Why
	}
}

// quantFallbackEligible AI 主调用失败后是否值得量化降级：超时/取消/网络/上游 5xx/
// 流中断/空内容这类「上游临时不可用」降级兜底；鉴权/路径/Base URL/配额类确定性
// 错误不降级——降级会掩盖需要用户修配置的问题。
func quantFallbackEligible(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, kw := range []string{"HTTP 401", "HTTP 403", "HTTP 404", "Base URL", "配额"} {
		if strings.Contains(msg, kw) {
			return false
		}
	}
	return true
}

// buildQuantFallbackPicks 量化降级推荐：AI 精选不可用时按量化排名取前 count 只规则合成。
// 一切数字来自候选池真实数据；短线计划价按 ATR 规则生成——止损距离 2×ATR%、止盈
// 3×ATR%、买入区间 ±0.5×ATR%（ATR% 钳在 [2.5,6]，对区间中值盈亏比恒 1.5，且天然满足
// shortPlanPricesValid 的四价关系）；action 一律 watch、置信度恒 35——没有 AI 证据链，
// 不冒充完整推荐，前端按 detail.degraded_source 展示降级标签。
func buildQuantFallbackPicks(recType string, cands []candidate, count int) []recPick {
	sorted := append([]candidate(nil), cands...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].Rank < sorted[b].Rank })
	if len(sorted) > count {
		sorted = sorted[:count]
	}
	picks := make([]recPick, 0, len(sorted))
	for _, c := range sorted {
		if c.Price <= 0 {
			continue
		}
		p := recPick{
			Symbol:         c.Symbol,
			Action:         model.RecActionWatch,
			Confidence:     35,
			DegradedSource: "quant_fallback",
		}
		p.Reason = []string{fmt.Sprintf("量化综合分 %.1f、池内第 %d 名（AI 精选超时，按量化排名降级入选）", c.Score, c.Rank)}
		for i, note := range c.Bonus {
			if i >= 2 {
				break
			}
			p.Reason = append(p.Reason, note)
		}
		p.Risks = []string{"本条为 AI 精选超时后的量化降级结果：未经 AI 解读与复核，量化分仅有排序意义，请自行核查基本面与消息面"}
		p.Evidence = []string{
			fmt.Sprintf("score=%.1f rank=%d", c.Score, c.Rank),
			fmt.Sprintf("现价 %.2f、当日涨跌 %.2f%%", c.Price, c.ChangePct),
		}
		if c.TurnoverRate > 0 {
			p.Evidence = append(p.Evidence, fmt.Sprintf("换手率 %.2f%%", c.TurnoverRate))
		}
		if recType == model.RecTypeShortTerm {
			atrp := 2.5
			if c.Factors != nil && c.Factors.ATRPct > atrp {
				atrp = c.Factors.ATRPct
			}
			if atrp > 6 {
				atrp = 6
			}
			p.BuyZoneLow = round2(c.Price * (1 - 0.005*atrp))
			p.BuyZoneHigh = round2(c.Price * (1 + 0.005*atrp))
			p.StopLoss = round2(c.Price * (1 - 0.02*atrp))
			p.TakeProfit = round2(c.Price * (1 + 0.03*atrp))
			p.ValidDays = 5
			p.Invalidation = "跌破规则止损价或放量破位（价位为 ATR 规则生成，未经 AI 校准）"
			p.Evidence = append(p.Evidence, fmt.Sprintf("计划价按 ATR 规则合成：止损距离 %.1f%%、止盈距离 %.1f%%（对现价盈亏比 1.5）", 2*atrp, 3*atrp))
		} else {
			p.Thesis = "AI 解读未完成（上游超时）：本条仅代表量化因子排序结果，长线逻辑请人工研究后再定。"
			p.ReviewCycle = "请尽快人工复核"
		}
		p.Disclaimer = "本条由量化规则在 AI 超时后自动生成，未经 AI 解读，仅供研究参考，不构成投资建议，据此操作风险自负。"
		picks = append(picks, normalizePick(p, c.Symbol, c))
	}
	return picks
}

// callWithRepair 调用 LLM，反编造校验（picks 必须∈候选池），失败有限次 repair，累计 token。
// 同时收集池内落选标的的一句话理由（rejected，best-effort 不参与合格判定）。
// run 承载 P0-2 关联元数据（prompt hash 在此按初始消息计算，attempt 1 基递增）。
func (s *RecommendationService) callWithRepair(ctx context.Context, userID int64, run *llmRun, cfg *model.LLMConfig, apiKey string, allowPrivate bool, messages []chatMessage, pool map[string]candidate, count int) ([]recPick, []recReject, chatUsage, int64, error) {
	var acc chatUsage
	var lastLatency int64
	convo := append([]chatMessage(nil), messages...)
	run.hashPrompt(messages)

	for attempt := 0; attempt <= moduleRepairAttempts("recommendation"); attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("recommendation", cfg.MaxTokens),
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
			Repair: attempt > 0, // repair 轮：契约开启时温度固定 0
			Meta:   run.chatMeta(userID, cfg, attempt+1),
		})
		run.record(res, err)
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
		// 校验失败：追加 repair，明确告知只能用候选池中的 symbol。坏输出只回灌开头
		// 片段定位——完整回灌会把一大段废文本重新塞进上下文，拖慢下一轮生成、
		// 更容易再次撞上游 60s 超时。
		symbols := poolSymbolList(pool)
		convo = append(convo,
			chatMessage{Role: "assistant", Content: moduleRepairFeed("recommendation", res.Content)},
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
// 第四返回值为复核 run 元数据（parent 回指主调）。
func (s *RecommendationService) reviewPicks(ctx context.Context, userID int64, cfg *model.LLMConfig, apiKey string, allowPrivate bool, recType string, picks []recPick, pool map[string]candidate, traceID, parentRunID string) ([]pickReview, string, chatUsage, *llmRun) {
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
				"factors": c.Factors, "score_dims": c.ScoreDims, "fin": c.Fin,
			},
		})
	}
	inputJSON, err := json.Marshal(rows)
	if err != nil {
		return nil, "", usage, nil
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
	run := newLLMRun(traceID, parentRunID, "rec_review", "rec_review.v1", recPromptVersion)
	run.hashData(string(inputJSON))
	run.hashPrompt(convo)
	type reviewOut struct {
		Reviews []pickReview `json:"reviews"`
		Overall string       `json:"overall"`
	}
	for attempt := 0; attempt <= moduleRepairAttempts("rec_review"); attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("rec_review", cfg.MaxTokens),
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
			Repair: attempt > 0, // repair 轮：契约开启时温度固定 0（llm_contract.go）
			Meta:   run.chatMeta(userID, cfg, attempt+1),
		})
		run.record(res, err)
		if err != nil {
			return nil, "", usage, run
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
				return valid, truncateRunes(strings.TrimSpace(out.Overall), 500), usage, run
			}
		}
		convo = append(convo,
			chatMessage{Role: "assistant", Content: moduleRepairFeed("rec_review", res.Content)},
			chatMessage{Role: "user", Content: "上一条输出不合格。请只输出 JSON：{\"reviews\":[{\"symbol\",\"verdict\":\"pass|warn|reject\",\"comment\",\"confidence\"}],\"overall\":\"...\"}，symbol 必须来自被审推荐。"},
		)
	}
	run.DegradedReason = "llm_output_invalid"
	return nil, "", usage, run
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
	// 服务端专属字段先清零（防模型伪造）：Review/Bear/QualityGate 只能由服务端复核、
	// 反方与质量门控链路回填——模型在输出 JSON 里自附这些字段会被 Unmarshal 吃进来，
	// verify/bear 关闭时无人覆盖就会以「复核通过/反方低危」的假面落库展示。
	// DegradedSource 不在此清（quant_fallback 构造路径先设值再过本函数）。
	p.Review, p.Bear, p.QualityGate = nil, nil, nil
	p.QuoteAsOf = "" // 服务端回填字段，模型自附一律剥除
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
	// P0-4 跨字段纪律（llm_semantic_validator.go，flag 控）：短线 buy 盈亏比 <1.5
	// 透明降级为 watch（prompt 纪律的程序化，沿上方 shortPlanPricesValid 降级先例）。
	return applyRecPickSemantics(p)
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
	// S0-4：Amount 缺失（=0）不再绕过流动性门槛——旧逻辑 `c.Amount > 0 &&` 让无成交额
	// 数据的自选股全部免检。自选进池前已按近 20 日日线中位数补齐成交额（buildPool），
	// 补不到（无日线）的标的与流动性不足同样拒绝：无数据只会诱导 LLM 编造依据。
	if f.minAmount > 0 && c.Amount < f.minAmount {
		return false
	}
	if f.blacklist[c.Market+":"+c.Symbol] {
		return false
	}
	return true
}

// medianAmountsFor 批量取一批标的近 20 根日线成交额的中位数（元）。自选股无榜单
// 成交额字段时的流动性判定依据（S0-4）；无日线的标的不在返回 map 中（=0）。
func medianAmountsFor(market string, symbols []string) map[string]float64 {
	out := map[string]float64{}
	if common.DB == nil || len(symbols) == 0 {
		return out
	}
	var rows []model.DailyBar
	if err := common.DB.Select("symbol", "trade_date", "amount").
		Where("market = ? AND symbol IN ?", market, symbols).
		Order("symbol, trade_date DESC").Find(&rows).Error; err != nil {
		return out
	}
	amts := map[string][]float64{}
	for _, r := range rows {
		if r.Amount > 0 && len(amts[r.Symbol]) < 20 {
			amts[r.Symbol] = append(amts[r.Symbol], r.Amount)
		}
	}
	for sym, list := range amts {
		sort.Float64s(list)
		out[sym] = median(list)
	}
	return out
}

// quoteFreshGateVersion 行情时效硬门判定版本（判定规则/口径变更时递增）。
// qf3：刷新前置到用户筛选**终判**之前（buildPool 内：静态筛选 → 刷新 → 用户筛选 →
// 名额分配）——qf2 的刷新发生在旧价筛选与名额分配之后，被旧价误排除的候选（旧价 9 元
// 撞「最低 10 元」而新价已达标）永远失去翻案机会，排除原因也是基于旧价的假账。
// qf2：核验从「终选名单喂 LLM 前」前置到「用户筛选复核/评分/排名之前」（qf1 只对
// Top 名单刷新 Price/ChangePct，评分与筛选仍建立在建池旧价上，新旧数据混合）。
const quoteFreshGateVersion = "qf3"

// applyFreshQuoteToCand 把一条已核验为 fresh 的行情应用到候选（纯函数，便于单测）：
// 刷新 Price/ChangePct/Amount 等 quote 派生字段并记 QuoteAsOf，随后复筛用户价格/涨停
// 条件（applyQuoteFilters，基于新价与当日涨停价）。返回复筛排除原因（空=存活）。
func applyFreshQuoteToCand(c *candidate, q *datasource.Quote, filters RecFilters) string {
	c.Price = round2(q.Price)
	c.ChangePct = round2(q.ChangePct)
	if q.Amount > 0 {
		c.Amount = round2(q.Amount)
	}
	if !q.DataTime.IsZero() {
		c.QuoteAsOf = q.DataTime.In(time.Local).Format("2006-01-02 15:04")
	}
	return applyQuoteFilters(*c, filters)
}

// freshenCandidates 对 pool 中给定下标的候选批量核验并刷新「当前有效行情」（P0-3：
// 前置到筛选/评分/排名之前）：
//   - fresh：刷新 quote 派生字段并复筛（applyFreshQuoteToCand）——旧价通过、新价不过
//     的在此透明淘汰（涨停判定用刷新后的价与当日涨停价）；
//   - stale/取不到：Excluded + gate_type=quote_stale 事件（影子标签自然结算可回验误伤）。
//
// 返回存活（可参与评分）的下标与门控记录。s.market==nil（单测注入环境）原样放行。
func (s *RecommendationService) freshenCandidates(ctx context.Context, pool []candidate, idxs []int, filters RecFilters) ([]int, []gateNote) {
	if s.market == nil || len(idxs) == 0 {
		return idxs, nil
	}
	refs := make([]QuoteRef, 0, len(idxs))
	for _, i := range idxs {
		refs = append(refs, QuoteRef{Market: pool[i].Market, Symbol: pool[i].Symbol})
	}
	fresh := s.market.FreshQuotesFor(ctx, refs)
	alive := make([]int, 0, len(idxs))
	var gates []gateNote
	for _, i := range idxs {
		fq, ok := fresh[QuoteKey(pool[i].Market, pool[i].Symbol)]
		if !ok || fq.Quote == nil || fq.Quote.Price <= 0 || fq.Fresh.Status != freshStatusFresh {
			reason := "行情时效硬门：全部数据源均未取到当前有效行情（可能停牌/退市/数据源故障），为避免基于旧价筛选、评分与定价，透明排除"
			if ok && fq.Quote != nil && !fq.Quote.DataTime.IsZero() {
				reason = fmt.Sprintf("行情时效硬门：行情仅更新至 %s（期望交易日 %s），基于旧价的筛选、评分与价位不可信，透明排除",
					fq.Quote.DataTime.In(time.Local).Format("2006-01-02 15:04"), fq.Fresh.ExpectedDate)
			}
			pool[i].Excluded = reason
			pool[i].SentToLLM = false
			gates = append(gates, gateNote{
				Symbol:      pool[i].Symbol,
				GateType:    model.GateQuoteStale,
				GateVersion: quoteFreshGateVersion,
				Reason:      reason,
			})
			continue
		}
		if reason := applyFreshQuoteToCand(&pool[i], fq.Quote, filters); reason != "" {
			pool[i].Excluded = reason
			continue
		}
		alive = append(alive, i)
	}
	return alive, gates
}

// freshenPool P0-3/qf3 行情时效核验前置：在用户筛选终判与名额分配**之前**，对全部
// 未被静态排除的候选核验并刷新「当前有效行情」——建池旧价从此只决定「谁进池」，
// 用户筛选终判、涨停判断、computeScore/computeCandFactors 与排名一律建立在刷新后
// 的行情上。stale/取不到 → 透明排除 + quote_stale 事件（影子标签自然结算可回验误伤）。
// 名额分配（assignScanQuota）在此之后进行，天然只发给存活者，无需补位轮
// （qf2 的「先发名额再刷新参评者+池满补位」顺序漏洞由此根治）。
func (s *RecommendationService) freshenPool(ctx context.Context, pool []candidate, filters RecFilters) []gateNote {
	if s.market == nil {
		return nil
	}
	idxs := make([]int, 0, len(pool))
	for i := range pool {
		if pool[i].Excluded == "" {
			idxs = append(idxs, i)
		}
	}
	_, gates := s.freshenCandidates(ctx, pool, idxs, filters)
	return gates
}

// applyQuoteFreshGate 终选兜底门：LLM 名单在 freshenPool 之后还要经历数秒级的日线
// 拉取与评分，喂模型前再核验一次（大概率命中行情缓存，零上游成本）——评分期间行情
// 失效（跨入午休/收盘停滞等）的候选在此透明剔除。fresh 的刷新 Price/ChangePct 与
// QuoteAsOf（与评分价的差异为数秒内的正常盘面演进，非「旧价」）。
// s.market 为 nil（单测注入环境）时跳过。
func (s *RecommendationService) applyQuoteFreshGate(ctx context.Context, market string, pool []candidate, llmCands *[]candidate) []gateNote {
	if s.market == nil || len(*llmCands) == 0 {
		return nil
	}
	refs := make([]QuoteRef, 0, len(*llmCands))
	for _, c := range *llmCands {
		refs = append(refs, QuoteRef{Market: market, Symbol: c.Symbol})
	}
	fresh := s.market.FreshQuotesFor(ctx, refs)
	idxBySym := make(map[string]int, len(pool))
	for i := range pool {
		idxBySym[pool[i].Symbol] = i
	}
	var gates []gateNote
	keptCands := (*llmCands)[:0]
	for _, c := range *llmCands {
		fq, ok := fresh[QuoteKey(market, c.Symbol)]
		if !ok || fq.Quote == nil || fq.Quote.Price <= 0 || fq.Fresh.Status != freshStatusFresh {
			reason := "行情时效硬门：全部数据源均未取到当前有效行情（可能停牌/退市/数据源故障），为避免基于旧价精选与定价，透明排除"
			if ok && fq.Quote != nil && !fq.Quote.DataTime.IsZero() {
				reason = fmt.Sprintf("行情时效硬门：行情仅更新至 %s（期望交易日 %s），基于旧价的精选与价位不可信，透明排除",
					fq.Quote.DataTime.In(time.Local).Format("2006-01-02 15:04"), fq.Fresh.ExpectedDate)
			}
			if i, ok := idxBySym[c.Symbol]; ok {
				pool[i].Excluded = reason
				pool[i].SentToLLM = false
			}
			gates = append(gates, gateNote{
				Symbol:      c.Symbol,
				GateType:    model.GateQuoteStale,
				GateVersion: quoteFreshGateVersion,
				Reason:      reason,
			})
			continue
		}
		asOf := fq.Quote.DataTime.In(time.Local).Format("2006-01-02 15:04")
		c.Price = round2(fq.Quote.Price)
		c.ChangePct = round2(fq.Quote.ChangePct)
		c.QuoteAsOf = asOf
		if i, ok := idxBySym[c.Symbol]; ok {
			pool[i].Price = c.Price
			pool[i].ChangePct = c.ChangePct
			pool[i].QuoteAsOf = asOf
		}
		keptCands = append(keptCands, c)
	}
	*llmCands = keptCands
	return gates
}

// buildPool 阶段①②：多源建池（自选 ∪ 按策略组合的榜单来源，来源可叠加）→
// 基础准入（candidateEligible：ST/退市/北交所/无行情/流动性/黑名单，不合格不进池）→
// 估值富化 → 静态筛选 → 行情时效核验刷新（freshenPool，qf3）→ 用户筛选
// （applyQuoteFilters 基于刷新后行情，不合格保留并标注 excluded 原因）→ 评分名额分配。
// 返回 全量池（含被筛掉者）、quote_stale 门控记录；未被筛掉的数量超过
// maxScanCandidates 时后来者标注「池满」。
func (s *RecommendationService) buildPool(ctx context.Context, userID int64, market, recType string, strat *strategyTemplate, filters RecFilters) ([]candidate, []gateNote, error) {
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
	// S0-4：自选无榜单成交额字段，先按近 20 日日线中位数批量补齐（Amount=0 不再
	// 绕过流动性门槛，candidateEligible 统一判定），补不到的按流动性不足拒绝。
	if groups, err := s.watchlist.List(ctx, userID); err == nil {
		var wlSyms []string
		for _, g := range groups {
			for _, it := range g.Items {
				if it.Market == market && it.QuoteOK {
					wlSyms = append(wlSyms, it.Symbol)
				}
			}
		}
		medAmt := medianAmountsFor(market, wlSyms)
		for _, g := range groups {
			for _, it := range g.Items {
				if it.Market != market || !it.QuoteOK {
					continue
				}
				add(candidate{Symbol: it.Symbol, Market: market, Name: it.Name,
					Price: round2(it.Price), ChangePct: round2(it.ChangePct),
					Amount: medAmt[it.Symbol]}, "watchlist")
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

	// M1 策略信号来源：因子宽表按推荐策略对应的内置选股策略全市场扫描，命中进池
	//（榜单只见「当天最热/最冷」，策略信号补上「形态对但不在榜上」的全市场供给）。
	// 宽表行情是最近收盘日口径，盘中生成会滞后——对新增标的批量拉实时行情覆盖，
	// 拉不到时保留收盘口径（best-effort；涨停判定等用户筛选按可得数据判）。
	if market == "cn" {
		if hits := strategySignalHits(ctx, recType, strat.Key, strategySignalPoolLimit); len(hits) > 0 {
			refs := make([]QuoteRef, 0, len(hits))
			for _, h := range hits {
				if _, exists := byKey[h.Symbol]; !exists {
					refs = append(refs, QuoteRef{Market: market, Symbol: h.Symbol})
				}
			}
			quotes := s.market.QuotesFor(ctx, refs)
			for _, h := range hits {
				c := candidate{Symbol: h.Symbol, Market: market, Name: h.Name,
					Price: round2(h.Price), ChangePct: round2(h.ChgPct),
					Amount: round2(h.AmountYi * 1e8), TurnoverRate: round2(h.TurnoverRate)}
				if q := quotes[QuoteKey(market, h.Symbol)]; q != nil && q.Price > 0 {
					c.Price = round2(q.Price)
					c.ChangePct = round2(q.ChangePct)
					if q.Amount > 0 {
						c.Amount = round2(q.Amount)
					}
				}
				add(c, "strategy_signal")
			}
		}
	}
	if len(pool) == 0 {
		return pool, nil, nil
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
			// P0-3 估值时效校验：换手/量比/振幅/涨停价随行情时刻变化，DataTime 过期
			//（昨日口径/解析失败零值）时丢弃本次富化、保留榜单兜底值——用昨日涨停价判
			// 「今日已涨停」或用旧换手做用户筛选都会失真（fail-closed，宁缺毋假）。
			if s.market.QuoteFreshnessOf(pool[i].Market, v.DataTime).Status != freshStatusFresh {
				continue
			}
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

	// 静态筛选先行（ETF/基金、板块前缀偏好）：排除结果与行情无关、永不翻案，
	// 先筛掉省得为它们拉取行情。
	for i := range pool {
		if reason := applyStaticFilters(pool[i], filters); reason != "" {
			pool[i].Excluded = reason
		}
	}
	// P0-3/qf3 行情时效核验前置到用户筛选终判之前：建池旧价只决定「谁进池」，价格/
	// 换手/涨停等用户筛选与后续评分排名一律建立在刷新后的当前有效行情上——qf2 把
	// 刷新放在旧价筛选之后，旧价 9 元被「最低 10 元」排除的候选即使新价已达标也
	// 永远失去翻案机会（排除原因还是基于旧价的假账）。stale 透明排除记事件。
	freshGates := s.freshenPool(ctx, pool, filters)
	// 用户筛选（透明标记式排除，基于刷新后行情），随后按来源轮转发放评分名额。
	for i := range pool {
		if pool[i].Excluded == "" {
			if reason := applyQuoteFilters(pool[i], filters); reason != "" {
				pool[i].Excluded = reason
			}
		}
	}
	assignScanQuota(pool)
	return pool, freshGates, nil
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
// 追高保护在此判定（需要近 5 日涨幅），最后按分排名、去相关贪心选出 LLM 名单
// （S1-3：相关性去重 + 同行业名额上限，返回被挤出者的门控记录供事件表落库）。
// 日线拉取失败的标的**透明排除**（"日线数据获取失败"）而非按中性 50 分混入排名——
// 否则既能挤占名单名额、又静默绕过近 5 日涨幅追高保护。追高/无日线剔除释放的名额
// 会从「池满」标的中按进池顺序补评一轮（只补一轮，拉取总量仍有界）。
func (s *RecommendationService) scorePool(ctx context.Context, recType string, strat *strategyTemplate, pool []candidate, filters RecFilters, industryBy map[string]string) []gateNote {
	sentiDate := time.Now().Format("2006-01-02")
	// F2 财务拉取预算（仅长线消耗）：单次生成最多回上游拉 finRecFetchBudget 只 F10，
	// 其余只吃本地缓存（缺失不惩罚），多次生成/详情页访问会逐步焐热缓存。
	finBudget := finRecFetchBudget
	// M3a 资金流历史补拉预算（短线/长线通用）：同款按需+缓存模式。
	flowBudget := fflowRecBudget
	// scoreRound 对给定下标拉日线并评分；返回本轮存活（未被排除）的数量。
	scoreRound := func(idxs []int) int {
		// M3a 龙虎榜/人气榜信号批量注入（本地表两次查询，最近有数据交易日口径）——
		// 必须先于 strategyAdjust 写回候选，加分项才看得见。
		syms := make([]string, 0, len(idxs))
		for _, i := range idxs {
			syms = append(syms, pool[i].Symbol)
		}
		lhbSigs := lhbSignalsFor(syms)
		popSigs := popSignalsFor(syms)
		// M3b 盘中因子批量注入（本地表一次查询，最近已同步交易日 T-1 口径）；
		// 写回点在下方 Factors 创建之后。
		intraSigs := intradaySignalsFor(syms)
		for _, i := range idxs {
			if sig, ok := lhbSigs[pool[i].Symbol]; ok {
				pool[i].LhbNetYi = sig.NetBuyYi
				pool[i].LhbReason = sig.Reason
				pool[i].OrgNetYi = sig.OrgNetYi
				pool[i].OrgBuys = sig.OrgBuys
			}
			if sig, ok := popSigs[pool[i].Symbol]; ok {
				pool[i].PopRank = sig.Rank
				pool[i].PopPrev = sig.PrevRank
				pool[i].PopNew = sig.IsNew
			}
		}
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
			pool[i].Factors = computeCandFactors(pool[i].Price, bars)
			// S0-4 价格版本 + S1-3 相关性序列：保存尾部收盘与交易日（61 根足够 60 日
			// 收益相关；日期供停牌错位下的交集对齐）与最近收盘日锚点（防前复权重锚
			// 的比对基准）。
			tail := bars
			if len(tail) > 61 {
				tail = tail[len(tail)-61:]
			}
			closes := make([]float64, len(tail))
			closeDates := make([]string, len(tail))
			for k, b := range tail {
				closes[k] = b.Close
				closeDates[k] = b.TradeDate
			}
			pool[i].closes = closes
			pool[i].closeDates = closeDates
			pool[i].lastBarDate = bars[len(bars)-1].TradeDate
			pool[i].lastBarClose = bars[len(bars)-1].Close

			// M3a 主力资金流：缓存优先、预算内补拉；有序列时融合量能维（0.6 原量能
			// + 0.4 资金分）并派生连续净流入天数因子，缺失不惩罚（评分原样）。
			if flows, _ := ensureStockFundFlow(ctx, s.em, pool[i].Market, pool[i].Symbol, &flowBudget); len(flows) > 0 {
				sc = applyFlowScore(sc, flows)
				pool[i].Factors.MainNetDays = mainNetStreakDays(flows)
				pool[i].Factors.MainNet5dYi = round2(mainNetSum(flows, 5) / 1e8)
			}
			// M3b 盘中因子写回（T-1 盘中形态，进短线策略加分与 LLM 名单）。
			if sig, ok := intraSigs[pool[i].Symbol]; ok {
				pool[i].Factors.IntradayDate = sig.TradeDate
				pool[i].Factors.Tail30Chg = sig.Tail30Chg
				pool[i].Factors.Tail30VolPct = sig.Tail30VolPct
				pool[i].Factors.MorningChg = sig.MorningChg
				pool[i].Factors.CloseVsVwap = sig.CloseVsVwap
				pool[i].Factors.PmVwapUp = sig.PmVwapUp
			}
			pool[i].ScoreDims = &scoreDims{Trend: sc.Trend, Momentum: sc.Momentum, Position: sc.Position, Volume: sc.Volume, Risk: sc.Risk}

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

			// F2 财务因子（仅长线）：最新一期 ROE/净利增速/营收增速进加分与 LLM 名单。
			if recType == model.RecTypeLongTerm {
				pool[i].Fin = financeFactorFor(ctx, pool[i].Symbol, &finBudget)
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
	// qf3：池满候选在 buildPool 已核验过一次行情，但评分是分钟级过程（拉日线 3~8s/批
	// ×多轮），补位发生在中途，行情可能已跨入午休/收盘停滞——进评分前再核验+复筛
	//（大概率命中行情缓存，零上游成本；旧价评分与旧价涨停判定在补位路径复活就前功尽弃）。
	var gates []gateNote
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
			aliveRefill, g2 := s.freshenCandidates(ctx, pool, refill, filters)
			gates = append(gates, g2...)
			if len(aliveRefill) > 0 {
				scoreRound(aliveRefill)
			}
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

	// S1-3 组合去相关（名单阶段生效，不改写 LLM 输出）：按分数贪心入选——与已入选
	// 标的近 60 日收益相关性超阈值只保留分高者；同行业（宇宙快照口径，未积累时该
	// 约束缺席）最多 recIndustryCap 只。被挤出者保留池内排名（透明），事件表记录
	// gate_type 供影子收益对照。
	chosen := make([]int, 0, maxLLMCandidates)
	for rank, r := range ranked {
		pool[r.idx].Rank = rank + 1
		if len(chosen) >= maxLLMCandidates {
			continue
		}
		blocked := false
		if ind := industryBy[pool[r.idx].Symbol]; ind != "" {
			same := 0
			for _, ci := range chosen {
				if industryBy[pool[ci].Symbol] == ind {
					same++
				}
			}
			if same >= recIndustryCap {
				gates = append(gates, gateNote{
					Symbol: pool[r.idx].Symbol, GateType: model.GateIndustryCap,
					Reason: fmt.Sprintf("同行业「%s」已有 %d 只入围名单，按组合去相关规则让位", ind, same),
				})
				blocked = true
			}
		}
		if !blocked && len(pool[r.idx].closes) > 0 {
			for _, ci := range chosen {
				if corr := pairwiseCorrAligned(pool[r.idx].closeDates, pool[r.idx].closes, pool[ci].closeDates, pool[ci].closes); corr >= recCorrThreshold {
					gates = append(gates, gateNote{
						Symbol: pool[r.idx].Symbol, GateType: model.GateCorrelation,
						Reason: fmt.Sprintf("与 %s 近60日收益相关性 %.2f≥%.2f，仅保留分高者", pool[ci].Symbol, corr, recCorrThreshold),
					})
					blocked = true
					break
				}
			}
		}
		if blocked {
			continue
		}
		chosen = append(chosen, r.idx)
		pool[r.idx].SentToLLM = true
	}
	return gates
}

// recCorrThreshold S1-3 名单相关性去重阈值；recIndustryCap 同行业名额上限。
const (
	recCorrThreshold = 0.85
	recIndustryCap   = 2
)

// recMarketContext LLM 精选时的市场环境锚点（给模型「今天是什么行情」的客观参照，
// 防止脱离大盘环境给出激进判断）。全部真实数据，缺失块置空。
type recMarketContext struct {
	Indices    []map[string]any `json:"indices,omitempty"`
	Breadth    map[string]any   `json:"breadth,omitempty"`
	MainNetYi  float64          `json:"main_fund_net_yi,omitempty"` // 两市主力净流入（亿元）
	BenchTrend string           `json:"bench_trend,omitempty"`      // 基准指数与长均线的位置关系
}

// buildMarketContext 拉取市场环境（LLM 锚点）并顺带产出 S1-1 regime 三档判定——
// 同一次 overview/基准拉取两用，避免重复上游请求。regime **不注入 prompt**（影子期
// 纯净对照），只落库与前端展示。
func (s *RecommendationService) buildMarketContext(ctx context.Context, market string) (*recMarketContext, RegimeResult) {
	mc := &recMarketContext{}
	var breadth *datasource.Breadth
	mainNetYi, hasFlow := 0.0, false
	if ov := s.market.GetOverview(ctx, market); ov != nil {
		for i, ix := range ov.Indices {
			if i >= 3 {
				break
			}
			mc.Indices = append(mc.Indices, map[string]any{"name": ix.Name, "change_pct": round2(ix.ChangePct)})
		}
		if ov.Breadth != nil {
			breadth = ov.Breadth
			mc.Breadth = map[string]any{
				"advances": ov.Breadth.Advances, "declines": ov.Breadth.Declines,
				"limit_up": ov.Breadth.LimitUp, "limit_down": ov.Breadth.LimitDown,
			}
		}
		if ov.FundFlow != nil {
			mainNetYi = round2(ov.FundFlow.MainNet / 1e8)
			hasFlow = true
			mc.MainNetYi = mainNetYi
		}
	}
	// 基准趋势（上证 vs MA60/MA200）：大盘弱势时模型应更保守。失败静默缺席。
	var benchBars []datasource.Bar
	if _, bars, err := s.market.GetBenchmarkBars(ctx, market, 250); err == nil && len(bars) > 0 {
		sort.Slice(bars, func(i, j int) bool { return bars[i].TradeDate < bars[j].TradeDate })
		benchBars = bars
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
	regime := computeRegime(benchBars, breadth, mainNetYi, hasFlow, defaultRegimeParams())
	return mc, regime
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
		if c.QuoteAsOf != "" {
			row["quote_as_of"] = c.QuoteAsOf // 现价的行情时刻（时效硬门核验过），模型据此声明数据时点
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
		// M3a 扩展信号：上过榜/进过人气榜才带字段（缺失=无该信号，prompt 已声明不得臆测）。
		if c.LhbNetYi != 0 {
			row["lhb_net_yi"] = c.LhbNetYi
			if c.LhbReason != "" {
				row["lhb_reason"] = c.LhbReason
			}
		}
		if c.OrgNetYi != 0 {
			row["org_net_yi"] = c.OrgNetYi
			row["org_buys"] = c.OrgBuys
		}
		if c.PopRank > 0 {
			row["pop_rank"] = c.PopRank
			if c.PopNew {
				row["pop_new"] = true
			}
		}
		if c.ScoreDims != nil {
			row["score_dims"] = c.ScoreDims
		}
		if c.Factors != nil {
			row["factors"] = c.Factors
		}
		if c.Fin != nil {
			row["fin"] = c.Fin
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
func (s *RecommendationService) buildMessages(userID int64, recType string, strat *strategyTemplate, market string, count int, llmCands []candidate, filters RecFilters, mktCtx *recMarketContext) []chatMessage {
	var sys strings.Builder
	// M3c：module=recommend 的自定义模板整段替换默认角色与铁律段（占位符宽容渲染）。
	// 反编造由 parseAndFilterPicks 程序化兜底，自定义文本弱化纪律也放不进池外标的。
	intro := recRoleIntro
	if custom, ok := promptOverrideFor(userID, model.PromptModuleRecommend, map[string]string{
		"type": recTypeLabel(recType), "strategy": strat.Name,
		"market": market, "count": strconv.Itoa(count),
	}); ok {
		intro = custom
	}
	sys.WriteString(intro)
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
	u.WriteString("同时请在 rejected 数组中，对名单内未入选的标的各给一句话落选理由（≤20 字），只解释名单内标的。\n\n")
	u.WriteString("【量化初选名单】（JSON；price 现价、change_pct 当日涨跌%、amount_yi 成交额亿元、turnover_rate 换手%、volume_ratio 量比、float_cap_yi 流通市值亿元、pe_ttm 市盈率TTM（负=亏损）、pb 市净率、senti_score 当日新闻聚合情绪分-1~1（senti_news 为条数，字段缺失=当日无相关新闻，不得臆测消息面）；lhb_net_yi 最近一次上龙虎榜的净买额亿元（负=净卖出，lhb_reason 为上榜原因，缺失=近期未上榜）、org_net_yi 机构席位净买额亿元（org_buys 为机构买入次数）、pop_rank 股吧人气榜名次（pop_new=true 新上榜；人气是关注度信号非基本面，高人气也意味着拥挤与情绪退潮风险）；factors：ma5/ma10/ma20/ma60 均线、chg_5d/chg_20d 近5/20日涨跌%、high_20d 创20日新高、bull_align 多头排列、vol_boost 今日量/5日均量、bias_20 MA20乖离%、volatility_20 波动率%、drawdown_20 近20日最大回撤%、pos_60 60日区间位置、rsi_14 RSI(14,Wilder)、macd_dif/macd_dea/macd_hist MACD(12,26,9)（柱=2×(DIF−DEA)）、macd_gold DIF在DEA上方、macd_cross_up 近3日金叉、boll_up/boll_mid/boll_low 布林带(20,2σ)、boll_pos 布林带内位置%、atr_14/atr_pct 真实波幅及其占现价%、chip_profit 获利盘%（收盘价下方筹码占比）、chip_avg_cost 筹码平均成本（chip_bars 为筹码窗口根数，缺失=未计算，不得臆测筹码面）、main_net_days 主力资金连续净流入天数（负=连续净流出，main_net_5d_yi 为近5日主力净额亿元，缺失=资金流数据暂不可得，不得臆测资金面）、盘中因子（intraday_date 为归属交易日的 T-1 盘中形态，缺失=盘中数据暂不可得，不得臆测盘中走势）：tail30_chg 尾盘30分钟涨幅%、tail30_vol_pct 尾盘30分钟量占全天%（均匀线12.5，>20 为尾盘异常放量）、morning_chg 早盘1小时涨幅%、close_vs_vwap 收盘相对全天均价偏离%（正=收在均价上方买方主导）、pm_vwap_up=true 下午均价高于上午（日内重心上移）；fin 财务摘要（长线名单）：roe 加权ROE%、revenue_yoy/net_profit_yoy 营收/净利同比%、gross_margin/net_margin 毛利率/净利率%、debt_ratio 资产负债率%、report 报告期（fin 缺失=财务数据暂不可得，不得臆测）；指标/估值字段缺失表示该数据暂不可得，不得臆测）：\n")
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
价位纪律：要求止盈>买入区间上沿>买入区间下沿>止损，价格贴近现价合理设置；止损建议参考 MA20 附近或现价-5%~-7%（可用名单中 factors.ma20 锚定）；止盈到止损的距离比（盈亏比）至少 1.5，不足时降为 watch 并说明。
输出纪律（生成有严格时限，超量输出会被上游截断导致整体作废）：reason ≤3 条、risks ≤3 条、evidence ≤4 条，每条 ≤40 字；invalidation 一句话。精炼比全面重要。`

const longTermSpec = `本次为【长线推荐】。名单含实时行情、估值快照（PE-TTM/PB/市值）、技术因子与 fin 财务摘要（最新一期报告的 ROE/营收与净利同比增速/毛利率/净利率/资产负债率，report 字段标注报告期）。fin 字段缺失表示该股财务数据暂不可得，如实说明、不得臆测；fin 只有最新一期，不含多期趋势与三表明细，判断长期成长持续性时应指出这一局限。每个 pick 需包含字段：
- symbol: 名单中的代码
- action: "buy"(可考虑逢低布局) 或 "watch"(观察等待)
- confidence: 0-100 整数
- reason: 字符串数组，长期看好/关注的理由
- risks: 字符串数组，主要风险
- evidence: 字符串数组，数据依据（引用名单中的具体字段与数值，含 PE/PB/市值与 fin 中的 ROE/增速等财务依据）
- thesis: 基本面/投资逻辑（一段话；只能基于名单给出的估值水位与 fin 财务摘要，不得虚构行业对比或未提供的财务明细）
- valuation_low / valuation_high: 合理估值区间（若估值数据缺失无法给出可填 0 并在 thesis 说明）
- key_metrics: 字符串数组，需持续跟踪的关键指标（如营收增速、毛利率、市占率）
- review_cycle: 复盘周期（如"每季度财报后"）
- disclaimer: 风险与免责提示
输出纪律（生成有严格时限，超量输出会被上游截断导致整体作废）：reason ≤3 条、risks ≤3 条、evidence ≤4 条、key_metrics ≤4 条，每条 ≤40 字；thesis ≤120 字。精炼比全面重要。`

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

// CreateStopLossAlert S1-4 执行纪律：对某条推荐的止损价一键创建到价提醒（price/lte，
// 命中自动暂停），复用现有 alert 链路。仅本人；无止损价的条目明确报错。
func (s *RecommendationService) CreateStopLossAlert(ctx context.Context, userID, recID int64, alerts *AlertService) (*model.AlertRule, error) {
	var rec model.Recommendation
	if err := common.DB.Where("id = ? AND user_id = ?", recID, userID).First(&rec).Error; err != nil {
		return nil, errors.New("推荐条目不存在")
	}
	var d recPick
	if rec.DetailJSON == "" || json.Unmarshal([]byte(rec.DetailJSON), &d) != nil || d.StopLoss <= 0 {
		return nil, errors.New("该推荐没有止损价，无法创建止损提醒")
	}
	return alerts.Create(ctx, userID, AlertInput{
		Symbol: rec.Symbol, Market: rec.Market, Name: rec.Name,
		Kind: model.AlertKindPrice, Op: "lte", Threshold: d.StopLoss, Once: true,
		Note: fmt.Sprintf("推荐止损提醒：%s 止损价 %.2f（一键创建）", rec.Name, d.StopLoss),
	})
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
		"candidate_count", "regime", "llm_config_id", "provider", "model", "prompt_version", "strategy_version",
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
