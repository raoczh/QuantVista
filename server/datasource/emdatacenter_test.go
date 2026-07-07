package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// withDcTestServer 把 datacenter 基址指到本地假服务并压缩限流间隔，测毕还原。
func withDcTestServer(t *testing.T, handler http.HandlerFunc, interval time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldURL, oldInterval, oldBackoff := dcBaseURL, dcMinInterval, dcRetryBackoff
	dcBaseURL = srv.URL
	dcMinInterval = interval
	dcRetryBackoff = time.Millisecond
	t.Cleanup(func() {
		dcBaseURL = oldURL
		dcMinInterval = oldInterval
		dcRetryBackoff = oldBackoff
		srv.Close()
	})
	return srv
}

func dcPage(pages int, rows ...string) string {
	data := "[" + join(rows, ",") + "]"
	return fmt.Sprintf(`{"result":{"pages":%d,"count":%d,"data":%s},"success":true,"message":"ok","code":0}`, pages, len(rows), data)
}

func join(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// 翻页：pages=2 两页数据，迭代器应先后给出两页后以 (nil,nil) 结束。
func TestDataCenterIterPaging(t *testing.T) {
	var calls int32
	withDcTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		switch r.URL.Query().Get("pageNumber") {
		case "1":
			fmt.Fprint(w, dcPage(2, `{"SECURITY_CODE":"000001"}`, `{"SECURITY_CODE":"000002"}`))
		case "2":
			fmt.Fprint(w, dcPage(2, `{"SECURITY_CODE":"000003"}`))
		default:
			t.Errorf("不应请求第三页")
			fmt.Fprint(w, `{"success":false,"code":9201,"message":"返回数据为空"}`)
		}
	}, time.Millisecond)

	it := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST", PageSize: 2})
	ctx := context.Background()

	p1, err := it.Next(ctx)
	if err != nil || len(p1) != 2 {
		t.Fatalf("第一页期望 2 行, got %d rows err=%v", len(p1), err)
	}
	p2, err := it.Next(ctx)
	if err != nil || len(p2) != 1 {
		t.Fatalf("第二页期望 1 行, got %d rows err=%v", len(p2), err)
	}
	p3, err := it.Next(ctx)
	if err != nil || p3 != nil {
		t.Fatalf("第三次应结束, got rows=%v err=%v", p3, err)
	}
	// done 后再调仍安全。
	if p4, err := it.Next(ctx); err != nil || p4 != nil {
		t.Fatalf("done 后 Next 应恒为 (nil,nil)")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("期望恰好 2 次 HTTP 请求, got %d", n)
	}
}

// 无数据（code=9201）：首页即空应返回 ErrNoData。
func TestDataCenterIterNoData(t *testing.T) {
	withDcTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":null,"result":null,"success":false,"message":"返回数据为空","code":9201}`)
	}, time.Millisecond)

	it := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
	if _, err := it.Next(context.Background()); !errors.Is(err, ErrNoData) {
		t.Fatalf("期望 ErrNoData, got %v", err)
	}
}

// 限流：全局最小间隔生效——连续两页请求的服务端观测间隔不小于 dcMinInterval。
func TestDataCenterThrottle(t *testing.T) {
	const interval = 80 * time.Millisecond
	var times []time.Time
	withDcTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		times = append(times, time.Now())
		fmt.Fprint(w, dcPage(2, `{"A":1}`))
	}, interval)

	it := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
	ctx := context.Background()
	if _, err := it.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := it.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 {
		t.Fatalf("期望 2 次请求, got %d", len(times))
	}
	if gap := times[1].Sub(times[0]); gap < interval-5*time.Millisecond {
		t.Fatalf("限流失效：两次请求间隔 %v < %v", gap, interval)
	}
}

// 瞬时错误重试：前两次 500，第三次成功，Next 应最终拿到数据。
func TestDataCenterRetryTransient(t *testing.T) {
	var calls int32
	withDcTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, dcPage(1, `{"A":1}`))
	}, time.Millisecond)

	it := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{ReportName: "RPT_TEST"})
	rows, err := it.Next(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("重试后应成功, rows=%d err=%v", len(rows), err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("期望 3 次尝试, got %d", calls)
	}
}

// DcRow 宽松取值：null 数值、字符串数值、日期尾巴。
func TestDcRowAccessors(t *testing.T) {
	raw := json.RawMessage(`{"NAME":"平安银行","AMT":null,"EPS":"0.25","ROE":3.14,"NOTICE_DATE":"2026-07-07 00:00:00","SHORT":"x"}`)
	row, err := ParseDcRow(raw)
	if err != nil {
		t.Fatal(err)
	}
	if row.String("NAME") != "平安银行" {
		t.Fatalf("String 取值错: %q", row.String("NAME"))
	}
	if v := row.Float("AMT"); v != 0 {
		t.Fatalf("null 数值应为 0, got %v", v)
	}
	if v := row.Float("EPS"); v != 0.25 {
		t.Fatalf("字符串数值应解析, got %v", v)
	}
	if v := row.Float("ROE"); v != 3.14 {
		t.Fatalf("数值取值错, got %v", v)
	}
	if d := row.Date("NOTICE_DATE"); d != "2026-07-07" {
		t.Fatalf("日期应截前 10 位, got %q", d)
	}
	if d := row.Date("SHORT"); d != "x" {
		t.Fatalf("短字符串日期应原样, got %q", d)
	}
	if row.String("MISSING") != "" || row.Float("MISSING") != 0 {
		t.Fatal("缺失键应为零值")
	}
}
