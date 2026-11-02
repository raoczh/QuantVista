package datasource

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sinaMin5Fixture = `[
{"day":"2026-11-02 15:00:00","open":"10.20","high":"10.30","low":"10.10","close":"10.25","volume":"20199"},
{"day":"2026-11-02 14:50:00","open":"10.00","high":"10.20","low":"9.90","close":"10.10","volume":"10000"},
{"day":"2026-11-02 14:55:00","open":"10.10","high":"10.30","low":"10.00","close":"10.20","volume":"0"}
]`

func TestSinaMin5NormalizesRealBars(test *testing.T) {
	observedAt := time.Date(2026, 11, 2, 7, 0, 1, 0, time.UTC)
	bars, err := parseSinaMin5Response([]byte(sinaMin5Fixture), observedAt)
	if err != nil || len(bars) != 3 {
		test.Fatalf("bars=%v err=%v", bars, err)
	}
	if bars[0].Time != "202611021450" || bars[1].Time != "202611021455" || bars[2].Time != "202611021500" {
		test.Fatalf("expected chronological source timestamps: %+v", bars)
	}
	if bars[0].Open != 10 || bars[0].High != 10.2 || bars[0].Low != 9.9 || bars[0].Close != 10.1 ||
		bars[0].Volume != 100 || bars[1].Volume != 0 || bars[2].Volume != 201 || bars[0].Source != "sina" {
		test.Fatalf("OHLC/share-to-lot normalization failed: %+v", bars)
	}
	// A current five-minute bucket is excluded until its source end timestamp.
	bars, err = parseSinaMin5Response([]byte(sinaMin5Fixture), observedAt.Add(-2*time.Minute))
	if err != nil || len(bars) != 2 || bars[1].Time != "202611021455" {
		test.Fatalf("unfinished candle must not become historical data: %+v %v", bars, err)
	}
}

func TestSinaMin5RejectsBadRowsAndConflicts(test *testing.T) {
	observedAt := time.Date(2026, 11, 2, 16, 0, 0, 0, sinaMarketLocation)
	for _, testCase := range []struct {
		name string
		from string
		to   string
	}{
		{"nonfinite_price", `"open":"10.20"`, `"open":"NaN"`},
		{"infinite_high", `"high":"10.30"`, `"high":"+Inf"`},
		{"invalid_low", `"low":"10.10"`, `"low":"11.00"`},
		{"invalid_high", `"high":"10.30"`, `"high":"10.00"`},
		{"missing_close", `"close":"10.25"`, `"close":""`},
		{"zero_price", `"open":"10.20"`, `"open":"0"`},
		{"negative_volume", `"volume":"20199"`, `"volume":"-1"`},
		{"infinite_volume", `"volume":"20199"`, `"volume":"Inf"`},
		{"overflow_volume", `"volume":"20199"`, `"volume":"1e40"`},
		{"bad_date", "2026-11-02 15:00:00", "2026-02-30 15:00:00"},
		{"future_date", "2026-11-02 15:00:00", "2026-11-03 15:00:00"},
		{"one_minute_timestamp", "2026-11-02 15:00:00", "2026-11-02 14:59:00"},
		{"fractional_timestamp", "2026-11-02 15:00:00", "2026-11-02 15:00:00.1"},
		{"daily_timestamp", "2026-11-02 15:00:00", "2026-11-02"},
		{"lunch_break", "2026-11-02 15:00:00", "2026-11-02 12:00:00"},
		{"weekend", "2026-11-02 15:00:00", "2026-11-01 15:00:00"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := strings.Replace(sinaMin5Fixture, testCase.from, testCase.to, 1)
			if bars, err := parseSinaMin5Response([]byte(fixture), observedAt); !errors.Is(err, ErrUpstream) || bars != nil {
				test.Fatalf("invalid response must fail without partial history: %+v %v", bars, err)
			}
		})
	}
	const sample = `{"day":"2026-11-02 15:00:00","open":"10","high":"11","low":"9","close":"10","volume":"101"}`
	if bars, err := parseSinaMin5Response([]byte("["+sample+","+sample+"]"), observedAt); err != nil || len(bars) != 1 {
		test.Fatalf("identical duplicate should be collapsed: %+v %v", bars, err)
	}
	conflict := strings.Replace(sample, `"volume":"101"`, `"volume":"199"`, 1)
	if _, err := parseSinaMin5Response([]byte("["+sample+","+conflict+"]"), observedAt); !errors.Is(err, ErrUpstream) {
		test.Fatalf("share-volume conflict must be detected before lot rounding: %v", err)
	}
	for _, fixture := range []string{"[]", "null", "[" + sample + "]"} {
		if _, err := parseSinaMin5Response([]byte(fixture), observedAt.Add(-2*time.Hour)); !errors.Is(err, ErrNoData) {
			test.Fatalf("empty or unfinished-only history must be no-data: %v", err)
		}
	}
}

func TestSinaMin5RequestContract(test *testing.T) {
	for _, testCase := range []struct {
		count     int
		requested int
		returned  int
	}{
		{0, 60, 3}, {2, 2, 2}, {10000, sinaMin5MaxCount, 3},
	} {
		test.Run(strconv.Itoa(testCase.count), func(test *testing.T) {
			calls := 0
			withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
				calls++
				query := request.URL.Query()
				if request.URL.Host != "money.finance.sina.com.cn" || query.Get("scale") != "5" ||
					query.Get("ma") != "no" || query.Get("symbol") != "sh600000" || query.Get("datalen") != strconv.Itoa(testCase.requested) {
					test.Fatalf("wrong minute request: %s", request.URL)
				}
				return strings.ReplaceAll(sinaMin5Fixture, "2026-11-02", "2020-11-02"), http.StatusOK, nil
			})
			bars, err := NewSinaAdapter().GetMin5Bars(context.Background(), "cn", "600000", testCase.count)
			if err != nil || len(bars) != testCase.returned || calls != 1 || bars[len(bars)-1].Time != "202011021500" {
				test.Fatalf("request/limit mismatch: %+v calls=%d err=%v", bars, calls, err)
			}
		})
	}
	withSinaTestHTTP(test, func(*http.Request) (string, int, error) {
		test.Fatal("unsupported or canceled requests must not reach HTTP")
		return "", 0, nil
	})
	if _, err := NewSinaAdapter().GetMin5Bars(context.Background(), "us", "600000", 3); !errors.Is(err, ErrNotSupported) {
		test.Fatal(err)
	}
	if _, err := NewSinaAdapter().GetMin5Bars(context.Background(), "cn", "600000&scale=1", 3); !errors.Is(err, ErrSymbolInvalid) {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSinaAdapter().GetMin5Bars(ctx, "cn", "600000", 3); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
}
