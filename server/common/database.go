package common

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB 主数据库句柄。生产 MySQL，开发 SQLite。
var DB *gorm.DB

// UsingSQLite 当前是否运行在 SQLite 上（开发/测试）。SQLite 不支持 SELECT ... FOR UPDATE，
// 行锁等 MySQL 专属子句按此分流（SQLite 单写者天然串行，跳过无碍）。
var UsingSQLite bool

// SQLitePath 开发环境 SQLite 文件路径（容器内挂载到 /data）。
var SQLitePath = "quantvista.db"

// IsLocalDSN SQL_DSN 是否指向本地 SQLite（空或 "local"，容忍大小写与空白）。
// config 的生产判定与 InitDB 的选库必须共用此口径，避免两处漂移。
func IsLocalDSN(dsn string) bool {
	d := strings.ToLower(strings.TrimSpace(dsn))
	return d == "" || d == "local"
}

// InitDB 根据 SQL_DSN 选择数据库。
//   - 空 / "local"        -> SQLite（开发）
//   - 其余                -> MySQL（生产，宝塔托管）
//
// 设计上仅主推 SQLite/MySQL；PostgreSQL 留待需要时再加分支（GORM 兼容）。
func InitDB() error {
	dsn := os.Getenv("SQL_DSN")

	gormCfg := &gorm.Config{
		PrepareStmt: true,
		Logger:      logger.Default.LogMode(logger.Warn),
	}

	var (
		db  *gorm.DB
		err error
	)

	switch {
	case IsLocalDSN(dsn):
		if p := os.Getenv("SQLITE_PATH"); p != "" {
			SQLitePath = p
		}
		SysLog("SQL_DSN 未设置，使用 SQLite：%s", SQLitePath)
		db, err = gorm.Open(sqlite.Open(SQLitePath), gormCfg)
		UsingSQLite = true
	case strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://"):
		return fmt.Errorf("PostgreSQL 暂未启用（仅 GORM 兼容），请使用 MySQL 或 SQLite")
	default:
		// MySQL：确保带 parseTime，否则 time.Time 字段扫描失败。
		if !strings.Contains(dsn, "parseTime") {
			if strings.Contains(dsn, "?") {
				dsn += "&parseTime=true"
			} else {
				dsn += "?parseTime=true"
			}
		}
		SysLog("使用 MySQL 作为主数据库")
		db, err = gorm.Open(mysql.Open(dsn), gormCfg)
	}

	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("获取底层连接失败: %w", err)
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	DB = db
	return nil
}
