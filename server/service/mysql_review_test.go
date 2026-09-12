package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	mysqlconfig "github.com/go-sql-driver/mysql"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// 此回归只接受本机专用容器端口，不读取应用 MYSQL_DSN 或 .env。
// QV_REVIEW_MYSQL=1 时连接无密码的临时 root，创建并仅删除本用例命名的独立库。
func setupMySQLReviewDB(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	if os.Getenv("QV_REVIEW_MYSQL") != "1" {
		t.Skip("仅在本机隔离 MySQL 容器上运行：127.0.0.1:33317，QV_REVIEW_MYSQL=1")
	}
	config := mysqlconfig.NewConfig()
	config.User, config.Net, config.Addr = "root", "tcp", "127.0.0.1:33317"
	config.ParseTime, config.Loc = true, time.Local
	config.Timeout, config.ReadTimeout, config.WriteTimeout = 5*time.Second, 15*time.Second, 15*time.Second
	admin, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("qv_review_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("清理本用例临时库失败：%v", err)
		}
	})
	config.DBName = name
	db, err := gorm.Open(mysqldriver.Open(config.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(6)
	t.Cleanup(func() { _ = sqlDB.Close() })
	var isolation string
	if err := db.Raw("SELECT @@transaction_isolation").Scan(&isolation).Error; err != nil || isolation != "REPEATABLE-READ" {
		t.Fatalf("回归必须在 MySQL 可重复读下运行：isolation=%s err=%v", isolation, err)
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	oldDB, oldSQLite := common.DB, common.UsingSQLite
	common.DB, common.UsingSQLite = db.WithContext(ctx), false
	t.Cleanup(func() { cancel(); common.DB, common.UsingSQLite = oldDB, oldSQLite })
	return common.DB
}

func TestMySQLPositionEditSeesConcurrentTrade(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.Position{}, &model.PositionTrade{}, &model.DailyBar{}, &model.PortfolioSnapshot{})
	const userID int64 = 961
	account := model.PortfolioAccount{UserID: userID, Kind: model.PortfolioKindReal, Name: "并发回归", Status: model.PortfolioStatusActive}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	p := seedHoldingWithLedger(t, userID, "600071", 10, 100, 0, 0, "2026-06-01")
	if err := db.Model(&p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	writerLocked, readerWaiting, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var writerSeen atomic.Bool
	var readerOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const before, after = "review_mysql_before_account_lock", "review_mysql_after_account_lock"
	if err := db.Callback().Query().Before("gorm:query").Register(before, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_accounts" && writerSeen.Load() {
			readerOnce.Do(func() { close(readerWaiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().After("gorm:query").Register(after, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_accounts" && writerSeen.CompareAndSwap(false, true) {
			close(writerLocked)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
				tx.AddError(tx.Statement.Context.Err())
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(before); db.Callback().Query().Remove(after) })
	svc := &PositionService{}
	writerResult, readerResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := svc.AddTrade(userID, p.ID, PositionTradeInput{Side: "buy", Price: 10, Quantity: 100, TradeDate: "2026-06-02"})
		writerResult <- err
	}()
	select {
	case <-writerLocked:
	case err := <-writerResult:
		t.Fatalf("加仓未进入持锁阶段：%v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("等待加仓持锁超时")
	}
	go func() {
		_, err := svc.Update(userID, p.ID, PositionInput{BuyPrice: p.BuyPrice, Quantity: p.Quantity, BuyDate: p.BuyDate,
			PositionType: p.PositionType, UserNote: "旧表单不能覆盖并发加仓"})
		readerResult <- err
	}()
	select {
	case <-readerWaiting:
	case err := <-readerResult:
		t.Fatalf("编辑未等待账户锁：%v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("等待编辑竞争账户锁超时")
	}
	unblock()
	if err := <-writerResult; err != nil {
		t.Fatalf("并发加仓失败：%v", err)
	}
	if err := <-readerResult; err == nil || !strings.Contains(err.Error(), "已有加/减仓流水") {
		t.Fatalf("旧编辑表单必须看到并发新增流水并被拒绝：%v", err)
	}
	var stored model.Position
	if err := db.First(&stored, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Quantity != 200 || stored.TotalBuyCost != 2000 || count != 2 {
		t.Fatalf("持仓汇总必须与两笔流水一致：quantity=%v cost=%v trades=%d", stored.Quantity, stored.TotalBuyCost, count)
	}
}

func TestMySQLPositionRiskWaitsForArchive(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.Position{}, &model.PositionExitAssessment{})
	account := model.PortfolioAccount{UserID: 1003, Kind: model.PortfolioKindReal, Name: "归档并发", Status: model.PortfolioStatusActive}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	p := model.Position{UserID: account.UserID, AccountID: account.ID, Symbol: "600106", Market: "cn",
		Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	row := evaluatePositionExit(positionExitInput{position: p, quote: freshExitQuote(now, 8, 8, 8), now: now,
		rules: []model.AlertRule{{Kind: model.AlertKindCostDrawdown, Threshold: 10}}, session: model.PositionExitSessionIntraday}, defaultPositionExitParams)
	writer := db.Begin()
	if writer.Error != nil {
		t.Fatal(writer.Error)
	}
	t.Cleanup(func() { writer.Rollback() })
	if err := writer.Model(&model.PortfolioAccount{}).Where("id = ?", account.ID).Update("status", model.PortfolioStatusArchived).Error; err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	const callback = "review_archive_wait"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_accounts" {
			once.Do(func() { close(waiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	result := make(chan error, 1)
	go func() {
		inserted, notify, err := persistPositionExitAssessment(context.Background(), &row)
		if err == nil && (inserted || notify) {
			err = fmt.Errorf("归档后仍提交风险事实 inserted=%v notify=%v", inserted, notify)
		}
		result <- err
	}()
	select {
	case <-waiting:
	case err := <-result:
		t.Fatalf("风险提交未等待账户锁：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("未进入账户锁等待")
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("归档提交后评估未结束")
	}
	var count int64
	if err := db.Model(&model.PositionExitAssessment{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("旧快照不得绕过归档：count=%d err=%v", count, err)
	}
}

func TestMySQLPaperCostMatchesCash(t *testing.T) {
	setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.PaperAccount{}, &model.PaperHolding{}, &model.PaperTrade{},
		&model.CorporateAction{}, &model.PaperCorpAdjust{}, &model.PortfolioSnapshot{})
	checkPaperLargeQuantityRoundTrip(t)
}

func TestMySQLPortfolioReadUsesConsistentSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.PaperAccount{}, &model.PaperHolding{}, &model.PaperTrade{},
		&model.PaperCorpAdjust{}, &model.CorporateAction{}, &model.PortfolioSnapshot{}, &model.Stock{}, &model.TradingCalendar{})
	now := reviewSnapshotClock(t)
	account, trade := seedRiskReadAccount(t, 1051, model.PortfolioKindPaper, now.Format("2006-01-02"))
	traded := false
	const callback = "review_risk_read_between_cash_and_holdings"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if traded || (tx.Statement.Table != "paper_accounts" && tx.Statement.Table != "paper_holdings") {
			return
		}
		traded = true
		if err := trade(); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	svc := NewPortfolioRiskService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})), nil)
	view, err := svc.Overview(t.Context(), account.UserID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !traded || view.TotalAssets.Value != 99995 || view.MarketValue != 10000 || view.Cash.Value != 89995 {
		t.Fatalf("两个数据库查询之间成交后仍须读到同一账本：traded=%v view=%+v", traded, view)
	}
	var cash model.PaperAccount
	if err := db.Where("account_id = ?", account.ID).First(&cash).Error; err != nil || cash.Cash != 69990 {
		t.Fatalf("并发交易必须确已独立提交：cash=%+v err=%v", cash, err)
	}
}

func TestMySQLPromptMutationPreservesNullableLegacyBaseline(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PromptTemplate{}, &model.PromptTemplateRevision{}, &model.PromptChampionState{})
	for i, action := range []string{"edit", "delete"} {
		t.Run(action, func(t *testing.T) {
			legacy := model.PromptTemplate{UserID: int64(1097 + i), Module: model.PromptModuleQa, Content: "MySQL 旧模板原文", Enabled: true}
			if err := db.Create(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&legacy).Updates(map[string]any{"content_hash": nil, "revision": nil}).Error; err != nil {
				t.Fatal(err)
			}
			var err error
			if action == "edit" {
				_, _, err = NewPromptService().Upsert(legacy.UserID, PromptInput{Module: legacy.Module, Content: "新模板原文", Enabled: true})
			} else {
				err = NewPromptService().Delete(legacy.UserID, legacy.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			var history []model.PromptTemplateRevision
			if err := db.Where("template_id = ?", legacy.ID).Order("revision").Find(&history).Error; err != nil {
				t.Fatal(err)
			}
			if len(history) == 0 || history[0].Content != legacy.Content || (action == "edit" && len(history) != 2) {
				t.Fatalf("NULL 元数据旧行的 %s 仍须保留原文：%+v", action, history)
			}
		})
	}
}

func TestMySQLPromptRuntimeKeepsContentAndGenerationTogether(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PromptTemplate{}, &model.PromptTemplateRevision{}, &model.PromptChampionState{})
	const userID int64 = 1101
	svc := NewPromptService()
	if _, _, err := svc.Upsert(userID, PromptInput{Module: model.PromptModuleRecommend, Content: "模板 A", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	changed := false
	const callback = "review_prompt_generation_content_interleave"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if changed || tx.Statement.Table != "prompt_champion_states" {
			return
		}
		changed = true
		if _, _, err := svc.Upsert(userID, PromptInput{Module: model.PromptModuleRecommend, Content: "模板 B", Enabled: true}); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	runtime := loadPromptRuntime(userID, model.PromptModuleRecommend)
	if !changed || runtime.ReadError != nil || !runtime.GenerationKnown ||
		!((runtime.Raw == "模板 A" && runtime.Generation == 1) || (runtime.Raw == "模板 B" && runtime.Generation == 2)) {
		t.Fatalf("实际模板与单调代次不能跨越一次编辑：runtime=%+v changed=%v", runtime, changed)
	}
}

func TestMySQLQuotaReservesOnlyAvailableActions(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.UserQuota{})
	const userID int64 = 1114
	if err := db.Create(&model.UserQuota{UserID: userID, ActionLimit: 1}).Error; err != nil {
		t.Fatal(err)
	}
	type reservation struct {
		ctx    context.Context
		finish func()
		err    error
	}
	results := make(chan reservation, 12)
	start := make(chan struct{})
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			ctx, finish, err := beginManualQuotaAction(t.Context(), userID)
			results <- reservation{ctx, finish, err}
		}()
	}
	close(start)
	accepted := []reservation{}
	for i := 0; i < cap(results); i++ {
		result := <-results
		if result.err == nil {
			accepted = append(accepted, result)
		} else if RefusalCodeOf(result.err) != RefusalQuotaExhausted {
			t.Errorf("并发额度预留出现非额度错误：%v", result.err)
		}
	}
	for _, item := range accepted {
		noteManualQuotaResponse(item.ctx, &chatResult{Content: "唯一获准的模型响应"})
		item.finish()
	}
	quota, err := getUserQuota(userID)
	if err != nil || len(accepted) != 1 || quota.ActionUsed != 1 {
		t.Fatalf("12 个并发动作仅可放行剩余的 1 次额度：accepted=%d quota=%+v err=%v", len(accepted), quota, err)
	}
}

func TestMySQLRiskCurveDoesNotMixOldSnapshotsWithNewLedger(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.PortfolioCashFlow{}, &model.Position{}, &model.PositionTrade{},
		&model.PositionCorpAdjust{}, &model.CorporateAction{}, &model.PortfolioSnapshot{}, &model.Stock{}, &model.TradingCalendar{}, &model.DailyBar{})
	account, trade := seedRiskReadAccount(t, 1052, model.PortfolioKindReal, "2026-08-03")
	for _, date := range []string{"2026-08-03", "2026-08-04"} {
		if err := db.Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.PortfolioSnapshot{UserID: account.UserID, AccountID: account.ID, Kind: model.PortfolioKindReal, TradeDate: date, MarketValue: 10000}).Error; err != nil {
			t.Fatal(err)
		}
	}
	traded := false
	const callback = "review_risk_read_old_snapshots_new_ledger"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if traded || tx.Statement.Table != "portfolio_snapshots" {
			return
		}
		traded = true
		if err := trade(); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	view, err := NewPortfolioRiskService(nil, nil).Risk(t.Context(), account.UserID, account.ID, NewPortfolioRiskParameters(30, 252, 0, "", "2026-08-04"))
	if err != nil {
		t.Fatal(err)
	}
	if !traded || len(view.Curve) != 2 || view.Curve[0].Assets != 99995 || view.Curve[1].Assets != 99995 || view.PartialCount != 0 {
		t.Fatalf("后录交易不能混入此前读取的完整历史快照：traded=%v view=%+v", traded, view)
	}
	var changed int64
	if err := db.Model(&model.PortfolioSnapshot{}).Where("account_id = ? AND partial = ?", account.ID, true).Count(&changed).Error; err != nil || changed != 2 {
		t.Fatalf("后录交易应已在独立事务提交并失效旧快照：changed=%d err=%v", changed, err)
	}
}

func TestMySQLNotificationConfigurationLimits(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.User{}, &model.UserPreference{}, &model.NotifyChannel{}, &model.BrowserNotificationDevice{}, &model.WebPushSubscription{})
	for _, id := range []int64{1135, 1136, 1137} {
		seedNotificationReviewUser(t, id)
	}
	if _, err := NewNotifyService().Create(1137, NotifyChannelInput{Kind: model.NotifyKindWebhook, Target: "https://hook.example.com/" + strings.Repeat("a", 800)}); err != nil {
		t.Fatalf("合法长地址的密文不能受原 512 字符字段截断：%v", err)
	}
	var wg sync.WaitGroup
	var channelWins, deviceWins atomic.Int32
	for i := 0; i < 18; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := NewNotifyService().Create(1135, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT_LIMIT_LOCAL_TEST"}); err == nil {
				channelWins.Add(1)
			} else if !strings.Contains(err.Error(), "上限") {
				t.Errorf("通道创建出现非预期错误：%v", err)
			}
		}()
		go func(index int) {
			defer wg.Done()
			if _, err := NewBrowserNotificationService().UpsertSubscription(1136, BrowserSubscriptionInput{DeviceKey: fmt.Sprintf("review-device-limit-%02d", index)}); err == nil {
				deviceWins.Add(1)
			} else if !strings.Contains(err.Error(), "上限") {
				t.Errorf("设备创建出现非预期错误：%v", err)
			}
		}(i)
	}
	wg.Wait()
	var channels, devices int64
	if err := db.Model(&model.NotifyChannel{}).Where("user_id = ?", 1135).Count(&channels).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.BrowserNotificationDevice{}).Where("user_id = ? AND enabled = ?", 1136, true).Count(&devices).Error; err != nil {
		t.Fatal(err)
	}
	if channels != maxChannelsPerUser || devices != maxBrowserDevices || channelWins.Load() != maxChannelsPerUser || deviceWins.Load() != maxBrowserDevices {
		t.Fatalf("并发创建不能突破数量上限：channels=%d channelWins=%d devices=%d deviceWins=%d", channels, channelWins.Load(), devices, deviceWins.Load())
	}
}

