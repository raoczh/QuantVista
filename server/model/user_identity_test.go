package model

import (
	"path/filepath"
	"testing"

	"quantvista/common"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type legacyGitHubUser struct {
	ID       int64  `gorm:"primaryKey"`
	Username string `gorm:"uniqueIndex;size:64"`
	GithubID string `gorm:"index;size:64"`
}

func (legacyGitHubUser) TableName() string { return "users" }

func TestGitHubIdentityMigration(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "正常迁移", true: "历史重复拒绝迁移"}[duplicate], func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "identity.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			old := common.DB
			common.DB = db
			sqlDB, _ := db.DB()
			t.Cleanup(func() { common.DB = old; _ = sqlDB.Close() })
			if err := db.AutoMigrate(&legacyGitHubUser{}); err != nil {
				t.Fatal(err)
			}
			rows := []legacyGitHubUser{{Username: "a"}, {Username: "b"}, {Username: "c", GithubID: "123"}}
			if duplicate {
				rows = append(rows, legacyGitHubUser{Username: "d", GithubID: "123"})
			}
			if err := db.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			err = prepareGitHubIdentityMigration()
			if duplicate {
				if err == nil {
					t.Fatal("历史重复绑定必须先核对归属")
				}
				var count int64
				if err := db.Model(&User{}).Where("github_id = ?", "").Count(&count).Error; err != nil || count != 2 {
					t.Fatalf("拒绝迁移时不得留下部分修改：count=%d err=%v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&User{}); err != nil {
				t.Fatal(err)
			}
			if err := prepareGitHubIdentityMigration(); err != nil {
				t.Fatalf("重复迁移必须幂等：%v", err)
			}
			if err := db.Create(&User{Username: "empty-a"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&User{Username: "empty-b"}).Error; err != nil {
				t.Fatalf("未绑定身份允许多个 NULL：%v", err)
			}
			if err := db.Create(&User{Username: "duplicate", GithubID: "123"}).Error; err == nil {
				t.Fatal("非空 GitHub 身份必须由数据库拒绝重复")
			}
		})
	}
}
