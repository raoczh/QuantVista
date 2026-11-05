package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

var errDailyHistoryIncomplete = errors.New("日线历史尚未补齐")

// CreatedAt cannot establish completion: older upserts did not refresh it.
// Unknown migration timestamps and legacy unadjusted Sina prices need a fetch,
// not a timestamp upgrade that would manufacture verification evidence.
func isVerifiedCompletedDailyRow(row model.DailyBar) bool {
	if row.UpdatedAt.IsZero() || (row.Market == "cn" && strings.EqualFold(strings.TrimSpace(row.Source), "sina")) {
		return false
	}
	closeTime, err := time.ParseInLocation("2006-01-02 15:04", row.TradeDate+" 15:05", datasource.ChinaMarketLocation())
	return err == nil && !row.UpdatedAt.Before(closeTime)
}

// A successful HTTP response is not proof of a full initialization. Preserve
// useful rows, but do not mark short/stale histories done without IPO evidence.
func validateInitializedDailyHistory(database *gorm.DB, market, symbol string, count int, lastDate string, now time.Time) error {
	if database == nil {
		return errors.New("数据库不可用")
	}
	now = now.In(datasource.ChinaMarketLocation())
	reader := &dataHealthReader{db: database, now: now}
	expectedDate := reader.expectedDateAfter(now, dailyHistoryCloseMinute)
	if reader.err != nil {
		return reader.err
	}
	if expectedDate == "" {
		return fmt.Errorf("%w：交易日历不完整，无法确认应有收盘日，请先回填日历；已有数据已保存", errDailyHistoryIncomplete)
	}
	// COUNT(*) includes migrated, forming and expired rows. Read one stock's
	// retained history with a hard cap; only its verified tail can supply the
	// standard research window. Older unknown rows must not poison a good tail.
	var rows []model.DailyBar
	if err := database.Select("market", "trade_date", "source", "updated_at").
		Where("market = ? AND symbol = ? AND trade_date >= ? AND trade_date <= ?", market, symbol, model.DailyBarRetentionCutoffAt(now), expectedDate).
		Order("trade_date DESC").Limit(1000).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) >= 1000 {
		return errors.New("单股日线历史核验超过 1000 根查询硬上限")
	}
	verifiedDates := make(map[string]bool, len(rows))
	verifiedCount, verifiedTailCount := 0, 0
	latestVerifiedDate := ""
	tailInterrupted := false
	for _, row := range rows {
		if !isVerifiedCompletedDailyRow(row) {
			tailInterrupted = true
			continue
		}
		verifiedDates[row.TradeDate] = true
		verifiedCount++
		if !tailInterrupted {
			verifiedTailCount++
		}
		if latestVerifiedDate == "" {
			latestVerifiedDate = row.TradeDate
		}
	}
	if verifiedTailCount >= wideBarLimit {
		if latestVerifiedDate == expectedDate {
			return nil
		}
		// A suspended last session does not excuse other missing sessions in
		// the tail: every intervening open day still needs explicit evidence.
		latest, err := time.ParseInLocation(time.DateOnly, latestVerifiedDate, now.Location())
		if err != nil {
			return err
		}
		covered, err := initializedDailyHistoryRangeCoveredDB(database, market, symbol, latest.AddDate(0, 0, 1).Format(time.DateOnly), expectedDate, verifiedDates)
		if err != nil {
			return err
		}
		if covered {
			return nil
		}
		return fmt.Errorf("%w：已保存截至 %s，已核验截至 %s，应有 %s；缺失日期须补采或确认停牌", errDailyHistoryIncomplete, lastDate, latestVerifiedDate, expectedDate)
	}
	listingDates, err := dailyBackfillListingDatesDB(database, now)
	if err != nil {
		return err
	}
	listingDate := listingDates[symbol]
	if listingDate != "" && listingDate >= model.DailyBarRetentionCutoffAt(now) && listingDate <= expectedDate {
		// Even an unverified pre-listing row contradicts the IPO exclusion.
		var preListingDates []string
		if err := database.Model(&model.DailyBar{}).Where("market = ? AND symbol = ? AND trade_date < ?", market, symbol, listingDate).
			Limit(1).Pluck("trade_date", &preListingDates).Error; err != nil {
			return err
		}
		if len(preListingDates) == 0 {
			covered, err := initializedDailyHistoryRangeCoveredDB(database, market, symbol, listingDate, expectedDate, verifiedDates)
			if err != nil {
				return err
			}
			if covered {
				return nil
			}
		}
	}
	return fmt.Errorf("%w：已保存 %d 根，保留窗口已核验 %d 根，连续已核验尾部 %d 根；标准研究窗口需要 %d 根且须覆盖 %s，未确认是完整的新股短历史", errDailyHistoryIncomplete, count, verifiedCount, verifiedTailCount, wideBarLimit, expectedDate)
}

// Initialization may prove an IPO older than the HTTP calendar API's 92-day
// window. Segment the checks without widening that public limit or scanning
// beyond the retained 400-natural-day history.
func initializedDailyHistoryRangeCoveredDB(database *gorm.DB, market, symbol, firstDate, lastDate string, verifiedDates map[string]bool) (bool, error) {
	first, firstError := time.ParseInLocation(time.DateOnly, firstDate, datasource.ChinaMarketLocation())
	last, lastError := time.ParseInLocation(time.DateOnly, lastDate, datasource.ChinaMarketLocation())
	if firstError != nil || lastError != nil || last.Before(first) || int(last.Sub(first).Hours()/24)+1 > model.DailyBarRetentionDays {
		return false, nil
	}
	for segmentFirst := first; !segmentFirst.After(last); segmentFirst = segmentFirst.AddDate(0, 0, maintenanceMaxNaturalDays) {
		segmentLast := segmentFirst.AddDate(0, 0, maintenanceMaxNaturalDays-1)
		if segmentLast.After(last) {
			segmentLast = last
		}
		known, err := knownCalendarRange(database, market, segmentFirst, segmentLast)
		if err != nil || !known {
			return false, err
		}
	}
	var openDates []string
	if err := database.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date >= ? AND trade_date <= ?", market, true, firstDate, lastDate).
		Order("trade_date").Limit(model.DailyBarRetentionDays+1).Pluck("trade_date", &openDates).Error; err != nil {
		return false, err
	}
	if len(openDates) == 0 || len(openDates) > model.DailyBarRetentionDays {
		return false, nil
	}
	suspended, err := dailyBackfillCellsDB(database, &model.StockUniverseDaily{}, market, []string{symbol}, openDates, true)
	if err != nil {
		return false, err
	}
	for _, date := range openDates {
		if !verifiedDates[date] && !suspended[symbol+"@"+date] {
			return false, nil
		}
	}
	return true, nil
}
