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
		t.Fatal("同一运行、同通道、同标的应由唯一约束拒绝重复")
	}
	// 同日参数/因子版本升级会建立新运行，必须允许相同标的在新运行中留下独立事实。
	run2 := &CandidateDiscoveryRun{OwnerType: JobOwnerSystem, Market: "cn", TradeDate: run.TradeDate, AsOf: now, DiscoveryVersion: run.DiscoveryVersion, FactorVersion: "fv-next", ParameterHash: "hash-next", Status: "success", StartedAt: now}
	if err := db.Create(run2).Error; err != nil {
		t.Fatalf("创建同日新因子版本 run 失败: %v", err)
	}
	item2 := *item
	item2.ID, item2.RunID = 0, run2.ID
	if err := db.Create(&item2).Error; err != nil {
		t.Fatalf("同日新运行应允许保存同一通道标的: %v", err)
	}
}
