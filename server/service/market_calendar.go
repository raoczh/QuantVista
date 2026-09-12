package service

import (
	"errors"
	"time"
)

// 上游开市日只能证明自身覆盖窗口中的休市日，窗口两侧的工作日仍属未知。
func calendarSourceWindow(days []string) (map[string]struct{}, string, string, error) {
	open := make(map[string]struct{}, len(days))
	minDate, maxDate := "", ""
	today := time.Now().Format("2006-01-02")
	for _, date := range days {
		if parsed, err := time.ParseInLocation("2006-01-02", date, time.Local); err != nil || parsed.Format("2006-01-02") != date || date > today {
			return nil, "", "", errors.New("上游交易日历包含无效或未来日期")
		}
		open[date] = struct{}{}
		if minDate == "" || date < minDate {
			minDate = date
		}
		if date > maxDate {
			maxDate = date
		}
	}
	if minDate == "" {
		return nil, "", "", errors.New("上游交易日历为空")
	}
	return open, minDate, maxDate, nil
}
