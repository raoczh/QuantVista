package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

type sinaTestTransport func(*http.Request) (*http.Response, error)

func (transport sinaTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

// All Sina adapter tests intercept every request in memory, including retries.
func withSinaTestHTTP(test *testing.T, handler func(*http.Request) (string, int, error)) {
	test.Helper()
	previousClient := httpClient
	httpClient = &http.Client{Transport: sinaTestTransport(func(request *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(request.Header.Get("Referer"), "https://finance.sina.com.cn") {
			test.Fatalf("missing Sina Referer: %v", request.Header)
		}
		if request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" {
			test.Fatal("public source requests must not carry credentials")
		}
		body, status, err := handler(request)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: status, Header: make(http.Header), Request: request,
			Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	test.Cleanup(func() { httpClient = previousClient })
}

func sinaSpotTestPage(startIndex, count int) string {
	entries := make([]string, 0, count)
	for entryIndex := startIndex; entryIndex < startIndex+count; entryIndex++ {
		code := fmt.Sprintf("600%03d", entryIndex)
		entries = append(entries, fmt.Sprintf(`{"code":%q,"symbol":%q,"name":"Page Name","trade":"9999","settlement":"9999","open":"9999","high":"9999","low":"9999","volume":999900,"amount":999900,"ticktime":"15:00:00","turnoverratio":1.25,"per":12.5,"pb":"1.1"}`, code, "sh"+code))
	}
	return "[" + strings.Join(entries, ",") + "]"
}

func sinaSpotTestQuote(quoteCode string, overrides map[int]string) string {
	fields := make([]string, 33)
	for fieldIndex := range fields {
		fields[fieldIndex] = "0"
	}
	fields[0], fields[1], fields[2], fields[3], fields[4], fields[5] = "Quote "+quoteCode, "10", "10", "10.5", "11", "9"
	fields[8], fields[9], fields[30], fields[31] = "12345", "129622.5", "2020-11-02", "15:00:00"
	for fieldIndex, value := range overrides {
		fields[fieldIndex] = value
	}
	return fmt.Sprintf("var hq_str_%s=%q;\n", quoteCode, strings.Join(fields, ","))
}

func TestSinaSpotSnapshotCompletePagingAndIdentity(test *testing.T) {
	countCalls, pageCalls, quoteCalls := 0, 0, 0
	withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
		query := request.URL.Query()
		switch {
		case strings.HasSuffix(request.URL.Path, "getHQNodeStockCount"):
			countCalls++
			if query.Get("node") != "hs_a" {
				test.Fatalf("wrong universe: %s", request.URL)
			}
			return `"101"`, http.StatusOK, nil
		case strings.HasSuffix(request.URL.Path, "getHQNodeData"):
			pageCalls++
			if query.Get("node") != "hs_a" || query.Get("sort") != "code" || query.Get("asc") != "1" || query.Get("num") != "100" {
				test.Fatalf("unstable paging parameters: %s", request.URL)
			}
			switch query.Get("page") {
			case "1":
				return sinaSpotTestPage(0, 100), http.StatusOK, nil
			case "2":
				return sinaSpotTestPage(100, 1), http.StatusOK, nil
			default:
				test.Fatalf("unexpected page: %s", request.URL)
			}
		case request.URL.Host == "hq.sinajs.cn":
			quoteCalls++
			if pageCalls != 2 {
				test.Fatal("raw enumeration must be complete before filtering or quoting")
			}
			quoteCodes := strings.Split(strings.TrimPrefix(request.URL.Path, "/list="), ",")
			if len(quoteCodes) > 100 || len(quoteCodes) == 0 {
				test.Fatalf("unsafe quote batch size: %d", len(quoteCodes))
			}
			var response strings.Builder
			// Reverse the response order: quotes must be joined by the explicit alias.
			for codeIndex := len(quoteCodes) - 1; codeIndex >= 0; codeIndex-- {
				response.WriteString(sinaSpotTestQuote(quoteCodes[codeIndex], nil))
			}
			return response.String(), http.StatusOK, nil
		}
		test.Fatalf("unexpected request: %s", request.URL)
		return "", 0, nil
	})
	rows, err := NewSinaAdapter().GetCNSpotSnapshot(context.Background())
	if err != nil || len(rows) != 101 || countCalls != 2 || pageCalls != 2 || quoteCalls != 2 {
		test.Fatalf("rows=%d count=%d pages=%d quotes=%d err=%v", len(rows), countCalls, pageCalls, quoteCalls, err)
	}
	expectedTime := time.Date(2020, 11, 2, 7, 0, 0, 0, time.UTC).Unix()
	for rowIndex, row := range rows {
		expectedCode := fmt.Sprintf("600%03d", rowIndex)
		if row.Symbol != expectedCode || row.Name != "Quote sh"+expectedCode || row.Source != "sina_spot" ||
			row.Price != 10.5 || row.PrevClose != 10 || row.Open != 10 || row.High != 11 || row.Low != 9 || row.ChangePct != 5 ||
			row.Volume != 123 || row.Amount != 129622.5 || row.DataTime != expectedTime ||
			row.TurnoverRate != 1.25 || row.PE != 12.5 || row.PB != 1.1 || row.PETTM != 0 || row.Industry != "" {
			test.Fatalf("wrong quote identity, timestamp or field semantics: %+v", row)
		}
	}
}

func TestSinaSpotSnapshotFiltersOnlyAfterRawCompleteness(test *testing.T) {
	const page = `[
	{"code":"000001","symbols":"sz000001","name":"Active"},
	{"code":"159915","symbol":"sz159915","name":"ETF"},
	{"code":"600000","symbol":"sh600000","name":"Suspended"},
	{"code":"830001","symbol":"bj830001","name":"Beijing"},
	{"code":"900901","symbol":"sh900901","name":"B-share"}]`
	withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, "getHQNodeStockCount"):
			return "5", http.StatusOK, nil
		case strings.HasSuffix(request.URL.Path, "getHQNodeData"):
			return page, http.StatusOK, nil
		case request.URL.Host == "hq.sinajs.cn":
			if request.URL.Path != "/list=sz000001,sh600000" {
				test.Fatalf("unsupported listings must not be mislabelled: %s", request.URL)
			}
			return sinaSpotTestQuote("sz000001", nil) + sinaSpotTestQuote("sh600000", map[int]string{
				1: "0", 3: "0", 4: "0", 5: "0", 8: "0", 9: "0", 30: "", 31: "",
			}), http.StatusOK, nil
		}
		test.Fatalf("unexpected request: %s", request.URL)
		return "", 0, nil
	})
	rows, err := NewSinaAdapter().GetCNSpotSnapshot(context.Background())
	if err != nil || len(rows) != 2 {
		test.Fatalf("raw five-listing count must not be compared with two supported rows: %+v %v", rows, err)
	}
	if rows[1].Symbol != "600000" || rows[1].Price != 0 || rows[1].DataTime != 0 || rows[1].PrevClose != 10 || rows[1].Volume != 0 {
		test.Fatalf("suspended dictionary row must stay zero-price/unknown-time: %+v", rows[1])
	}
}

