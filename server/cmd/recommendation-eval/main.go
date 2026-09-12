// recommendation-eval 只读 SQLite 的独立排序研究入口。
// 不读取 .env，不执行 InitDB/AutoMigrate，不启动应用、任务或行情/AI 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"time"
	_ "time/tzdata"

	"quantvista/service"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("recommendation-eval", flag.ContinueOnError)
	fs.SetOutput(out)
	var req service.RankingResearchRequest
	path := fs.String("sqlite", "", "现有 SQLite 数据库文件，强制 mode=ro")
	fs.StringVar(&req.Source, "source", "recommendations", "recommendations 或 snapshots")
	fs.StringVar(&req.RecType, "type", "short_term", "short_term 或 long_term")
	fs.StringVar(&req.Profile, "profile", "", "评分侧重，如 momentum、pullback、value")
	fs.IntVar(&req.Horizon, "horizon", 0, "持有交易日：5、10、20、60")
	fs.StringVar(&req.Target, "target", "alpha", "net 或 alpha")
	fs.StringVar(&req.AsOf, "as-of", "", "截止日 YYYY-MM-DD，默认最近完整日期")
	fs.IntVar(&req.MaxDates, "max-dates", 480, "最大交易日数：120~1080")
	fs.IntVar(&req.MaxSymbols, "max-symbols", 200, "历史抽样标的数：10~500")
	fs.IntVar(&req.TopK, "top-k", 5, "每组选择数：1~10")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || fs.NArg() != 0 {
		return fmt.Errorf("必须用 -sqlite 显式指定现有文件")
	}
	absolute, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("读取数据库文件失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("数据库路径必须是普通文件")
	}
	uriPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&_pragma=query_only(1)"}
	db, err := gorm.Open(sqlite.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fmt.Errorf("打开只读数据库失败: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	report, err := service.RunRankingResearch(ctx, db, req)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(report)
}

func main() {
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	time.Local = zone
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
