package model

import "time"

const CNBenchmarkSymbol = "000001.SH"

// BenchmarkDaily 保存已收盘基准，供只读离线评估使用。首写后保持不变，避免评估时
// 隐式请求外部服务；不与同代码个股的 daily_bars 混用。
type BenchmarkDaily struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	Market     string    `gorm:"size:8;uniqueIndex:idx_benchmark_day" json:"market"`
	Symbol     string    `gorm:"size:16;uniqueIndex:idx_benchmark_day" json:"symbol"`
	TradeDate  string    `gorm:"size:10;uniqueIndex:idx_benchmark_day" json:"trade_date"`
	Close      float64   `gorm:"type:decimal(20,6)" json:"close"`
	Source     string    `gorm:"size:32" json:"source"`
	ObservedAt time.Time `json:"observed_at"`
}
