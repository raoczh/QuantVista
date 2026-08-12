package model

import (
	"path/filepath"
	"testing"

	"quantvista/common"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type legacyPosition struct {
	ID     int64 `gorm:"primaryKey"`
	UserID int64
	Symbol string
}

func (legacyPosition) TableName() string { return "positions" }

type legacyPositionTrade struct {
	ID         int64 `gorm:"primaryKey"`
	UserID     int64
	PositionID int64
}

func (legacyPositionTrade) TableName() string { return "position_trades" }

type legacyPortfolioSnapshot struct {
	ID        int64 `gorm:"primaryKey"`
	UserID    int64
	Kind      string
	TradeDate string
}

func (legacyPortfolioSnapshot) TableName() string { return "portfolio_snapshots" }

type legacyPaperAccount struct {
	ID     int64 `gorm:"primaryKey"`
	UserID int64 `gorm:"uniqueIndex"`
}

func (legacyPaperAccount) TableName() string { return "paper_accounts" }

func TestPortfolioMigrationFirstAndRepeatedStartup(t *testing.T) {
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "portfolio.db")) + "?cache=private"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	oldDB, oldSQLite := common.DB, common.UsingSQLite
	common.DB, common.UsingSQLite = db, true
	t.Cleanup(func() { common.DB, common.UsingSQLite = oldDB, oldSQLite })
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&legacyPosition{}, &legacyPositionTrade{}, &legacyPortfolioSnapshot{}, &legacyPaperAccount{}); err != nil {
		t.Fatal(err)
	}
	p := legacyPosition{UserID: 7, Symbol: "600000"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyPositionTrade{UserID: 7, PositionID: p.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyPortfolioSnapshot{UserID: 7, Kind: PortfolioKindReal, TradeDate: "2026-08-01"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyPaperAccount{UserID: 7}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyPaperAccount{UserID: 8}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("首次启动迁移: %v", err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("重复启动迁移: %v", err)
	}
	var accounts []PortfolioAccount
	if err := db.Where("user_id = ? AND kind = ?", 7, PortfolioKindReal).Find(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !accounts[0].IsDefault {
		t.Fatalf("默认账户必须唯一: %+v", accounts)
	}
	var gotP Position
	var gotT PositionTrade
	var gotS PortfolioSnapshot
	if err := db.First(&gotP, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&gotT).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&gotS).Error; err != nil {
		t.Fatal(err)
	}
	if gotP.AccountID != accounts[0].ID || gotT.AccountID != accounts[0].ID || gotS.AccountID != accounts[0].ID {
		t.Fatalf("旧事实未统一归属: p=%d t=%d s=%d account=%d", gotP.AccountID, gotT.AccountID, gotS.AccountID, accounts[0].ID)
	}
	var paperAccounts []PortfolioAccount
	if err := db.Where("user_id = ? AND kind = ?", 7, PortfolioKindPaper).Find(&paperAccounts).Error; err != nil {
		t.Fatal(err)
	}
	if len(paperAccounts) != 1 {
		t.Fatalf("模拟默认账户迁移错误: %+v", paperAccounts)
	}
	if gotP.AccountID == paperAccounts[0].ID {
		t.Fatalf("同一用户的真实事实不得误归模拟账户: real=%d paper=%d", gotP.AccountID, paperAccounts[0].ID)
	}
	var secondLegacy PaperAccount
	if err := db.Where("user_id = ?", 8).First(&secondLegacy).Error; err != nil {
		t.Fatal(err)
	}
	if secondLegacy.AccountID <= 0 || secondLegacy.AccountID == paperAccounts[0].ID {
		t.Fatalf("两个旧模拟账户必须迁入不同的命名账户: user7=%d user8=%d", paperAccounts[0].ID, secondLegacy.AccountID)
	}
}
