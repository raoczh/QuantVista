package service

import (
	"errors"
	"fmt"
	"time"

	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

// Missing weekdays are unknown, not closed. Weekends need no guessed calendar
// row because Shanghai/Shenzhen stock exchanges do not open on makeup workdays.
func knownCalendarRange(database *gorm.DB, market string, first, last time.Time) (bool, error) {
	if last.Before(first) {
		return true, nil
	}
	if int(last.Sub(first).Hours()/24)+1 > maintenanceMaxNaturalDays {
		return false, nil
	}
	var rows []model.TradingCalendar
	if err := database.Select("trade_date").Where("market = ? AND trade_date >= ? AND trade_date <= ?",
		market, first.Format(time.DateOnly), last.Format(time.DateOnly)).
		Limit(maintenanceMaxNaturalDays + 1).Find(&rows).Error; err != nil {
		return false, err
	}
	if len(rows) > maintenanceMaxNaturalDays {
		return false, errors.New("交易日历范围超过查询上限")
	}
	knownDates := make(map[string]bool, len(rows))
	for _, row := range rows {
		knownDates[row.TradeDate] = true
	}
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !knownDates[day.Format(time.DateOnly)] {
			return false, nil
		}
	}
	return true, nil
}

// The health report's 16:30 grace period is not a snapshot's business date.
// An unconfirmed weekday after close is only a requested target; execution must
// verify the source date before publishing any prices or calendar facts.
func wideSnapshotPlanDate(database *gorm.DB, now time.Time) (string, error) {
	if database == nil {
		return "", errors.New("数据库不可用")
	}
	now = now.In(datasource.ChinaMarketLocation())
	reader := &dataHealthReader{db: database, now: now}
	open, known := reader.isTradingDayToday(now)
	if reader.err != nil {
		return "", reader.err
	}
	minute := now.Hour()*60 + now.Minute()
	if open || !known {
		if minute >= sessionPreAuctionMin && minute < sessionCloseMin+5 {
			return "", errors.New("全市场增量同步需要完整收盘快照，请在北京时间 15:05 后执行；盘中可使用日线补采修复历史")
		}
		if minute >= sessionCloseMin+5 {
			return now.Format(time.DateOnly), nil
		}
	}
	expected := reader.prevOpenTradeDate(now.Format(time.DateOnly))
	if reader.err != nil {
		return "", reader.err
	}
	if expected == "" {
		return "", fmt.Errorf("交易日历不完整，无法确定 %s 之前的收盘快照日期，请先回填日历", now.Format(time.DateOnly))
	}
	return expected, nil
}

func validateWideSnapshotDate(rows []datasource.SpotRow, tradeDate, expectedDate string, now time.Time) error {
	now = now.In(datasource.ChinaMarketLocation())
	if expectedDate != "" && tradeDate != expectedDate {
		return fmt.Errorf("快照实际交易日为 %s，与补采目标 %s 不一致，未写入行情；请核对日历或等待上游更新", tradeDate, expectedDate)
	}
	if tradeDate == now.Format(time.DateOnly) && now.Hour()*60+now.Minute() < sessionCloseMin+5 {
		return errors.New("当前快照尚未进入完整收盘窗口，不能写成全日数据，请在北京时间 15:05 后同步")
	}
	var latest time.Time
	for _, row := range rows {
		if row.Price <= 0 || row.Volume <= 0 {
			continue
		}
		if row.DataTime <= 0 {
			return fmt.Errorf("活跃股票 %s 缺少行情时间，快照不完整", row.Symbol)
		}
		observed := time.Unix(row.DataTime, 0).In(datasource.ChinaMarketLocation())
		if observed.Format(time.DateOnly) != tradeDate {
			return fmt.Errorf("活跃股票 %s 的行情不属于 %s，快照混有其他交易日数据", row.Symbol, tradeDate)
		}
		if observed.After(now.Add(time.Minute)) {
			return fmt.Errorf("活跃股票 %s 的行情时间晚于当前时刻", row.Symbol)
		}
		if observed.After(latest) {
			latest = observed
		}
	}
	if latest.IsZero() || latest.Hour()*60+latest.Minute() < sessionCloseMin {
		return errors.New("快照未包含收盘时刻的有效行情，不能把盘中快照当成完整日线")
	}
	return nil
}
