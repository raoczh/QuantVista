package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

func completedBenchmarkBars(bars []datasource.Bar, now time.Time, completedDate string) ([]datasource.Bar, string) {
	date := now.In(time.Local).Format("2006-01-02")
	beforeClose := now.In(time.Local).Hour()*60+now.In(time.Local).Minute() < sessionCloseMin
	out := make([]datasource.Bar, 0, len(bars))
	for _, b := range bars {
		if b.TradeDate > completedDate || b.TradeDate > date || beforeClose && b.TradeDate == date {
			continue
		}
		if _, err := time.Parse("2006-01-02", b.TradeDate); err != nil || b.Close <= 0 || !finiteRecNumber(b.Close) {
			return nil, "基准日期或收盘价无效"
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TradeDate < out[j].TradeDate })
	for i := 1; i < len(out); i++ {
		if out[i].TradeDate == out[i-1].TradeDate {
			return nil, "基准日线存在重复日期"
		}
	}
	if len(out) == 0 {
		return nil, "缺少完整收盘基准"
	}
	if out[len(out)-1].TradeDate < completedDate {
		return out, fmt.Sprintf("基准仅截至 %s，尚缺 %s 的完整收盘", out[len(out)-1].TradeDate, completedDate)
	}
	return out, ""
}

func persistCompletedBenchmark(ctx context.Context, market string, bars []datasource.Bar, now time.Time) {
	if common.DB == nil || market != "cn" {
		return
	}
	bars, _ = completedBenchmarkBars(bars, now, moodTargetDate(now.In(time.Local), sessionCloseMin))
	rows := make([]model.BenchmarkDaily, 0, len(bars))
	for _, b := range bars {
		rows = append(rows, model.BenchmarkDaily{Market: market, Symbol: model.CNBenchmarkSymbol, TradeDate: b.TradeDate, Close: b.Close, Source: b.Source, ObservedAt: now})
	}
	if len(rows) == 0 {
		return
	}
	if err := common.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200).Error; err != nil {
		common.SysWarn("基准历史缓存保存失败：%v", err)
	}
}
