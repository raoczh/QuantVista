package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const tencentBenchmarkFixture = `{"code":0,"data":{"sh000001":{
	"day":[
		["2026-11-02","3614.31","3638.03","3648.13","3594.77","565372836"],
		["2026-10-29","3676.77","3651.43","3678.46","3645.26","443564144.000"],
		["2026-10-30","3605.65","3599.02","3660.21","3595.26","497973940.000"]
	],
	"qfqday":[["2026-11-02","10","10","10","10","1"]]
},"sz000001":{"day":[["2026-11-02","11","11","11","11","1"]]}}}`

func tencentQuoteTestRecord(test *testing.T, exchangeCode, name, price string) string {
	test.Helper()
	fields, err := parseTencentFields(tencentFixture)
	if err != nil {
		test.Fatal(err)
	}
	fields[1] = name
	fields[2] = exchangeCode[2:]
	fields[3] = price
	return "v_" + exchangeCode + "=\"" + strings.Join(fields, "~") + "\";"
}

func tencentGBKFixture(test *testing.T, text string) []byte {
	test.Helper()
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(text))
	if err != nil {
		test.Fatal(err)
	}
	return encoded
}

func TestTencentIndicesUseExchangeQualifiedIdentity(test *testing.T) {
	// Missing index, shuffled rows, an unsolicited SZ stock with the same numeric
	// code, and a nonfinite index must not shift any other index's identity.
	body := strings.Join([]string{
		tencentQuoteTestRecord(test, "sz399006", "ChiNext", "2300"),
		tencentQuoteTestRecord(test, "sz000001", "Not the index", "11.1"),
		`v_sz399001="";`,
		tencentQuoteTestRecord(test, "sh000688", "Invalid", "NaN"),
		tencentQuoteTestRecord(test, "sh000001", "Shanghai", "3638.03"),
	}, "\n")
	encoded := tencentGBKFixture(test, body)
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		codes := make([]string, 0, len(CNIndices))
		for _, index := range CNIndices {
			codes = append(codes, index.Sina)
		}
		if request.Host != "qt.gtimg.cn" || request.URL.Path != "/q="+strings.Join(codes, ",") {
			test.Errorf("must request explicit CN index codes: %s %s", request.Host, request.URL)
		}
		writer.Write(encoded)
	})
	indices, err := NewTencentAdapter().GetIndices(context.Background(), "cn")
	if err != nil {
		test.Fatal(err)
	}
	if len(indices) != 2 || indices[0].Code != "000001" || indices[0].Price != 3638.03 || indices[1].Code != "399006" || indices[1].Price != 2300 {
		test.Fatalf("index identity/order corrupted: %+v", indices)
	}
	for _, index := range indices {
		_, offset := index.DataTime.Zone()
		if index.Source != "tencent" || index.DataTime.IsZero() || offset != 8*60*60 {
			test.Fatalf("source/timezone lost: %+v", index)
		}
	}
}

func TestTencentIndicesRejectIdentityConflicts(test *testing.T) {
	for _, testcase := range []struct {
		name string
		body string
		want error
	}{
		{"empty", `v_sh000001="";`, ErrNoData},
		{"wrong exchange only", tencentQuoteTestRecord(test, "sz000001", "Stock", "11"), ErrNoData},
		{"conflict", tencentQuoteTestRecord(test, "sh000001", "Index", "3600") + tencentQuoteTestRecord(test, "sh000001", "Index", "3700"), ErrUpstream},
		{"wrong embedded identity", strings.Replace(tencentQuoteTestRecord(test, "sh000001", "Index", "3600"), "~000001~", "~399001~", 1), ErrUpstream},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			encoded := tencentGBKFixture(test, testcase.body)
			withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) { writer.Write(encoded) })
			if indices, err := NewTencentAdapter().GetIndices(context.Background(), "cn"); !errors.Is(err, testcase.want) || len(indices) != 0 {
				test.Fatalf("indices=%+v err=%v, want %v", indices, err, testcase.want)
			}
		})
	}
}

