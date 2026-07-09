package model

import "time"

// M3a 扩展数据六张表：龙虎榜/机构统计/人气榜/涨停池/市场情绪聚合/个股资金流。
// 全部为可由上游重建的缓存表（东财 datacenter/push2ex/emappdata/push2his），
// 无 user_id（市场级公共数据）。涨停池与情绪聚合的历史上游不可回溯，
// 靠每日盘后快照自行积累——库损丢的是历史情绪序列，当日数据可重拉。

// LhbEntry 龙虎榜每日详情（RPT_DAILYBILLBOARD_DETAILSNEW）。同一只股票同日可因
// 多个上榜原因出现多行，唯一键含 change_type（上榜类别代码）。
type LhbEntry struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	Symbol     string `gorm:"size:16;uniqueIndex:idx_lhb_key;index:idx_lhb_sym" json:"symbol"`
	Market     string `gorm:"size:8;uniqueIndex:idx_lhb_key" json:"market"`
	TradeDate  string `gorm:"size:10;uniqueIndex:idx_lhb_key;index" json:"trade_date"`
	ChangeType string `gorm:"size:24;uniqueIndex:idx_lhb_key" json:"change_type"`
	Name       string `gorm:"size:64" json:"name"`

	Reason       string  `gorm:"size:128" json:"reason"` // 上榜原因（EXPLANATION）
	Note         string  `gorm:"size:128" json:"note"`   // 东财附注（EXPLAIN，如「1家机构买入」）
	Close        float64 `json:"close"`
	ChangePct    float64 `json:"change_pct"`
	NetBuy       float64 `json:"net_buy"`   // 龙虎榜净买额（元）
	BuyAmt       float64 `json:"buy_amt"`   // 龙虎榜买入额（元）
	SellAmt      float64 `json:"sell_amt"`  // 龙虎榜卖出额（元）
	DealAmt      float64 `json:"deal_amt"`  // 龙虎榜成交额（元）
	NetRatio     float64 `json:"net_ratio"` // 净买额占当日总成交比 %
	TurnoverRate float64 `json:"turnover_rate"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LhbOrgDaily 机构买卖每日统计（RPT_ORGANIZATION_TRADE_DETAILS）：机构专用席位
// 的买卖次数与净买额——「龙虎榜机构买入」推荐加分项的权威来源。
type LhbOrgDaily struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_lhborg_key;index:idx_lhborg_sym" json:"symbol"`
	Market    string `gorm:"size:8;uniqueIndex:idx_lhborg_key" json:"market"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_lhborg_key;index" json:"trade_date"`
	Name      string `gorm:"size:64" json:"name"`

	Close     float64 `json:"close"`
	ChangePct float64 `json:"change_pct"`
	BuyTimes  int     `json:"buy_times"`  // 机构席位买入次数
	SellTimes int     `json:"sell_times"` // 机构席位卖出次数
	BuyAmt    float64 `json:"buy_amt"`    // 机构买入额（元）
	SellAmt   float64 `json:"sell_amt"`   // 机构卖出额（元）
	NetBuy    float64 `json:"net_buy"`    // 机构净买额（元）
	NetRatio  float64 `json:"net_ratio"`  // 净买额占总成交比 %
	Reason    string  `gorm:"size:128" json:"reason"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PopularityRank 股吧人气榜每日快照（前 100）。prev_rank<=0 = 昨日不在榜（新上榜，
// 上游 hisRc 负值语义，实测为 -3，不硬编码具体值）。
type PopularityRank struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_pop_key;index:idx_pop_sym" json:"symbol"`
	Market    string `gorm:"size:8;uniqueIndex:idx_pop_key" json:"market"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_pop_key;index" json:"trade_date"`

	Rank     int  `json:"rank"`      // 当前名次 1~100
	PrevRank int  `json:"prev_rank"` // 昨日名次；<=0 = 新上榜
	IsNew    bool `json:"is_new"`    // 新上榜（prev_rank<=0）

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LimitUpStock 涨停池每日明细（push2ex getTopicZTPool 盘后快照）。
type LimitUpStock struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_ztpool_key;index:idx_ztpool_sym" json:"symbol"`
	Market    string `gorm:"size:8;uniqueIndex:idx_ztpool_key" json:"market"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_ztpool_key;index" json:"trade_date"`
	Name      string `gorm:"size:64" json:"name"`

	Price        float64 `json:"price"`
	Amount       float64 `json:"amount"`    // 成交额（元）
	FloatCap     float64 `json:"float_cap"` // 流通市值（元）
	TurnoverRate float64 `json:"turnover_rate"`
	Streak       int     `json:"streak"`        // 连板数
	FirstSealAt  int     `json:"first_seal_at"` // 首次封板时间 HHMMSS
	LastSealAt   int     `json:"last_seal_at"`
	SealFund     float64 `json:"seal_fund"`   // 封板资金（元）
	BreakCount   int     `json:"break_count"` // 炸板次数
	Industry     string  `gorm:"size:32" json:"industry"`
	StatDays     int     `json:"stat_days"`  // x 天 y 板：天数
	StatCount    int     `json:"stat_count"` // x 天 y 板：板数

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MarketMoodDaily 市场情绪温度计每日聚合（涨停/炸板/连板高度/昨涨停溢价）。
// 由涨停池三接口盘后聚合而来，市场分析与收盘日报的「情绪段」数据源。
type MarketMoodDaily struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Market    string `gorm:"size:8;uniqueIndex:idx_mood_key" json:"market"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_mood_key;index" json:"trade_date"`

	LimitUpCount int     `json:"limit_up_count"` // 涨停家数（收盘口径）
	BrokenCount  int     `json:"broken_count"`   // 炸板家数
	BrokenRate   float64 `json:"broken_rate"`    // 炸板率 % = 炸板/(涨停+炸板)
	MaxStreak    int     `json:"max_streak"`     // 最高连板
	// StreakDistJSON 连板分布 {"1":50,"2":10,"3":4}（键=连板数，值=家数）。
	StreakDistJSON string  `gorm:"size:256" json:"streak_dist_json"`
	YztCount       int     `json:"yzt_count"`     // 昨日涨停家数
	YztAvgChg      float64 `json:"yzt_avg_chg"`   // 昨涨停今日平均涨跌幅 %（打板溢价）
	YztUpRatio     float64 `json:"yzt_up_ratio"`  // 昨涨停今日红盘比例 %
	SealFundTop    float64 `json:"seal_fund_top"` // 最大单股封板资金（元，情绪强度参考）

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FundFlowDaily 个股逐日资金流（push2his fflow/daykline 按需拉取+缓存）。
// 金额单位元；close/change_pct 为上游随行返回的收盘口径（免二次对齐日线）。
type FundFlowDaily struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_fflow_key;index:idx_fflow_sym" json:"symbol"`
	Market    string `gorm:"size:8;uniqueIndex:idx_fflow_key" json:"market"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_fflow_key" json:"trade_date"`

	MainNet   float64 `json:"main_net"` // 主力净流入（元）= 超大单 + 大单
	SuperNet  float64 `json:"super_net"`
	LargeNet  float64 `json:"large_net"`
	MediumNet float64 `json:"medium_net"`
	SmallNet  float64 `json:"small_net"`
	MainPct   float64 `json:"main_pct"` // 主力净占比 %
	Close     float64 `json:"close"`
	ChangePct float64 `json:"change_pct"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
