package datasource

import (
	"context"
	"errors"
)

// GetDailyBarsFresh tries independent adjusted sources until one covers the
// requested business date and history length. A current but truncated primary
// must not hide a complete backup. Short IPO histories remain usable when no
// source can supply more rows; freshness describes the date, not the row count.
func (manager *Manager) GetDailyBarsFresh(ctx context.Context, market, symbol string, limit int, expectedDate string) ([]Bar, bool, error) {
	if limit <= 0 {
		limit = 120
	}
	parentContext := ctx
	totalBudget, _ := manager.budgets("daily_bars")
	ctx, cancel := context.WithTimeout(ctx, totalBudget)
	defer cancel()

	call := func(sourceContext context.Context, adapter Adapter) ([]Bar, error) {
		if provider, declared := adapter.(AdjustedDailyProvider); market == "cn" && declared && !provider.ProvidesForwardAdjustedDailyBars() {
			return nil, ErrNotSupported
		}
		bars, err := adapter.GetDailyBars(sourceContext, market, symbol, limit)
		if err == nil && len(bars) == 0 {
			err = ErrNoData
		}
		if err == nil {
			for index := range bars {
				if bars[index].Source == "" {
					bars[index].Source = adapter.Name()
				}
			}
		}
		return bars, err
	}
	ordered := make([]Adapter, 0, len(manager.adapters))
	cooled := make([]Adapter, 0, len(manager.adapters))
	for _, adapter := range manager.adapters {
		if manager.health.AvailableForMarket(adapter.Name(), "daily_bars", market) {
			ordered = append(ordered, adapter)
		} else {
			cooled = append(cooled, adapter)
		}
	}
	ordered = append(ordered, cooled...)
	var newest []Bar
	var lastError error
	for _, adapter := range ordered {
		if ctx.Err() != nil {
			break
		}
		bars, succeeded, err := attemptSource(manager, ctx, market, "daily_bars", adapter, call)
		if succeeded {
			latest := bars[len(bars)-1].TradeDate
			if (expectedDate == "" || latest >= expectedDate) && len(bars) >= limit {
				bars = manager.EnrichDailyTradingFacts(ctx, market, symbol, bars)
				if err := parentContext.Err(); err != nil {
					return nil, false, err
				}
				return bars, true, nil
			}
			if len(newest) == 0 || latest > newest[len(newest)-1].TradeDate ||
				(latest == newest[len(newest)-1].TradeDate && len(bars) > len(newest)) {
				newest = bars
			}
			continue
		}
		if errors.Is(err, ErrSymbolInvalid) {
			return nil, false, err
		}
		if !errors.Is(err, ErrNotSupported) {
			lastError = err
		}
	}
	if err := parentContext.Err(); err != nil {
		return nil, false, err
	}
	if len(newest) > 0 {
		newest = manager.EnrichDailyTradingFacts(ctx, market, symbol, newest)
		if err := parentContext.Err(); err != nil {
			return nil, false, err
		}
		return newest, expectedDate == "" || newest[len(newest)-1].TradeDate >= expectedDate, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if lastError == nil {
		lastError = ErrNoData
	}
	return nil, false, lastError
}
