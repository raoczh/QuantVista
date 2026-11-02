package datasource

import (
	"context"
	"sync"
)

func (provider *EastMoneyAdapter) ProvidesForwardAdjustedDailyBars() bool { return true }
func (provider *TencentAdapter) ProvidesForwardAdjustedDailyBars() bool   { return true }
func (provider *SinaAdapter) ProvidesForwardAdjustedDailyBars() bool      { return false }

func (manager *Manager) GetCNSpotSnapshot(ctx context.Context) ([]SpotRow, error) {
	return routeCap(manager, ctx, "cn", "spot_snapshot", func(ctx context.Context, adapter Adapter) ([]SpotRow, error) {
		provider, supported := adapter.(SpotSnapshotProvider)
		if !supported {
			return nil, ErrNotSupported
		}
		rows, err := provider.GetCNSpotSnapshot(ctx)
		if err == nil && len(rows) == 0 {
			err = ErrNoData
		}
		return rows, err
	})
}

func (manager *Manager) GetMin5Bars(ctx context.Context, market, symbol string, count int) ([]Min5Bar, error) {
	return routeCap(manager, ctx, market, "min5", func(ctx context.Context, adapter Adapter) ([]Min5Bar, error) {
		provider, supported := adapter.(Min5Provider)
		if !supported {
			return nil, ErrNotSupported
		}
		bars, err := provider.GetMin5Bars(ctx, market, symbol, count)
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
	})
}

type minuteResult struct {
	bars          []Min5Bar
	previousClose float64
}

func (manager *Manager) GetMin1Bars(ctx context.Context, market, symbol string, count int) ([]Min5Bar, float64, error) {
	result, err := routeCap(manager, ctx, market, "min1", func(ctx context.Context, adapter Adapter) (minuteResult, error) {
		provider, supported := adapter.(Min1Provider)
		if !supported {
			return minuteResult{}, ErrNotSupported
		}
		bars, previousClose, err := provider.GetMin1Bars(ctx, market, symbol, count)
		if err == nil && len(bars) == 0 {
			err = ErrNoData
		}
		return minuteResult{bars: bars, previousClose: previousClose}, err
	})
	return result.bars, result.previousClose, err
}

func (provider *EastMoneyAdapter) GetAnnouncements(ctx context.Context, market, symbol string, limit int) ([]EMAnnouncement, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	return GetEMAnnouncements(ctx, symbol, limit)
}

func (manager *Manager) GetAnnouncements(ctx context.Context, market, symbol string, limit int) ([]EMAnnouncement, error) {
	return routeCap(manager, ctx, market, "announcements", func(ctx context.Context, adapter Adapter) ([]EMAnnouncement, error) {
		provider, supported := adapter.(AnnouncementProvider)
		if !supported {
			return nil, ErrNotSupported
		}
		announcements, err := provider.GetAnnouncements(ctx, market, symbol, limit)
		if err == nil && len(announcements) == 0 {
			err = ErrNoData
		}
		return announcements, err
	})
}

var sharedAnnouncementManager = sync.OnceValue(DefaultManager)

// GetAnnouncements preserves the existing injectable feed signature in FinanceService.
func GetAnnouncements(ctx context.Context, symbol string, limit int) ([]EMAnnouncement, error) {
	return sharedAnnouncementManager().GetAnnouncements(ctx, "cn", symbol, limit)
}