func TestMySQLJobFailureNoticeMergesAfterConcurrentCommit(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.User{}, &model.JobFailureNotification{})
	const userID int64 = 1133
	seedNotificationReviewUser(t, userID)
	owner := int64(userID)
	firstRun := model.JobRun{ID: 201, UserID: userID, OwnerType: model.JobOwnerUser, OwnerUserID: &owner, Kind: JobKindAnalysis, Status: model.JobStatusFailed}
	secondRun := firstRun
	secondRun.ID = 202
	ready, release := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	pause := func(tx *gorm.DB) {
		if paused.CompareAndSwap(false, true) {
			close(ready)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
				tx.AddError(tx.Statement.Context.Err())
			}
		}
	}
	const before, after = "review:notice_before_owner_lock", "review:notice_after_initial_read"
	if err := db.Callback().Query().Before("gorm:query").Register(before, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			pause(tx)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().After("gorm:query").Register(after, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_failure_notifications" {
			pause(tx)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(before); db.Callback().Query().Remove(after) })
	type result struct {
		claim jobFailureNoticeClaim
		err   error
	}
	done := make(chan result, 1)
	now := time.Now()
	go func() {
		claim, err := reserveJobFailureNotice(secondRun, true, now.Add(time.Second))
		done <- result{claim, err}
	}()
	select {
	case <-ready:
	case r := <-done:
		t.Fatalf("未到达通知并发同步点：%v", r.err)
	case <-time.After(5 * time.Second):
		t.Fatal("通知并发同步超时")
	}
	first, err := reserveJobFailureNotice(firstRun, true, now)
	unblock()
	second := <-done
	if err != nil || !first.Send || second.err != nil || second.claim.Send || second.claim.Notice.MergeRootID == nil || *second.claim.Notice.MergeRootID != first.Notice.ID {
		t.Fatalf("并发已提交的同类失败应合并而不是再次外发：first=%+v firstErr=%v second=%+v secondErr=%v", first, err, second.claim, second.err)
	}
}

