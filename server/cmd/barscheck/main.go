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
	"fmt"
	"os"
	"strings"

	"quantvista/common"
	"quantvista/model"
)

// wantIndexes 期望存在的索引 → 期望列（顺序敏感）。与 model.DailyBar 的 gorm 标签对应，
// 三者职责见该结构体的注释——都不能删。
var wantIndexes = map[string][]string{
	"idx_bar_symbol_date":    {"symbol", "market", "trade_date"},
	"idx_bar_market_date":    {"market", "trade_date"},
	"idx_market_symbol_date": {"market", "symbol", "trade_date"},
}

func main() {
	if err := common.InitDB(); err != nil {
		fmt.Println("INITDB ERROR:", err)
		os.Exit(1)
	}
	failed := false

	fmt.Println("== 索引 ==")
	mig := common.DB.Migrator()
	indexes, err := mig.GetIndexes(&model.DailyBar{})
	if err != nil {
		fmt.Println("读取索引失败:", err)
		os.Exit(1)
	}
	got := map[string][]string{}
	unique := map[string]bool{}
	for _, idx := range indexes {
		got[idx.Name()] = idx.Columns()
		if u, ok := idx.Unique(); ok && u {
			unique[idx.Name()] = true
		}
	}
	for name, wantCols := range wantIndexes {
		gotCols, ok := got[name]
		switch {
		case !ok:
			fmt.Printf("  缺失  %s  期望列 (%s)\n", name, strings.Join(wantCols, ", "))
			failed = true
		case strings.Join(gotCols, ",") != strings.Join(wantCols, ","):
			fmt.Printf("  列不符 %s  实际 (%s) 期望 (%s)\n", name, strings.Join(gotCols, ", "), strings.Join(wantCols, ", "))
			failed = true
		default:
			fmt.Printf("  OK    %s (%s)\n", name, strings.Join(gotCols, ", "))
		}
	}
	// 唯一性单独判定：列对了但少了 UNIQUE，upsert 一样会退化成重复插入。
	if !unique["idx_bar_symbol_date"] {
		fmt.Println("  警告  idx_bar_symbol_date 不是 UNIQUE —— upsert 会静默插入重复行")
		failed = true
	}
	for name, cols := range got {
		if _, expected := wantIndexes[name]; !expected && name != "PRIMARY" {
			fmt.Printf("  额外  %s (%s)\n", name, strings.Join(cols, ", "))
		}
	}

	fmt.Println("== 行数与跨度 ==")
	var stat struct {
		Total   int64
		Symbols int64
		MinDate string
		MaxDate string
	}
	if err := common.DB.Model(&model.DailyBar{}).
		Select("COUNT(*) AS total, COUNT(DISTINCT symbol) AS symbols, MIN(trade_date) AS min_date, MAX(trade_date) AS max_date").
		Scan(&stat).Error; err != nil {
		fmt.Println("统计失败:", err)
		os.Exit(1)
	}
	fmt.Printf("  总行数 %d  标的数 %d  区间 %s ~ %s\n", stat.Total, stat.Symbols, stat.MinDate, stat.MaxDate)
	if stat.Symbols > 0 {
		fmt.Printf("  每标的均行数 %.1f\n", float64(stat.Total)/float64(stat.Symbols))
	}
	// 保留期核对：早于清理下限的行说明保留任务没跑或没生效。
	if cutoff := model.DailyBarRetentionCutoff(); stat.MinDate != "" && stat.MinDate < cutoff {
		var stale int64
		common.DB.Model(&model.DailyBar{}).Where("trade_date < ?", cutoff).Count(&stale)
		fmt.Printf("  早于保留下限 %s 的行 %d 条（保留期 %d 天，等待清理任务处理）\n",
			cutoff, stale, model.DailyBarRetentionDays)
	} else {
		fmt.Printf("  保留下限 %s（保留期 %d 天）内无超期数据\n", cutoff, model.DailyBarRetentionDays)
	}

	fmt.Println("== 自然键重复 ==")
	type dup struct {
		Market    string
		Symbol    string
		TradeDate string
		N         int64
	}
	var dups []dup
	if err := common.DB.Model(&model.DailyBar{}).
		Select("market, symbol, trade_date, COUNT(*) AS n").
		Group("market, symbol, trade_date").Having("COUNT(*) > 1").
		Limit(20).Scan(&dups).Error; err != nil {
		fmt.Println("重复检查失败:", err)
		os.Exit(1)
	}
	if len(dups) == 0 {
		fmt.Println("  无重复")
	} else {
		failed = true
		fmt.Printf("  发现重复自然键（最多列 20 组）——建唯一索引前必须先去重：\n")
		for _, d := range dups {
			fmt.Printf("    %s %s %s → %d 行\n", d.Market, d.Symbol, d.TradeDate, d.N)
		}
	}

	if failed {
		fmt.Println("\n结论：存在需要处理的问题（见上）")
		os.Exit(1)
	}
	fmt.Println("\n结论：daily_bars 索引与数据完整性检查通过")
}
