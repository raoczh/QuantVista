package service

import (
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestMySQLScreenerHistoryKeepsOneRevisionSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.ScreenerStrategy{}, &model.ScreenerStrategyRevision{})
	svc := NewScreenerService()
	tree := leafV("chg_pct", ">", 0)
	first, err := svc.SaveStrategy(713, SaveStrategyRequest{Name: "历史快照", Tree: &tree})
	if err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	const callback = "review_screener_history_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "screener_strategies" && once.CompareAndSwap(false, true) {
			close(read)
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
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	type historyReply struct {
		view *StrategyHistoryView
		err  error
	}
	done := make(chan historyReply, 1)
	go func() { v, e := svc.StrategyHistory(713, first.ID); done <- historyReply{v, e} }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("历史没有进入第一段读取")
	}
	_, err = svc.SaveStrategy(713, SaveStrategyRequest{ID: first.ID, BaseRevisionID: first.CurrentRevisionID, Name: "并发新版本", Tree: &tree})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-done:
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		if reply.view.CurrentRevisionID != first.CurrentRevisionID || len(reply.view.Revisions) != 1 || reply.view.Revisions[0].ID != first.CurrentRevisionID {
			t.Errorf("历史混入另一个读取时点的新版本: %+v", reply.view)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("历史读取未结束")
	}
}

func TestMySQLScreenerMigrationSeesConcurrentBaseline(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.ScreenerStrategy{}, &model.ScreenerStrategyRevision{})
	strategy := model.ScreenerStrategy{UserID: 714, Name: "并发迁移", Period: "swing", Risk: "mid", TreeJSON: `{"factor":"close","op":">","value":1}`}
	if err := db.Create(&strategy).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	const callback = "review_screener_migration_baseline"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "screener_strategy_revisions" && once.CompareAndSwap(false, true) {
			close(read)
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
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	done := make(chan error, 1)
	go func() { done <- model.MigrateScreenerStrategyRevisions() }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("迁移没有读取基线")
	}
	hash, err := model.ScreenerStrategyContentHash(strategy.Name, strategy.Desc, strategy.Period, strategy.Risk, strategy.TreeJSON)
	if err != nil {
		t.Fatal(err)
	}
	baseline := model.ScreenerStrategyRevision{UserID: strategy.UserID, StrategyID: strategy.ID, Revision: 1, ContentHash: hash, Name: strategy.Name, Period: strategy.Period, Risk: strategy.Risk, TreeJSON: strategy.TreeJSON}
	if err := db.Create(&baseline).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&strategy).UpdateColumn("current_revision_id", baseline.ID).Error; err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("并发实例已创建合法基线，本实例仍因旧快照而迁移失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("迁移没有结束")
	}
	var stored model.ScreenerStrategy
	if err := common.DB.First(&stored, strategy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CurrentRevisionID != baseline.ID {
		t.Errorf("基线指针漂移: %+v", stored)
	}
}