func TestMySQLWebPushConcurrentEndpointOwnership(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.User{}, &model.UserPreference{}, &model.BrowserNotificationDevice{}, &model.WebPushSubscription{})
	setValidVAPIDEnv(t)
	for _, id := range []int64{1130, 1131} {
		seedNotificationReviewUser(t, id)
	}
	ready, release := make(chan struct{}, 2), make(chan struct{})
	var visits atomic.Int32
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review:webpush_concurrent_endpoint_lookup"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "web_push_subscriptions" && visits.Add(1) <= 2 {
			ready <- struct{}{}
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
				tx.AddError(tx.Statement.Context.Err())
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	type result struct {
		userID int64
		device *BrowserDeviceView
		err    error
	}
	done := make(chan result, 2)
	for _, id := range []int64{1130, 1131} {
		go func(userID int64) {
			view, err := NewBrowserNotificationService().UpsertSubscription(userID,
				validBrowserSubscription(fmt.Sprintf("review-endpoint-user-%d", userID), "绑定竞态", "https://push.example.com/shared-local-test"))
			done <- result{userID, view, err}
		}(id)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case r := <-done:
			t.Fatalf("绑定请求未到达同步点：user=%d err=%v", r.userID, r.err)
		case <-time.After(5 * time.Second):
			t.Fatal("等待两个独立用户读取订阅超时")
		}
	}
	unblock()
	winners := []result{}
	for i := 0; i < 2; i++ {
		if r := <-done; r.err == nil {
			winners = append(winners, r)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("同一 endpoint 只能绑定给一名用户，不能由 MySQL upsert 更新另一用户：成功数=%d", len(winners))
	}
	var subscriptions []model.WebPushSubscription
	if err := db.Find(&subscriptions).Error; err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 1 || subscriptions[0].UserID != winners[0].userID || subscriptions[0].DeviceID != winners[0].device.ID {
		t.Fatal("成功响应必须与最终订阅归属一致")
	}
}

