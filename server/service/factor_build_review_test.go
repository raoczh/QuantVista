package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func seedFactorBuildReview(t *testing.T, db *gorm.DB, status string) {
	t.Helper()
	for _, row := range []any{
		&model.MarketSyncState{Market: "cn", Symbol: "600081", Name: "审查样本", InitStatus: status, LastBarDate: "2026-09-08"},
		&model.DailyBar{Market: "cn", Symbol: "600081", TradeDate: "2026-09-08", Open: 10, High: 10, Low: 10, Close: 10,
			Volume: 1000, Amount: 1000000, Source: "eastmoney"},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestFactorBuildReviewSnapshotDoesNotUpgradeOldShortHistory(t *testing.T) {
	setupTestDB(t)
	seedFactorBuildReview(t, common.DB, "pending")
	table, err := buildFactorTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.MarketSyncState{}).Where("symbol = ?", "600081").Update("init_status", "done").Error; err != nil {
		t.Fatal(err)
	}
	if n, err := SnapshotFactorTable(table); err != nil || n != 0 {
		t.Fatalf("构建后完成初始化不能把旧的一根日线快照冻结为完整历史：n=%d err=%v", n, err)
	}
	current, err := buildFactorTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := SnapshotFactorTable(current); err != nil || n != 1 {
		t.Fatalf("使用完成初始化之后重新构建的表仍应允许固化：n=%d err=%v", n, err)
	}
}

func TestMySQLFactorBuildReviewUsesOneReadSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.MarketSyncState{}, &model.DailyBar{}, &model.CorporateAction{}, &model.TradingCalendar{}, &model.FactorSnapshotDaily{})
	seedFactorBuildReview(t, db, "done")
	const hook = "review_factor_concurrent_daily_commit"
	written := false
	if err := db.Callback().Row().Before("gorm:row").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table != "daily_bars" || written {
			return
		}
		written = true
		err := db.Transaction(func(writer *gorm.DB) error {
			if err := writer.Create(&model.DailyBar{Market: "cn", Symbol: "600081", TradeDate: "2026-09-09",
				Open: 11, High: 11, Low: 11, Close: 11, Volume: 1000, Amount: 1100000, Source: "eastmoney"}).Error; err != nil {
				return err
			}
			return writer.Model(&model.MarketSyncState{}).Where("symbol = ?", "600081").Update("last_bar_date", "2026-09-09").Error
		})
		if err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Row().Remove(hook) })
	table, err := buildFactorTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !written || table.Len() != 1 || table.TradeDate != "2026-09-08" || table.LastDates[0] != table.TradeDate || table.Col("close")[0] != 10 {
		t.Fatalf("并发同步不能把 09-09 的 11 元日线归入 09-08 因子：written=%v table=%+v", written, table)
	}
}

func TestMySQLFactorBuildReviewCancelInterruptsBlockedRead(t *testing.T) {
	for _, table := range []string{"market_sync_states", "corporate_actions", "daily_bars", "trading_calendars"} {
		t.Run(table, func(t *testing.T) {
			db := setupMySQLReviewDB(t, &model.MarketSyncState{}, &model.DailyBar{}, &model.CorporateAction{}, &model.TradingCalendar{})
			seedFactorBuildReview(t, db, "done")
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			locker, err := sqlDB.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer locker.Close()
			if _, err := locker.ExecContext(t.Context(), "LOCK TABLES `"+table+"` WRITE"); err != nil {
				t.Fatal(err)
			}
			defer locker.ExecContext(context.Background(), "UNLOCK TABLES")
			entered := make(chan struct{}, 1)
			beforeRead := func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					select {
					case entered <- struct{}{}:
					default:
					}
				}
			}
			const hook = "review_factor_blocked_read"
			if err := db.Callback().Row().Before("gorm:row").Register(hook, beforeRead); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Query().Before("gorm:query").Register(hook, beforeRead); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Row().Remove(hook); db.Callback().Query().Remove(hook) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := buildFactorTable(ctx); result <- err }()
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("未进入目标数据库读取：%v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("等待读取超时")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("取消须返回取消错误：%v", err)
				}
			case <-time.After(700 * time.Millisecond):
				t.Error("取消后查询仍在等待数据库锁，调用方超时未生效")
				_, _ = locker.ExecContext(context.Background(), "UNLOCK TABLES")
				if err := <-result; err != nil && !errors.Is(err, context.Canceled) {
					t.Logf("释放锁后返回：%v", err)
				}
			}
		})
	}
}

func TestFactorBuildReviewRetainsRebuildRequestedDuringAnotherBuild(t *testing.T) {
	setupTestDB(t)
	resetFactorTable()
	t.Cleanup(resetFactorTable)
	seedFactorBuildReview(t, common.DB, "done")
	now := time.Now()
	if err := common.DB.Create(&model.CandidateDiscoveryRun{Market: "cn", TradeDate: "2026-09-08",
		DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion, ParameterHash: discoveryParameterHash(),
		Status: DiscoveryRunStatusOK, FinishedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	const hook = "review_factor_snapshot_pause"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "factor_snapshot_dailies" {
			once.Do(func() { close(ready); <-release })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	t.Cleanup(unblock)
	first := make(chan error, 1)
	go func() { _, err := RebuildFactorTable(t.Context(), "第一轮"); first <- err }()
	select {
	case <-ready:
	case err := <-first:
		t.Fatalf("构建未进入落快照前阶段：%v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("等待第一轮构建超时")
	}
	if err := common.DB.Model(&model.DailyBar{}).Where("symbol = ?", "600081").Updates(map[string]any{
		"open": 11, "high": 11, "low": 11, "close": 11,
	}).Error; err != nil {
		unblock()
		<-first
		t.Fatal(err)
	}
	RebuildFactorTableAsync("同日第二轮数据已提交")
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	found := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if table := CurrentFactorTable(); table != nil && table.Col("close")[0] == 11 {
			found = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 等待发布后的快照/调度收尾，避免后台操作逃逸到其他用例。
	factorBuildMu.Lock()
	factorBuildMu.Unlock()
	if !found {
		t.Fatal("已有构建期间提交的同日数据更新丢失，当前因子仍是旧价格")
	}
}
