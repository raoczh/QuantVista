package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The six-column layout and lot units were checked against the public endpoint
// on 2026-11-02. Values are synthetic; extra columns must not become amount.
const tencentDailyFixture = `{"code":0,"data":{"sh600000":{"qfqday":[
	["2026-11-02","10.0","10.2","10.5","9.8","1164223",999999,88],
	["2026-10-29","9.6","9.7","9.9","9.5","261955.000"],
	["2026-10-30",9.7,10,10.1,9.6,300000],
	["2026-10-30",9.7,10,10.1,9.6,300000]
],"day":[["2026-11-02","99","99","99","99","1"]]}}}`

// Every shared-client request is rewritten to this local server. No request can
// reach an upstream host, including retries or EastMoney's backup hostname.
func withFreeDataServer(test *testing.T, handler http.HandlerFunc) {
	test.Helper()
	server := httptest.NewServer(handler)
	test.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		test.Fatal(err)
	}
	localClient := server.Client()
	test.Cleanup(localClient.CloseIdleConnections)
	originalClient := httpClient
	httpClient = &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: reviewTransport(func(request *http.Request) (*http.Response, error) {
			localRequest := request.Clone(request.Context())
			localRequest.URL.Scheme = serverURL.Scheme
			localRequest.URL.Host = serverURL.Host
			localRequest.Host = request.URL.Host
			return localClient.Transport.RoundTrip(localRequest)
		}),
	}
	test.Cleanup(func() { httpClient = originalClient })
}

func TestTencentDailyFixturePreservesAdjustedPricesAndLots(test *testing.T) {
	bars, err := parseTencentDailyResponse([]byte(tencentDailyFixture), "sh600000", "qfqday")
	if err != nil {
		test.Fatal(err)
	}
	if len(bars) != 3 || bars[0].TradeDate != "2026-10-29" || bars[2].TradeDate != "2026-11-02" {
		test.Fatalf("unexpected sorted, deduplicated bars: %+v", bars)
	}
	if bars[0].Volume != 261955 || bars[2].Volume != 1164223 || bars[2].Close != 10.2 {
		test.Fatalf("adjustment or lot units changed: %+v", bars)
	}
	for _, bar := range bars {
		if bar.Source != "tencent" || bar.Amount != 0 || bar.TurnoverRate != 0 {
			test.Fatalf("source/missing fields fabricated: %+v", bar)
		}
	}
}

func TestTencentDailyRequestBoundedTailWindow(test *testing.T) {
	for _, testcase := range []struct {
		name       string
		limit      int
		wantLimit  int
		wantLength int
	}{
		{name: "tail", limit: 2, wantLimit: 2, wantLength: 2},
		{name: "default", limit: 0, wantLimit: 120, wantLength: 3},
		{name: "negative default", limit: -1, wantLimit: 120, wantLength: 3},
		{name: "bounded", limit: 1000000, wantLimit: TencentMaxDailyBars, wantLength: 3},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			var requests atomic.Int32
			withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				wantParameter := "sh600000,day,,," + strconv.Itoa(testcase.wantLimit) + ",qfq"
				if request.Host != "web.ifzq.gtimg.cn" || request.URL.Path != "/appstock/app/fqkline/get" || request.URL.Query().Get("param") != wantParameter {
					test.Errorf("unexpected daily URL: host=%s url=%s", request.Host, request.URL)
				}
				if request.Header.Get("Referer") != "https://gu.qq.com/" {
					test.Error("missing Tencent referer")
				}
				fmt.Fprint(writer, tencentDailyFixture)
			})
			bars, err := NewTencentAdapter().GetDailyBars(context.Background(), "cn", "600000", testcase.limit)
			if err != nil || len(bars) != testcase.wantLength || requests.Load() != 1 {
				test.Fatalf("bars=%+v requests=%d err=%v", bars, requests.Load(), err)
			}
			if bars[len(bars)-1].TradeDate != "2026-11-02" || (testcase.limit == 2 && bars[0].TradeDate != "2026-10-30") {
				test.Fatalf("must return the most recent ascending tail: %+v", bars)
			}
		})
	}
}

