package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sinaDailyRawFixture = `[
{"day":"2025-07-15","open":"13.70","high":"13.90","low":"13.50","close":"13.52","volume":"81426999"},
{"day":"2025-07-16","open":"13.54","high":"13.60","low":"13.26","close":"13.48","volume":"81426900"}
]`

const sinaDailyFactorFixture = `var sh600000qfq={"total":3,"data":[
{"d":"2026-07-16","f":"1.0000000000000000"},
{"d":"2025-07-16","f":"1.0472440944882000"},
{"d":"1900-01-01","f":"1.1"}]}; /* source metadata */`

var dailyFixtureObservedAt = time.Date(2026, 11, 4, 16, 0, 0, 0, ChinaMarketLocation())

func TestSinaDailyAdjustmentEffectiveDatesAndVolume(test *testing.T) {
	factors, err := parseSinaQFQFactors([]byte(`var sh600000qfq={"total":4,"data":[
		{"d":"2025-07-19","f":"1"},{"d":"2025-07-16","f":"2"},
		{"d":"1900-01-01","f":"4"},{"d":"2025-07-16","f":"2.0"}]} /* comment */;`), "sh600000")
	if err != nil || len(factors) != 3 {
		test.Fatalf("factors=%+v err=%v", factors, err)
	}
	var input []Bar
	for _, date := range []string{"1991-07-15", "2025-07-15", "2025-07-16", "2025-07-18", "2025-07-21"} {
		input = append(input, Bar{TradeDate: date, Open: 40, High: 42, Low: 38, Close: 41.02, Volume: 814269, Source: "sina_raw"})
	}
	original := append([]Bar(nil), input...)
	bars, err := applySinaForwardAdjustment(input, factors)
	if err != nil {
		test.Fatal(err)
	}
	for index, wantClose := range []float64{10.26, 10.26, 20.51, 20.51, 41.02} {
		if bars[index].Close != wantClose || bars[index].Volume != 814269 || bars[index].Source != "sina_qfq" ||
			bars[index].Amount != 0 || bars[index].TurnoverRate != 0 {
			test.Fatalf("ex-date/weekend/sentinel or units mismatch: %+v", bars[index])
		}
	}
	if !reflect.DeepEqual(input, original) {
		test.Fatal("adjustment mutated raw bars")
	}
}

func TestSinaDailyFactorsRejectUnsafeOrIncompleteResponse(test *testing.T) {
	valid := `var sh600000qfq={"total":1,"data":[{"d":"1900-01-01","f":"1"}]}`
	for name, body := range map[string]string{
		"wrong symbol":                strings.Replace(valid, "sh600000", "sz000001", 1),
		"longer variable":             strings.Replace(valid, "qfq=", "qfqevil=", 1),
		"assignment chain":            strings.Replace(valid, "qfq=", "qfq=other=", 1),
		"no var boundary":             strings.Replace(valid, "var ", "var", 1),
		"bare JSON":                   strings.TrimPrefix(valid, "var sh600000qfq="),
		"empty factors":               `var sh600000qfq={"total":0,"data":[]}`,
		"missing total":               `var sh600000qfq={"data":[{"d":"1900-01-01","f":"1"}]}`,
		"incomplete total":            strings.Replace(valid, `"total":1`, `"total":2`, 1),
		"null":                        `var sh600000qfq=null`,
		"zero factor":                 strings.Replace(valid, `"f":"1"`, `"f":"0"`, 1),
		"negative factor":             strings.Replace(valid, `"f":"1"`, `"f":"-1"`, 1),
		"NaN factor":                  strings.Replace(valid, `"f":"1"`, `"f":"NaN"`, 1),
		"infinite factor":             strings.Replace(valid, `"f":"1"`, `"f":"1e309"`, 1),
		"invalid date":                strings.Replace(valid, "1900-01-01", "2025-02-30", 1),
		"future effective date":       strings.Replace(valid, "1900-01-01", time.Now().In(ChinaMarketLocation()).AddDate(0, 0, 1).Format(time.DateOnly), 1),
		"noncanonical date":           strings.Replace(valid, "1900-01-01", "2025-1-01", 1),
		"conflicting duplicate":       `var sh600000qfq={"total":2,"data":[{"d":"1900-01-01","f":"1"},{"d":"1900-01-01","f":"2"}]}`,
		"trailing JS":                 valid + `; alert("no")`,
		"trailing JSON":               valid + `{}`,
		"trailing expression":         valid + ` + run()`,
		"comment then JS":             valid + `/* safe */;run()`,
		"line comment then JS":        valid + "// safe\nrun()",
		"unicode line ending then JS": valid + "// safe\u2028run()",
		"unterminated comment":        valid + "/* missing end",
	} {
		test.Run(name, func(test *testing.T) {
			if factors, err := parseSinaQFQFactors([]byte(body), "sh600000"); !errors.Is(err, ErrUpstream) || len(factors) != 0 {
				test.Fatalf("unsafe factors accepted: factors=%+v err=%v", factors, err)
			}
		})
	}
	for _, suffix := range []string{"", ";", " /* metadata */ ; // trailing comment", "; // note\n /* metadata */"} {
		if _, err := parseSinaQFQFactors([]byte(valid+suffix), "sh600000"); err != nil {
			test.Fatalf("safe suffix %q rejected: %v", suffix, err)
		}
	}
}

