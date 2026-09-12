package model

// DailyBarWriteLock 为同一标的的窗口写入和全量重锚提供共同的事务锁。
// 独立于 stocks / market_sync_states：ETF 等非全市场宇宙标的也必须参与，
// 但不能因为获得锁就改变“用户跟踪”或历史初始化的标的集合。
type DailyBarWriteLock struct {
	Market string `gorm:"size:8;primaryKey"`
	Symbol string `gorm:"size:16;primaryKey"`
}
