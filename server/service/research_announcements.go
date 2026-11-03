package service

import (
	"context"
	"errors"

	"quantvista/common"
	"quantvista/datasource"
)

func (market *MarketService) researchAnnouncementBriefs(ctx context.Context, symbol string, limit int) ([]announcementBrief, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	market.financeOnce.Do(func() {
		market.finance = NewFinanceService()
		market.finance.announcementFeed = func(fetchContext context.Context, symbol string, limit int) ([]datasource.EMAnnouncement, error) {
			return market.mgr.GetAnnouncements(fetchContext, "cn", symbol, limit)
		}
	})
	refreshError := market.finance.refreshAnnouncements(ctx, symbol)
	rows, readError := latestAnnouncementBriefsContext(ctx, symbol, limit)
	return rows, errors.Join(refreshError, readError)
}
