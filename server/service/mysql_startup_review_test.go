package service

import (
	"testing"

	"quantvista/model"

	"gorm.io/gorm"
)

func TestMySQLPortfolioMigrationKeepsConcurrentDefault(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PortfolioAccount{}, &model.Position{})
	position := model.Position{UserID: 1871, Symbol: "600071", Market: "cn"}
	if err := db.Create(&position).Error; err != nil {
		t.Fatal(err)
	}
	concurrent := model.PortfolioAccount{UserID: position.UserID, Kind: model.PortfolioKindReal,
		Name: "并发保存的默认账户", IsDefault: true, Currency: "USD"}
	inserted := false
	const callback = "review_migration_concurrent_default"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "portfolio_accounts" || inserted {
			return
		}
		inserted = true
		// 独立连接提交，在迁移原有普通读快照建立之后出现。
		if err := db.Create(&concurrent).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(callback) })
	if err := model.MigratePortfolioAccounts(); err != nil {
		t.Fatalf("并发创建默认账户不应中断归属迁移: %v", err)
	}
	var saved model.Position
	if err := db.First(&saved, position.ID).Error; err != nil {
		t.Fatal(err)
	}
	var accounts []model.PortfolioAccount
	if err := db.Where("user_id = ?", position.UserID).Find(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	if !inserted || saved.AccountID != concurrent.ID || len(accounts) != 1 || accounts[0].Name != concurrent.Name || accounts[0].Currency != "USD" {
		t.Fatalf("须使用并保留并发保存的真实默认账户: account_id=%d concurrent_id=%d accounts=%+v", saved.AccountID, concurrent.ID, accounts)
	}
}

func TestMySQLPromptMigrationKeepsConcurrentGeneration(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.PromptTemplate{}, &model.PromptChampionState{})
	for _, userID := range []int64{1872, 1873} {
		if err := db.Create(&model.PromptTemplate{UserID: userID, Module: "stock", Content: "review", Revision: 2}).Error; err != nil {
			t.Fatal(err)
		}
	}
	inserted := false
	const callback = "review_migration_concurrent_generation"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		row, ok := tx.Statement.Dest.(*model.PromptChampionState)
		if !ok || inserted {
			return
		}
		inserted = true
		if err := db.Create(&model.PromptChampionState{UserID: row.UserID, Module: row.Module, Generation: 12}).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(callback) })
	if err := model.MigratePromptChampionStates(); err != nil {
		t.Fatalf("并发创建代次锚不能中断后续模板迁移: %v", err)
	}
	var states []model.PromptChampionState
	if err := db.Order("generation DESC").Find(&states).Error; err != nil {
		t.Fatal(err)
	}
	if !inserted || len(states) != 2 || states[0].Generation != 12 || states[1].Generation != 2 {
		t.Fatalf("代次不能倒退且全部模板都应有锚: %+v", states)
	}
}
