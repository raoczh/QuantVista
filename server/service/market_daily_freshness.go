package service

import (
	"hash/fnv"
	"sync/atomic"
	"time"

	"quantvista/datasource"
)

type dailyBarReadPolicy struct {
	expectedDate string
	cacheScope   string
	cacheTTL     time.Duration
}

// API maintenance and background tracking use different MarketService objects.
// Bounded revision stripes invalidate both instances without allocating an
// unbounded symbol map or expiring every stock after each maintenance write.
var dailyBarCacheRevisions [1024]atomic.Uint64

func dailyBarRevision(market, symbol string) *atomic.Uint64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(QuoteKey(market, symbol)))
	return &dailyBarCacheRevisions[hasher.Sum32()%uint32(len(dailyBarCacheRevisions))]
}

func dailyBarPolicyAt(market string, now time.Time, tradingDay bool, previousOpen string) dailyBarReadPolicy {
	policy := dailyBarReadPolicy{cacheScope: "default", cacheTTL: dailyBarsCacheTTL}
	if market != "cn" {
		return policy
	}
	now = now.In(datasource.ChinaMarketLocation())
	minute := now.Hour()*60 + now.Minute()
	phase := "closed"
	policy.expectedDate = previousOpen
	if tradingDay {
		switch {
		case minute < sessionPreAuctionMin:
			phase = "pre_open"
		case minute < sessionCloseMin:
			phase, policy.cacheTTL = "forming", time.Minute
		default:
			phase, policy.expectedDate = "post_close", now.Format(time.DateOnly)
			if minute < 16*60+30 {
				phase, policy.cacheTTL = "closing_update", time.Minute
			}
		}
	}
	// A cached forming candle must not become a completed close just because the
	// wall clock crossed 15:00. A new trading date also gets a separate cache key.
	policy.cacheScope = now.Format(time.DateOnly) + ":" + phase
	return policy
}

func currentDailyBarPolicy(market string, now time.Time) dailyBarReadPolicy {
	if market != "cn" {
		return dailyBarPolicyAt(market, now, false, "")
	}
	now = now.In(datasource.ChinaMarketLocation())
	return dailyBarPolicyAt(market, now, isTradingDayToday(now), prevOpenTradeDate(now.Format(time.DateOnly)))
}

func (service *MarketService) invalidateDailyBars(market, symbol string) {
	dailyBarRevision(market, symbol).Add(1)
	service.responseCache().ForgetPrefix("bars:qfq-v2:" + market + ":" + symbol + ":")
}
