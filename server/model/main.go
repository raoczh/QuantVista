package model

import "quantvista/common"

// AllModels 需要 AutoMigrate 的模型清单。新增表只往这里加。
// 注意：AutoMigrate 只建表/加列/加索引，不做删列/改类型等破坏性变更——
// 那类变更参照 new-api 在迁移函数里写一次性 SQL（见 docs/DEPLOYMENT.md）。
func AllModels() []any {
	return []any{
		&User{},
		&UserPreference{},
		&RefreshToken{},
		&Option{},
		&UserQuota{},
		&LLMConfig{},
		&Stock{},
		&StockQuote{},
		&DailyBar{},
		&TradingCalendar{},
		&MarketSnapshot{},
		&DataSyncLog{},
		&Watchlist{},
		&WatchlistItem{},
		&Position{},
		&AnalysisRecord{},
		&RecommendationBatch{},
		&Recommendation{},
		&RecommendationStatus{},
		&AlertRule{},
		&AlertEvent{},
		&AiConversation{},
		&AiConversationMessage{},
		&StockScore{},
		&PaperAccount{},
		&PaperHolding{},
		&PaperTrade{},
		&NotifyChannel{},
		&PromptTemplate{},
		&ThesisCard{},
		&ResearchNote{},
		&DailyReport{},
		&News{},
		&StockSentiment{},
		&EarningsForecast{},
		&EarningsExpress{},
		&DisclosureSchedule{},
		&Announcement{},
		&FinanceIndicator{},
		&FinanceStatement{},
	}
}

// Migrate 启动时自动迁移表结构。
func Migrate() error {
	common.SysLog("开始数据库自动迁移 ...")
	if err := common.DB.AutoMigrate(AllModels()...); err != nil {
		return err
	}
	common.SysLog("数据库自动迁移完成")
	return nil
}
