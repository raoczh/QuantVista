package service

import (
	"context"
	"fmt"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// Permanent outcomes require the complete path to the first trigger or expiry.
// A missing bar may hide an earlier stop; only an observed suspension excuses it.
func validateTrackingSettlement(ctx context.Context, recommendation model.Recommendation, tradeDates []string, bars []datasource.Bar, result trackResult, validDays int) error {
	if !frozenTerminal(result.Outcome) {
		return nil
	}
	through := result.LastDate
	if result.Outcome == model.RecOutcomeExpired && validDays > 0 && validDays <= len(tradeDates) {
		through = tradeDates[validDays-1]
	}
	observedDates := make(map[string]bool, len(bars))
	for _, bar := range bars {
		observedDates[bar.TradeDate] = true
	}
	missingDates := make([]string, 0)
	for _, date := range tradeDates {
		if date > through {
			break
		}
		if !observedDates[date] {
			missingDates = append(missingDates, date)
		}
	}
	if len(missingDates) == 0 {
		return nil
	}
	var suspendedDates []string
	if err := common.DB.WithContext(ctx).Model(&model.StockUniverseDaily{}).
		Where("market = ? AND symbol = ? AND suspended = ? AND trade_date IN ?",
			recommendation.Market, recommendation.Symbol, true, missingDates).
		Pluck("trade_date", &suspendedDates).Error; err != nil {
		return fmt.Errorf("读取追踪停牌事实失败: %w", err)
	}
	for _, date := range suspendedDates {
		observedDates[date] = true
	}
	for _, date := range missingDates {
		if !observedDates[date] {
			return fmt.Errorf("追踪缺少 %s 日线且无已确认停牌记录，暂不结算，请先补采", date)
		}
	}
	return nil
}
