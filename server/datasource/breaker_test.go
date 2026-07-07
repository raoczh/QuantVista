package datasource

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// newTestEM 构造带假 fetch 的东财适配器，记录每次请求命中的 host。
func newTestEM(fetch func(url string) ([]byte, int, error)) (*EastMoneyAdapter, *[]string) {
	hosts := &[]string{}
	e := &EastMoneyAdapter{br: newEmBreaker()}
	e.fetch = func(_ context.Context, url string, _ map[string]string) ([]byte, int, error) {
		h := url
		if i := strings.Index(url, "://"); i >= 0 {
			h = url[i+3:]
		}
		if i := strings.Index(h, "/"); i >= 0 {
			h = h[:i]
		}
		*hosts = append(*hosts, h)
		return fetch(url)
	}
	return e, hosts
}

// TestBreakerDegradeToDelay push2 主域限流（EOF）→ 换 push2delay 重试成功 → 记住降级，
// 后续请求直接走备用域不再碰主域。
func TestBreakerDegradeToDelay(t *testing.T) {
	e, hosts := newTestEM(func(url string) ([]byte, int, error) {
		if strings.Contains(url, emBackupHost) {
			return []byte(`{"ok":1}`), 200, nil
		}
		return nil, 0, io.EOF
	})
	ctx := context.Background()
	body, status, err := e.get(ctx, "https://33.push2.eastmoney.com/api/qt/stock/get?secid=1.600000", nil)
	if err != nil || status != 200 || string(body) != `{"ok":1}` {
		t.Fatalf("首次请求应经备用域成功: body=%s status=%d err=%v", body, status, err)
	}
	if !e.br.isDegraded("push2") {
		t.Fatal("应记住 push2 族已降级")
	}
	// 第二次请求：应直接打备用域（总请求数 2+1=3，且第 3 次 host 是 push2delay）。
	if _, _, err := e.get(ctx, "https://push2.eastmoney.com/api/qt/stock/fflow/kline/get", nil); err != nil {
		t.Fatalf("降级后的请求应成功: %v", err)
	}
	if len(*hosts) != 3 {
		t.Fatalf("预期 3 次底层请求（主域失败+备用域成功+降级后直连备用域），得到 %d: %v", len(*hosts), *hosts)
	}
	if (*hosts)[2] != emBackupHost {
		t.Fatalf("降级后应直连备用域，实际 %s", (*hosts)[2])
	}
}

// TestBreakerTripsWithoutBackup push2his 无备用域：连续 5 次限流 → 熔断快速失败（不再发底层请求），
// 冷却期过后恢复放行。
func TestBreakerTripsWithoutBackup(t *testing.T) {
	e, hosts := newTestEM(func(string) ([]byte, int, error) {
		return nil, 0, errors.New("connection reset")
	})
	now := time.Now()
	e.br.now = func() time.Time { return now }
	ctx := context.Background()
	url := "https://12.push2his.eastmoney.com/api/qt/stock/kline/get?secid=1.600000"

	for i := 0; i < emBreakThreshold; i++ {
		if _, _, err := e.get(ctx, url, nil); err == nil {
			t.Fatalf("第 %d 次应失败", i+1)
		}
	}
	if len(*hosts) != emBreakThreshold {
		t.Fatalf("前 %d 次都应发出底层请求，实际 %d", emBreakThreshold, len(*hosts))
	}
	// 第 6 次：熔断中，快速失败且不发请求。
	_, _, err := e.get(ctx, url, nil)
	if err == nil || !strings.Contains(err.Error(), "熔断") {
		t.Fatalf("熔断中应快速失败: %v", err)
	}
	if len(*hosts) != emBreakThreshold {
		t.Fatalf("熔断中不应发底层请求，实际 %d 次", len(*hosts))
	}
	// 冷却期过后恢复放行。
	now = now.Add(emBreakCooldown + time.Second)
	_, _, _ = e.get(ctx, url, nil)
	if len(*hosts) != emBreakThreshold+1 {
		t.Fatalf("冷却后应恢复放行，实际 %d 次", len(*hosts))
	}
}

// TestBreakerSuccessResetsStreak 失败 4 次后一次成功应清零计数，不触发熔断。
func TestBreakerSuccessResetsStreak(t *testing.T) {
	fail := true
	e, hosts := newTestEM(func(string) ([]byte, int, error) {
		if fail {
			return nil, 0, io.EOF
		}
		return []byte(`{}`), 200, nil
	})
	ctx := context.Background()
	url := "https://push2ex.eastmoney.com/getTopicZDFenBu"
	for i := 0; i < emBreakThreshold-1; i++ {
		_, _, _ = e.get(ctx, url, nil)
	}
	fail = false
	if _, _, err := e.get(ctx, url, nil); err != nil {
		t.Fatalf("成功请求不应报错: %v", err)
	}
	fail = true
	// 再失败 1 次：若计数未清零会到 5 触发熔断，下一次将快速失败。
	_, _, _ = e.get(ctx, url, nil)
	before := len(*hosts)
	_, _, _ = e.get(ctx, url, nil)
	if len(*hosts) != before+1 {
		t.Fatal("成功后计数应清零，不应进入熔断快速失败")
	}
}

// TestBreakerCtxCancelNotCounted 调用方取消/超时不算限流，不推进熔断计数。
func TestBreakerCtxCancelNotCounted(t *testing.T) {
	e, _ := newTestEM(func(string) ([]byte, int, error) {
		return nil, 0, context.Canceled
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	url := "https://88.push2his.eastmoney.com/api/qt/stock/kline/get"
	for i := 0; i < emBreakThreshold+2; i++ {
		_, _, _ = e.get(ctx, url, nil)
	}
	if !e.br.allow("push2his") {
		t.Fatal("ctx 取消导致的失败不应触发熔断")
	}
}

// TestBreakerNonPush2Bypass 非 push2 族域名不经断路器（datacenter/公告等自有治理）。
func TestBreakerNonPush2Bypass(t *testing.T) {
	e, hosts := newTestEM(func(string) ([]byte, int, error) {
		return []byte(`{}`), 200, nil
	})
	if _, _, err := e.get(context.Background(), "https://datacenter-web.eastmoney.com/api/data/v1/get", nil); err != nil {
		t.Fatalf("非 push2 族应直连: %v", err)
	}
	if len(*hosts) != 1 || !strings.Contains((*hosts)[0], "datacenter") {
		t.Fatalf("应直连原始域: %v", *hosts)
	}
}

// TestEmHostFamily 域族识别口径。
func TestEmHostFamily(t *testing.T) {
	cases := []struct {
		host   string
		family string
		backup bool
	}{
		{"33.push2.eastmoney.com", "push2", true},
		{"push2.eastmoney.com", "push2", true},
		{"push2delay.eastmoney.com", "push2", true},
		{"12.push2his.eastmoney.com", "push2his", false},
		{"push2ex.eastmoney.com", "push2ex", false},
		{"datacenter-web.eastmoney.com", "", false},
		{"np-anotice-stock.eastmoney.com", "", false},
	}
	for _, c := range cases {
		f, b := emHostFamily(c.host)
		if f != c.family || b != c.backup {
			t.Fatalf("%s: 期望 (%s,%v) 得到 (%s,%v)", c.host, c.family, c.backup, f, b)
		}
	}
}
