package model

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type legacyJobRun struct {
	ID              int64  `gorm:"primaryKey"`
	UserID          int64  `gorm:"not null;index"`
	Kind            string `gorm:"size:64;not null"`
	RequestHash     string `gorm:"size:64;not null"`
	Status          string `gorm:"size:16;not null"`
	SnapshotVersion int    `gorm:"not null"`
	RequestSnapshot string `gorm:"type:text;not null"`
	QueuedAt        time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (legacyJobRun) TableName() string { return "job_runs" }

type legacyJobEvent struct {
	ID        int64 `gorm:"primaryKey;autoIncrement"`
	UserID    int64 `gorm:"not null;index"`
	JobRunID  int64 `gorm:"not null;index"`
	Type      string
	Status    string
	CreatedAt time.Time
}

func (legacyJobEvent) TableName() string { return "job_events" }

func TestJobRunSQLiteLegacyMigrationMakesSystemOwnerNullable(t *testing.T) {
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "job-owner.db")) + "?cache=private"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("打开 SQLite 测试库: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := db.AutoMigrate(&legacyJobRun{}, &legacyJobEvent{}); err != nil {
		t.Fatalf("创建旧 JobRun: %v", err)
	}
	legacy := &legacyJobRun{UserID: 42, Kind: "qa", RequestHash: strings.Repeat("a", 64),
		Status: JobStatusSuccess, SnapshotVersion: 1, RequestSnapshot: `{}`, QueuedAt: time.Now()}
	if err := db.Create(legacy).Error; err != nil {
		t.Fatal(err)
	}
	legacyEvent := &legacyJobEvent{UserID: 42, JobRunID: legacy.ID, Type: "status", Status: JobStatusSuccess}
	if err := db.Create(legacyEvent).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&JobRun{}, &JobEvent{}); err != nil {
		t.Fatalf("升级 JobRun owner schema: %v", err)
	}

	columns, err := db.Migrator().ColumnTypes(&JobRun{})
	if err != nil {
		t.Fatal(err)
	}
	foundNullable := false
	for _, column := range columns {
		if column.Name() != "user_id" {
			continue
		}
		nullable, ok := column.Nullable()
		foundNullable = ok && nullable
	}
	if !foundNullable {
		t.Fatal("升级后 job_runs.user_id 必须允许 SQL NULL")
	}

	var migrated JobRun
	if err := db.First(&migrated, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.OwnerType != JobOwnerUser || migrated.OwnerUserID == nil || *migrated.OwnerUserID != 42 || migrated.UserID != 42 {
		t.Fatalf("旧用户作业归属未兼容: %+v", migrated)
	}
	system := &JobRun{OwnerType: JobOwnerSystem, Kind: "snapshot_market", RequestHash: strings.Repeat("b", 64),
		Status: JobStatusQueued, SnapshotVersion: 1, RequestSnapshot: `{}`, QueuedAt: time.Now()}
	if err := db.Create(system).Error; err != nil {
		t.Fatalf("SQLite 应允许 system + NULL user_id: %v", err)
	}
	systemEvent := &JobEvent{OwnerType: JobOwnerSystem, JobRunID: system.ID, Type: "created", Status: JobStatusQueued}
	if err := db.Create(systemEvent).Error; err != nil {
		t.Fatalf("SQLite 应允许 system JobEvent 使用 NULL user_id: %v", err)
	}
}

func TestJobOwnerSchemaIsMySQLCompatible(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "quantvista:unused@tcp(127.0.0.1:1)/quantvista?charset=utf8mb4&parseTime=True&loc=Local",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("初始化 MySQL 方言: %v", err)
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&JobRun{}); err != nil {
		t.Fatal(err)
	}
	ownerUser := stmt.Schema.LookUpField("OwnerUserID")
	triggeredBy := stmt.Schema.LookUpField("TriggeredBy")
	ownerType := stmt.Schema.LookUpField("OwnerType")
	if ownerUser == nil || ownerUser.DBName != "user_id" || ownerUser.NotNull {
		t.Fatalf("MySQL user_id 必须映射为可空列: %+v", ownerUser)
	}
	if triggeredBy == nil || triggeredBy.NotNull {
		t.Fatalf("MySQL triggered_by 必须为可空列: %+v", triggeredBy)
	}
	if ownerType == nil || !ownerType.NotNull || ownerType.DefaultValueInterface != "user" {
		t.Fatalf("MySQL owner_type 默认契约异常: %+v", ownerType)
	}
	indexes := make(map[string]bool)
	for _, index := range stmt.Schema.ParseIndexes() {
		indexes[index.Name] = true
	}
	for _, name := range []string{"idx_job_run_active", "idx_job_run_kind_status", "idx_job_run_status_queued"} {
		if !indexes[name] {
			t.Fatalf("MySQL schema 缺少索引 %s", name)
		}
	}
}
