package model

import "time"

// P3b 板块估值聚合表。每日 16:10 全市场增量成功后，由 clist 快照按 f100 行业名
// groupBy 聚合落库（AggregateBoardValuationAsync）。范围诚实声明：**只覆盖行业板块**
//（kind=industry）——个股 f100 只有行业归属，概念板块无 f100 覆盖，按施工图放弃。
// 市场级公共数据无 user_id；历史靠每日快照自行积累（同涨停池诚实原则），
// 250 日时序分位在查询侧按积累天数如实计算。
type BoardValuationDaily struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Kind      string `gorm:"size:16;uniqueIndex:idx_boardval_key" json:"kind"` // industry（概念无 f100 覆盖，预留）
	BoardCode string `gorm:"size:16;uniqueIndex:idx_boardval_key;index:idx_boardval_code" json:"board_code"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_boardval_key;index" json:"trade_date"`
	BoardName string `gorm:"size:64;index" json:"board_name"`

	// 中位数只吃正样本（PE 亏损为负、缺失/停牌为 0，聚合前按 >0 过滤）。
	// MedianPETTM 显式 column tag：GORM 默认把 PETTM 蛇形化成 pettm（F2 批 YoY→yo_y 同款坑）。
	MedianPETTM float64 `gorm:"column:median_pe_ttm" json:"median_pe_ttm"` // 板块内正 TTM 市盈率中位数
	MedianPB    float64 `json:"median_pb"`     // 板块内正市净率中位数
	PosPECount  int     `json:"pos_pe_count"`  // 正 TTM PE 样本数（中位数的分母）
	StockCount  int     `json:"stock_count"`   // 板块内个股总数（含亏损/停牌）
	// 横截面分位：该板块中位 TTM PE 在全部行业板块中的百分位（0~100，越高越贵），
	// 当日即可用；无有效 PE 样本的板块记 -1（区分「最便宜」与「算不出」）。
	PctRank float64 `json:"pct_rank"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