func TestSinaSpotSnapshotRejectsIncompleteEnumeration(test *testing.T) {
	for _, testCase := range []struct {
		name       string
		count      string
		firstPage  string
		secondPage string
	}{
		{"ninety_five_percent", "100", sinaSpotTestPage(0, 95), "[]"},
		{"duplicate_page", "200", sinaSpotTestPage(0, 100), sinaSpotTestPage(0, 100)},
		{"overlap", "101", sinaSpotTestPage(0, 100), sinaSpotTestPage(99, 1)},
		{"empty_tail", "101", sinaSpotTestPage(0, 100), "[]"},
		{"oversized_page", "1", sinaSpotTestPage(0, 2), "[]"},
		{"over_page_limit", "8001", "[]", "[]"},
		{"invalid_count", `"100.5"`, "[]", "[]"},
		{"null_count", "null", "[]", "[]"},
		{"business_error", `{"__ERROR":3,"__ERRORMSG":"Service not found"}`, "[]", "[]"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
				if strings.HasSuffix(request.URL.Path, "getHQNodeStockCount") {
					return testCase.count, http.StatusOK, nil
				}
				if request.URL.Host == "hq.sinajs.cn" {
					test.Fatal("incomplete universe must fail before quotes")
				}
				if request.URL.Query().Get("page") == "1" {
					return testCase.firstPage, http.StatusOK, nil
				}
				return testCase.secondPage, http.StatusOK, nil
			})
			if rows, err := NewSinaAdapter().GetCNSpotSnapshot(context.Background()); !errors.Is(err, ErrUpstream) || rows != nil {
				test.Fatalf("partial or repeated pages must fail atomically: %+v %v", rows, err)
			}
		})
	}
}

