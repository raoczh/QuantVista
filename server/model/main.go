package model

import (
	"errors"
	"sort"

	"quantvista/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
		&DailyBarWriteLock{},
		&TradingCalendar{},
		&MarketSnapshot{},
		&DataSyncLog{},
		&Watchlist{},
		&WatchlistItem{},
		&WatchlistBatch{},
		&WatchlistBatchItem{},
		&PortfolioAccount{},
		&Position{},
		&PositionTrade{},
		&ImportBatch{},
		&ImportRow{},
		&ImportRowClaim{},
		&ImportEffect{},
		&PortfolioSnapshot{},
		&PortfolioCashFlow{},
		&TargetAllocationRevision{},
		&AnalysisRecord{},
		&RecommendationBatch{},
		&RankingModelArtifact{},
		&RankingScoringPolicy{},
		&RankingPolicyAudit{},
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
		&BrowserNotificationPreference{},
		&BrowserNotificationDevice{},
		&WebPushSubscription{},
		&BrowserNotificationEvent{},
		&BrowserNotificationDelivery{},
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
		&BenchmarkDaily{},
		&CorporateAction{},
		&RestrictedRelease{},
		&IpoSubscription{},
		&PositionCorpAdjust{},
		&PaperCorpAdjust{},
		&SellReview{},
		&PositionExitAssessment{},
		&PositionExitOutcome{},
		&CandidateDiscoveryRun{},
		&CandidateDiscoveryItem{},
		&CandidateAuditRun{},
		&CandidateAuditItem{},
	}
}

