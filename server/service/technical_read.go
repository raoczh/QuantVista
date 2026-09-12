package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// 日线与报价分别校验。盘中允许使用上一交易日完整日线，但不能把更旧的历史当作当前技术面。
func technicalBarsIssue(ctx context.Context, market string, quoteTime time.Time, fresh quoteFreshInfo, bars []datasource.Bar) string {
	if len(bars) == 0 {
		return "日线数据暂不可用"
	}
	previous := ""
	for _, bar := range bars {
		_, err := time.Parse("2006-01-02", bar.TradeDate)
		if err != nil || bar.TradeDate <= previous || bar.Close <= 0 || bar.Low <= 0 || bar.High < bar.Low ||
			bar.Close < bar.Low || bar.Close > bar.High || math.IsNaN(bar.Close) || math.IsNaN(bar.High) || math.IsNaN(bar.Low) ||
			math.IsInf(bar.Close, 0) || math.IsInf(bar.High, 0) || math.IsInf(bar.Low, 0) {
			return "日线日期或价格异常，技术指标与评分不可用"
		}
		previous = bar.TradeDate
	}
	quoteDate := quoteTime.In(time.Local).Format("2006-01-02")
	if previous > quoteDate {
		return "日线日期晚于行情时点，技术指标与评分不可用"
	}
	minimum := fresh.ExpectedDate
	if minimum == "" {
		return "日线时效无法核验，技术指标与评分不可用"
	}
	if previous >= minimum {
		return ""
	}
	if market == "cn" && (fresh.MarketState == marketStateTrading || fresh.MarketState == marketStateBreak || fresh.MarketState == marketStatePreOpen) {
		var dates []string
		if common.DB == nil || common.DB.WithContext(ctx).Model(&model.TradingCalendar{}).
			Where("market = ? AND is_open = ? AND trade_date < ?", market, true, minimum).
			Order("trade_date DESC").Limit(1).Pluck("trade_date", &dates).Error != nil || len(dates) == 0 {
			return "缺少上一交易日历，日线时效无法核验"
		}
		// 盘前仍使用昨收报价时，不能额外放宽到前日。
		if quoteDate == minimum && quoteDate == time.Now().In(time.Local).Format("2006-01-02") && previous >= dates[0] {
			return ""
		}
	}
	return fmt.Sprintf("日线仅更新至 %s，未达到行情所需日期 %s，技术指标与评分不可用", previous, minimum)
}
