package model

import "time"

// 公司行动与打新日历（B8/B9）：来自东财 datacenter 四张 RPT_* 报表，每日 19:25 增量同步。
//
// **单位口径铁律**（与 datasource/emcorpaction.go 的换算收口一一对应，落库后全链路统一）：
//   - 股数 = 股（上游万股已换算）、金额 = 元（上游万元已换算）、比例 = 百分比数值（上游小数已 ×100）；
//   - 送转与派息保持「每 10 股」口径原值（BonusRatio=2 表示每 10 股送 2 股）——
//     这是 A 股方案的通行表述，B8 折算公式直接按此口径写，**不要预先除以 10**。

// 公司行动方案进度（ASSIGN_PROGRESS 原文透传，这里给出常见取值供消费方判断）。
const (
	CorpActionProgressImplemented = "实施分配" // 已确定除权除息日，可用于持仓折算
)

// CorporateAction 分红送转方案（RPT_SHAREBONUS_DET）。
//
// 同一方案用上游 PLAN_NOTICE_DATE（预案公告日）稳定识别；ExDate 会在预案推进和日期
// 订正时变化，不能单独承担身份。写入统一经过 service 层按 PlanNoticeDate 查找并就地
// 更新，兼容升级前没有该列的数据。存储唯一索引额外保留 ExDate 与 NoticeDate，以容纳
// 稳定字段缺失时同报告期的多份方案；它们不是业务身份。
type CorporateAction struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	Symbol string `gorm:"size:16;index;uniqueIndex:idx_corpaction_storage_v2_uniq" json:"symbol"`
	Market string `gorm:"size:8;uniqueIndex:idx_corpaction_storage_v2_uniq" json:"market"`
	Name   string `gorm:"size:64" json:"name"`

	ReportDate string `gorm:"size:10;uniqueIndex:idx_corpaction_storage_v2_uniq" json:"report_date"` // 报告期 YYYY-MM-DD
	ExDate     string `gorm:"size:10;index;uniqueIndex:idx_corpaction_storage_v2_uniq" json:"ex_date"`
	RecordDate string `gorm:"size:10" json:"record_date"` // 股权登记日
	// PlanNoticeDate 方案首次预案公告日，跨「预案 -> 实施」及日期订正保持稳定。
	PlanNoticeDate string `gorm:"size:10;index;uniqueIndex:idx_corpaction_storage_v2_uniq" json:"plan_notice_date"`
	NoticeDate     string `gorm:"size:10;index;uniqueIndex:idx_corpaction_storage_v2_uniq" json:"notice_date"`

	BonusRatio     float64 `gorm:"type:decimal(20,4)" json:"bonus_ratio"`     // 每 10 股送股（股）
	TransferRatio  float64 `gorm:"type:decimal(20,4)" json:"transfer_ratio"`  // 每 10 股转增（股）
	DividendPretax float64 `gorm:"type:decimal(20,4)" json:"dividend_pretax"` // 每 10 股派息税前（元）
	DividendYield  float64 `gorm:"type:decimal(20,4)" json:"dividend_yield"`  // 股息率 %

	Progress    string `gorm:"size:32" json:"progress"`      // 方案进度（实施分配/董事会预案…）
	PlanProfile string `gorm:"size:255" json:"plan_profile"` // 方案描述原文

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HasAdjustment 该方案是否需要生成持仓账务调整（送/转/派任一非零）。
// 纯派息不改变持仓成本，但要作为已实现收益入账，故三者取或。
func (c *CorporateAction) HasAdjustment() bool {
	return c.BonusRatio > 0 || c.TransferRatio > 0 || c.DividendPretax > 0
}

// RestrictedRelease 限售解禁（RPT_LIFT_STAGE）。
//
// **自然唯一键 (symbol, market, free_date, free_type)**：同一只股票同一天可能有多批
// 不同类型的限售股同时解禁（如「首发原股东限售股份」+「定向增发机构配售股份」），
// 只按 (symbol, free_date) 去重会把多批合并成一批、规模少算。free_type 上游为中文
// 类型名，是该日多批之间唯一稳定的区分维度（上游无批次 ID）。
type RestrictedRelease struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	Symbol string `gorm:"size:16;index;uniqueIndex:idx_lift_uniq" json:"symbol"`
	Market string `gorm:"size:8;uniqueIndex:idx_lift_uniq" json:"market"`
	Name   string `gorm:"size:64" json:"name"`

	FreeDate string `gorm:"size:10;index;uniqueIndex:idx_lift_uniq" json:"free_date"` // 解禁日
	FreeType string `gorm:"size:64;uniqueIndex:idx_lift_uniq" json:"free_type"`       // 解禁类型

	// FreeShares **本次**解禁股数（股）——上游 CURRENT_FREE_SHARES（万股）已换算。
	// 注意上游同名字段 FREE_SHARES 是「解禁后已流通股数」，不是本次解禁量，勿混。
	FreeShares    float64 `gorm:"type:decimal(20,4)" json:"free_shares"`
	LiftMarketCap float64 `gorm:"type:decimal(20,4)" json:"lift_market_cap"` // 解禁市值（元）
	FreeRatio     float64 `gorm:"type:decimal(20,4)" json:"free_ratio"`      // 占解禁前流通股 %
	TotalRatio    float64 `gorm:"type:decimal(20,4)" json:"total_ratio"`     // 占总股本 %

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 申购品种。
const (
	IpoKindStock = "stock" // 新股
	IpoKindCb    = "cb"    // 可转债
)

