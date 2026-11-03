package service

import (
	"context"
	"errors"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// 同一报告只读同一事务；记住首个查询错误，不把读取失败转换成零覆盖。
type dataHealthReader struct {
	db         *gorm.DB
	now        time.Time
	err        error
	tradeDates []string
}

func (r *dataHealthReader) failed(err error) bool {
	if err == nil {
		return false
	}
	if r.err == nil {
		r.err = err
	}
	return true
}

func (r *dataHealthReader) missingOrFailed(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) || r.failed(err)
}

func (r *dataHealthReader) tooMany(got, limit int) bool {
	if got <= limit {
		return false
	}
	r.failed(errors.New("数据健康日期数超过查询上限"))
	return true
}

func recentOpenDatesDB(db *gorm.DB, market, end string, days int) ([]string, error) {
	if db == nil {
		return nil, errors.New("数据库不可用")
	}
	var dates []string
	if err := db.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date <= ?", market, true, end).
		Order("trade_date DESC").Limit(days).Pluck("trade_date", &dates).Error; err != nil {
		return nil, err
	}
	reverseStrings(dates)
	return dates, nil
}

func (r *dataHealthReader) recentOpenDates(market, end string, days int) []string {
	dates, err := recentOpenDatesDB(r.db, market, end, days)
	r.failed(err)
	return dates
}

func (r *dataHealthReader) isTradingDayToday(now time.Time) (open, known bool) {
	var row model.TradingCalendar
	err := r.db.Where("market = ? AND trade_date = ?", "cn", now.Format("2006-01-02")).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		weekend := now.Weekday() == time.Saturday || now.Weekday() == time.Sunday
		return false, weekend
	}
	if r.failed(err) {
		return false, false
	}
	return row.IsOpen, true
}

func (r *dataHealthReader) prevOpenTradeDate(before string) string {
	var dates []string
	if r.failed(r.db.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date < ?", "cn", true, before).
		Order("trade_date DESC").Limit(1).Pluck("trade_date", &dates).Error) {
		return ""
	}
	if len(dates) > 0 {
		previous, previousErr := time.ParseInLocation(time.DateOnly, dates[0], time.Local)
		boundary, boundaryErr := time.ParseInLocation(time.DateOnly, before, time.Local)
		if previousErr != nil || boundaryErr != nil {
			return ""
		}
		known, err := knownCalendarRange(r.db, "cn", previous.AddDate(0, 0, 1), boundary.AddDate(0, 0, -1))
		if r.failed(err) || !known {
			return ""
		}
		return dates[0]
	}
	return ""
}

func (r *dataHealthReader) wideExpectedDate(now time.Time) string {
	return r.expectedDateAfter(now, 16*60+30)
}

// 今日开休市尚未确认，不影响只要求上一完整交易日的历史数据域。
// 真正需要今日数据时才返回未知，不能把缺行猜成休市或污染其他域。
func (r *dataHealthReader) expectedDateAfter(now time.Time, readyMinute int) string {
	if now.Hour()*60+now.Minute() >= readyMinute {
		open, known := r.isTradingDayToday(now)
		if !known {
			return ""
		}
		if open {
			return now.Format("2006-01-02")
		}
	}
	return r.prevOpenTradeDate(now.Format("2006-01-02"))
}

func (r *dataHealthReader) openDaysBehind(observed, expected string) int {
	if observed == "" || expected == "" {
		return -1
	}
	if observed >= expected {
		return 0
	}
	var count int64
	if r.failed(r.db.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date > ? AND trade_date <= ?", "cn", true, observed, expected).
		Count(&count).Error) || count == 0 {
		return -1
	}
	return int(count)
}

func buildDataHealthReportContext(ctx context.Context, now time.Time, days int) (*DataHealthReport, error) {
	ctx = jobSubmissionContext(ctx)
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var report *DataHealthReport
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		reader := &dataHealthReader{db: tx, now: now}
		report = reader.build(now, days)
		return reader.err
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// HTTP 入口必须接收查询错误与请求取消；保留旧默认包装仅供兼容调用。
func BuildDataHealthReportForDaysContext(ctx context.Context, days int) (*DataHealthReport, error) {
	return buildDataHealthReportContext(ctx, time.Now(), days)
}
