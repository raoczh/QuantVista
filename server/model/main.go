package model

import "quantvista/common"

// AllModels 需要 AutoMigrate 的模型清单。新增表只往这里加。
// AutoMigrate 会执行当前 GORM 支持的兼容性变更（包括放宽非空约束），但不会
// 删除列或代替需要数据重写的业务迁移；特殊迁移仍应显式编码并保持启动幂等。
func AllModels() []any {
	return []any{
		&User{},
		&UserPreference{},
		&OnboardingProgress{},
		&RefreshToken{},
		&Option{},
		&UserQuota{},
		&LLMConfig{},
		&LLMCallLog{},
		&JobRun{},
		&JobStep{},
		&JobEvent{},
		&JobFailureNotification{},
		&TodoInboxState{},
		&ResearchArtifact{},
		&StrategyRunResult{},
		&LLMTask{},
		&Stock{},
		&StockQuote{},
		&DailyBar{},
		&TradingCalendar{},
		&MarketSnapshot{},
		&DataSyncLog{},
		&Watchlist{},
		&WatchlistItem{},
		&WatchlistBatch{},
		&WatchlistBatchItem{},
		&Position{},
		&PositionTrade{},
		&ImportBatch{},
		&ImportRow{},
		&ImportRowClaim{},
		&ImportEffect{},
		&PortfolioSnapshot{},
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
		&PromptTemplateRevision{},
		&PromptChampionState{},
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
		&MarketSyncState{},
		&ScreenerStrategy{},
		&ScreenerStrategyRevision{},
		&LhbEntry{},
		&LhbOrgDaily{},
		&PopularityRank{},
		&LimitUpStock{},
		&MarketMoodDaily{},
		&FundFlowDaily{},
		&IntradayFactorDaily{},
		&ReportRating{},
		&OrgSurvey{},
		&BoardValuationDaily{},
		&GuardEvent{},
		&RecommendationLabel{},
		&RecommendationCandidateEvent{},
		&RecommendationSelectionOutcome{},
		&RecommendationReflection{},
		&LLMExperiment{},
		&LLMExperimentModuleLock{},
		&LLMExperimentRun{},
		&LLMReleaseAudit{},
		&LLMModuleRoute{},
		&StockUniverseDaily{},
		&FactorSnapshotDaily{},
		&CorporateAction{},
		&RestrictedRelease{},
		&IpoSubscription{},
		&PositionCorpAdjust{},
		&PaperCorpAdjust{},
		&SellReview{},
	}
}

// Migrate 启动时自动迁移表结构。
func Migrate() error {
	common.SysLog("开始数据库自动迁移 ...")
	if err := common.DB.AutoMigrate(AllModels()...); err != nil {
		return err
	}
	// P0-4 存量自定义策略迁移（幂等）：为旧可变行固化 revision 1，并回填当前
	// revision 指针。失败时阻断启动，避免新扫描继续从兼容字段读取无版本内容。
	if err := MigrateScreenerStrategyRevisions(); err != nil {
		return err
	}
	// 新增字符串列在旧库里可能是 SQL NULL，而 service 层统一把「尚无稳定身份」表示为
	// 空串。先归一再跑兼容匹配，避免旧预案无法接续、被重复插入。
	if err := common.DB.Exec(
		"UPDATE corporate_actions SET plan_notice_date = '' WHERE plan_notice_date IS NULL",
	).Error; err != nil {
		return err
	}
	// 公司行动业务身份已从会变化的 ExDate 升级为 PlanNoticeDate。AutoMigrate 不会
	// 删除旧索引；保留它会让同一报告期、同一除权日的两次独立方案无法并存。
	if m := common.DB.Migrator(); m.HasIndex(&CorporateAction{}, "idx_corpaction_uniq") {
		if err := m.DropIndex(&CorporateAction{}, "idx_corpaction_uniq"); err != nil {
			return err
		}
	}
	// v2 把 NoticeDate 纳入仅用于容纳弱身份数据的存储索引。旧索引不删除会继续
	// 拒绝 PlanNoticeDate/ExDate 均空、但公告日不同的两份预案。
	if m := common.DB.Migrator(); m.HasIndex(&CorporateAction{}, "idx_corpaction_storage_uniq") {
		if err := m.DropIndex(&CorporateAction{}, "idx_corpaction_storage_uniq"); err != nil {
			return err
		}
	}
	// P0-6 存量模板基线迁移（幂等）：legacy prompt_templates 行回填 content_hash/revision
	// 并补建基线快照——保证升级后的首次修改/删除仍能回查升级前原文。
	if err := MigratePromptTemplateBaselines(); err != nil {
		common.SysWarn("prompt 模板基线迁移未完成（不阻断启动，下次启动重试）: %v", err)
	}
	if err := MigratePromptChampionStates(); err != nil {
		common.SysWarn("prompt champion generation 迁移未完成（不阻断启动，下次启动重试）: %v", err)
	}
	common.SysLog("数据库自动迁移完成")
	return nil
}
