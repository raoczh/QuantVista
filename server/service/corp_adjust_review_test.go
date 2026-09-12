package service

import (
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func seedCorpAdjustReview(t *testing.T) (*model.Position, *model.CorporateAction, model.PositionCorpAdjust) {
	t.Helper()
	p, source := seedAdjustCase(t, 12070, "2026-08-04", 0, 10, 1)
	if err := common.DB.Model(source).Updates(map[string]any{"plan_notice_date": "2026-07-01", "notice_date": "2026-07-01"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCorpAdjusts(p.UserID, "2026-08-05"); err != nil {
		t.Fatal(err)
	}
	rows, err := ListCorpAdjusts(p.UserID, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取待确认建议：rows=%+v err=%v", rows, err)
	}
	return p, source, rows[0]
}

func TestCorpAdjustActionRequiresObservedVersion(t *testing.T) {
	for _, action := range []string{"confirm", "dismiss"} {
		t.Run(action, func(t *testing.T) {
			setupTestDB(t)
			p, source, before := seedCorpAdjustReview(t)
			if before.ContextVersion == "" {
				t.Fatal("列表必须返回用户所见建议的版本")
			}
			if _, err := GenerateCorpAdjusts(p.UserID, "2026-08-05"); err != nil {
				t.Fatal(err)
			}
			rows, err := ListCorpAdjusts(p.UserID, model.CorpAdjustPending)
			if err != nil || len(rows) != 1 || rows[0].ContextVersion != before.ContextVersion {
				t.Fatalf("内容相同的例行刷新不能废弃确认版本：rows=%+v err=%v", rows, err)
			}
			if err := common.DB.Model(source).Update("transfer_ratio", 20).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := GenerateCorpAdjusts(p.UserID, "2026-08-05"); err != nil {
				t.Fatal(err)
			}
			rows, err = ListCorpAdjusts(p.UserID, model.CorpAdjustPending)
			if err != nil || len(rows) != 1 || rows[0].ContextVersion == before.ContextVersion {
				t.Fatalf("实际方案变化必须产生新版本：rows=%+v err=%v", rows, err)
			}
			svc := &PositionService{}
			apply := func(version string) error {
				if action == "confirm" {
					_, err := svc.ConfirmCorpAdjustForAccountContext(t.Context(), p.UserID, before.AccountID, before.ID, version)
					return err
				}
				_, err := svc.DismissCorpAdjustForAccountContext(t.Context(), p.UserID, before.AccountID, before.ID, version)
				return err
			}
			if err := apply(before.ContextVersion); err == nil {
				t.Fatal("不能用旧页面批准或忽略用户未见过的新方案")
			}
			if err := common.DB.First(p, p.ID).Error; err != nil || p.Quantity != 1000 {
				t.Fatalf("旧版本请求不能改动持仓：p=%+v err=%v", p, err)
			}
			if err := apply(rows[0].ContextVersion); err != nil {
				t.Fatalf("用户重新核对新方案后应可处理：%v", err)
			}
		})
	}
}

func TestCorpAdjustPostponedSourceRefreshesOldBarrier(t *testing.T) {
	setupTestDB(t)
	p, source, adj := seedCorpAdjustReview(t)
	future := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	if err := common.DB.Model(source).Updates(map[string]any{"ex_date": future, "record_date": previousDate(future)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCorpAdjusts(p.UserID, time.Now().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	rows, err := ListCorpAdjusts(p.UserID, model.CorpAdjustPending)
	if err != nil || len(rows) != 0 {
		t.Fatalf("已顺延的方案不能继续作为今日待折算项：rows=%+v err=%v", rows, err)
	}
	if err := common.DB.First(&adj, adj.ID).Error; err != nil || adj.ExDate != future {
		t.Fatalf("历史建议应保留并同步正式的新日期：adj=%+v err=%v", adj, err)
	}
	if err := common.DB.First(p, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensurePositionShareActionsProcessedTx(common.DB, *p, time.Now().Format("2006-01-02")); err != nil {
		t.Fatalf("旧日期不能继续拦截尚未除权持仓的正常交易：%v", err)
	}
}

func TestCorpAdjustCanceledSourceCanBeReviewedAndDismissed(t *testing.T) {
	setupTestDB(t)
	p, source, adj := seedCorpAdjustReview(t)
	if err := common.DB.Model(source).Update("progress", "取消分配").Error; err != nil {
		t.Fatal(err)
	}
	svc := &PositionService{}
	if _, err := svc.ConfirmCorpAdjust(p.UserID, adj.ID); err == nil {
		t.Fatal("来源已取消实施，不能继续按旧方案确认")
	}
	if _, err := GenerateCorpAdjusts(p.UserID, time.Now().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	rows, err := ListCorpAdjusts(p.UserID, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 || !rows[0].ManualReview || !strings.Contains(rows[0].ReviewReason, "非实施分配") {
		t.Fatalf("已取消来源应如实展示人工核对原因：rows=%+v err=%v", rows, err)
	}
	if _, err := svc.DismissCorpAdjustForAccountContext(t.Context(), p.UserID, adj.AccountID, adj.ID, rows[0].ContextVersion); err != nil {
		t.Fatalf("核对取消信息后应允许明确忽略：%v", err)
	}
}

func TestCorpAdjustConfirmationRejectsChangedSource(t *testing.T) {
	setupTestDB(t)
	p, source, adj := seedCorpAdjustReview(t)
	source.TransferRatio = 20
	if err := storeCorporateActions([]model.CorporateAction{*source}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PositionService{}).ConfirmCorpAdjust(p.UserID, adj.ID); err == nil {
		t.Error("正式送转方案已变更，旧建议不能继续提交")
	}
	if err := common.DB.First(p, p.ID).Error; err != nil || p.Quantity != 1000 || p.RealizedPnl != 0 {
		t.Fatalf("过期方案不能改变账本：p=%+v err=%v", p, err)
	}
}

func TestMySQLCorpAdjustChecksSourceAfterLock(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.Position{}, &model.PositionTrade{},
		&model.CorporateAction{}, &model.PositionCorpAdjust{}, &model.PortfolioSnapshot{})
	const userID int64 = 12071
	account := model.PortfolioAccount{UserID: userID, Name: "本机来源锁测试", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	p := seedHoldingWithLedger(t, userID, "600079", 10, 100, 0, 0, "2026-08-01")
	if err := db.Model(&p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	source := model.CorporateAction{Symbol: p.Symbol, Market: p.Market, ReportDate: "2026-06-30", ExDate: "2026-08-04",
		RecordDate: "2026-08-03", TransferRatio: 10, Progress: model.CorpActionProgressImplemented}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	adj := model.PositionCorpAdjust{UserID: userID, AccountID: account.ID, PositionID: p.ID, CorporateActionID: source.ID,
		Symbol: p.Symbol, Market: p.Market, ExDate: source.ExDate, RecordDate: source.RecordDate, TransferRatio: 10,
		QtyBefore: 100, QtyAfter: 200, EntitledQty: 100, CostBefore: 10, CostAfter: 5, Status: model.CorpAdjustPending}
	if err := db.Create(&adj).Error; err != nil {
		t.Fatal(err)
	}
	locked, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var writerErr error
	go func() {
		writerErr = db.Transaction(func(tx *gorm.DB) error {
			var current model.CorporateAction
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, source.ID).Error; err != nil {
				return err
			}
			close(locked)
			<-release
			return tx.Model(&model.CorporateAction{}).Where("id = ?", source.ID).Update("transfer_ratio", 20).Error
		})
		close(writerDone)
	}()
	defer func() {
		unblock()
		<-writerDone
		if writerErr != nil {
			t.Errorf("来源写入失败：%v", writerErr)
		}
	}()
	select {
	case <-locked:
	case <-writerDone:
		t.Fatalf("来源事务未取得锁：%v", writerErr)
	case <-time.After(5 * time.Second):
		t.Fatal("来源事务持锁超时")
	}
	waiting := make(chan struct{})
	var once sync.Once
	const callback = "review_corp_source_lock"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "corporate_actions" {
			once.Do(func() { close(waiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	done := make(chan error, 1)
	go func() {
		_, err := (&PositionService{}).ConfirmCorpAdjustForAccountContext(t.Context(), userID, account.ID, adj.ID, positionCorpAdjustContextVersion(adj))
		done <- err
	}()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("确认未核验正在更新的正式来源：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("确认未发起来源锁定读取")
	}
	unblock()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "方案已更新") {
			t.Errorf("锁后应按最新来源拒绝旧折算：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("来源释放后确认未完成")
	}
	if err := db.First(&p, p.ID).Error; err != nil || p.Quantity != 100 || p.RemainingCost != 1000 {
		t.Fatalf("并发来源更新后不能提交旧折算：p=%+v err=%v", p, err)
	}
}

func TestCorpAdjustRevertedRemainsActionable(t *testing.T) {
	setupTestDB(t)
	p, _, adj := seedCorpAdjustReview(t)
	svc := &PositionService{}
	if _, err := svc.ConfirmCorpAdjust(p.UserID, adj.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevertCorpAdjust(p.UserID, adj.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCorpAdjusts(p.UserID, time.Now().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	rows, err := ListCorpAdjusts(p.UserID, model.CorpAdjustPending)
	if err != nil || len(rows) != 1 || rows[0].ID != adj.ID || rows[0].Status != model.CorpAdjustReverted {
		t.Fatalf("撤销的待处理折算必须继续可见，才能再次确认或忽略：rows=%+v err=%v", rows, err)
	}
}

func TestPaperCorpAdjustRejectsChangedOrCanceledSource(t *testing.T) {
	for _, kind := range []string{"holding", "closed_cash"} {
		for _, change := range []string{"terms", "canceled"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				setupTestDB(t)
				const userID int64 = 12072
				account, err := (&PaperService{}).GetOrCreateAccount(userID)
				if err != nil {
					t.Fatal(err)
				}
				today := time.Now().In(time.Local).Format("2006-01-02")
				holding := model.PaperHolding{UserID: userID, AccountID: account.AccountID, Symbol: "600079", Market: "cn", Quantity: 100, AvgCost: 10}
				if kind == "holding" {
					if err := common.DB.Create(&holding).Error; err != nil {
						t.Fatal(err)
					}
				}
				trades := []model.PaperTrade{{UserID: userID, AccountID: account.AccountID, Symbol: holding.Symbol, Market: holding.Market,
					Side: model.PaperSideBuy, Price: 10, Quantity: 100, Amount: 1000, TradeDate: previousDate(today)}}
				if kind == "closed_cash" {
					trades = append(trades, model.PaperTrade{UserID: userID, AccountID: account.AccountID, Symbol: holding.Symbol, Market: holding.Market,
						Side: model.PaperSideSell, Price: 10, Quantity: 100, Amount: 1000, TradeDate: today})
				}
				if err := common.DB.Create(&trades).Error; err != nil {
					t.Fatal(err)
				}
				source := model.CorporateAction{Symbol: holding.Symbol, Market: holding.Market, ReportDate: "2026-06-30", PlanNoticeDate: previousDate(today),
					ExDate: today, RecordDate: previousDate(today), Progress: model.CorpActionProgressImplemented, DividendPretax: 1}
				if kind == "holding" {
					source.TransferRatio = 10
				}
				if err := common.DB.Create(&source).Error; err != nil {
					t.Fatal(err)
				}
				observed := source
				if change == "canceled" {
					source.Progress = "取消分配"
				} else {
					source.DividendPretax = 2
				}
				if err := storeCorporateActions([]model.CorporateAction{source}); err != nil {
					t.Fatal(err)
				}
				executed := false
				if kind == "holding" {
					executed = applyPaperCorpAdjust(holding, observed)
				} else {
					executed = applyPaperCashDividend(userID, account.AccountID, holding.Symbol, holding.Market, "模拟现金分红", observed)
				}
				if executed {
					t.Error("模拟盘不能用已失效的旧来源折算或补发现金")
				}
				beforeCash := account.Cash
				if err := common.DB.First(account, account.ID).Error; err != nil || account.Cash != beforeCash {
					t.Fatalf("过期来源不应改变现金：account=%+v err=%v", account, err)
				}
				var audits int64
				if err := common.DB.Model(&model.PaperCorpAdjust{}).Where("user_id = ?", userID).Count(&audits).Error; err != nil || audits != 0 {
					t.Fatalf("过期来源不应写成已处理：audits=%d err=%v", audits, err)
				}
				if kind == "holding" && change == "canceled" {
					if _, err := (&PaperService{}).TradeByAccount(t.Context(), userID, account.AccountID, TradeInput{Symbol: holding.Symbol, Market: holding.Market, Side: model.PaperSideSell, Price: 10, Quantity: 100}); err != nil {
						t.Errorf("正式取消的方案不能继续阻塞模拟交易：%v", err)
					}
				}
			})
		}
	}
}
