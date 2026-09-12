package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSharedSourceThrottleSpacesConcurrentCallers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		call     func(context.Context) error
		mu       *sync.Mutex
		last     *time.Time
		interval *time.Duration
	}{
		{"datacenter", dcThrottle, &dcMu, &dcLast, &dcMinInterval},
		{"reports", repThrottle, &repMu, &repLast, &repMinInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldInterval := *tc.interval
			tc.mu.Lock()
			oldLast := *tc.last
			*tc.last = time.Time{}
			tc.mu.Unlock()
			*tc.interval = 40 * time.Millisecond
			t.Cleanup(func() {
				*tc.interval = oldInterval
				tc.mu.Lock()
				*tc.last = oldLast
				tc.mu.Unlock()
			})
			const calls = 6
			start := make(chan struct{})
			results := make(chan error, calls)
			for i := 0; i < calls; i++ {
				go func() { <-start; results <- tc.call(context.Background()) }()
			}
			began := time.Now()
			close(start)
			for i := 0; i < calls; i++ {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if elapsed := time.Since(began); elapsed < time.Duration(calls-1)**tc.interval {
				t.Fatalf("并发请求共用节流器却同时放行：%d 次仅耗时 %v", calls, elapsed)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := tc.call(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("取消请求应退出等待：%v", err)
			}
		})
	}
}

func TestEastMoneyMissingPriceDoesNotBecomeQuote(t *testing.T) {
	for _, raw := range []string{"null", `"NaN"`, `"+Inf"`, `"-"`} {
		if value, ok := emNum(json.RawMessage(raw)); ok {
			t.Errorf("无效数值 %s 不得被解析为行情值 %v", raw, value)
		}
	}
	e := NewEastMoneyAdapter()
	e.fetch = func(context.Context, string, map[string]string) ([]byte, int, error) {
		return []byte(`{"data":{"f43":null,"f58":"测试股票","f86":1788768000}}`), 200, nil
	}
	if _, err := e.GetQuote(context.Background(), "cn", "600000"); !errors.Is(err, ErrNoData) {
		t.Fatalf("现价 null 应交给路由换源，不能返回零价成功：%v", err)
	}
}

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnnouncementBadDateCannotBecomeToday(t *testing.T) {
	old := httpClient
	httpClient = &http.Client{Transport: reviewTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{
			"success":1,"data":{"list":[
			{"art_code":"bad-date","title":"缺时间公告","notice_date":"unknown"},
			{"art_code":"valid-date","title":"有效公告","notice_date":"2026-08-31 00:00:00"}
		]}}`))}, nil
	})}
	t.Cleanup(func() { httpClient = old })
	items, err := GetEMAnnouncements(context.Background(), "600000", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ArtCode != "valid-date" || items[0].NoticeDate.Format("2006-01-02") != "2026-08-31" {
		t.Fatalf("坏日期公告不得伪造为今日发布：%+v", items)
	}
}

func TestDataCenterExactPageLimitIsComplete(t *testing.T) {
	withDcTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, dcPage(corpActionMaxPages, `{"SECURITY_CODE":"600000"}`))
	}, 0)
	consumed := 0
	count, err := dcPages(context.Background(), NewEastMoneyAdapter(), DataCenterQuery{ReportName: "RPT_TEST"}, "边界", func(DcRow) error {
		consumed++
		return nil
	})
	if err != nil || count != corpActionMaxPages || consumed != corpActionMaxPages {
		t.Fatalf("恰好到最后允许页应完整成功：count=%d consumed=%d err=%v", count, consumed, err)
	}
}

func TestLimitUpPoolRejectsTruncatedPages(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.fetch = func(_ context.Context, url string, _ map[string]string) ([]byte, int, error) {
		if strings.Contains(url, "Pageindex=0") {
			return []byte(`{"rc":0,"data":{"tc":2,"qdate":20260907,"pool":[{"c":"600000","n":"样本","p":10000}]}}`), 200, nil
		}
		return []byte(`{"rc":0,"data":{"tc":2,"qdate":20260907,"pool":[]}}`), 200, nil
	}
	if _, err := e.GetZTPool(context.Background(), "20260907"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("只拿到一半涨停池不能作为完整情绪统计：%v", err)
	}
}

func TestLhbRejectsNonAShares(t *testing.T) {
	for _, symbol := range []string{"900901", "200002", "510300", "159915"} {
		if lhbStockRow("058001001", symbol) {
			t.Errorf("非 A 股 %s 不得进入 A 股龙虎榜", symbol)
		}
		row := dcRowFrom(t, fmt.Sprintf(`{"SECURITY_CODE":%q,"SECURITY_NAME_ABBR":"样本","TRADE_DATE":"2026-09-07"}`, symbol))
		if _, ok, err := parseLhbOrgRowStrict(row); err != nil || ok {
			t.Errorf("非 A 股 %s 应作为不支持的行过滤：ok=%v err=%v", symbol, ok, err)
		}
	}
}

func TestBreakerCancellationDoesNotResetFailures(t *testing.T) {
	e, _ := newTestEM(func(string) ([]byte, int, error) { return nil, 0, io.EOF })
	url := "https://push2his.eastmoney.com/api/qt/stock/kline/get"
	for i := 0; i < emBreakThreshold-1; i++ {
		_, _, _ = e.get(context.Background(), url, nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _ = e.get(ctx, url, nil)
	_, _, _ = e.get(context.Background(), url, nil)
	if e.br.allow("push2his") {
		t.Fatal("用户取消不能当作上游恢复成功并重置熔断计数")
	}
}