func TestMySQLWatchlistMoveSeesRemovalAfterGroupLock(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.Watchlist{}, &model.WatchlistItem{})
	const userID int64 = 1121
	source, target := model.Watchlist{UserID: userID, Name: "源组"}, model.Watchlist{UserID: userID, Name: "目标组"}
	for _, group := range []*model.Watchlist{&source, &target} {
		if err := db.Create(group).Error; err != nil {
			t.Fatal(err)
		}
	}
	item := model.WatchlistItem{UserID: userID, WatchlistID: source.ID, Symbol: "510300", Market: "cn"}
	duplicate := model.WatchlistItem{UserID: userID, WatchlistID: target.ID, Symbol: item.Symbol, Market: item.Market}
	for _, row := range []*model.WatchlistItem{&item, &duplicate} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	writer := db.Begin()
	defer writer.Rollback()
	if _, err := lockedWatchlistGroup(writer, userID, target.ID); err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review:watchlist_move_scope_before_group_lock"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "watchlists" {
			once.Do(func() {
				close(ready)
				select {
				case <-release:
				case <-tx.Statement.Context.Done():
					tx.AddError(tx.Statement.Context.Err())
				}
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	done := make(chan error, 1)
	go func() {
		_, err := (&WatchlistService{}).UpdateItem(userID, item.ID, WatchlistItemUpdateInput{WatchlistID: &target.ID})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("未到达分组锁：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待分组锁超时")
	}
	if err := writer.Delete(&duplicate).Error; err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatalf("目标重复项已在分组锁释放前删除，不应被旧快照误拒绝：%v", err)
	}
	var stored model.WatchlistItem
	if err := db.First(&stored, item.ID).Error; err != nil || stored.WatchlistID != target.ID {
		t.Fatalf("移动结果不正确：%+v err=%v", stored, err)
	}
}