// IpoSubscription 新股 / 可转债申购（RPTA_APP_IPOAPPLY + RPT_BOND_CB_LIST 两源合表）。
//
// 业务稳定身份是 (kind, code)：code 为标的代码（新股=股票代码、可转债=转债代码），
// 同一发行的 apply_date 被上游订正时就地更新；完整窗口同步还会清理取消项和历史重复。
// 表上的旧存储唯一索引仍含 apply_date，service 层负责按稳定身份收敛。**申购代码
// apply_code 不进身份**——
// 沪市新股申购代码与股票代码不同、可转债申购代码是独立的 CORRECODE，
// 上游订正申购代码时应就地更新而不是新增一行。
type IpoSubscription struct {
	ID   int64  `gorm:"primaryKey" json:"id"`
	Kind string `gorm:"size:8;uniqueIndex:idx_ipo_uniq" json:"kind"` // stock / cb
	Code string `gorm:"size:16;uniqueIndex:idx_ipo_uniq" json:"code"`
	Name string `gorm:"size:64" json:"name"`

	ApplyCode string `gorm:"size:16" json:"apply_code"`                                // 申购代码（用户下单敲的）
	ApplyDate string `gorm:"size:10;index;uniqueIndex:idx_ipo_uniq" json:"apply_date"` // 网上申购日

	IssuePrice float64 `gorm:"type:decimal(20,4)" json:"issue_price"` // 发行价（元；0=未定价）
	ApplyUpper float64 `gorm:"type:decimal(20,4)" json:"apply_upper"` // 网上申购上限（股；可转债不适用为 0）
	PayDate    string  `gorm:"size:10" json:"pay_date"`               // 中签缴款日
	BallotDate string  `gorm:"size:10" json:"ballot_date"`            // 中签号公布日
	ListDate   string  `gorm:"size:10" json:"list_date"`              // 上市日（未定为空）
	Board      string  `gorm:"size:32" json:"board"`                  // 板块（新股）

	// 可转债专属：正股关联与评级、发行规模（亿元）。
	StockCode    string  `gorm:"size:16;index" json:"stock_code"`
	StockName    string  `gorm:"size:64" json:"stock_name"`
	Rating       string  `gorm:"size:16" json:"rating"`
	IssueScaleYi float64 `gorm:"type:decimal(20,4)" json:"issue_scale_yi"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 除权除息持仓调整建议状态机（B8）。
//
//	pending   → 待用户确认（job/查询时生成，幂等）
//	confirmed → 用户已确认并写入账本（含 adjust 流水）
//	reverted  → 用户撤销了确认，账本已回滚（终态之一，可再次确认）
//	dismissed → 用户忽略本次调整（终态，不再提示）
const (
	CorpAdjustPending   = "pending"
	CorpAdjustConfirmed = "confirmed"
	CorpAdjustReverted  = "reverted"
	CorpAdjustDismissed = "dismissed"
)

// PositionCorpAdjust 除权除息持仓调整建议（B8）。
//
// **纪律：程序绝不静默改写用户真实账本**——本表是「建议 + 审计」，只有用户显式确认
// 才会落 PositionTrade{side:adjust} 并改写 Position。调整前后的数量/成本、来源公司行动
// 与状态全部显式成列（**不塞进 note**），撤销与重复确认的判定都基于这些列。
//
// **唯一键 (user_id, position_id, corporate_action_id)**：同一笔持仓对同一个公司行动
// 只生成一条建议，重复扫描幂等；撤销后状态回 reverted 复用同一行（不新增），
// 保证「一次除权只调一次」在任何重跑顺序下都成立。
type PositionCorpAdjust struct {
	ID         int64 `gorm:"primaryKey" json:"id"`
	UserID     int64 `gorm:"index;uniqueIndex:idx_corpadj_account_uniq,priority:1" json:"user_id"`
	AccountID  int64 `gorm:"index;uniqueIndex:idx_corpadj_account_uniq,priority:2" json:"account_id"`
	PositionID int64 `gorm:"index;uniqueIndex:idx_corpadj_account_uniq,priority:3" json:"position_id"`
	// CorporateActionID 来源公司行动（显式外键列，撤销/重复确认判定与审计都靠它）。
	CorporateActionID int64 `gorm:"index;uniqueIndex:idx_corpadj_account_uniq,priority:4" json:"corporate_action_id"`

	Symbol     string `gorm:"size:16;index" json:"symbol"`
	Market     string `gorm:"size:8" json:"market"`
	Name       string `gorm:"size:64" json:"name"`
	ExDate     string `gorm:"size:10;index" json:"ex_date"`
	RecordDate string `gorm:"size:10" json:"record_date"`

	// 方案快照（生成时固化，避免上游后续修订让已确认的调整对不上账）。
	BonusRatio     float64 `gorm:"type:decimal(20,4)" json:"bonus_ratio"`
	TransferRatio  float64 `gorm:"type:decimal(20,4)" json:"transfer_ratio"`
	DividendPretax float64 `gorm:"type:decimal(20,4)" json:"dividend_pretax"`
	PlanProfile    string  `gorm:"size:255" json:"plan_profile"`

	// 调整前后账面（生成时按当时持仓算；确认时会用行锁内的实时值二次校验）。
	QtyBefore float64 `gorm:"type:decimal(20,4)" json:"qty_before"`
	// EntitledQty 股权登记日收盘时实际有权数量；除权日新买入的份额不参与本次送转派息。
	EntitledQty float64 `gorm:"type:decimal(20,4)" json:"entitled_qty"`
	QtyAfter    float64 `gorm:"type:decimal(20,4)" json:"qty_after"`
	CostBefore  float64 `gorm:"type:decimal(20,4)" json:"cost_before"` // 每股加权成本（元）
	CostAfter   float64 `gorm:"type:decimal(20,4)" json:"cost_after"`
	// CashDividend 本次到手现金分红（元，税前）= 每 10 股派息 × EntitledQty / 10。
	CashDividend float64 `gorm:"type:decimal(20,4)" json:"cash_dividend"`

	// ManualReview 表示历史流水已发生在送转之前，当前聚合态不能安全自动倒序折算。
	// 此类建议只用于把风险显式呈现并提供人工确认后的忽略入口，绝不允许确认入账。
	ManualReview bool   `gorm:"default:false" json:"manual_review"`
	ReviewReason string `gorm:"size:255" json:"review_reason"`

	// PeakBefore/PeakAfter 持仓期最高价（D15）折算前后值。**撤销时按 PeakBefore 原值
	// 还原而非反算折算公式**——反算会引入舍入漂移，让撤销后的峰值与折算前差几厘。
	// PeakBefore=0 表示折算时该持仓尚无峰值记录（旧建议），撤销时不动峰值。
	PeakBefore float64 `gorm:"type:decimal(20,4)" json:"peak_before"`
	PeakAfter  float64 `gorm:"type:decimal(20,4)" json:"peak_after"`

	Status string `gorm:"size:16;index" json:"status"` // pending/confirmed/reverted/dismissed
	// TradeID 确认时写入的 adjust 流水 id（撤销时据此删除；未确认为 0）。
	TradeID     int64      `json:"trade_id"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
	RevertedAt  *time.Time `json:"reverted_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PaperCorpAdjust 模拟盘除权除息自动调整审计（B8）。
//
// 模拟盘是虚拟账户、无真实后果，**自动执行**（与真实持仓的「必须用户确认」相反）。
// **唯一键 (user_id, corporate_action_id)**：按 action 保证重跑不重复调整或发钱。
// 当前 holding 自动折算；已清仓的纯现金分红也会落零数量审计，同一 action 对同一用户
// 仍只执行一次。模拟账户 Reset 必须连本表一并清空，否则新账户会被旧审计误判已处理。
type PaperCorpAdjust struct {
	ID                int64 `gorm:"primaryKey" json:"id"`
	UserID            int64 `gorm:"index;uniqueIndex:idx_papercorpadj_account_uniq,priority:1" json:"user_id"`
	AccountID         int64 `gorm:"index;uniqueIndex:idx_papercorpadj_account_uniq,priority:2" json:"account_id"`
	CorporateActionID int64 `gorm:"index;uniqueIndex:idx_papercorpadj_account_uniq,priority:3" json:"corporate_action_id"`

	Symbol     string `gorm:"size:16;index" json:"symbol"`
	Market     string `gorm:"size:8" json:"market"`
	Name       string `gorm:"size:64" json:"name"`
	ExDate     string `gorm:"size:10;index" json:"ex_date"`
	RecordDate string `gorm:"size:10" json:"record_date"`

	QtyBefore    float64 `gorm:"type:decimal(20,4)" json:"qty_before"`
	EntitledQty  float64 `gorm:"type:decimal(20,4)" json:"entitled_qty"`
	QtyAfter     float64 `gorm:"type:decimal(20,4)" json:"qty_after"`
	CostBefore   float64 `gorm:"type:decimal(20,4)" json:"cost_before"`
	CostAfter    float64 `gorm:"type:decimal(20,4)" json:"cost_after"`
	CashDividend float64 `gorm:"type:decimal(20,4)" json:"cash_dividend"` // 记入模拟账户现金（元）
	PlanProfile  string  `gorm:"size:255" json:"plan_profile"`

	CreatedAt time.Time `json:"created_at"`
}
