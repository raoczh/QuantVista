package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func quoteReviewTime(test *testing.T, value string) time.Time {
	test.Helper()
	parsed, err := time.ParseInLocation(time.DateTime, value, datasource.ChinaMarketLocation())
	if err != nil {
		test.Fatal(err)
	}
	return parsed
}

func TestQuoteCalendarDoesNotPromoteSparseHistory(test *testing.T) {
	setupTestDB(test)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-06-01", IsOpen: true}).Error; err != nil {
		test.Fatal(err)
	}
	oldQuote := quoteReviewTime(test, "2026-06-01 15:00:00")
	for _, nowValue := range []string{"2026-07-05 10:00:00", "2026-07-06 08:00:00", "2026-07-06 10:00:00"} {
		now := quoteReviewTime(test, nowValue)
		freshness := newCNQuoteFreshnessJudge(common.DB, now)(oldQuote)
		if freshness.Status != freshStatusUnknown || freshness.ExpectedDate != "" {
			test.Fatalf("sparse MAX date became current at %s: %+v", nowValue, freshness)
		}
	}
	judge := newCNQuoteFreshnessJudge(common.DB, quoteReviewTime(test, "2026-07-06 10:00:00"))
	if freshness := judge(quoteReviewTime(test, "2026-05-29 15:00:00")); freshness.Status != freshStatusStale || freshness.ExpectedDate != "" {
		test.Fatalf("a known later open day must still prove an older quote stale: %+v", freshness)
	}
	// The scheduler's legacy estimate is deliberately unchanged.
	if got := prevOpenTradeDateDB(common.DB, "2026-07-06"); got != "2026-06-01" {
		test.Fatalf("quote fix unexpectedly changed scheduling fallback: %s", got)
	}
}

func TestQuoteCalendarKeepsConfirmedHolidayClose(test *testing.T) {
	setupTestDB(test)
	first := quoteReviewTime(test, "2026-02-13 00:00:00")
	last := quoteReviewTime(test, "2026-02-24 00:00:00")
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		date := day.Format(time.DateOnly)
		row := model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: date == "2026-02-13" || date == "2026-02-24"}
		if err := common.DB.Create(&row).Error; err != nil {
			test.Fatal(err)
		}
	}
	previousClose := quoteReviewTime(test, "2026-02-13 15:00:00")
	for _, nowValue := range []string{"2026-02-23 10:00:00", "2026-02-24 08:00:00"} {
		freshness := newCNQuoteFreshnessJudge(common.DB, quoteReviewTime(test, nowValue))(previousClose)
		if freshness.Status != freshStatusFresh || freshness.ExpectedDate != "2026-02-13" {
			test.Fatalf("complete holiday evidence rejected at %s: %+v", nowValue, freshness)
		}
	}
	if err := common.DB.Where("market = ? AND trade_date = ?", "cn", "2026-02-20").Delete(&model.TradingCalendar{}).Error; err != nil {
		test.Fatal(err)
	}
	if freshness := newCNQuoteFreshnessJudge(common.DB, quoteReviewTime(test, "2026-02-24 08:00:00"))(previousClose); freshness.Status != freshStatusUnknown {
		test.Fatalf("a missing intervening weekday was inferred closed: %+v", freshness)
	}
}