func TestTencentBenchmarkAndTradingDaysUseRawIndexOnly(test *testing.T) {
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		parameter := strings.Split(request.URL.Query().Get("param"), ",")
		if len(parameter) != 6 || parameter[0] != "sh000001" || parameter[1] != "day" || parameter[5] != "" {
			test.Errorf("benchmark must not use stock mapping/adjustment: %s", request.URL)
		}
		fmt.Fprint(writer, tencentBenchmarkFixture)
	})
	adapter := NewTencentAdapter()
	name, bars, err := adapter.GetBenchmarkBars(context.Background(), "cn", 2)
	if err != nil || name != CNIndices[0].Name || len(bars) != 2 || bars[1].Close != 3638.03 || bars[0].TradeDate != "2026-10-30" {
		test.Fatalf("unexpected benchmark: name=%s bars=%+v err=%v", name, bars, err)
	}
	for _, bar := range bars {
		if bar.Source != "tencent" || bar.Amount != 0 || bar.TurnoverRate != 0 {
			test.Fatalf("benchmark facts must remain unmodified: %+v", bar)
		}
	}
	days, err := adapter.GetTradingDays(context.Background(), "cn", 10000)
	if err != nil || !reflect.DeepEqual(days, []string{"2026-10-29", "2026-10-30", "2026-11-02"}) {
		test.Fatalf("a short calendar must retain its real range: days=%v err=%v", days, err)
	}
}

func TestTencentCalendarNeverUsesAdjustedOrOtherSymbolData(test *testing.T) {
	for _, testcase := range []struct {
		name string
		body string
	}{
		{"adjusted only", `{"code":0,"data":{"sh000001":{"qfqday":[["2026-11-02","10","10","10","10","1"]]}}}`},
		{"Shenzhen stock only", `{"code":0,"data":{"sz000001":{"day":[["2026-11-02","10","10","10","10","1"]]}}}`},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, testcase.body) })
			adapter := NewTencentAdapter()
			if _, _, err := adapter.GetBenchmarkBars(context.Background(), "cn", 2); !errors.Is(err, ErrNoData) {
				test.Fatalf("unrelated data must not be used as benchmark: %v", err)
			}
			if _, err := adapter.GetTradingDays(context.Background(), "cn", 2); !errors.Is(err, ErrNoData) {
				test.Fatalf("unrelated data must not become trading dates: %v", err)
			}
		})
	}
}

func TestTencentDataTimeAlwaysUsesShanghai(test *testing.T) {
	originalLocation := time.Local
	time.Local = time.UTC
	test.Cleanup(func() { time.Local = originalLocation })
	fields := make([]string, 31)
	fields[30] = "20261102150403"
	timestamp := tencentDataTime(fields)
	want := time.Date(2026, 11, 2, 7, 4, 3, 0, time.UTC)
	if !timestamp.Equal(want) || timestamp.Location().String() != "Asia/Shanghai" {
		test.Fatalf("host timezone must not shift quote time: %s", timestamp)
	}
	for _, malformed := range []string{"", "unknown", "20260230150403", "202611021504030", "20261102150403.123"} {
		fields[30] = malformed
		if timestamp := tencentDataTime(fields); !timestamp.IsZero() {
			test.Fatalf("malformed timestamp must remain unknown: %q -> %s", malformed, timestamp)
		}
	}
}

func TestTencentQuoteAndValuationNeverReturnNonfiniteNumbers(test *testing.T) {
	fields, err := parseTencentFields(tencentFixture)
	if err != nil {
		test.Fatal(err)
	}
	for _, fieldIndex := range []int{4, 5, 6, 32, 33, 34, 38, 39, 43, 46, 47, 48, 49, 52, 53} {
		fields[fieldIndex] = "NaN"
	}
	fields[35] = "13.1/10/+Inf"
	fields[44] = "1e308" // finite before the yuan scaling, infinite afterwards
	fields[45] = "+Inf"
	encoded := tencentGBKFixture(test, `v_sh600000="`+strings.Join(fields, "~")+`";`)
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) { writer.Write(encoded) })
	quote, err := NewTencentAdapter().GetQuote(context.Background(), "cn", "600000")
	if err != nil {
		test.Fatal(err)
	}
	valuation, err := NewTencentAdapter().GetValuation(context.Background(), "cn", "600000")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := json.Marshal(quote); err != nil {
		test.Fatalf("quote contains nonfinite fields: %v", err)
	}
	if _, err := json.Marshal(valuation); err != nil {
		test.Fatalf("valuation contains nonfinite fields: %v", err)
	}
	if quote.Amount != 0 || quote.Volume != 0 || valuation.TotalCap != 0 || valuation.FloatCap != 0 {
		test.Fatalf("invalid optional fields must remain missing: quote=%+v valuation=%+v", quote, valuation)
	}
	for _, invalidPrice := range []string{"NaN", "+Inf", "-Inf", "1e309", "-1"} {
		fields[3] = invalidPrice
		if _, err := parseTencentValuation(fields, "cn", "600000", "tencent"); !errors.Is(err, ErrNoData) {
			test.Fatalf("invalid price must not validate a snapshot: %q %v", invalidPrice, err)
		}
	}
	if value := tencentAtof(fields, -1); value != 0 {
		test.Fatalf("negative field index: %v", value)
	}
}
