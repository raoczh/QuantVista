package model

import "time"

// 自选股与持仓：均按 user_id 隔离，标的用 symbol+market 自然键（与数据源/行情一致，
// 不依赖 stocks 表主键；stocks 表在查询时惰性 upsert，这里冗余 name 便于无行情时展示）。

// Watchlist 自选股分组。
type Watchlist struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	UserID    int64     `gorm:"index" json:"user_id"`
	Name      string    `gorm:"size:64" json:"name"`
	SortOrder int       `gorm:"default:0" json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 机会池漏斗阶段：自选条目的研究进度（空 = 未标注，兼容旧数据）。
// 漏斗：发现 → 初筛 → 重点观察 → 等待价格 → 已生成计划 → 已买入；
// 任一阶段可转 已放弃（记录当时价格与原因，供「错过机会」复盘）/ 已复盘。
const (
	StageDiscovered   = "discovered"
	StageScreening    = "screening"
	StageWatching     = "watching"
	StageWaitingPrice = "waiting_price"
	StagePlanned      = "planned"
	StageBought       = "bought"
	StagePassed       = "passed"
	StageReviewed     = "reviewed"
)

// WatchlistItem 自选股条目。唯一约束 user_id+watchlist_id+symbol+market，避免同组重复添加。
type WatchlistItem struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	UserID      int64  `gorm:"index;index:idx_wli_uniq,unique" json:"user_id"`
	WatchlistID int64  `gorm:"index;index:idx_wli_uniq,unique" json:"watchlist_id"`
	Symbol      string `gorm:"size:16;index:idx_wli_uniq,unique" json:"symbol"`
	Market      string `gorm:"size:8;index:idx_wli_uniq,unique" json:"market"`
	Name        string `gorm:"size:64" json:"name"`
	Note        string `gorm:"size:512" json:"note"`           // 备注
	FocusReason string `gorm:"size:512" json:"focus_reason"`   // 关注原因
	IsPinned    bool   `gorm:"default:false" json:"is_pinned"` // 重点关注

	ResearchStage string     `gorm:"size:16;index:idx_wli_stage" json:"research_stage"` // 机会池漏斗阶段
	PassedReason  string     `gorm:"size:255" json:"passed_reason"`                     // 放弃原因（stage=passed）
	PassedPrice   float64    `gorm:"type:decimal(20,4)" json:"passed_price"`            // 放弃时价格（错过机会复盘基准）
	StageAt       *time.Time `json:"stage_at"`                                          // 阶段变更时间

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 持仓类型与状态。
const (
	PositionTypeShortTerm = "short_term"
	PositionTypeLongTerm  = "long_term"

	PositionStatusHolding = "holding"
	PositionStatusClosed  = "closed"
)

// Position 已购入持仓。买入必填 buy_price/buy_date/quantity；卖出后填 sell_* 与复盘。
// 盈亏在读取时用实时行情计算，不落库快照（保证展示始终最新）。
type Position struct {
	ID           int64  `gorm:"primaryKey" json:"id"`
	UserID       int64  `gorm:"index" json:"user_id"`
	Symbol       string `gorm:"size:16;index" json:"symbol"`
	Market       string `gorm:"size:8" json:"market"`
	Name         string `gorm:"size:64" json:"name"`
	PositionType string `gorm:"size:16;default:long_term" json:"position_type"` // short_term / long_term
	Status       string `gorm:"size:16;default:holding" json:"status"`          // holding / closed
	Currency     string `gorm:"size:8;default:CNY" json:"currency"`

	BuyPrice float64 `gorm:"type:decimal(20,4)" json:"buy_price"`
	BuyDate  string  `gorm:"size:10" json:"buy_date"` // YYYY-MM-DD
	Quantity float64 `gorm:"type:decimal(20,4)" json:"quantity"`
	BuyFee   float64 `gorm:"type:decimal(20,4)" json:"buy_fee"`
	BuyTax   float64 `gorm:"type:decimal(20,4)" json:"buy_tax"`

	BuyReason string `gorm:"size:512" json:"buy_reason"`
	UserNote  string `gorm:"size:512" json:"user_note"`

	// 买入前风险计划（建仓时的风险预算，供风险计算器与止损提示）。
	PlanStopLoss   float64 `gorm:"type:decimal(20,4)" json:"plan_stop_loss"`   // 计划止损价
	PlanTakeProfit float64 `gorm:"type:decimal(20,4)" json:"plan_take_profit"` // 计划止盈价
	ChecklistJSON  string  `gorm:"type:text" json:"checklist_json"`            // 买入前检查清单快照（勾选状态）

	SellPrice  float64 `gorm:"type:decimal(20,4)" json:"sell_price"`
	SellDate   string  `gorm:"size:10" json:"sell_date"`
	SellFee    float64 `gorm:"type:decimal(20,4)" json:"sell_fee"`
	SellTax    float64 `gorm:"type:decimal(20,4)" json:"sell_tax"`
	SellReason string  `gorm:"size:512" json:"sell_reason"`
	ReviewNote string  `gorm:"size:512" json:"review_note"`

	// 卖出结构化复盘（在自由文本 ReviewNote 之上的固定维度）。
	SellPlanned   string `gorm:"size:16" json:"sell_planned"`    // yes/no/partial 是否按计划卖出
	AiVerdict     string `gorm:"size:16" json:"ai_verdict"`      // right/wrong/mixed/unused 当时 AI 判断对错
	LessonLearned string `gorm:"size:512" json:"lesson_learned"` // 下次策略调整点

	// ---- 账本汇总（B5 分批加/减仓；由 PositionTrade 流水在同一事务内回写）----
	// RealizedPnl 累计已实现盈亏（元，已扣该部分的买入分摊费税与卖出费税）。
	// 持仓中也可能非 0（已部分兑现），组合总览据此把「已落袋」与「浮盈」分开统计。
	RealizedPnl float64 `gorm:"type:decimal(20,4)" json:"realized_pnl"`
	// TotalBuyCost 累计买入总成本（元，含全部买入费税）；TotalSellNet 累计卖出净收入
	//（元，已扣全部卖出费税）；TotalBuyQty 累计买入股数。三者只增不减，是「已平仓收益率」
	// 与「一共买过多少」的来源——BuyPrice/Quantity 表达的是**当前剩余**持仓，
	// 全部卖出后 Quantity 归 0，无法还原总账。
	// TotalBuyCost=0 表示该持仓尚未补建流水（旧数据），消费方回退旧算式，见 computeView。
	TotalBuyCost float64 `gorm:"type:decimal(20,4)" json:"total_buy_cost"`
	TotalSellNet float64 `gorm:"type:decimal(20,4)" json:"total_sell_net"`
	TotalBuyQty  float64 `gorm:"type:decimal(20,4)" json:"total_buy_qty"`

	// ---- 持仓期最高价（D15 移动止盈）----
	// PeakPrice 自 PeakFrom 起该标的到过的最高价（元/股，**账面口径**，与 BuyPrice 同口径）；
	// PeakDate 为该最高价所在交易日；PeakFrom 为峰值统计起始日。
	//
	// **加减仓与折算的口径定夺（改动前必读，反例测试锁定）**：
	//   - 建仓：PeakPrice=买入价、PeakFrom=买入日（无买入日则建仓当日）；
	//   - **加仓：峰值重置为加仓价、PeakFrom 重置为加仓日**——加权成本已变，加仓前的
	//     高点不再是「按当前这本账赚到过的利润」。不重置的后果是刚加完仓（往往是回调
	//     买入）当场被判成大幅回撤，系统性误报；重置是保守选择：宁可漏报不误报。
	//   - **减仓：不重置**——剩余仓位的持有期是连续的，它赚到过的高点依然算数。
	//   - **除权除息折算：按价格侧公式同步折算**（新峰值 =(旧峰值−每10股派息/10)/(1+(送+转)/10)），
	//     否则送转除权当天会凭空出现一个等于送转比例的假回撤；撤销折算时按落库的
	//     PeakBefore 原值还原（不用反算，避免舍入漂移）。
	//   - 平仓：字段保留供复盘，不再更新（持仓已不存在，回撤无意义）。
	//
	// PeakBackfilled=true 表示 PeakPrice 由本地日线**前复权序列**回填而非逐交易日累积。
	// 前复权序列在除权后整段重刷，与账面实际成交价可能有出入（方向上偏低 = 偏保守、
	// 少触发），展示与提醒文案必须据此标注，不得当作精确的账面历史最高价。
	PeakPrice      float64 `gorm:"type:decimal(20,4)" json:"peak_price"`
	PeakDate       string  `gorm:"size:10" json:"peak_date"`
	PeakFrom       string  `gorm:"size:10" json:"peak_from"`
	PeakBackfilled bool    `gorm:"default:false" json:"peak_backfilled"`

	// 来源推荐血缘（一键建仓时写入；0=手动建仓无来源）。供「AI 推荐 vs 实际买入」对比。
	RecommendationID int64 `gorm:"index" json:"recommendation_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 持仓流水方向。
