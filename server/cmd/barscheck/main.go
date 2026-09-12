// barscheck 是 daily_bars 的只读体检工具：核对索引是否真的建在库上、自然键有无重复、
// 数据跨度与保留期是否相符。不做任何写入，可直接对生产库执行。
//
//	SQL_DSN=... go run ./cmd/barscheck
//
// 存在的意义：daily_bars 的唯一索引 idx_bar_symbol_date 是 upsert 的冲突目标
// （market.go / marketwide.go 的 OnConflict），它一旦缺失，每日同步不会报错，而是
// 静默插入重复行——等发现时表已经污染。AutoMigrate 正常会建它，但旧库、手工建表、
// 迁移中断都可能留下缺口，值得有个能直接在生产上跑的核对入口。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"github.com/glebarez/sqlite"
	mysqlconfig "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// wantIndexes 期望存在的索引 → 期望列（顺序敏感）。与 model.DailyBar 的 gorm 标签对应，
// 三者职责见该结构体的注释——都不能删。
var wantIndexes = map[string][]string{
	"idx_bar_symbol_date":    {"symbol", "market", "trade_date"},
	"idx_bar_market_date":    {"market", "trade_date"},
	"idx_market_symbol_date": {"market", "symbol", "trade_date"},
}

func main() {
	var output bytes.Buffer
	failed := false
	err := withInspectionDB(context.Background(), os.Getenv("SQL_DSN"), os.Getenv("SQLITE_PATH"), func(db *gorm.DB) error {
		var err error
		failed, err = inspectBars(db, &output)
		return err
	})
	fmt.Print(output.String())
	if err != nil {
		// 驱动错误可能携带连接信息，不把临时凭证写进终端日志。
		fmt.Fprintln(os.Stderr, "检查未完成：连接、读取或只读事务失败，请核对连接配置、权限与数据库状态")
		os.Exit(1)
	}
	if failed {
		fmt.Println("\n结论：存在需要处理的问题（见上）")
		os.Exit(1)
	}
	fmt.Println("\n结论：daily_bars 索引与数据完整性检查通过")
}

// withInspectionDB 使用独立连接和只读快照，不初始化应用全局 DB、迁移或调度器。
func withInspectionDB(ctx context.Context, dsn, sqlitePath string, inspect func(*gorm.DB) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	driverName := "mysql"
	local := common.IsLocalDSN(dsn)
	opts := &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}
	if local {
		if sqlitePath == "" {
			sqlitePath = "quantvista.db"
		}
		path, err := filepath.Abs(sqlitePath)
		if err != nil {
			return err
		}
		path = filepath.ToSlash(path)
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		uri := url.URL{Scheme: "file", Path: path}
		query := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)"}}
		uri.RawQuery = query.Encode()
		driverName, dsn = sqlite.DriverName, uri.String()
		opts.Isolation = sql.LevelDefault
	} else {
		config, err := mysqlconfig.ParseDSN(dsn)
		if err != nil {
			return err
		}
		config.ParseTime = true
		dsn = config.FormatDSN()
	}
	sqlDB, err := sql.Open(driverName, dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.PingContext(ctx); err != nil {
		return err
	}
	tx, err := sqlDB.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var dialector gorm.Dialector = mysql.New(mysql.Config{Conn: tx, SkipInitializeWithVersion: true})
	if local {
		dialector = &sqlite.Dialector{Conn: tx}
	}
	db, err := gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	if err != nil {
		return err
	}
	if err := inspect(db.WithContext(ctx)); err != nil {
		return err
	}
	return tx.Commit()
}

