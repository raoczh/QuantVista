package model

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCandidateDiscoveryModelsMigrateAndEnforceGlobalIdentity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:discovery-model-test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&CandidateDiscoveryRun{}, &CandidateDiscoveryItem{}); err != nil {
		t.Fatalf("首次 AutoMigrate 失败: %v", err)
	}
	if err := db.AutoMigrate(&CandidateDiscoveryRun{}, &CandidateDiscoveryItem{}); err != nil {
		t.Fatalf("重复 AutoMigrate 失败: %v", err)
	}
	now := time.Now()
	run := &CandidateDiscoveryRun{OwnerType: JobOwnerSystem, Market: "cn", TradeDate: "2042-01-02", AsOf: now, DiscoveryVersion: "test", FactorVersion: "fv-test", ParameterHash: "hash", Status: "success", StartedAt: now}
	if err := db.Create(run).Error; err != nil {
		t.Fatalf("创建 system/global run 失败: %v", err)
	}
	owner := int64(7)
	bad := &CandidateDiscoveryRun{OwnerType: JobOwnerUser, OwnerUserID: &owner, Market: "cn", TradeDate: "2042-01-03", AsOf: now, DiscoveryVersion: "test", FactorVersion: "fv-test", ParameterHash: "hash2", StartedAt: now}
	if err := db.Create(bad).Error; err == nil {
		t.Fatal("发现 run 不应允许绑定普通用户")
	}
	item := &CandidateDiscoveryItem{RunID: run.ID, TradeDate: run.TradeDate, Market: "cn", DiscoveryVersion: run.DiscoveryVersion, Channel: "trend_breakout", Symbol: "600001", Rank: 1, Score: 80, AsOf: now}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("创建发现 item 失败: %v", err)
	}
	dup := *item
	dup.ID = 0
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("同日、同版本、同通道、同标的应由唯一约束拒绝重复")
	}
}
