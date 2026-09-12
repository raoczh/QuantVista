package setting

import (
	"errors"
	"path/filepath"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func startupSettingsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "settings.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Option{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	oldDB, oldKey := common.DB, common.EncryptionKey
	common.DB, common.EncryptionKey = db, "qv-review-startup-encryption"
	apply(map[string]string{})
	t.Cleanup(func() {
		common.DB, common.EncryptionKey = oldDB, oldKey
		apply(map[string]string{})
	})
	t.Setenv("GITHUB_CLIENT_ID", "qv-review-env-client")
	t.Setenv("GITHUB_CLIENT_SECRET", "qv-review-env-secret")
	return db
}

func TestStartupSeedsRollbackTogether(t *testing.T) {
	db := startupSettingsDB(t)
	failure := errors.New("review: second seed write failed")
	const callback = "review_startup_seed_failure"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*model.Option); ok && row.Key == keyGitHubClientSecret {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(callback) })
	if err := Init(); !errors.Is(err, failure) {
		t.Errorf("种子写入失败必须中止初始化: %v", err)
	}
	var count int64
	if err := db.Model(&model.Option{}).Count(&count).Error; err != nil || count != 0 {
		t.Errorf("凭证必须整体回滚: rows=%d err=%v", count, err)
	}
	if GitHubClientID() != "" || HasGitHubSecret() || GitHubOAuthEnabled() {
		t.Fatal("持久化失败后不能发布环境凭证")
	}
}

func TestStartupSeedEncryptionFailureIsReturned(t *testing.T) {
	db := startupSettingsDB(t)
	common.EncryptionKey = ""
	if err := Init(); !errors.Is(err, common.ErrEncryptionKeyMissing) {
		t.Errorf("加密失败必须中止初始化: %v", err)
	}
	var count int64
	if err := db.Model(&model.Option{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("加密失败不能留下半份凭证: rows=%d err=%v", count, err)
	}
}

func TestStartupSeedsPreserveConcurrentDatabaseOptions(t *testing.T) {
	db := startupSettingsDB(t)
	cipher, err := common.Encrypt("qv-review-database-secret")
	if err != nil {
		t.Fatal(err)
	}
	inserted := false
	const callback = "review_startup_concurrent_seed"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		row, ok := tx.Statement.Dest.(*model.Option)
		if !ok || row.Key != keyGitHubClientID || inserted {
			return
		}
		inserted = true
		// 在相同写入边界模拟另一实例先保存的配置，避免 SQLite 跨连接写锁影响反例。
		for _, saved := range []model.Option{{Key: keyGitHubClientID, Value: "qv-review-database-client"}, {Key: keyGitHubClientSecret, Value: cipher}} {
			if err := tx.Session(&gorm.Session{NewDB: true}).Create(&saved).Error; err != nil {
				tx.AddError(err)
				return
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(callback) })
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	stored, err := model.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || stored[keyGitHubClientID] != "qv-review-database-client" || stored[keyGitHubClientSecret] != cipher {
		t.Fatal("环境种子覆盖了已保存的数据库凭证")
	}
	if GitHubClientID() != "qv-review-database-client" || GitHubClientSecret() != "qv-review-database-secret" {
		t.Fatal("初始化必须发布真实持久化值")
	}
}

func TestStartupSeedsPreserveExplicitEmptyDatabaseOptions(t *testing.T) {
	db := startupSettingsDB(t)
	if err := db.Create(&[]model.Option{{Key: keyGitHubClientID}, {Key: keyGitHubClientSecret}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	if GitHubClientID() != "" || HasGitHubSecret() || GitHubOAuthEnabled() {
		t.Fatal("数据库显式空值仍优先于环境配置")
	}
}
