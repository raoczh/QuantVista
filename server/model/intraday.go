package model

import "time"

// IntradayFactorDaily 每股每日盘中因子（M3b：腾讯 5 分钟线盘后聚合，一行一股一天）。
// 市场级公共缓存表，无 user_id。
//
// 设计取舍：只存物化因子、不存原始 5 分钟 bar——因子是 5 分钟线的消费终点
//（推荐短线加分项 + 未来的盘中策略回测都只吃因子列），原始 bar 体积是日线的
// 48 倍（全市场约 25 万行/天），个人库存储不划算。上游可回溯约 18 个交易日，
// 更早历史靠每日盘后快照积累（与涨停池同款诚实原则）；日后若新增盘中因子，
// 从当日起积累即可，不追溯。
//
// 诚实门槛：盘中策略回测需累计 ≥60 个交易日样本才开放（样本不足的回测结论
// 不可信）；当前仅作推荐 T-1 加分信号消费。
type IntradayFactorDaily struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_intraday_key;index:idx_intraday_sym" json:"symbol"`
	Market    string `gorm:"size:8;uniqueIndex:idx_intraday_key" json:"market"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_intraday_key;index" json:"trade_date"`

	Tail30Chg    float64 `json:"tail30_chg"`     // 尾盘30分钟涨幅 %（14:30 收盘价 → 15:00 收盘价）
	Tail30VolPct float64 `json:"tail30_vol_pct"` // 尾盘30分钟成交量占全天 %（均匀分布基线 12.5%）
	MorningChg   float64 `json:"morning_chg"`    // 早盘1小时涨幅 %（开盘价 → 10:30 收盘价）
	CloseVsVwap  float64 `json:"close_vs_vwap"`  // 收盘价相对全天 VWAP 偏离 %（正=收在均价上方）
	PmVwapUp     bool    `json:"pm_vwap_up"`     // 下午 VWAP > 上午 VWAP（日内重心上移）
	Vwap         float64 `json:"vwap"`           // 全天 VWAP（量×典型价(O+H+L+C)/4 估算，上游无成交额）
	BarCount     int     `json:"bar_count"`      // 当日 5 分钟根数（完整=48；<46 不落行）

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
