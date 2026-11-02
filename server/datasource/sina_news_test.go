package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Synthetic content using the verified result.status/data.feed.list schema.
const sinaNewsFixture = `{"result":{"status":{"code":0,"msg":"OK"},"timestamp":"collection time must not be used","data":{"feed":{"list":[
{"id":5187125,"rich_text":"<b>\u3010Source headline\u3011</b><p>Source body &amp; facts.</p><script>untrusted script</script><style>hidden style</style><p>Second paragraph.</p>","create_time":"2026-11-02 16:53:40","update_time":"2099-01-01 00:00:00","docurl":"https://finance.sina.cn/7x24/2026-11-02/detail-example.d.html","ext":"{\"stocks\":[{\"market\":\"cn\",\"symbol\":\"sh600000\"}]}"},
{"id":9007199254740993,"rich_text":"No separate title in this source item.","create_time":"2026-11-02 16:52:40","docurl":""}
]}}}}`

func TestSinaFastNewsPreservesProvenance(test *testing.T) {
	items, err := parseSinaFastNews([]byte(sinaNewsFixture))
	if err != nil || len(items) != 2 {
		test.Fatalf("items=%+v err=%v", items, err)
	}
	first := items[0]
	expectedTime := time.Date(2026, 11, 2, 8, 53, 40, 0, time.UTC)
	if first.SourceID != "5187125" || first.Title != "Source headline" || !first.PublishTime.Equal(expectedTime) ||
		first.Summary != "\u3010Source headline\u3011 Source body & facts. Second paragraph." ||
		first.URL != "https://finance.sina.cn/7x24/2026-11-02/detail-example.d.html" || len(first.Symbols) != 0 {
		test.Fatalf("source ID, title, HTML cleanup or publication time changed: %+v", first)
	}
	if items[1].SourceID != "9007199254740993" || items[1].Title != "No separate title in this source item." || items[1].URL != "" || len(items[1].Symbols) != 0 {
		test.Fatalf("numeric IDs must not lose precision; absent title/link must not be fabricated: %+v", items[1])
	}
}

func TestSinaFastNewsRejectsBadTimesAndSourceFailures(test *testing.T) {
	for _, invalidTime := range []string{"", "15:00:00", "2026-02-30 16:53:40", "2026-11-02 16:53:40.123"} {
		fixture := strings.Replace(sinaNewsFixture, "2026-11-02 16:53:40", invalidTime, 1)
		items, err := parseSinaFastNews([]byte(fixture))
		if err != nil || len(items) != 1 || items[0].SourceID != "9007199254740993" {
			test.Errorf("invalid publication time must be discarded, never replaced by now: %+v %v", items, err)
		}
	}
	for _, fixture := range []string{
		`{"result":{"status":{"code":1},"data":{"feed":{"list":[]}}}}`,
		`{"result":{"status":{},"data":{"feed":{"list":[]}}}}`,
		`<html>not JSON</html>`,
	} {
		if _, err := parseSinaFastNews([]byte(fixture)); !errors.Is(err, ErrUpstream) {
			test.Errorf("failed upstream must not become a successful empty page: %v", err)
		}
	}
	for _, fixture := range []string{
		`{"result":{"status":{"code":0},"data":{"feed":{"list":[]}}}}`,
		`{"result":{"status":{"code":0},"data":{"feed":{"list":[{"id":1,"rich_text":"bad time","create_time":""}]}}}}`,
		`{"result":{"status":{"code":0},"data":{"feed":{"list":[{"id":0,"rich_text":"missing ID","create_time":"2026-11-02 16:53:40"}]}}}}`,
		`{"result":{"status":{"code":0},"data":{"feed":{"list":[{"id":1,"rich_text":"deleted","create_time":"2026-11-02 16:53:40","is_delete":1}]}}}}`,
	} {
		if _, err := parseSinaFastNews([]byte(fixture)); !errors.Is(err, ErrNoData) {
			test.Errorf("no valid source items must signal no-data: %v", err)
		}
	}
}

