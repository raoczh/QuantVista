package model

import "time"

// StockUniverseDaily 全市场股票宇宙每日快照（S0-3 point-in-time 基础）。
//
// 目的：防幸存者偏差——历史回放的候选宇宙必须按 as_of 日重建（退市/ST/停牌/行业
// 归属在当日的真实状态），`available_at` 语义由 trade_date 承载（收盘快照当日可知）。
// 消费端（S3 历史重放/召回评估）后置，落库动作提前到第一批：越早积累越好。
//
// 体积：约 5500 行/日 ≈ 135 万行/年，与 daily_bars 同量级，个人库可承受。
// 数据源：东财 clist 全市场快照（SyncMarketWide 每日 16:10 增量顺手落，同一次上游请求零额外成本）。
type StockUniverseDaily struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_sud_key" json:"trade_date"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_sud_key" json:"symbol"`
	Market    string `gorm:"size:8" json:"market"`
	Name      string `gorm:"size:64" json:"name"` // 当日名称（ST 戴帽/摘帽、更名历史由逐日快照天然留痕）

	IsST      bool `json:"is_st"`      // 当日名称含 ST/退（isSTName 口径）
	Suspended bool `json:"suspended"`  // 当日停牌（快照价格/成交量无效）

	Close        float64 `gorm:"type:decimal(20,4)" json:"close"`
	PrevClose    float64 `gorm:"type:decimal(20,4)" json:"prev_close"`
	Amount       float64 `json:"amount"`        // 成交额（元）
	TurnoverRate float64 `gorm:"type:decimal(12,4)" json:"turnover_rate"`
	PE           float64 `gorm:"type:decimal(12,4)" json:"pe"`     // 市盈率（动）；亏损为负、缺失/停牌为 0
	PETTM        float64 `gorm:"type:decimal(12,4)" json:"pe_ttm"`
	PB           float64 `gorm:"type:decimal(12,4)" json:"pb"`
	Industry     string  `gorm:"size:32" json:"industry"` // 东财行业板块名（f100；退市壳票为空）

	CreatedAt time.Time `json:"created_at"`
}
