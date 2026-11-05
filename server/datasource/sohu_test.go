package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sohuDailyFactsFixture = `[{"status":0,"code":"cn_600000","hq":[
["2025-07-16","13.54","13.48","-0.04","-0.30%","13.26","13.60","814269","109214.18","0.27%"]],
"stat":["ignored summary, never a trade row"]}]`

func TestSohuDailyFactsRequestAndUnits(test *testing.T) {
	replaceSourceLimiter(test, &sohuLimiter, 0)
	var calls atomic.Int32
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		query := request.URL.Query()
		if request.Host != "q.stock.sohu.com" || request.URL.Path != "/hisHq" || query.Get("code") != "cn_600000" ||
			query.Get("start") != "20250714" || query.Get("end") != "20250718" || query.Get("order") != "A" ||
			query.Get("period") != "d" || query.Get("rt") != "json" || query.Get("stat") != "1" || len(query) != 7 {
			test.Errorf("unexpected Sohu URL: %s%s", request.Host, request.URL)
		}
		if request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" {
			test.Error("public facts must not require credentials")
		}
		fmt.Fprint(writer, sohuDailyFactsFixture)
	})
	adapter := NewSohuAdapter()
	facts, err := adapter.GetDailyTradingFacts(test.Context(), "cn", "600000", "2025-07-14", "2025-07-18")
	if err != nil || len(facts) != 1 || calls.Load() != 1 {
		test.Fatalf("facts=%+v calls=%d err=%v", facts, calls.Load(), err)
	}
	if facts[0] != (DailyTradingFact{Market: "cn", Symbol: "600000", TradeDate: "2025-07-16", Volume: 814269, Amount: 1092141800, TurnoverRate: 0.27, Source: "sohu"}) {
		test.Fatalf("wrong units/identity: %+v", facts[0])
	}
	if _, err := adapter.GetDailyBars(test.Context(), "cn", "600000", 5); !errors.Is(err, ErrNotSupported) {
		test.Fatal("raw Sohu prices must not become a daily fallback")
	}
	if _, err := adapter.GetQuote(test.Context(), "cn", "600000"); !errors.Is(err, ErrNotSupported) || calls.Load() != 1 {
		test.Fatal("unsupported capabilities made an HTTP call")
	}
}

func TestSohuDailyFactsRejectsBadEnvelopesAndRows(test *testing.T) {
	for name, body := range map[string]string{
		"failed status":        strings.Replace(sohuDailyFactsFixture, `"status":0`, `"status":1`, 1),
		"missing status":       strings.Replace(sohuDailyFactsFixture, `"status":0,`, "", 1),
		"wrong symbol":         strings.Replace(sohuDailyFactsFixture, "cn_600000", "cn_000001", 1),
		"object envelope":      `{"status":0,"code":"cn_600000","hq":[]}`,
		"extra envelope":       `[{"status":0,"code":"cn_600000"},{"status":0,"code":"cn_000001"}]`,
		"HTML":                 `<html>verification</html>`,
		"trailing JS":          sohuDailyFactsFixture + ";run()",
		"date outside range":   strings.Replace(sohuDailyFactsFixture, "2025-07-16", "2025-07-21", 1),
		"invalid date":         strings.Replace(sohuDailyFactsFixture, "2025-07-16", "2025-02-30", 1),
		"missing column":       strings.Replace(sohuDailyFactsFixture, `,"0.27%"`, "", 1),
		"NaN amount":           strings.Replace(sohuDailyFactsFixture, `"109214.18"`, `"NaN"`, 1),
		"unit overflow":        strings.Replace(sohuDailyFactsFixture, `"109214.18"`, `"1e308"`, 1),
		"negative amount":      strings.Replace(sohuDailyFactsFixture, `"109214.18"`, `"-10"`, 1),
		"inconsistent OHLC":    strings.Replace(sohuDailyFactsFixture, `"13.60"`, `"1"`, 1),
		"fractional lots":      strings.Replace(sohuDailyFactsFixture, `"814269"`, `"1.5"`, 1),
		"overflow lots":        strings.Replace(sohuDailyFactsFixture, `"814269"`, `"9223372036854775808"`, 1),
		"missing percent unit": strings.Replace(sohuDailyFactsFixture, "0.27%", "0.27", 1),
		"negative turnover":    strings.Replace(sohuDailyFactsFixture, "0.27%", "-0.27%", 1),
		"infinite turnover":    strings.Replace(sohuDailyFactsFixture, "0.27%", "+Inf%", 1),
	} {
		test.Run(name, func(test *testing.T) {
			if facts, err := parseSohuDailyFacts([]byte(body), "600000", "2025-07-14", "2025-07-18", dailyFixtureObservedAt); !errors.Is(err, ErrUpstream) || len(facts) != 0 {
				test.Fatalf("invalid response accepted: %+v %v", facts, err)
			}
		})
	}
	row := []string{"2025-07-16", "13.54", "13.48", "-0.04", "-0.30%", "13.26", "13.60", "814269", "109214.18", "0.27%"}
	for _, secondDate := range []string{"2025-07-16", "2025-07-15", "2025-07-19"} {
		second := append([]string(nil), row...)
		second[0] = secondDate
		body, _ := json.Marshal([]any{map[string]any{"status": 0, "code": "cn_600000", "hq": [][]string{row, second}}})
		if facts, err := parseSohuDailyFacts(body, "600000", "2025-07-14", "2025-07-20", dailyFixtureObservedAt); !errors.Is(err, ErrUpstream) || len(facts) != 0 {
			test.Fatalf("duplicate/unordered/weekend accepted: %+v %v", facts, err)
		}
	}
	for _, payload := range []string{`[]`, `[{"status":0,"code":"cn_600000","hq":[]}]`} {
		if _, err := parseSohuDailyFacts([]byte(payload), "600000", "2025-07-14", "2025-07-18", dailyFixtureObservedAt); !errors.Is(err, ErrNoData) {
			test.Fatalf("empty response classification: %v", err)
		}
	}
	missingTurnover := strings.Replace(sohuDailyFactsFixture, "0.27%", "-", 1)
	facts, err := parseSohuDailyFacts([]byte(missingTurnover), "600000", "2025-07-14", "2025-07-18", dailyFixtureObservedAt)
	if err != nil || facts[0].TurnoverRate != 0 || facts[0].Amount != 1092141800 {
		test.Fatalf("explicitly unavailable turnover must stay missing: %+v %v", facts, err)
	}
}