// Migrate 启动时自动迁移表结构。
func Migrate() error {
	common.SysLog("开始数据库自动迁移 ...")
	if err := prepareGitHubIdentityMigration(); err != nil {
		return err
	}
	// P2 账户维度是一次需要“先补列和数据、后建唯一索引”的升级。若直接对旧库执行
	// 全量 AutoMigrate，多条 paper_accounts 会先得到相同的 account_id=0，新唯一索引
	// 将在归属迁移运行前冲突。这里仍只用 GORM 表达 DDL，业务数据回填保持幂等。
	if err := preparePortfolioAccountMigration(); err != nil {
		return err
	}
	if err := common.DB.AutoMigrate(AllModels()...); err != nil {
		return err
	}
	// 账户维度升级后，旧的 user/kind 级唯一索引会阻止同一用户拥有多个账户。
	// AutoMigrate 只创建新索引，不保证移除这些遗留约束。
	for _, old := range []struct {
		model any
		name  string
	}{
		{&PortfolioSnapshot{}, "idx_psnap_uniq"},
		{&PaperHolding{}, "idx_ph_uniq"},
		// 只删旧的 user_id **唯一**索引；同名普通索引是 PaperAccount.UserID 的 gorm:"index"
		// 每次 AutoMigrate 重建的合法索引，曾被一并列入而每次启动都「建了再删」。
		{&PaperAccount{}, "uni_paper_accounts_user_id"},
		{&PaperCorpAdjust{}, "idx_papercorpadj_uniq"},
		{&PositionCorpAdjust{}, "idx_corpadj_uniq"},
		{&ImportBatch{}, "idx_import_dedupe"},
		{&ImportRowClaim{}, "idx_import_claim"},
	} {
		if common.DB.Migrator().HasIndex(old.model, old.name) {
			if err := common.DB.Migrator().DropIndex(old.model, old.name); err != nil {
				return err
			}
		}
	}
	// GuardEvent v2 把 position_id 纳入唯一键，使同一股票的多笔持仓可分别发送统一
	// 卖出风险通知。AutoMigrate 不会删除旧索引；保留 idx_guard_key 会继续按 symbol
	// 冲突并吞掉第二笔持仓。普通守护事件的 position_id 均为 0，原有去重语义不变。
	if m := common.DB.Migrator(); m.HasIndex(&GuardEvent{}, "idx_guard_key") {
		if err := m.DropIndex(&GuardEvent{}, "idx_guard_key"); err != nil {
			return err
		}
	}
	// 发现明细最初按“交易日+版本+通道+股票”唯一，参数或因子版本在同日升级时，
	// 新运行会与旧事实撞索引。现改为 run+channel+symbol；旧索引必须显式删除，
	// AutoMigrate 只会创建新索引，不会移除旧约束。
	if m := common.DB.Migrator(); m.HasIndex(&CandidateDiscoveryItem{}, "idx_cdi_day_channel_symbol") {
		if err := m.DropIndex(&CandidateDiscoveryItem{}, "idx_cdi_day_channel_symbol"); err != nil {
			return err
		}
	}
	// 运行身份 v2 把 factor_version 纳入唯一键；旧同名约束无法由 AutoMigrate 原地
	// 改列，改用新索引名创建后再删除旧索引，升级过程不需要用户手工 SQL。
	if m := common.DB.Migrator(); m.HasIndex(&CandidateDiscoveryRun{}, "idx_cdr_identity") {
		if err := m.DropIndex(&CandidateDiscoveryRun{}, "idx_cdr_identity"); err != nil {
			return err
		}
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

// prepareGitHubIdentityMigration 在唯一索引创建前将未绑定身份归一为 NULL。
// 历史非空重复必须人工核对归属，不能在迁移时猜测应该保留哪个账号。
func prepareGitHubIdentityMigration() error {
	if !common.DB.Migrator().HasTable(&User{}) || !common.DB.Migrator().HasColumn(&User{}, "GithubID") {
		return nil
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		var duplicates []struct{ GithubID string }
		if err := tx.Model(&User{}).Select("github_id").Where("github_id IS NOT NULL AND github_id <> ?", "").
			Group("github_id").Having("COUNT(*) > 1").Limit(1).Find(&duplicates).Error; err != nil {
			return err
		}
		if len(duplicates) != 0 {
			return errors.New("存在同一 GitHub 身份绑定多个账号的历史记录，请核对归属后再迁移；绑定关系未改动")
		}
		return tx.Model(&User{}).Where("github_id = ?", "").UpdateColumn("github_id", nil).Error
	})
}

func preparePortfolioAccountMigration() error {
	if err := common.DB.AutoMigrate(&PortfolioAccount{}); err != nil {
		return err
	}
	for _, row := range []any{
		&Position{}, &PositionTrade{}, &PortfolioSnapshot{},
		&PaperAccount{}, &PaperHolding{}, &PaperTrade{},
		&PositionCorpAdjust{}, &PaperCorpAdjust{},
		&ImportBatch{}, &ImportRowClaim{}, &ImportEffect{},
	} {
		if !common.DB.Migrator().HasTable(row) || common.DB.Migrator().HasColumn(row, "AccountID") {
			continue
		}
		if err := common.DB.Migrator().AddColumn(row, "AccountID"); err != nil {
			return err
		}
	}
	return MigratePortfolioAccounts()
}

// MigratePortfolioAccounts 把升级前按 user_id 隔离的真实/模拟账本归入各自默认账户。
// 全部 UPDATE 都只触及 account_id=0，默认账户由唯一 DefaultKey 防重复，重复启动无副作用。
func MigratePortfolioAccounts() error {
	if common.DB == nil {
		return nil
	}
	type ownerKind struct {
		UserID int64
		Kind   string
	}
	owners := map[ownerKind]struct{}{}
	collect := func(table string, kind string) error {
		var ids []int64
		if !common.DB.Migrator().HasTable(table) {
			return nil
		}
		if err := common.DB.Table(table).Where("user_id > 0").Distinct().Pluck("user_id", &ids).Error; err != nil {
			return err
		}
		for _, id := range ids {
			owners[ownerKind{UserID: id, Kind: kind}] = struct{}{}
		}
		return nil
	}
	for _, item := range []struct{ table, kind string }{
		{"positions", PortfolioKindReal}, {"position_trades", PortfolioKindReal},
		{"position_corp_adjusts", PortfolioKindReal},
		{"paper_accounts", PortfolioKindPaper}, {"paper_holdings", PortfolioKindPaper}, {"paper_trades", PortfolioKindPaper}, {"paper_corp_adjusts", PortfolioKindPaper},
	} {
		if err := collect(item.table, item.kind); err != nil {
			return err
		}
	}
	if common.DB.Migrator().HasTable(&ImportBatch{}) {
		var ids []int64
		if err := common.DB.Model(&ImportBatch{}).Where("user_id > 0 AND kind IN ?", []string{ImportKindPosition, ImportKindTrade}).Distinct().Pluck("user_id", &ids).Error; err != nil {
			return err
		}
		for _, id := range ids {
			owners[ownerKind{UserID: id, Kind: PortfolioKindReal}] = struct{}{}
		}
	}
	if common.DB.Migrator().HasTable(&PortfolioSnapshot{}) {
		var rows []ownerKind
		if err := common.DB.Model(&PortfolioSnapshot{}).Select("DISTINCT user_id, kind").Where("user_id > 0").Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if row.Kind == PortfolioKindReal || row.Kind == PortfolioKindPaper {
				owners[row] = struct{}{}
			}
		}
	}

	orderedOwners := make([]ownerKind, 0, len(owners))
	for owner := range owners {
		orderedOwners = append(orderedOwners, owner)
	}
	sort.Slice(orderedOwners, func(i, j int) bool {
		if orderedOwners[i].UserID != orderedOwners[j].UserID {
			return orderedOwners[i].UserID < orderedOwners[j].UserID
		}
		return orderedOwners[i].Kind < orderedOwners[j].Kind
	})
	return common.DB.Transaction(func(tx *gorm.DB) error {
		// 多实例按同一顺序获取默认账户锁，避免随机 map 顺序造成交叉等待。
		for _, owner := range orderedOwners {
			key := portfolioDefaultKey(owner.UserID, owner.Kind)
			name := "默认真实账户"
			if owner.Kind == PortfolioKindPaper {
				name = "默认模拟账户"
			}
			row := PortfolioAccount{UserID: owner.UserID, Name: name, Kind: owner.Kind, Currency: "CNY",
				Status: PortfolioStatusActive, IsDefault: true, DefaultKey: &key}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "default_key"}}, DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
			// 冲突不改已有账户；MySQL 使用当前读，避免旧快照漏掉并发创建的默认账户。
			row = PortfolioAccount{}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("default_key = ?", key).First(&row).Error; err != nil {
				return err
			}
			updates := []struct {
				model any
				where string
				args  []any
			}{
				{&PortfolioSnapshot{}, "user_id = ? AND kind = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID, owner.Kind}},
			}
			if owner.Kind == PortfolioKindReal {
				updates = append(updates,
					struct {
						model any
						where string
						args  []any
					}{&Position{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID}},
					struct {
						model any
						where string
						args  []any
					}{&PositionTrade{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID}},
					struct {
						model any
						where string
						args  []any
					}{&PositionCorpAdjust{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID}},
				)
			} else {
				updates = append(updates,
					struct {
						model any
						where string
						args  []any
					}{&PaperAccount{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID}},
					struct {
						model any
						where string
						args  []any
					}{&PaperHolding{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID}},
					struct {
						model any
						where string
						args  []any
					}{&PaperTrade{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID}},
					struct {
						model any
						where string
						args  []any
					}{&PaperCorpAdjust{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{owner.UserID}},
				)
			}
			for _, update := range updates {
				if !tx.Migrator().HasTable(update.model) {
					continue
				}
				if err := tx.Model(update.model).Where(update.where, update.args...).UpdateColumn("account_id", row.ID).Error; err != nil {
					return err
				}
			}
			if owner.Kind == PortfolioKindReal && tx.Migrator().HasTable(&ImportBatch{}) {
				if err := tx.Model(&ImportBatch{}).Where("user_id = ? AND kind IN ? AND (account_id = 0 OR account_id IS NULL)", owner.UserID, []string{ImportKindPosition, ImportKindTrade}).UpdateColumn("account_id", row.ID).Error; err != nil {
					return err
				}
				batchIDs := tx.Model(&ImportBatch{}).Select("id").Where("user_id = ? AND account_id = ?", owner.UserID, row.ID)
				for _, audit := range []any{&ImportRowClaim{}, &ImportEffect{}} {
					if !tx.Migrator().HasTable(audit) {
						continue
					}
					if err := tx.Model(audit).Where("user_id = ? AND batch_id IN (?) AND (account_id = 0 OR account_id IS NULL)", owner.UserID, batchIDs).UpdateColumn("account_id", row.ID).Error; err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}