func TestSinaSpotSnapshotCountChangeAndMissingBatchQuote(test *testing.T) {
	for _, missingQuote := range []bool{false, true} {
		test.Run(strconv.FormatBool(missingQuote), func(test *testing.T) {
			countCalls := 0
			withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
				switch {
				case strings.HasSuffix(request.URL.Path, "getHQNodeStockCount"):
					countCalls++
					if countCalls > 1 {
						return "3", http.StatusOK, nil
					}
					return "2", http.StatusOK, nil
				case strings.HasSuffix(request.URL.Path, "getHQNodeData"):
					return sinaSpotTestPage(0, 2), http.StatusOK, nil
				default:
					response := sinaSpotTestQuote("sh600000", nil)
					if !missingQuote {
						response += sinaSpotTestQuote("sh600001", nil)
					}
					return response, http.StatusOK, nil
				}
			})
			if rows, err := NewSinaAdapter().GetCNSpotSnapshot(context.Background()); !errors.Is(err, ErrUpstream) || rows != nil {
				test.Fatalf("count changes and missing quotes must fail the round: %+v %v", rows, err)
			}
		})
	}
}

func TestSinaSpotQuoteValidation(test *testing.T) {
	observedAt := time.Date(2026, 11, 2, 16, 0, 0, 0, sinaMarketLocation)
	entries := []sinaSpotEntry{{Code: "600000", Name: "Sample"}}
	validQuote := sinaSpotTestQuote("sh600000", nil)
	for _, fixture := range []string{
		"", `var hq_str_sh600000="";`, `var hq_str_sh600000="truncated,10";`,
		validQuote + validQuote, sinaSpotTestQuote("sz600000", nil),
		sinaSpotTestQuote("sh600000", map[int]string{30: "", 31: "15:00:00"}),
		sinaSpotTestQuote("sh600000", map[int]string{30: "2026-02-30"}),
		sinaSpotTestQuote("sh600000", map[int]string{30: "2099-11-02"}),
		sinaSpotTestQuote("sh600000", map[int]string{3: "NaN"}),
		sinaSpotTestQuote("sh600000", map[int]string{4: "9"}),
		sinaSpotTestQuote("sh600000", map[int]string{8: "1e40"}),
		sinaSpotTestQuote("sh600000", map[int]string{9: "Inf"}),
		sinaSpotTestQuote("sh600000", map[int]string{3: "0"}),
	} {
		if rows, err := parseSinaSpotQuotes([]byte(fixture), entries, observedAt); !errors.Is(err, ErrUpstream) || rows != nil {
			test.Errorf("invalid source quote must fail, not gain today's date: %+v %v", rows, err)
		}
	}
	gbkFixture, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(sinaSpotTestQuote("sh600000", map[int]string{0: "\u6d66\u53d1\u94f6\u884c"})))
	if err != nil {
		test.Fatal(err)
	}
	rows, err := parseSinaSpotQuotes(gbkFixture, entries, observedAt)
	if err != nil || len(rows) != 1 || rows[0].Name != "\u6d66\u53d1\u94f6\u884c" {
		test.Fatalf("GBK quote name must decode exactly: %+v %v", rows, err)
	}
	entries[0].PE = json.RawMessage(`"NaN"`)
	if _, err := parseSinaSpotQuotes([]byte(validQuote), entries, observedAt); !errors.Is(err, ErrUpstream) {
		test.Fatalf("nonfinite valuation must not become a real zero: %v", err)
	}
}

