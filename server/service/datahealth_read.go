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
	db              *gorm.DB
	now             time.Time
	err             error
	tradeDates      []string
	calendarMissing bool
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

func (r *dataHealthReader) isTradingDayToday(now time.Time) bool {
	var row model.TradingCalendar
	err := r.db.Where("market = ? AND trade_date = ?", "cn", now.Format("2006-01-02")).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		r.calendarMissing = true
		return false
	}
	return !r.failed(err) && row.IsOpen
}

func (r *dataHealthReader) prevOpenTradeDate(before string) string {
	var dates []string
	if r.failed(r.db.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date < ?", "cn", true, before).
		Order("trade_date DESC").Limit(1).Pluck("trade_date", &dates).Error) {
		return ""
	}
	if len(dates) > 0 {
		return dates[0]
	}
	r.calendarMissing = true
	return prevOpenTradeDateDB(nil, before) // 仅估计缺口窗口；新鲜度保持 unknown。
}

func (r *dataHealthReader) wideExpectedDate(now time.Time) string {
	if r.isTradingDayToday(now) && now.Hour()*60+now.Minute() >= 16*60+30 {
		return now.Format("2006-01-02")
	}
	return r.prevOpenTradeDate(now.Format("2006-01-02"))
}

func (r *dataHealthReader) openDaysBehind(observed, expected string) int {
	if observed == "" || expected == "" || r.calendarMissing {
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