func TestMySQLDefaultWatchlistConcurrentCreation(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.Watchlist{})
	ready, release := make(chan struct{}, 2), make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_default_group_concurrency"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "watchlists" || calls.Add(1) > 2 {
			return
		}
		ready <- struct{}{}
		select {
		case <-release:
		case <-tx.Statement.Context.Done():
			tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	type outcome struct {
		group *model.Watchlist
		err   error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() { group, err := (&WatchlistService{}).EnsureDefaultGroup(1029); results <- outcome{group, err} }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("两个请求未同时读到无分组")
		}
	}
	unblock()
	var id int64
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err != nil || result.group == nil {
				t.Fatalf("并发初始化失败：%v", result.err)
			}
			if id != 0 && id != result.group.ID {
				t.Fatalf("创建了不同分组：%d/%d", id, result.group.ID)
			}
			id = result.group.ID
		case <-time.After(5 * time.Second):
			t.Fatal("初始化未完成")
		}
	}
	var count int64
	if err := db.Model(&model.Watchlist{}).Where("user_id = ?", 1029).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("默认分组必须唯一：count=%d err=%v", count, err)
	}
}

func TestMySQLDefaultPortfolioConcurrentCreation(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.PaperAccount{})
	ready, release := make(chan struct{}, 2), make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_default_portfolio_concurrency"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "portfolio_accounts" || calls.Add(1) > 2 {
			return
		}
		ready <- struct{}{}
		select {
		case <-release:
		case <-tx.Statement.Context.Done():
			tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	type outcome struct {
		account *model.PortfolioAccount
		err     error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			account, err := EnsureDefaultPortfolioAccount(1030, model.PortfolioKindPaper)
			results <- outcome{account, err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("两个请求未同时读到无默认账户")
		}
	}
	unblock()
	var id int64
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err != nil || result.account == nil {
				t.Errorf("并发默认账户初始化失败：%v", result.err)
				continue
			}
			if id != 0 && id != result.account.ID {
				t.Errorf("产生了不同默认账户：%d/%d", id, result.account.ID)
			}
			id = result.account.ID
		case <-time.After(5 * time.Second):
			t.Fatal("默认账户初始化未完成")
		}
	}
	var count int64
	if err := db.Model(&model.PaperAccount{}).Where("account_id = ?", id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("应只有一个关联现金账户：count=%d err=%v", count, err)
	}
}

