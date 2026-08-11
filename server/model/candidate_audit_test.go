package model

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCandidateAuditAutoMigrateOwnerAndUniqueKeys(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:candidate-audit-model?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&CandidateAuditRun{}, &CandidateAuditItem{}); err != nil {
		t.Fatalf("首次 AutoMigrate: %v", err)
	}
	if err := db.AutoMigrate(&CandidateAuditRun{}, &CandidateAuditItem{}); err != nil {
		t.Fatalf("重复 AutoMigrate: %v", err)
	}
	if !db.Migrator().HasTable(&CandidateAuditRun{}) || !db.Migrator().HasTable(&CandidateAuditItem{}) {
		t.Fatal("审计运行表或明细表未迁移")
	}

	now := time.Now()
	run := CandidateAuditRun{Market: "cn", SignalDate: "2044-01-05", OutcomeDate: "2044-01-08",
		AuditVersion: "cma1", ParameterHash: "hash", DiscoveryVersion: "d1", FactorVersion: "f1",
		OutcomeVersion: "o1", ParameterJSON: `{}`, DataAsOf: now, Status: CandidateAuditStatusSuccess,
		StartedAt: now, FinishedAt: &now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatalf("创建 system/global 审计运行: %v", err)
	}
	if run.OwnerType != JobOwnerSystem || run.OwnerUserID != nil {
		t.Fatalf("运行归属错误: %+v", run)
	}
	duplicate := run
	duplicate.ID, duplicate.JobRunID = 0, nil
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("相同交易日对、版本和参数 hash 必须命中唯一键")
	}

	uid := int64(7)
	invalid := run
	invalid.ID, invalid.OwnerUserID = 0, &uid
	invalid.ParameterHash = "other"
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("全局运行不得绑定用户")
	}

	item := CandidateAuditItem{RunID: run.ID, UserID: 7, BatchID: 11, Symbol: "600001",
		AuditType: CandidateAuditTypeMissedLeader, Market: "cn", SignalDate: run.SignalDate,
		OutcomeDate: run.OutcomeDate, ConclusionCode: "missed_leader", FunnelStage: "absent",
		PrimaryReasonCode: "not_discovered_marketwide", OutcomeStatus: "observed"}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("创建审计明细: %v", err)
	}
	duplicateItem := item
	duplicateItem.ID = 0
	if err := db.Create(&duplicateItem).Error; err == nil {
		t.Fatal("run/user/batch/symbol/type 必须命中唯一键")
	}
}