const (
	PositionTradeBuy  = "buy"
	PositionTradeSell = "sell"
	// PositionTradeAdjust 除权除息折算（B8）：不是买卖，只改数量与每股成本，
	// 现金分红计入 RealizedPnl（真金到账）。**必须由用户显式确认才写**。
	PositionTradeAdjust = "adjust"
)

// PositionTrade 持仓分批加/减仓流水（B5）。
//
// **口径铁律**：`Position.BuyPrice` 恒为当前持仓的加权平均成本、`Position.Quantity`
// 恒为当前持仓数量——全部既有消费方（tracking 的 actual_position 标签、todo 止损、
// guard 事件、组合总览）读法零改动。本表是明细来源，汇总值在同一事务内回写 Position。
//
// 单位：price=元/股，quantity=股，fee/tax=元，trade_date=YYYY-MM-DD。
type PositionTrade struct {
	ID         int64 `gorm:"primaryKey" json:"id"`
	UserID     int64 `gorm:"index;index:idx_ptrade_pos" json:"user_id"`
	PositionID int64 `gorm:"index:idx_ptrade_pos" json:"position_id"`

	Side     string  `gorm:"size:8" json:"side"` // buy=加仓 / sell=减仓
	Price    float64 `gorm:"type:decimal(20,4)" json:"price"`
	Quantity float64 `gorm:"type:decimal(20,4)" json:"quantity"`
	Fee      float64 `gorm:"type:decimal(20,4)" json:"fee"`
	Tax      float64 `gorm:"type:decimal(20,4)" json:"tax"`

	TradeDate string `gorm:"size:10" json:"trade_date"`
	Note      string `gorm:"size:255" json:"note"`

	// RealizedPnl 该笔卖出结转的已实现盈亏（元）。买入笔恒 0。
	// adjust 笔（B8 除权除息）记本次到手的税前现金分红——那是真金到账，
	// 与卖出兑现同属「已实现」，不计入会让分红股的累计收益长期少算。
	RealizedPnl float64 `gorm:"type:decimal(20,4)" json:"realized_pnl"`
	// AvgCostAfter 该笔之后的加权平均成本、QuantityAfter 该笔之后的持仓数量。
	// 落库快照供流水明细还原当时账面，避免前端二次推算与服务端算法漂移。
	AvgCostAfter  float64 `gorm:"type:decimal(20,4)" json:"avg_cost_after"`
	QuantityAfter float64 `gorm:"type:decimal(20,4)" json:"quantity_after"`

	// ---- 除权除息折算审计（B8）----
	// AvgCostBefore/QuantityBefore 该笔**之前**的账面。adjust 笔必填（折算前后可逐笔核对）；
	// buy/sell 笔顺带填写便于明细阅读。**0 值有歧义**（首笔建仓前确实是 0，旧流水也是 0），
	// 仅在 adjust 笔上作为审计依据使用，不要拿它反推其它笔的历史。
	AvgCostBefore  float64 `gorm:"type:decimal(20,4)" json:"avg_cost_before"`
	QuantityBefore float64 `gorm:"type:decimal(20,4)" json:"quantity_before"`
	// CorporateActionID 来源公司行动（仅 adjust 笔非 0）；AdjustID 关联的调整建议行，
	// **状态的唯一权威在 PositionCorpAdjust.Status**，不在流水里冗余状态列（防两处不一致）。
	CorporateActionID int64 `gorm:"index" json:"corporate_action_id"`
	AdjustID          int64 `gorm:"index" json:"adjust_id"`

	// Backfilled 该笔是否为旧持仓惰性补建的等价首笔买入（非用户真实录入的加仓动作）。
	Backfilled bool `gorm:"default:false" json:"backfilled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 资产快照账户类型（B7）。
const (
	SnapshotKindReal  = "real"  // 真实持仓账本
	SnapshotKindPaper = "paper" // 模拟盘
)

// PortfolioSnapshot 每交易日盘后资产快照（B7 资产曲线）。
// 唯一键 (user_id, kind, trade_date)，job 幂等 upsert。
//
// **fail-closed**：市值一律走 FreshQuotesFor，stale/取不到的标的不用旧价冒充——
// 该用户当日快照标 Partial 并记缺口数与说明，曲线上如实呈现「当日部分标的无有效价」。
// 单位：金额=元。
type PortfolioSnapshot struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	UserID    int64  `gorm:"index;index:idx_psnap_uniq,unique" json:"user_id"`
	Kind      string `gorm:"size:8;index:idx_psnap_uniq,unique" json:"kind"`        // real / paper
	TradeDate string `gorm:"size:10;index:idx_psnap_uniq,unique" json:"trade_date"` // YYYY-MM-DD

	MarketValue   float64 `gorm:"type:decimal(20,4)" json:"market_value"`   // 持仓市值（已定价部分）
	Cost          float64 `gorm:"type:decimal(20,4)" json:"cost"`           // 持仓成本（已定价部分）
	UnrealizedPnl float64 `gorm:"type:decimal(20,4)" json:"unrealized_pnl"` // 浮动盈亏
	RealizedCum   float64 `gorm:"type:decimal(20,4)" json:"realized_cum"`   // 累计已实现盈亏
	Cash          float64 `gorm:"type:decimal(20,4)" json:"cash"`           // 可用现金（仅 paper）
	PositionCount int     `json:"position_count"`                           // 持仓标的笔数

	// Partial 当日存在无有效行情的持仓（未计入市值/浮盈）；MissingCount 为其笔数，
	// Note 为口径说明。曲线消费方必须据此标注，不得把 partial 点当完整净值。
	Partial      bool   `gorm:"default:false" json:"partial"`
	MissingCount int    `json:"missing_count"`
	Note         string `gorm:"size:255" json:"note"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