func TestMySQLDailyWindowCannotCrossRebase(t *testing.T) {
	setupMySQLReviewDB(t, &model.DailyBar{}, &model.DailyBarWriteLock{}, &model.TradingCalendar{}, &model.MarketSyncState{},
		&model.FactorSnapshotDaily{}, &model.PositionExitOutcome{})
	checkDailyWindowCannotCrossRebase(t)
}

func TestMySQLDailyBarValidationAfterLock(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.DailyBar{}, &model.DailyBarWriteLock{}, &model.TradingCalendar{},
		&model.MarketSyncState{}, &model.FactorSnapshotDaily{}, &model.PositionExitOutcome{})
	const symbol = "600092"
	old := seedAdjustmentReviewBars(t, symbol, false)
	key := model.DailyBarWriteLock{Market: "cn", Symbol: symbol}
	if err := db.Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	writerLocked, readerWaiting, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, waitingOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- db.Transaction(func(tx *gorm.DB) error {
			var row model.DailyBarWriteLock
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("market = ? AND symbol = ?", "cn", symbol).First(&row).Error; err != nil {
				return err
			}
			close(writerLocked)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
				return tx.Statement.Context.Err()
			}
			// 独立事务不经过进程内标记，模拟另一个服务进程提交新的复权基准。
			return tx.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).
				Updates(map[string]any{"open": 5, "high": 5, "low": 5, "close": 5}).Error
		})
	}()
	select {
	case <-writerLocked:
	case err := <-writerDone:
		t.Fatalf("外部写入未取得锁：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待外部事务持锁超时")
	}
	const callback = "review_mysql_daily_lock_attempt"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_bar_write_locks" {
			waitingOnce.Do(func() { close(readerWaiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(callback) })
	fresh := wideGenBars(wideGenDates(250, time.Now().AddDate(0, 0, -2)), 5)
	fake := &fakeWideSource{bars: map[string][]datasource.Bar{symbol: fresh}}
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- (&MarketService{wide: fake}).persistDailyBars(context.Background(), "cn", symbol, old[len(old)-3:])
	}()
	select {
	case <-readerWaiting:
	case err := <-readerDone:
		t.Fatalf("窗口写入未等待数据库标的锁：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("等待窗口写入取得锁超时")
	}
	unblock()
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	var prices []float64
	if err := db.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).Distinct("close").Pluck("close", &prices).Error; err != nil || len(prices) != 1 || prices[0] != 5 || fake.barsCalls[symbol] != 1 {
		t.Fatalf("锁后必须看到新基准并重拉，不能用旧读视图覆盖窗口：prices=%v fetches=%d err=%v", prices, fake.barsCalls[symbol], err)
	}
}

