package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// The benchmark has already been fetched for tracking. Its real index dates can
// fill a missing calendar window, including today's observed open session,
// without a second upstream request or guessing future weekdays.
func persistTrackingCalendar(ctx context.Context, market string, bars []datasource.Bar) error {
	if market != "cn" || len(bars) == 0 {
		return nil
	}
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	dates := make([]string, 0, len(bars))
	for _, bar := range bars {
		if bar.Close <= 0 || !finiteRecNumber(bar.Close) {
			return errors.New("基准价格无效，不能用于确认追踪交易日历")
		}
		dates = append(dates, bar.TradeDate)
	}
	openDates, firstDate, lastDate, err := calendarSourceWindow(dates)
	if err != nil {
		return err
	}
	first, _ := time.ParseInLocation(time.DateOnly, firstDate, time.Local)
	last, _ := time.ParseInLocation(time.DateOnly, lastDate, time.Local)
	if last.Sub(first) > maintenanceMaxLookbackDays*24*time.Hour {
		return errors.New("基准日期跨度超过追踪日历修复上限")
	}
	rows := make([]model.TradingCalendar, 0, int(last.Sub(first).Hours()/24)+1)
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		date := day.Format(time.DateOnly)
		_, isOpen := openDates[date]
		rows = append(rows, model.TradingCalendar{Market: market, TradeDate: date, IsOpen: isOpen})
	}
	// Existing local facts are not reclassified by an incidental tracking read;
	// correcting them remains an explicit calendar maintenance operation.
	return common.DB.WithContext(ctx).Select("Market", "TradeDate", "IsOpen").
		Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200).Error
}

// A current-day cached candle must not prevent a fresh quote from updating the
// tracking price. After close, only an actual closing quote can supersede it.
func dropSupersededTrackingDay(bars []datasource.Bar, now time.Time, recommendationDate string, quoteTime time.Time) []datasource.Bar {
	today := now.Format(time.DateOnly)
	quoteTime = quoteTime.In(now.Location())
	if len(bars) == 0 || today <= recommendationDate || quoteTime.IsZero() ||
		quoteTime.Format(time.DateOnly) != today || bars[len(bars)-1].TradeDate != today {
		return bars
	}
	if now.Hour()*60+now.Minute() >= sessionCloseMin && quoteTime.Hour()*60+quoteTime.Minute() < sessionCloseMin {
		return bars
	}
	return bars[:len(bars)-1]
}

// 追踪有效期与节点收益使用完整的市场日轴；缺日不能当作停牌或休市跳过。
func trackingTradeDates(ctx context.Context, market, from, to string) ([]string, error) {
	start, startErr := time.ParseInLocation("2006-01-02", from, time.Local)
	end, endErr := time.ParseInLocation("2006-01-02", to, time.Local)
	if startErr != nil || endErr != nil || start.After(end) {
		return nil, errors.New("推荐追踪日期无效")
	}
	var rows []model.TradingCalendar
	if err := common.DB.WithContext(ctx).Where("market = ? AND trade_date > ? AND trade_date <= ?", market, from, to).
		Order("trade_date").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取追踪交易日历失败: %w", err)
	}
	byDate := make(map[string]bool, len(rows))
	for _, row := range rows {
		byDate[row.TradeDate] = row.IsOpen
	}
	dates := make([]string, 0, len(rows))
	for day := start.AddDate(0, 0, 1); !day.After(end); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		open, known := byDate[date]
		if !known {
			return nil, fmt.Errorf("缺少 %s 的交易日历，暂不能更新推荐追踪", date)
		}
		if open {
			dates = append(dates, date)
		}
	}
	return dates, nil
}
