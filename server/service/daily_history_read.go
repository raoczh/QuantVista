package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

// A forming candle belongs in an online response, not in completed history.
// Keep the same five-minute publication allowance as full-market maintenance.
const dailyHistoryCloseMinute = 15*60 + 5

func (service *MarketService) dailyTime() time.Time {
	if service.dailyNow != nil {
		return service.dailyNow().In(datasource.ChinaMarketLocation())
	}
	return time.Now().In(datasource.ChinaMarketLocation())
}

func completedDailyBarsAt(market string, bars []datasource.Bar, now time.Time) []datasource.Bar {
	if market != "cn" {
		return bars
	}
	now = now.In(datasource.ChinaMarketLocation())
	today := now.Format(time.DateOnly)
	completed := make([]datasource.Bar, 0, len(bars))
	for _, candle := range bars {
		if candle.TradeDate != "" && (candle.TradeDate < today || (candle.TradeDate == today && now.Hour()*60+now.Minute() >= dailyHistoryCloseMinute)) {
			completed = append(completed, candle)
		}
	}
	return completed
}

// The source still refreshes the current price basis. After a checked write,
// read the committed series, including older history and same-day trading facts
// the backup did not supply. Never concatenate independently adjusted prices.
func (service *MarketService) refreshDailyHistory(ctx context.Context, market, symbol string, limit int, expectedDate string, now time.Time) ([]datasource.Bar, error) {
	if stored, reusable := service.readRecentlyRepairedHistory(ctx, market, symbol, limit, expectedDate, now); reusable {
		return stored, nil
	}
	upstream, _, fetchError := service.mgr.GetDailyBarsFresh(ctx, market, symbol, limit, expectedDate)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fetchError != nil {
		if errors.Is(fetchError, datasource.ErrSymbolInvalid) {
			return nil, fetchError
		}
		stored, readError := service.readStoredDailyHistory(ctx, market, symbol, limit, now, true)
		if readError != nil {
			return nil, errors.Join(fetchError, readError)
		}
		if len(stored) == 0 {
			return nil, fetchError
		}
		// Dates and original sources are unchanged. Consumer freshness and minimum
		// window gates still apply; a successful local read is not a fresh quote.
		common.SysWarn("daily history fallback market=%s symbol=%s rows=%d through=%s", market, symbol, len(stored), stored[len(stored)-1].TradeDate)
		return stored, nil
	}
	if err := validateAdjustedBars(market, upstream); err != nil {
		return nil, err
	}
	completed := completedDailyBarsAt(market, upstream, now)
	if persistenceError := service.persistDailyBarsChecked(ctx, market, symbol, completed, upstream, false); persistenceError != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(persistenceError, errBarBasisUnverified) || errors.Is(persistenceError, errRebaseInProgress) {
			return nil, persistenceError
		}
		common.SysWarn("daily history persistence failed market=%s symbol=%s: %v", market, symbol, persistenceError)
		// An independently valid upstream series remains displayable during a DB
		// outage, but it must not be extended with uncommitted local prices.
		return upstream, nil
	}
	stored, readError := service.readStoredDailyHistory(ctx, market, symbol, limit, now, false)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if readError != nil || len(stored) == 0 {
		return upstream, nil
	}
	// Another writer may have changed the price basis after our transaction.
	// In that case retain the upstream's whole window rather than joining it to
	// a newly rebased local prefix, particularly for today's forming candle.
	storedCloses := make(map[string]float64, len(stored))
	storedUsesRatio := false
	for _, candle := range stored {
		storedCloses[candle.TradeDate] = candle.Close
		storedUsesRatio = storedUsesRatio || candle.Source == "sina_qfq"
	}
	upstreamUsesRatio, hasOverlap := false, false
	for _, candle := range completed {
		upstreamUsesRatio = upstreamUsesRatio || candle.Source == "sina_qfq"
		if _, exists := storedCloses[candle.TradeDate]; exists {
			hasOverlap = true
		}
	}
	if !hasOverlap || storedUsesRatio != upstreamUsesRatio {
		return upstream, nil
	}
	if _, mismatch := closeMismatch(storedCloses, completed, rebaseTolerance); mismatch {
		return upstream, nil
	}
	lastStoredDate := stored[len(stored)-1].TradeDate
	for _, candle := range upstream {
		if candle.TradeDate > lastStoredDate {
			stored = append(stored, candle)
		}
	}
	if len(stored) > limit {
		stored = stored[len(stored)-limit:]
	}
	return stored, nil
}

