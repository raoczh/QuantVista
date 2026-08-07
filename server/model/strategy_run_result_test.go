package model

import (
	"reflect"
	"testing"
)

func TestStrategyRunResultAutoMigrateIdempotent(t *testing.T) {
	db := openScreenerModelTestDB(t)
	found := false
	for _, item := range AllModels() {
		if reflect.TypeOf(item) == reflect.TypeOf(&StrategyRunResult{}) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("StrategyRunResult 必须加入 AllModels")
	}
	if err := db.AutoMigrate(AllModels()...); err != nil {
		t.Fatalf("首次 AutoMigrate: %v", err)
	}
	if err := db.AutoMigrate(AllModels()...); err != nil {
		t.Fatalf("重复 AutoMigrate: %v", err)
	}
	if !db.Migrator().HasTable(&StrategyRunResult{}) {
		t.Fatal("自动迁移未创建 strategy_run_results")
	}
}