func TestSinaSpotPageAliasValidation(test *testing.T) {
	for _, fixture := range []string{
		`[{"code":"000001","name":"Sample","symbol":"sh000001"}]`,
		`[{"code":"600000","name":"Sample","symbol":"sh600001"}]`,
		`[{"code":"600000","name":"Sample","symbol":"sh600000","symbols":"sz600000"}]`,
		`[{"code":"600000","name":"Sample","symbol":"sh"}]`,
		`[{"code":"60000x","name":"Sample"}]`,
	} {
		if _, err := parseSinaSpotPage([]byte(fixture)); !errors.Is(err, ErrUpstream) {
			test.Errorf("invalid exchange/symbol alias must fail: %s %v", fixture, err)
		}
	}
}

func TestSinaSpotSuspendedLastPriceIsNotAnActiveBar(test *testing.T) {
	observedAt := time.Date(2026, 11, 2, 18, 0, 0, 0, ChinaMarketLocation())
	entries := []sinaSpotEntry{{Code: "601326", Name: "Suspended"}}
	fields := map[int]string{1: "0", 2: "3.57", 3: "3.57", 4: "0", 5: "0", 8: "0", 9: "0", 30: "2026-11-02", 31: "15:35:53", 32: "03"}
	rows, err := parseSinaSpotQuotes([]byte(sinaSpotTestQuote("sh601326", fields)), entries, observedAt)
	if err != nil || len(rows) != 1 || rows[0].Price != 3.57 || rows[0].Volume != 0 || rows[0].Open != 0 || rows[0].DataTime == 0 {
		test.Fatalf("suspended last price was rejected or converted into an active bar: %+v %v", rows, err)
	}
	fields[8] = "100"
	if _, err := parseSinaSpotQuotes([]byte(sinaSpotTestQuote("sh601326", fields)), entries, observedAt); !errors.Is(err, ErrUpstream) {
		test.Fatalf("trading volume without valid OHLC must still be rejected: %v", err)
	}
}

func TestSinaSpotSnapshotCancellationStopsPaging(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pageCalls, quoteCalls := 0, 0
	var cancellationTime time.Time
	withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
		if strings.HasSuffix(request.URL.Path, "getHQNodeStockCount") {
			return "101", http.StatusOK, nil
		}
		if request.URL.Host == "hq.sinajs.cn" {
			quoteCalls++
		}
		pageCalls++
		cancellationTime = time.Now().Add(20 * time.Millisecond)
		time.AfterFunc(20*time.Millisecond, cancel)
		return sinaSpotTestPage(0, 100), http.StatusOK, nil
	})
	rows, err := NewSinaAdapter().GetCNSpotSnapshot(ctx)
	if !errors.Is(err, context.Canceled) || rows != nil || pageCalls != 1 || quoteCalls != 0 {
		test.Fatalf("cancellation must stop the next page and quotes: pages=%d quotes=%d err=%v", pageCalls, quoteCalls, err)
	}
	if elapsed := time.Since(cancellationTime); elapsed > 150*time.Millisecond {
		test.Fatalf("canceled paging wait took too long: %v", elapsed)
	}
}