func TestQuoteCalendarTodayEvidenceDoesNotWaiveSessionGate(test *testing.T) {
	setupTestDB(test)
	cases := []struct {
		name, now, observed, status string
	}{
		{"live", "2026-07-06 10:00:00", "2026-07-06 09:58:00", freshStatusFresh},
		{"lunch_close", "2026-07-06 12:00:00", "2026-07-06 11:29:00", freshStatusFresh},
		{"lunch_stagnation", "2026-07-06 12:00:00", "2026-07-06 09:58:00", freshStatusStale},
		{"auction", "2026-07-06 09:20:00", "2026-07-06 09:19:00", freshStatusFresh},
		{"before_auction", "2026-07-06 09:20:00", "2026-07-06 00:00:00", freshStatusUnknown},
		{"future_clock", "2026-07-06 10:00:00", "2026-07-06 10:02:00", freshStatusStale},
		{"old_weekday", "2026-07-06 10:00:00", "2026-07-03 15:00:00", freshStatusUnknown},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			freshness := newCNQuoteFreshnessJudge(common.DB, quoteReviewTime(test, testCase.now))(quoteReviewTime(test, testCase.observed))
			if freshness.Status != testCase.status {
				test.Fatalf("want %s, got %+v", testCase.status, freshness)
			}
		})
	}
	var count int64
	if err := common.DB.Model(&model.TradingCalendar{}).Count(&count).Error; err != nil || count != 0 {
		test.Fatalf("quote evidence must not write guessed calendar facts: count=%d err=%v", count, err)
	}
	if err := common.DB.Create(&[]model.TradingCalendar{
		{Market: "cn", TradeDate: "2026-07-03", IsOpen: true},
		{Market: "cn", TradeDate: "2026-07-06", IsOpen: false},
	}).Error; err != nil {
		test.Fatal(err)
	}
	now := quoteReviewTime(test, "2026-07-06 10:00:00")
	if freshness := newCNQuoteFreshnessJudge(common.DB, now)(now); freshness.Status == freshStatusFresh {
		test.Fatalf("source clock overrode a confirmed holiday: %+v", freshness)
	}
}

type sourceQuoteReviewAdapter struct {
	refusalTestAdapter
	source   string
	observed time.Time
}

func (adapter sourceQuoteReviewAdapter) Name() string { return adapter.source }

func (adapter sourceQuoteReviewAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	return &datasource.Quote{Market: market, Symbol: symbol, Price: 11, High: 11, Low: 10, Source: adapter.source, DataTime: adapter.observed}, nil
}

func TestGetFreshQuoteAndBatchJudgeShareTrustedCalendar(test *testing.T) {
	setupTestDB(test)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-06-01", IsOpen: true}).Error; err != nil {
		test.Fatal(err)
	}
	oldSource := sourceQuoteReviewAdapter{source: "old-review", observed: quoteReviewTime(test, "2026-06-01 15:00:00")}
	market := NewMarketService(datasource.NewManagerWithAdapters(oldSource))
	market.quoteNow = func() time.Time { return quoteReviewTime(test, "2026-07-05 10:00:00") }
	quote, freshness, err := market.GetFreshQuote(test.Context(), "cn", "600001")
	if err != nil || quote == nil || freshness.Status != freshStatusUnknown {
		test.Fatalf("stale calendar quote was admitted: quote=%+v freshness=%+v err=%v", quote, freshness, err)
	}
	if actual := market.FreshnessJudge("cn")(quote.DataTime); actual != freshness {
		test.Fatalf("batch/display judge diverged from routed quote: %+v vs %+v", actual, freshness)
	}
	if view := market.FreshnessView("cn", quote); view.StaleReason == "" || view.FreshnessStatus != freshStatusUnknown {
		test.Fatalf("unknown calendar missing an actionable limitation: %+v", view)
	}
	currentSource := sourceQuoteReviewAdapter{source: "current-review", observed: quoteReviewTime(test, "2026-07-06 09:58:00")}
	market = NewMarketService(datasource.NewManagerWithAdapters(oldSource, currentSource))
	market.quoteNow = func() time.Time { return quoteReviewTime(test, "2026-07-06 10:00:00") }
	quote, freshness, err = market.GetFreshQuote(test.Context(), "cn", "600001")
	if err != nil || quote == nil || quote.Source != currentSource.source || freshness.Status != freshStatusFresh {
		test.Fatalf("missing calendar blocked genuine today's fallback quote: quote=%+v freshness=%+v err=%v", quote, freshness, err)
	}
}