func TestMySQLUnadjustedDerivedAudit(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.DailyBar{}, &model.DailyBarWriteLock{}, &model.MarketSyncState{},
		&model.TradingCalendar{}, &model.FactorSnapshotDaily{}, &model.PositionExitOutcome{}, &model.Position{}, &model.PositionTrade{})
	const symbol = "600093"
	bars := seedAdjustmentReviewBars(t, symbol, true)
	p := seedHoldingWithPeak(t, 999, symbol, "本机质量审计", 10, 100, 20, bars[0].TradeDate)
	if err := db.Model(p).Updates(map[string]any{"peak_date": bars[20].TradeDate, "peak_backfilled": true}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := model.FactorSnapshotDaily{Market: "cn", Symbol: symbol, TradeDate: bars[len(bars)-1].TradeDate,
		LastBarDate: bars[len(bars)-1].TradeDate, FactorsJSON: `{"close":99}`, FactorVersion: "fv2"}
	if err := db.Create(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if deleted, err := CleanupDailyBarsBefore(bars[21].TradeDate); err != nil || deleted != 21 {
		t.Fatalf("MySQL 保留期删除失败：deleted=%d err=%v", deleted, err)
	}
	if err := db.First(p, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if p.PeakPrice != 20 || p.PeakDataQuality != model.FactorQualityUnverifiedAdjustment {
		t.Fatalf("MySQL 清理必须保留峰值原值并记录待核验状态：%+v", p)
	}
	var usable int64
	if err := usableFactorSnapshots(db).Model(&model.FactorSnapshotDaily{}).Count(&usable).Error; err != nil || usable != 0 {
		t.Fatalf("MySQL 清理不得洗掉旧快照质量证据：usable=%d err=%v", usable, err)
	}
}

type mysqlLegacyIdentityUser struct {
	ID       int64  `gorm:"primaryKey"`
	Username string `gorm:"uniqueIndex;size:64"`
	GithubID string `gorm:"index;size:64"`
}

func (mysqlLegacyIdentityUser) TableName() string { return "users" }

func TestMySQLIdentityMigrationAndUniqueBinding(t *testing.T) {
	db := setupMySQLReviewDB(t, &mysqlLegacyIdentityUser{})
	rows := []mysqlLegacyIdentityUser{{Username: "legacy-a"}, {Username: "legacy-b"}, {Username: "legacy-c", GithubID: "777777"}}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		if err := model.Migrate(); err != nil {
			t.Fatalf("隔离 MySQL 第 %d 次迁移失败：%v", pass, err)
		}
	}
	for _, name := range []string{"new-a", "new-b"} {
		if err := db.Create(&model.User{Username: name}).Error; err != nil {
			t.Fatalf("未绑定身份应存 NULL 并允许多个账号：%v", err)
		}
	}
	if err := db.Create(&model.User{Username: "duplicate", GithubID: "777777"}).Error; err == nil {
		t.Fatal("MySQL 必须拒绝重复非空 GitHub 身份")
	}
	var unbound int64
	if err := db.Model(&model.User{}).Where("github_id IS NULL").Count(&unbound).Error; err != nil || unbound != 4 {
		t.Fatalf("旧空串与新未绑定账号应统一存 NULL：count=%d err=%v", unbound, err)
	}
}