func TestTencentDailyRejectsMissingAdjustmentAndBadPayloads(test *testing.T) {
	for _, testcase := range []struct {
		name string
		body string
		want error
	}{
		{"only raw day", `{"code":0,"data":{"sh600000":{"day":[["2026-11-02","10","10","10","10","1"]]}}}`, ErrNoData},
		{"empty qfqday with raw day", `{"code":0,"data":{"sh600000":{"qfqday":[],"day":[["2026-11-02","10","10","10","10","1"]]}}}`, ErrNoData},
		{"wrong code", `{"code":0,"data":{"sz000001":{"qfqday":[["2026-11-02","10","10","10","10","1"]]}}}`, ErrNoData},
		{"no data", `{"code":0,"data":{}}`, ErrNoData},
		{"null series", `{"code":0,"data":{"sh600000":{"qfqday":null}}}`, ErrNoData},
		{"failed status", `{"code":-1,"data":{}}`, ErrUpstream},
		{"missing status", `{"data":{}}`, ErrUpstream},
		{"html", `<html>upstream unavailable</html>`, ErrUpstream},
		{"invalid rows", `{"code":0,"data":{"sh600000":{"qfqday":{}}}}`, ErrUpstream},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			bars, err := parseTencentDailyResponse([]byte(testcase.body), "sh600000", "qfqday")
			if !errors.Is(err, testcase.want) || len(bars) != 0 {
				test.Fatalf("bars=%+v err=%v, want %v", bars, err, testcase.want)
			}
		})
	}
}

func TestTencentDailyRejectsInvalidRows(test *testing.T) {
	validRow := []any{"2026-10-30", "10", "10.5", "11", "9", "100.000"}
	for _, testcase := range []struct {
		name       string
		fieldIndex int
		value      any
	}{
		{"invalid date", 0, "2026-02-30"},
		{"noncanonical date", 0, "2026-1-02"},
		{"numeric date", 0, 20261102},
		{"empty numeric", 1, ""},
		{"dash numeric", 1, "-"},
		{"nan", 1, "NaN"},
		{"infinity", 2, "+Inf"},
		{"overflow", 3, "1e309"},
		{"null", 4, nil},
		{"object", 2, map[string]any{}},
		{"boolean", 2, true},
		{"zero price", 1, "0"},
		{"negative adjusted price", 1, "-0.12"},
		{"high below close", 3, "10"},
		{"low above open", 4, "10.1"},
		{"negative volume", 5, "-1"},
		{"nonfinite volume", 5, "NaN"},
		{"overflow volume", 5, "9223372036854775808"},
		{"fractional lot count", 5, "1.25"},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			invalidRow := append([]any(nil), validRow...)
			invalidRow[testcase.fieldIndex] = testcase.value
			raw, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"sh600000": map[string]any{"qfqday": [][]any{validRow, invalidRow}}}})
			if err != nil {
				test.Fatal(err)
			}
			if bars, err := parseTencentDailyResponse(raw, "sh600000", "qfqday"); !errors.Is(err, ErrUpstream) || len(bars) != 0 {
				test.Fatalf("invalid rows cannot be silently dropped: bars=%+v err=%v", bars, err)
			}
		})
	}
	for _, rows := range []string{
		`[["2026-10-30","10","10","10","10"]]`,
		`[["2026-10-30","10","10","10","10","1"],["2026-10-30","11","11","11","11","1"]]`,
	} {
		raw := `{"code":0,"data":{"sh600000":{"qfqday":` + rows + `}}}`
		if _, err := parseTencentDailyResponse([]byte(raw), "sh600000", "qfqday"); !errors.Is(err, ErrUpstream) {
			test.Fatalf("short/conflicting rows must fail: %v", err)
		}
	}
}