// Maintenance just verified these completed prices. Reuse that work for the
// same short cache interval, but never suppress an intraday chart refresh.
func (service *MarketService) readRecentlyRepairedHistory(ctx context.Context, market, symbol string, limit int, expectedDate string, now time.Time) ([]datasource.Bar, bool) {
	if common.DB == nil || expectedDate == "" || market != "cn" {
		return nil, false
	}
	minute := now.Hour()*60 + now.Minute()
	if isTradingDayToday(now) && minute >= sessionPreAuctionMin && minute < dailyHistoryCloseMinute {
		return nil, false
	}
	var latest []model.DailyBar
	if err := common.DB.WithContext(ctx).Select("trade_date", "updated_at").Where("market = ? AND symbol = ?", market, symbol).
		Order("trade_date DESC").Limit(1).Find(&latest).Error; err != nil || len(latest) == 0 {
		return nil, false
	}
	row := latest[0]
	reuseTTL := dailyBarsCacheTTL
	if minute >= dailyHistoryCloseMinute && minute < 16*60+30 {
		reuseTTL = time.Minute
	}
	if row.TradeDate < expectedDate || row.UpdatedAt.IsZero() || row.UpdatedAt.After(now) || now.Sub(row.UpdatedAt) > reuseTTL {
		return nil, false
	}
	bars, err := service.readStoredDailyHistory(ctx, market, symbol, limit, now, true)
	minimumRows := limit
	if limit == wideBarLimit+1 {
		minimumRows = wideBarLimit
	}
	if err != nil || len(bars) < minimumRows || bars[len(bars)-1].TradeDate < expectedDate {
		return nil, false
	}
	return bars, true
}

func (service *MarketService) readStoredDailyHistory(ctx context.Context, market, symbol string, limit int, now time.Time, requireVerifiedState bool) ([]datasource.Bar, error) {
	if common.DB == nil {
		return nil, nil
	}
	if _, writing := dailyBarWrites.Load(QuoteKey(market, symbol)); writing {
		return nil, errRebaseInProgress
	}
	var rows []model.DailyBar
	var quotes []model.StockQuote
	err := readSnapshotTx(ctx, func(database *gorm.DB) error {
		var blockedCount int64
		if err := database.Model(&model.DailyBarWriteLock{}).Where("market = ? AND symbol = ? AND basis_unverified = ?", market, symbol, true).Count(&blockedCount).Error; err != nil {
			return err
		}
		if blockedCount > 0 {
			return errBarBasisUnverified
		}
		if requireVerifiedState {
			var states []model.MarketSyncState
			if err := database.Select("init_status", "last_error").Where("market = ? AND symbol = ?", market, symbol).Limit(1).Find(&states).Error; err != nil {
				return err
			}
			if len(states) > 0 && states[0].InitStatus != "done" && strings.HasPrefix(states[0].LastError, errBarBasisUnverified.Error()) {
				return errBarBasisUnverified
			}
			if err := database.Select("price", "prev_close", "data_time").Where("market = ? AND symbol = ?", market, symbol).Limit(1).Find(&quotes).Error; err != nil {
				return err
			}
		}
		return database.Where("market = ? AND symbol = ? AND trade_date <= ?", market, symbol, now.Format(time.DateOnly)).
			Order("trade_date DESC").Limit(limit + 1).Find(&rows).Error
	})
	if err != nil {
		return nil, fmt.Errorf("read persisted daily history: %w", err)
	}
	today := now.In(datasource.ChinaMarketLocation()).Format(time.DateOnly)
	if len(rows) > 0 && market == "cn" && rows[0].TradeDate == today {
		closeTime, _ := time.ParseInLocation("2006-01-02 15:04", today+" 15:05", datasource.ChinaMarketLocation())
		if now.Before(closeTime) || rows[0].UpdatedAt.IsZero() || rows[0].UpdatedAt.Before(closeTime) {
			rows = rows[1:]
		}
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if err := validateLocalAdjustedBars(market, rows); err != nil {
		return nil, err
	}
	bars := make([]datasource.Bar, 0, len(rows))
	for rowIndex := len(rows) - 1; rowIndex >= 0; rowIndex-- {
		row := rows[rowIndex]
		if market == "cn" && !isVerifiedCompletedDailyRow(row) {
			return nil, fmt.Errorf("%w (%s)", errDailyHistoryIncomplete, row.TradeDate)
		}
		bars = append(bars, datasource.Bar{
			TradeDate: row.TradeDate, Open: row.Open, High: row.High, Low: row.Low, Close: row.Close,
			Volume: row.Volume, Amount: row.Amount, TurnoverRate: row.TurnoverRate, Source: row.Source,
			AmountSource: row.AmountSource, TurnoverSource: row.TurnoverSource,
		})
	}
	if len(bars) == 0 {
		return nil, nil
	}
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}
	if requireVerifiedState && len(quotes) > 0 && !quotes[0].DataTime.IsZero() {
		latest := bars[len(bars)-1]
		quoteDate := quotes[0].DataTime.In(datasource.ChinaMarketLocation()).Format(time.DateOnly)
		if quoteDate > latest.TradeDate && quotes[0].PrevClose > 0 && relDiff(latest.Close, quotes[0].PrevClose) > rebaseTolerance {
			return nil, errBarBasisUnverified
		}
	}
	return datasource.ValidateDailyBars(bars)
}
