package service

import (
	"testing"
	"time"
)

func tm(hm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-17 "+hm, time.Local)
	return t
}

func TestCnMarketState(t *testing.T) {
	cases := []struct {
		hm      string
		trading bool
		want    string
	}{
		{"09:14", true, marketStatePreOpen},
		{"09:31", true, marketStateTrading},
		{"11:29", true, marketStateTrading},
		{"11:45", true, marketStateBreak},
		{"13:01", true, marketStateTrading},
		{"14:59", true, marketStateTrading},
		{"15:01", true, marketStatePostClose},
		{"10:00", false, marketStateClosed}, // 非交易日恒 closed
	}
	for _, c := range cases {
		if got := cnMarketState(tm(c.hm), c.trading); got != c.want {
			t.Errorf("cnMarketState(%s, trading=%v)=%s, want %s", c.hm, c.trading, got, c.want)
		}
	}
}

func TestExpectedQuoteDate(t *testing.T) {
	prev := "2026-07-16"
	if got := expectedQuoteDate(tm("09:20"), true, prev); got != prev {
		t.Errorf("交易日盘前应取上一开市日，got %s", got)
	}
	if got := expectedQuoteDate(tm("09:26"), true, prev); got != "2026-07-17" {
		t.Errorf("交易日出价后应取今天，got %s", got)
	}
	if got := expectedQuoteDate(tm("10:00"), false, prev); got != prev {
		t.Errorf("非交易日应取最近开市日，got %s", got)
	}
}

func TestQuoteFreshness(t *testing.T) {
	exp := "2026-07-17"
	// 交易时段：9 分钟内 fresh、11 分钟 stale。
	if fi := quoteFreshness(tm("10:51"), tm("11:00"), marketStateTrading, exp); fi.Status != freshStatusFresh {
		t.Errorf("交易时段 9 分钟应 fresh，got %s", fi.Status)
	}
	if fi := quoteFreshness(tm("10:49"), tm("11:00"), marketStateTrading, exp); fi.Status != freshStatusStale {
		t.Errorf("交易时段 11 分钟应 stale，got %s", fi.Status)
	}
	// 午休恢复宽容：13:05 收到 11:29 的午盘末价算 fresh。
	if fi := quoteFreshness(tm("11:29"), tm("13:05"), marketStateTrading, exp); fi.Status != freshStatusFresh {
		t.Errorf("午休恢复宽容应 fresh，got %s", fi.Status)
	}
	// 盘前宽容今天：竞价期数据时间为今天。
	if fi := quoteFreshness(tm("09:20"), tm("09:22"), marketStatePreOpen, "2026-07-16"); fi.Status != freshStatusFresh {
		t.Errorf("盘前竞价当日数据应 fresh，got %s", fi.Status)
	}
	// 收盘后：当日 fresh、昨日 stale。
	if fi := quoteFreshness(tm("15:00"), tm("16:00"), marketStatePostClose, exp); fi.Status != freshStatusFresh {
		t.Errorf("盘后当日收盘应 fresh，got %s", fi.Status)
	}
	stale := time.Date(2026, 7, 16, 15, 0, 0, 0, time.Local)
	if fi := quoteFreshness(stale, tm("16:00"), marketStatePostClose, exp); fi.Status != freshStatusStale {
		t.Errorf("盘后昨日数据应 stale，got %s", fi.Status)
	}
	// 休市（closed）：上一开市日收盘 fresh。
	closedNow := time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local) // 周六
	if fi := quoteFreshness(time.Date(2026, 7, 17, 15, 0, 0, 0, time.Local), closedNow, marketStateClosed, "2026-07-17"); fi.Status != freshStatusFresh {
		t.Errorf("休市日上一开市收盘应 fresh，got %s", fi.Status)
	}
	// 零值 stale。
	if fi := quoteFreshness(time.Time{}, tm("11:00"), marketStateTrading, exp); fi.Status != freshStatusStale {
		t.Errorf("零值应 stale，got %s", fi.Status)
	}
}
