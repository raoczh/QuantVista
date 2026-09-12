package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/model"
)

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