func TestSinaFastNewsDuplicateIDs(test *testing.T) {
	const entry = `{"id":5187125,"rich_text":"Original title","create_time":"2026-11-02 16:53:40"}`
	wrapEntries := func(entries string) []byte {
		return []byte(`{"result":{"status":{"code":0},"data":{"feed":{"list":[` + entries + `]}}}}`)
	}
	if items, err := parseSinaFastNews(wrapEntries(entry + "," + entry)); err != nil || len(items) != 1 {
		test.Fatalf("identical source IDs should be collapsed: %+v %v", items, err)
	}
	conflict := strings.Replace(entry, "Original title", "Different title", 1)
	if _, err := parseSinaFastNews(wrapEntries(entry + "," + conflict)); !errors.Is(err, ErrUpstream) {
		test.Fatalf("conflicting IDs must fail: %v", err)
	}
}

func TestSinaFastNewsTextBoundsAndUnsafeURL(test *testing.T) {
	content := strings.Repeat("\u65b0", 1500)
	encodedContent, err := json.Marshal(content)
	if err != nil {
		test.Fatal(err)
	}
	fixture := `{"result":{"status":{"code":0},"data":{"feed":{"list":[{"id":"123","rich_text":` +
		string(encodedContent) + `,"create_time":"2026-11-02 15:00:00","docurl":"javascript:alert(1)"}]}}}}`
	items, err := parseSinaFastNews([]byte(fixture))
	if err != nil || len(items) != 1 {
		test.Fatalf("string source ID and long source text should be supported: %+v %v", items, err)
	}
	if items[0].SourceID != "123" || items[0].Title != strings.Repeat("\u65b0", 512) ||
		items[0].Summary != strings.Repeat("\u65b0", 1024) || items[0].URL != "" {
		test.Fatalf("storage limits must preserve source prefixes and reject unsafe URLs: %+v", items[0])
	}
	if cleaned := stripSinaNewsHTML(`<p>A <span>B</span> &amp; C.</p><iframe>hidden</iframe><noscript>hidden</noscript><br>D<!-- hidden -->`); cleaned != "A B & C. D" {
		test.Fatalf("HTML and hidden content must not enter plain-text news: %q", cleaned)
	}
}

func TestSinaFastNewsHTTPContract(test *testing.T) {
	for _, pageSize := range []int{0, 1, 100} {
		test.Run(strconv.Itoa(pageSize), func(test *testing.T) {
			withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
				expectedSize := "20"
				if pageSize == 1 {
					expectedSize = "1"
				}
				query := request.URL.Query()
				if request.URL.Host != "zhibo.sina.com.cn" || query.Get("page") != "1" || query.Get("page_size") != expectedSize ||
					query.Get("zhibo_id") != "152" || query.Get("tag_id") != "0" || query.Get("dire") != "f" {
					test.Fatalf("unexpected public news endpoint: %s", request.URL)
				}
				return sinaNewsFixture, http.StatusOK, nil
			})
			items, err := GetSinaFastNews(context.Background(), pageSize)
			if err != nil || len(items) == 0 || (pageSize == 1 && len(items) != 1) {
				test.Fatalf("page size was not respected: %+v %v", items, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withSinaTestHTTP(test, func(*http.Request) (string, int, error) {
		test.Fatal("canceled news request must not reach HTTP")
		return "", 0, nil
	})
	if _, err := GetSinaFastNews(ctx, 20); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
}

func TestSinaIndependentEndpointsRejectHTTPFailures(test *testing.T) {
	withSinaTestHTTP(test, func(*http.Request) (string, int, error) {
		return "not successful", http.StatusTeapot, nil
	})
	if _, err := NewSinaAdapter().GetMin5Bars(context.Background(), "cn", "600000", 3); !errors.Is(err, ErrUpstream) {
		test.Errorf("minute HTTP failure: %v", err)
	}
	if _, err := NewSinaAdapter().GetCNSpotSnapshot(context.Background()); !errors.Is(err, ErrUpstream) {
		test.Errorf("snapshot HTTP failure: %v", err)
	}
	if _, err := GetSinaFastNews(context.Background(), 20); !errors.Is(err, ErrUpstream) {
		test.Errorf("news HTTP failure: %v", err)
	}
}
