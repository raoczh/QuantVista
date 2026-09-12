package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"github.com/glebarez/sqlite"
	mysqlconfig "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func inspectionSQLiteDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "行情 空格#%&.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.DailyBar{}); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func TestInspectionDoesNotCreateMissingSQLiteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	called := false
	err := withInspectionDB(context.Background(), "local", path, func(*gorm.DB) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("不存在的数据库必须拒绝检查: called=%v err=%v", called, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("只读检查不能创建数据库文件: %v", err)
	}
}

func TestInspectionSQLiteReadOnlyAndMarketIdentity(t *testing.T) {
	db, path := inspectionSQLiteDB(t)
	for _, market := range []string{"cn", "hk"} {
		if err := db.Create(&model.DailyBar{Market: market, Symbol: "000001", TradeDate: time.Now().Format("2006-01-02")}).Error; err != nil {
			t.Fatal(err)
		}
	}
	before := common.DB
	var output bytes.Buffer
	err := withInspectionDB(context.Background(), "LOCAL", path, func(reader *gorm.DB) error {
		failed, err := inspectBars(reader, &output)
		if err != nil {
			return err
		}
		if failed {
			return errors.New("有效索引和数据被误判为失败")
		}
		if err := reader.Exec("UPDATE daily_bars SET close = 999").Error; err == nil {
			return errors.New("只读连接允许了写入")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "总行数 2  标的数 2") || common.DB != before {
		t.Fatalf("跨市场同代码须分别计数且不能修改全局数据库: %s", output.String())
	}
	var changed int64
	if err := db.Model(&model.DailyBar{}).Where("close = ?", 999).Count(&changed).Error; err != nil || changed != 0 {
		t.Fatalf("检查改变了业务数据: rows=%d err=%v", changed, err)
	}
}

func TestInspectionStaleCountFailureIsNotSuccess(t *testing.T) {
	db, path := inspectionSQLiteDB(t)
	if err := db.Create(&model.DailyBar{Market: "cn", Symbol: "000001", TradeDate: "2000-01-01"}).Error; err != nil {
		t.Fatal(err)
	}
	failure := errors.New("review: stale count failed")
	var output bytes.Buffer
	err := withInspectionDB(context.Background(), "local", path, func(reader *gorm.DB) error {
		const callback = "review_inspection_stale_count_failure"
		if err := reader.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "daily_bars" {
				if _, exists := tx.Statement.Clauses["WHERE"]; exists {
					tx.AddError(failure)
				}
			}
		}); err != nil {
			return err
		}
		defer reader.Callback().Query().Remove(callback)
		_, err := inspectBars(reader, &output)
		return err
	})
	if !errors.Is(err, failure) || strings.Contains(output.String(), "的行 0 条") {
		t.Fatalf("统计失败不能伪装成零条超期数据: output=%s err=%v", output.String(), err)
	}
}

func TestInspectionReportsDuplicateAndMissingIndex(t *testing.T) {
	db, path := inspectionSQLiteDB(t)
	if err := db.Migrator().DropIndex(&model.DailyBar{}, "idx_bar_symbol_date"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Create(&model.DailyBar{Market: "cn", Symbol: "000001", TradeDate: time.Now().Format("2006-01-02")}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	err := withInspectionDB(context.Background(), "local", path, func(reader *gorm.DB) error {
		failed, err := inspectBars(reader, &output)
		if err == nil && !failed {
			return errors.New("缺少唯一索引且存在重复记录仍被判为通过")
		}
		return err
	})
	if err != nil || !strings.Contains(output.String(), "缺失  idx_bar_symbol_date") || !strings.Contains(output.String(), "→ 2 行") {
		t.Fatalf("体检未指出实际损坏: output=%s err=%v", output.String(), err)
	}
}

func TestInspectionCancellationStopsBeforeOpening(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := withInspectionDB(ctx, "local", filepath.Join(t.TempDir(), "absent.db"), func(*gorm.DB) error {
		t.Fatal("取消后不能进入检查回调")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消原因丢失: %v", err)
	}
}

func TestMySQLInspectionReadOnlyConsistentSnapshot(t *testing.T) {
	if os.Getenv("QV_REVIEW_MYSQL") != "1" {
		t.Skip("只在本机隔离端口 127.0.0.1:33317 运行")
	}
	// 不读取 SQL_DSN；这里只创建并清理本用例生成的独立临时库。
	t.Setenv("SQL_DSN", "qv-review-must-not-use-application-dsn")
	config := mysqlconfig.NewConfig()
	config.User, config.Net, config.Addr = "root", "tcp", "127.0.0.1:33317"
	config.ParseTime = true
	config.Timeout, config.ReadTimeout, config.WriteTimeout = 5*time.Second, 15*time.Second, 15*time.Second
	admin, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("qv_barsreview_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("清理本用例临时库失败: %v", err)
		}
	})
	config.DBName = name
	writer, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := writer.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := writer.AutoMigrate(&model.DailyBar{}); err != nil {
		t.Fatal(err)
	}
	date := time.Now().Format("2006-01-02")
	if err := writer.Create(&model.DailyBar{Market: "cn", Symbol: "000001", TradeDate: date}).Error; err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	err = withInspectionDB(context.Background(), config.FormatDSN(), "", func(reader *gorm.DB) error {
		if failed, err := inspectBars(reader, &first); err != nil || failed {
			return fmt.Errorf("首次检查失败: failed=%v err=%v", failed, err)
		}
		if err := writer.Create(&model.DailyBar{Market: "hk", Symbol: "000001", TradeDate: date}).Error; err != nil {
			return err
		}
		if failed, err := inspectBars(reader, &second); err != nil || failed {
			return fmt.Errorf("第二次检查失败: failed=%v err=%v", failed, err)
		}
		if err := reader.Exec("UPDATE daily_bars SET close = 999").Error; err == nil {
			return errors.New("MySQL 只读事务允许修改数据")
		}
		return nil
	})
	if err != nil || first.String() != second.String() || !strings.Contains(first.String(), "总行数 1  标的数 1") {
		t.Fatalf("并发写入改变了只读事务快照: first=%s second=%s err=%v", first.String(), second.String(), err)
	}
}
