package service

import (
	"context"
	"database/sql"
	"time"

	"quantvista/common"
	"quantvista/datasource"

	"gorm.io/gorm"
)

// Quote consumers must not inherit the scheduler's weekday/MAX-date fallback.
// A previous close is usable only when every intervening weekday is known.
func newCNQuoteFreshnessJudge(database *gorm.DB, now time.Time) func(time.Time) quoteFreshInfo {
	now = now.In(datasource.ChinaMarketLocation())
	today := now.Format(time.DateOnly)
	minute := now.Hour()*60 + now.Minute()
	weekend := now.Weekday() == time.Saturday || now.Weekday() == time.Sunday
	tradingDay, calendarKnown := false, weekend
	previousOpen := ""
	lastKnownOpen := ""
	if database != nil {
		options := &sql.TxOptions{}
		if database.Dialector.Name() == "mysql" {
			options.Isolation, options.ReadOnly = sql.LevelRepeatableRead, true
		}
		// MAX(open date) and intervening coverage must see the same calendar,
		// including when a backfill commits between these reads.
		err := database.Transaction(func(transaction *gorm.DB) error {
			reader := &dataHealthReader{db: transaction, now: now}
			tradingDay, calendarKnown = reader.isTradingDayToday(now)
			if !tradingDay || minute < sessionQuoteReadyMin {
				dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
				boundary := dayStart.AddDate(0, 0, -1)
				dates := reader.recentOpenDates("cn", boundary.Format(time.DateOnly), 1)
				if len(dates) > 0 {
					previous, parseErr := time.ParseInLocation(time.DateOnly, dates[0], now.Location())
					if reader.failed(parseErr) {
						return reader.err
					}
					lastKnownOpen = dates[0]
					known, coverageErr := knownCalendarRange(transaction, "cn", previous.AddDate(0, 0, 1), boundary)
					if reader.failed(coverageErr) {
						return reader.err
					}
					if known {
						previousOpen = lastKnownOpen
					}
				}
			}
			return reader.err
		}, options)
		if err != nil {
			previousOpen, lastKnownOpen = "", ""
		}
	}
	state := cnMarketState(now, tradingDay)
	expected := expectedQuoteDate(now, tradingDay, previousOpen)
	if !calendarKnown {
		if minute < sessionQuoteReadyMin {
			state = marketStatePreOpen
		} else {
			state, expected = marketStateUnknown, ""
		}
	}
	return func(dataTime time.Time) quoteFreshInfo {
		if dataTime.IsZero() || dataTime.After(now.Add(time.Minute)) {
			return quoteFreshInfo{Status: freshStatusStale, MarketState: state, ExpectedDate: expected}
		}
		observed := dataTime.In(now.Location())
		// A real same-day session timestamp can establish today's quote date
		// when only today's calendar row is absent. It cannot override a known
		// holiday, establish earlier weekdays, or waive the intraday delay gate.
		canEstablishToday := tradingDay || (!calendarKnown && !weekend)
		if canEstablishToday && minute >= sessionPreAuctionMin && observed.Format(time.DateOnly) == today &&
			observed.Hour()*60+observed.Minute() >= sessionPreAuctionMin {
			return quoteFreshness(observed, now, cnMarketState(now, true), today)
		}
		if expected == "" {
			status := freshStatusUnknown
			// Even incomplete coverage can prove staleness when a later open
			// day is known. It can never prove that the last known day is current.
			if lastKnownOpen != "" && observed.Format(time.DateOnly) < lastKnownOpen {
				status = freshStatusStale
			}
			return quoteFreshInfo{Status: status, MarketState: state}
		}
		return quoteFreshness(observed, now, state, expected)
	}
}

func (service *MarketService) freshnessJudge(ctx context.Context, market string) func(time.Time) quoteFreshInfo {
	if market != "cn" {
		return func(time.Time) quoteFreshInfo {
			return quoteFreshInfo{Status: freshStatusUnknown, MarketState: marketStateClosed}
		}
	}
	now := time.Now()
	if service.quoteNow != nil {
		now = service.quoteNow()
	}
	database := common.DB
	if database != nil {
		database = database.WithContext(jobSubmissionContext(ctx))
	}
	return newCNQuoteFreshnessJudge(database, now)
}