func TestSohuDailyFactsInvalidRequestsNeverReachNetwork(test *testing.T) {
	var calls atomic.Int32
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) { calls.Add(1) })
	for _, request := range []struct{ market, symbol, from, to string }{
		{"us", "600000", "2025-07-14", "2025-07-18"},
		{"cn", "600000&x", "2025-07-14", "2025-07-18"},
		{"cn", "510300", "2025-07-14", "2025-07-18"},
		{"cn", "600000", "2020-01-01", "2025-07-18"},
		{"cn", "600000", "2025-07-18", "2025-07-14"},
		{"cn", "600000", "2025-02-30", "2025-07-14"},
		{"cn", "600000", "2025-07-14", "2099-01-01"},
	} {
		if _, err := NewSohuAdapter().GetDailyTradingFacts(test.Context(), request.market, request.symbol, request.from, request.to); err == nil {
			test.Fatalf("invalid request succeeded: %+v", request)
		}
	}
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	if _, err := NewSohuAdapter().GetDailyTradingFacts(ctx, "cn", "600000", "2025-07-14", "2025-07-18"); !errors.Is(err, context.Canceled) {
		test.Fatalf("cancellation lost: %v", err)
	}
	if calls.Load() != 0 {
		test.Fatalf("invalid requests made %d HTTP calls", calls.Load())
	}
	from := time.Date(2000, 1, 1, 0, 0, 0, 0, ChinaMarketLocation())
	if _, _, err := validateDailyFactsWindow(from.Format(time.DateOnly), from.AddDate(0, 0, 399).Format(time.DateOnly), dailyFixtureObservedAt); err != nil {
		test.Fatalf("valid historical 400-day window rejected: %v", err)
	}
	if _, _, err := validateDailyFactsWindow(from.Format(time.DateOnly), from.AddDate(0, 0, 400).Format(time.DateOnly), dailyFixtureObservedAt); err == nil {
		test.Fatal("401-day request accepted")
	}
}

func TestSohuHTTPGovernanceAndResponseBoundaries(test *testing.T) {
	if publicSourceLimiter("q.stock.sohu.com") != sohuLimiter || cap(sohuLimiter.slots) != 1 || sohuLimiter.interval != 500*time.Millisecond ||
		publicSourceLimiter("q.stock.sohu.com.evil.example") != nil {
		test.Fatal("Sohu family governance missing or overly broad")
	}
	for _, scenario := range []string{"redirect", "oversize", "local wait"} {
		test.Run(scenario, func(test *testing.T) {
			limiter := replaceSourceLimiter(test, &sohuLimiter, 0)
			var calls atomic.Int32
			withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if scenario == "redirect" {
					http.Redirect(writer, request, "https://other.example/verify", http.StatusFound)
					return
				}
				fmt.Fprint(writer, strings.Repeat("x", maxHTTPResponseBytes+1))
			})
			budget := 2 * time.Second
			if scenario == "local wait" {
				budget = 80 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(test.Context(), budget)
			defer cancel()
			if scenario == "local wait" {
				limiter.deferUntil(time.Now().Add(time.Hour))
			}
			facts, err := NewSohuAdapter().GetDailyTradingFacts(ctx, "cn", "600000", "2025-07-14", "2025-07-18")
			if err == nil || len(facts) != 0 {
				test.Fatalf("unsafe response accepted: %+v %v", facts, err)
			}
			if scenario == "local wait" {
				var localWait *localSourceWaitError
				if !errors.As(err, &localWait) || !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
					test.Fatalf("local wait misclassified: calls=%d err=%v", calls.Load(), err)
				}
			} else if !errors.Is(err, ErrUpstream) || calls.Load() != 1 {
				test.Fatalf("redirect/body protection bypassed: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
