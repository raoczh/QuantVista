package datasource

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 东财域名断路器（S1）：push2 族接口被限流（302/403/429/502/断连/EOF）时，
// 把 host 换到备用域 push2delay.eastmoney.com 重试并记住降级（实例级，进程内生效）；
// 无备用域的接口（push2his/push2ex）连续 emBreakThreshold 次限流则熔断一段时间快速失败，
// 避免每次请求都白等 8s 超时拖垮上层聚合接口。
// 注意：push2dhis 不可用，别把它加成 push2his 的备用域（StockNova 实战结论）。
const (
	emBackupHost     = "push2delay.eastmoney.com"
	emBreakThreshold = 5
	emBreakCooldown  = 2 * time.Minute
)

// emBreaker 按域族（push2/push2his/push2ex）记录降级与熔断状态。
type emBreaker struct {
	mu        sync.Mutex
	degraded  map[string]bool      // 已切换到备用域的域族（记住降级，不回切）
	streak    map[string]int       // 连续限流次数
	openUntil map[string]time.Time // 熔断截止时刻
	now       func() time.Time     // 可注入时钟（单测）
}

func newEmBreaker() *emBreaker {
	return &emBreaker{
		degraded:  map[string]bool{},
		streak:    map[string]int{},
		openUntil: map[string]time.Time{},
		now:       time.Now,
	}
}

// allow 该域族当前是否放行请求（熔断窗口内快速失败）。
func (b *emBreaker) allow(family string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now().After(b.openUntil[family])
}

func (b *emBreaker) isDegraded(family string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.degraded[family]
}

// degrade 主域限流而备用域可用：记住降级，后续请求直接走备用域。
func (b *emBreaker) degrade(family string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.degraded[family] = true
	b.streak[family] = 0
}

// success 任一次成功即清零连续限流计数。
func (b *emBreaker) success(family string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.streak[family] = 0
}

// fail 记一次限流失败；连续达到阈值则打开熔断窗口。
func (b *emBreaker) fail(family string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.streak[family]++
	if b.streak[family] >= emBreakThreshold {
		b.openUntil[family] = b.now().Add(emBreakCooldown)
		b.streak[family] = 0
	}
}

// emHostFamily 识别东财 push2 域族。返回族名与是否存在备用域；非 push2 族返回 ""（不经断路器）。
func emHostFamily(host string) (string, bool) {
	host = strings.ToLower(host)
	switch {
	case strings.Contains(host, "push2his."):
		return "push2his", false // push2dhis 不可用，无备用域
	case strings.Contains(host, "push2ex."):
		return "push2ex", false
	case strings.Contains(host, "push2delay."):
		return "push2", true // 已是备用域的请求仍归 push2 族计数
	case strings.Contains(host, "push2."):
		return "push2", true
	default:
		return "", false
	}
}

// emRateLimited 是否为限流类失败：网络层错误（EOF/断连/重置，doGet 已重试一次仍失败）
// 或典型限流状态码。调用方 ctx 已取消/超时不算限流（那是预算问题，不该计入熔断）。
func emRateLimited(ctx context.Context, err error, status int) bool {
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		return true
	}
	return status == 302 || status == 403 || status == 429 || status == 502
}

// get 东财 push2 族请求统一入口：降级改写 + 备用域重试 + 熔断快速失败。
// 非 push2 族 URL 直连（datacenter/公告等有各自的限流治理）。
func (e *EastMoneyAdapter) get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return nil, 0, perr
	}
	family, hasBackup := emHostFamily(u.Host)
	if family == "" {
		return e.fetch(ctx, rawURL, headers)
	}
	if !e.br.allow(family) {
		return nil, 0, fmt.Errorf("%w: 东财 %s 连续限流已熔断，快速失败（约 %s 后自动恢复）", ErrUpstream, family, emBreakCooldown)
	}
	if hasBackup && e.br.isDegraded(family) {
		u.Host = emBackupHost
		rawURL = u.String()
	}
	body, status, err := e.fetch(ctx, rawURL, headers)
	if !emRateLimited(ctx, err, status) {
		e.br.success(family)
		return body, status, err
	}
	// 限流类失败：有备用域且尚未降级 → 立即换 push2delay 重试一次，成功则记住降级。
	if hasBackup && !e.br.isDegraded(family) {
		u.Host = emBackupHost
		b2, s2, e2 := e.fetch(ctx, u.String(), headers)
		if !emRateLimited(ctx, e2, s2) {
			e.br.degrade(family)
			return b2, s2, e2
		}
	}
	e.br.fail(family)
	return body, status, err
}
