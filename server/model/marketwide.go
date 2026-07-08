package model

import "time"

// MarketSyncState 全市场日线覆盖的每股状态（M1）：既是全市场标的字典，又是
// 历史初始化的断点续传进度表（参照 StockNova init_progress 思路）。
//
// 有意不复用 stocks 表：stocks 是"用户查过/持有的标的"语义，SyncTrackedDailyBars
// 按 800 只上限轮转同步它——把全市场 5500 只灌进去会稀释轮转、让已跟踪标的的
// 日线新鲜度失守。全市场宇宙独立成表，两条链路互不干扰。
type MarketSyncState struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	Symbol string `gorm:"size:16;index:idx_mss_symbol_market,unique" json:"symbol"`
	Market string `gorm:"size:8;index:idx_mss_symbol_market,unique" json:"market"`
	Name   string `gorm:"size:64" json:"name"`
	// InitStatus 历史初始化状态：pending 待拉 / done 完成 / failed 连续失败放弃
	//（退市整理、长期停牌标的 kline 拉不到，fail_count 达阈值后不再重试）。
	InitStatus string `gorm:"size:16;index;default:pending" json:"init_status"`
	// BarsCount 最近一次初始化/重锚落库的根数（审计用，非实时精确值）。
	BarsCount int `json:"bars_count"`
	// LastBarDate 该股 daily_bars 内最新交易日（增量同步维护，YYYY-MM-DD）。
	LastBarDate string `gorm:"size:10" json:"last_bar_date"`
	// AdjustEpoch 最近一次除权全量重锚日（YYYY-MM-DD，空=从未重锚）。
	AdjustEpoch string `gorm:"size:10" json:"adjust_epoch"`
	// FailCount 初始化连续失败计数（成功清零；达 wideInitMaxFail 置 failed）。
	FailCount int    `json:"fail_count"`
	LastError string `gorm:"size:256" json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