func TestSinaDailyCoverageGapAndInvalidAdjustedPricesFailClosed(test *testing.T) {
	bars := []Bar{{TradeDate: "2025-07-15", Open: 10, High: 11, Low: 9, Close: 10, Volume: 100}}
	for _, factors := range [][]sinaQFQFactor{
		nil,
		{{date: "2025-07-16", value: 1}},
		{{date: "1900-01-01", value: 1e300}},
		{{date: "1900-01-01", value: 1e-310}},
	} {
		if adjusted, err := applySinaForwardAdjustment(bars, factors); !errors.Is(err, ErrUpstream) || len(adjusted) != 0 {
			test.Fatalf("must not invent prices/factor=1: bars=%+v err=%v", adjusted, err)
		}
	}
}

func TestSinaDailyRequestsAreBoundedAndShareGovernance(test *testing.T) {
	for _, limit := range []int{0, -1, 1, 50000} {
		test.Run(strconv.Itoa(limit), func(test *testing.T) {
			replaceSourceLimiter(test, &sinaLimiter, 0)
			var calls atomic.Int32
			wantLimit := limit
			if wantLimit <= 0 {
				wantLimit = 120
			}
			wantLimit = min(wantLimit, SinaMaxDailyBars)
			withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if publicSourceLimiter(request.Host) != sinaLimiter || request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" {
					test.Errorf("request escaped Sina governance: %s", request.Host)
				}
				if request.Host == "money.finance.sina.com.cn" {
					query := request.URL.Query()
					if query.Get("datalen") != strconv.Itoa(wantLimit) || query.Get("symbol") != "sh600000" || query.Get("scale") != "240" || query.Get("ma") != "no" {
						test.Errorf("unexpected raw URL: %s", request.URL)
					}
					fmt.Fprint(writer, sinaDailyRawFixture)
					return
				}
				if request.Host != "finance.sina.com.cn" || request.URL.Path != "/realstock/company/sh600000/qfq.js" {
					test.Errorf("unexpected factor URL: %s%s", request.Host, request.URL)
				}
				fmt.Fprint(writer, sinaDailyFactorFixture)
			})
			bars, err := NewSinaAdapter().GetDailyBars(test.Context(), "cn", "600000", limit)
			if err != nil || len(bars) != min(2, wantLimit) || calls.Load() != 2 {
				test.Fatalf("bars=%+v calls=%d err=%v", bars, calls.Load(), err)
			}
			last := bars[len(bars)-1]
			if last.Close != 12.87 || last.Volume != 814269 || last.Source != "sina_qfq" {
				test.Fatalf("unexpected adjusted sample: %+v", last)
			}
		})
	}
}

func TestSinaDailyFactorFailureNeverFallsBackToRaw(test *testing.T) {
	replaceSourceLimiter(test, &sinaLimiter, 0)
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		if request.Host == "money.finance.sina.com.cn" {
			fmt.Fprint(writer, sinaDailyRawFixture)
			return
		}
		fmt.Fprint(writer, `var sh600000qfq={"total":0,"data":[]}`)
	})
	manager := NewManagerWithAdapters(NewSinaAdapter())
	if bars, err := manager.GetDailyBars(test.Context(), "cn", "600000", 2); !errors.Is(err, ErrUpstream) || len(bars) != 0 {
		test.Fatalf("raw fallback contaminated adjusted route: bars=%+v err=%v", bars, err)
	}
	if !NewSinaAdapter().ProvidesForwardAdjustedDailyBars() {
		test.Fatal("adjusted capability missing")
	}
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	if bars, err := NewSinaAdapter().GetDailyBars(ctx, "cn", "600000", 2); !errors.Is(err, context.Canceled) || len(bars) != 0 {
		test.Fatalf("cancellation lost: bars=%+v err=%v", bars, err)
	}
}

func TestSinaDailyRawRejectsMalformedFieldsAndShareConflicts(test *testing.T) {
	var rows []map[string]string
	if err := json.Unmarshal([]byte(sinaDailyRawFixture), &rows); err != nil {
		test.Fatal(err)
	}
	for _, mutation := range []struct{ field, value string }{
		{"day", "2025-02-30"}, {"day", "2099-01-01"}, {"close", "NaN"}, {"open", "0"},
		{"high", "1"}, {"low", "90"}, {"volume", "-1"}, {"volume", "1.5"}, {"volume", "9223372036854775808"},
	} {
		invalid := make(map[string]string)
		for key, value := range rows[0] {
			invalid[key] = value
		}
		invalid[mutation.field] = mutation.value
		body, _ := json.Marshal([]map[string]string{invalid})
		if bars, err := parseSinaRawDailyResponse(body, dailyFixtureObservedAt); !errors.Is(err, ErrUpstream) || len(bars) != 0 {
			test.Fatalf("invalid %s=%s accepted: %+v, %v", mutation.field, mutation.value, bars, err)
		}
	}
	rows[1] = map[string]string{}
	for key, value := range rows[0] {
		rows[1][key] = value
	}
	rows[1]["volume"] = "81426901"
	body, _ := json.Marshal(rows)
	if bars, err := parseSinaRawDailyResponse(body, dailyFixtureObservedAt); !errors.Is(err, ErrUpstream) || len(bars) != 0 {
		test.Fatalf("share conflict hidden by lot conversion: %+v %v", bars, err)
	}
}