func inspectBars(db *gorm.DB, out io.Writer) (bool, error) {
	failed := false
	fmt.Fprintln(out, "== 索引 ==")
	mig := db.Migrator()
	indexes, err := mig.GetIndexes(&model.DailyBar{})
	if err != nil {
		return false, fmt.Errorf("读取索引失败: %w", err)
	}
	got := map[string][]string{}
	unique := map[string]bool{}
	for _, idx := range indexes {
		got[idx.Name()] = idx.Columns()
		if u, ok := idx.Unique(); ok && u {
			unique[idx.Name()] = true
		}
	}
	names := make([]string, 0, len(wantIndexes))
	for name := range wantIndexes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		wantCols := wantIndexes[name]
		gotCols, ok := got[name]
		switch {
		case !ok:
			fmt.Fprintf(out, "  缺失  %s  期望列 (%s)\n", name, strings.Join(wantCols, ", "))
			failed = true
		case strings.Join(gotCols, ",") != strings.Join(wantCols, ","):
			fmt.Fprintf(out, "  列不符 %s  实际 (%s) 期望 (%s)\n", name, strings.Join(gotCols, ", "), strings.Join(wantCols, ", "))
			failed = true
		default:
			fmt.Fprintf(out, "  OK    %s (%s)\n", name, strings.Join(gotCols, ", "))
		}
	}
	// 唯一性单独判定：列对了但少了 UNIQUE，upsert 一样会退化成重复插入。
	if !unique["idx_bar_symbol_date"] {
		fmt.Fprintln(out, "  警告  idx_bar_symbol_date 不是 UNIQUE —— upsert 会静默插入重复行")
		failed = true
	}
	for name, cols := range got {
		if _, expected := wantIndexes[name]; !expected && name != "PRIMARY" {
			fmt.Fprintf(out, "  额外  %s (%s)\n", name, strings.Join(cols, ", "))
		}
	}

	fmt.Fprintln(out, "== 行数与跨度 ==")
	var stat struct {
		Total   int64
		Symbols int64
		MinDate string
		MaxDate string
	}
	if err := db.Model(&model.DailyBar{}).
		Select("COUNT(*) AS total, COALESCE(MIN(trade_date), '') AS min_date, COALESCE(MAX(trade_date), '') AS max_date").
		Scan(&stat).Error; err != nil {
		return false, fmt.Errorf("统计失败: %w", err)
	}
	symbols := db.Model(&model.DailyBar{}).Select("market, symbol").Group("market, symbol")
	if err := db.Table("(?) AS bar_symbols", symbols).Count(&stat.Symbols).Error; err != nil {
		return false, fmt.Errorf("标的统计失败: %w", err)
	}
	fmt.Fprintf(out, "  总行数 %d  标的数 %d  区间 %s ~ %s\n", stat.Total, stat.Symbols, stat.MinDate, stat.MaxDate)
	if stat.Symbols > 0 {
		fmt.Fprintf(out, "  每标的均行数 %.1f\n", float64(stat.Total)/float64(stat.Symbols))
	}
	// 保留期核对：早于清理下限的行说明保留任务没跑或没生效。
	if cutoff := model.DailyBarRetentionCutoff(); stat.MinDate != "" && stat.MinDate < cutoff {
		var stale int64
		if err := db.Model(&model.DailyBar{}).Where("trade_date < ?", cutoff).Count(&stale).Error; err != nil {
			return false, fmt.Errorf("超期行统计失败: %w", err)
		}
		fmt.Fprintf(out, "  早于保留下限 %s 的行 %d 条（保留期 %d 天，等待清理任务处理）\n",
			cutoff, stale, model.DailyBarRetentionDays)
	} else {
		fmt.Fprintf(out, "  保留下限 %s（保留期 %d 天）内无超期数据\n", cutoff, model.DailyBarRetentionDays)
	}

	fmt.Fprintln(out, "== 自然键重复 ==")
	type dup struct {
		Market    string
		Symbol    string
		TradeDate string
		N         int64
	}
	var dups []dup
	if err := db.Model(&model.DailyBar{}).
		Select("market, symbol, trade_date, COUNT(*) AS n").
		Group("market, symbol, trade_date").Having("COUNT(*) > 1").
		Order("market, symbol, trade_date").Limit(20).Scan(&dups).Error; err != nil {
		return false, fmt.Errorf("重复检查失败: %w", err)
	}
	if len(dups) == 0 {
		fmt.Fprintln(out, "  无重复")
	} else {
		failed = true
		fmt.Fprintln(out, "  发现重复自然键（最多列 20 组）——建唯一索引前必须先去重：")
		for _, d := range dups {
			fmt.Fprintf(out, "    %s %s %s → %d 行\n", d.Market, d.Symbol, d.TradeDate, d.N)
		}
	}

	return failed, nil
}