func TestValidateDailyBarsChecksAllFieldsWithoutMutatingInput(test *testing.T) {
	valid := Bar{TradeDate: "2026-11-02", Open: 10, Close: 10.5, High: 11, Low: 9, Volume: 100, Source: "fixture"}
	previous := valid
	previous.TradeDate = "2026-10-30"
	input := []Bar{valid, previous, valid}
	original := append([]Bar(nil), input...)
	output, err := ValidateDailyBars(input)
	if err != nil || !reflect.DeepEqual(input, original) || !reflect.DeepEqual(output, []Bar{previous, valid}) {
		test.Fatalf("validator must preserve original and source: input=%+v output=%+v err=%v", input, output, err)
	}
	if _, err := ValidateDailyBars(nil); !errors.Is(err, ErrNoData) {
		test.Fatalf("empty series: %v", err)
	}
	for _, testcase := range []struct {
		name   string
		mutate func(*Bar)
	}{
		{"nan amount", func(bar *Bar) { bar.Amount = math.NaN() }},
		{"inf amount", func(bar *Bar) { bar.Amount = math.Inf(1) }},
		{"negative amount", func(bar *Bar) { bar.Amount = -1 }},
		{"negative turnover", func(bar *Bar) { bar.TurnoverRate = -1 }},
		{"nan turnover", func(bar *Bar) { bar.TurnoverRate = math.NaN() }},
		{"inf turnover", func(bar *Bar) { bar.TurnoverRate = math.Inf(-1) }},
		{"negative volume", func(bar *Bar) { bar.Volume = -1 }},
		{"nan price", func(bar *Bar) { bar.Open = math.NaN() }},
		{"infinite price", func(bar *Bar) { bar.Close = math.Inf(1) }},
		{"nonpositive qfq price", func(bar *Bar) { bar.Low = -2 }},
		{"invalid date", func(bar *Bar) { bar.TradeDate = "2026-13-01" }},
		{"conflicting amount", func(bar *Bar) { bar.Amount = 100 }},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			invalid := valid
			testcase.mutate(&invalid)
			if bars, err := ValidateDailyBars([]Bar{valid, invalid}); !errors.Is(err, ErrUpstream) || len(bars) != 0 {
				test.Fatalf("invalid series returned as success: bars=%+v err=%v", bars, err)
			}
		})
	}
}

func TestTencentDailyErrorsAndCancellation(test *testing.T) {
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(writer, "unavailable")
	})
	adapter := NewTencentAdapter()
	if _, err := adapter.GetDailyBars(context.Background(), "cn", "600000", 2); !errors.Is(err, ErrUpstream) {
		test.Fatalf("HTTP failure should remain upstream error: %v", err)
	}
	if _, err := adapter.GetDailyBars(context.Background(), "us", "600000", 2); !errors.Is(err, ErrNotSupported) {
		test.Fatalf("unsupported market: %v", err)
	}
	if _, err := adapter.GetDailyBars(context.Background(), "cn", "6&x=10", 2); !errors.Is(err, ErrSymbolInvalid) {
		test.Fatalf("invalid symbol: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.GetDailyBars(ctx, "cn", "600000", 2); !errors.Is(err, context.Canceled) {
		test.Fatalf("cancellation must retain identity: %v", err)
	}
}

func TestTencentDailyCapsReturnedExtraCurrentBar(test *testing.T) {
	rows := make([][]string, 0, TencentMaxDailyBars+1)
	firstDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for dayIndex := 0; dayIndex <= TencentMaxDailyBars; dayIndex++ {
		rows = append(rows, []string{firstDate.AddDate(0, 0, dayIndex).Format(time.DateOnly), "10", "10", "10", "10", "1"})
	}
	response, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"sh600000": map[string]any{"qfqday": rows}}})
	if err != nil {
		test.Fatal(err)
	}
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Query().Get("param"), ",640,") {
			test.Errorf("request exceeds documented cap: %s", request.URL)
		}
		writer.Write(response)
	})
	bars, err := NewTencentAdapter().GetDailyBars(context.Background(), "cn", "600000", 50000)
	if err != nil || len(bars) != TencentMaxDailyBars || bars[0].TradeDate != rows[1][0] || bars[len(bars)-1].TradeDate != rows[len(rows)-1][0] {
		test.Fatalf("must cap extra current bar to newest 640: count=%d err=%v", len(bars), err)
	}
}
