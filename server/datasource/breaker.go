package datasource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 东财域名断路器（S1）：push2 族接口过载/断连时尝试备用域 push2delay，
// 降级仅在冷却窗口内生效，到期只放行一个主域恢复试探（实例级，进程内生效）；
// 无备用域的接口（push2his/push2ex）连续 emBreakThreshold 次限流则熔断一段时间快速失败，
// 避免每次请求都白等 8s 超时拖垮上层聚合接口。
// 注意：push2dhis 不可用，别把它加成 push2his 的备用域（StockNova 实战结论）。
const (
	emBackupHost     = "push2delay.eastmoney.com"
	emBreakThreshold = 5
	emBreakCooldown  = 2 * time.Minute
	emEmptyPoolCode  = 102
)

// emBreaker 按域族（push2/push2his/push2ex）记录降级与熔断状态。
type emBreaker struct {
	mu            sync.Mutex
	degraded      map[string]bool
	degradedUntil map[string]time.Time
	streak        map[string]int
	openUntil     map[string]time.Time
	probing       map[string]bool
	now           func() time.Time // 可注入时钟（单测）
}

func newEmBreaker() *emBreaker {
	return &emBreaker{
		degraded:      map[string]bool{},
		degradedUntil: map[string]time.Time{},
		streak:        map[string]int{},
		openUntil:     map[string]time.Time{},
		probing:       map[string]bool{},
		now:           time.Now,
	}
}

// allow 该域族当前是否放行请求（熔断窗口内快速失败）。
func (breaker *emBreaker) allow(family string) bool {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	return !breaker.now().Before(breaker.openUntil[family])
}

func (breaker *emBreaker) isDegraded(family string) bool {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	return breaker.degraded[family]
}

type emRequestRoute struct {
	backup bool
	probe  bool
}

type emRequestOutcome int

const (
	emRequestCanceled emRequestOutcome = iota
	emPrimarySucceeded
	emBackupSucceeded
	emRequestFailed
	emRequestRejected
)

// begin 原子选择路由。降级恢复期间其他请求仍可走备用域；熔断半开期间则快速失败。
func (breaker *emBreaker) begin(family string, hasBackup bool) (emRequestRoute, bool) {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	now := breaker.now()
	if now.Before(breaker.openUntil[family]) {
		return emRequestRoute{}, false
	}
	if !breaker.openUntil[family].IsZero() {
		if breaker.probing[family] {
			return emRequestRoute{}, false
		}
		breaker.probing[family] = true
		return emRequestRoute{probe: true}, true
	}
	if hasBackup && breaker.degraded[family] {
		if now.Before(breaker.degradedUntil[family]) || breaker.probing[family] {
			return emRequestRoute{backup: true}, true
		}
		breaker.probing[family] = true
		return emRequestRoute{probe: true}, true
	}
	return emRequestRoute{}, true
}

// complete 只把经过状态码和正文校验的成功记为恢复；取消既不清零，也不记故障。
func (breaker *emBreaker) complete(family string, route emRequestRoute, outcome emRequestOutcome) {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	if route.probe {
		delete(breaker.probing, family)
	}
	switch outcome {
	case emPrimarySucceeded, emBackupSucceeded:
		breaker.streak[family] = 0
		delete(breaker.openUntil, family)
		if outcome == emPrimarySucceeded {
			delete(breaker.degraded, family)
			delete(breaker.degradedUntil, family)
		} else if !route.backup {
			breaker.degraded[family] = true
			breaker.degradedUntil[family] = breaker.now().Add(emBreakCooldown)
		}
		// 已降级后的正常备用域成功不续期，否则会永久不回切。
	case emRequestFailed:
		breaker.streak[family]++
		if route.probe || breaker.streak[family] >= emBreakThreshold {
			breaker.openUntil[family] = breaker.now().Add(emBreakCooldown)
			breaker.streak[family] = 0
		}
	case emRequestRejected:
		if route.probe {
			// 业务错误/空数据不是恢复证据，也不应导致连续试探。
			if breaker.degraded[family] {
				breaker.degradedUntil[family] = breaker.now().Add(emBreakCooldown)
			}
			if !breaker.openUntil[family].IsZero() {
				breaker.openUntil[family] = breaker.now().Add(emBreakCooldown)
			}
		}
	}
}

func (breaker *emBreaker) degrade(family string) {
	breaker.complete(family, emRequestRoute{}, emBackupSucceeded)
}

func (breaker *emBreaker) success(family string) {
	breaker.complete(family, emRequestRoute{backup: true}, emBackupSucceeded)
}

func (breaker *emBreaker) fail(family string) {
	breaker.complete(family, emRequestRoute{}, emRequestFailed)
}

// emHostFamily 识别东财 push2 域族。返回族名与是否存在备用域；非 push2 族返回 ""（不经断路器）。
func emHostFamily(host string) (string, bool) {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	switch {
	case hostInDomain(host, "push2his.eastmoney.com"):
		return "push2his", false // push2dhis 不可用，无备用域
	case hostInDomain(host, "push2ex.eastmoney.com"):
		return "push2ex", false
	case hostInDomain(host, emBackupHost):
		return "push2", true // 已是备用域的请求仍归 push2 族计数
	case hostInDomain(host, "push2.eastmoney.com"):
		return "push2", true
	default:
		return "", false
	}
}

// emRateLimited 是否为可降级/熔断的上游失败。业务拒绝、正常无数据与取消不推进计数，
// 但它们也不是成功，不能用 !emRateLimited 代替成功判断。
func emRateLimited(ctx context.Context, err error, status int) bool {
	if emRequestInterrupted(ctx, err) || errors.Is(err, ErrNoData) {
		return false
	}
	var businessErr *upstreamBusinessError
	if errors.As(err, &businessErr) {
		return false
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		status = statusErr.statusCode
	} else if err != nil {
		return true
	}
	return retryableHTTPStatus(status) || (status >= 300 && status < 400) || status == http.StatusForbidden
}

func emRequestInterrupted(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// http.Client 自身超时属于上游故障；直接/包装的调用预算错误则不记故障。
		var requestErr *url.Error
		return !errors.As(err, &requestErr)
	}
	return false
}

// 即使测试替换了 fetch，也必须先校验 HTTP 与东财 JSON envelope，之后才能记成功。
func validateEMFetch(body []byte, status int, err error) error {
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &httpStatusError{statusCode: status}
	}
	if err := validateHTTPBody(body); err != nil {
		return err
	}
	payload := bytes.TrimSpace(body)
	if len(payload) == 0 || payload[0] != '{' {
		return fmt.Errorf("%w: invalid eastmoney response", ErrUpstream)
	}
	return nil
}

// get 东财 push2 族请求统一入口：降级改写 + 备用域重试 + 熔断快速失败。
// 非 push2 族 URL 直连（datacenter/公告等有各自的限流治理）。
func (adapter *EastMoneyAdapter) get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, err
	}
	family, hasBackup := emHostFamily(parsedURL.Hostname())
	if family == "" {
		return adapter.fetch(ctx, rawURL, headers)
	}
	// Let the canonical endpoint select healthy backends. Arbitrary numbered
	// push2/push2his hosts are not a documented, independently healthy pool.
	if !hostInDomain(parsedURL.Hostname(), emBackupHost) {
		primaryHost := family + ".eastmoney.com"
		if port := parsedURL.Port(); port != "" {
			primaryHost = net.JoinHostPort(primaryHost, port)
		}
		parsedURL.Host = primaryHost
	}
	ctx, cancel := context.WithTimeout(ctx, httpRequestBudget)
	defer cancel()
	route, allowed := adapter.br.begin(family, hasBackup)
	if !allowed {
		return nil, 0, fmt.Errorf("%w: 东财 %s 连续限流已熔断，快速失败（约 %s 后自动恢复）", ErrUpstream, family, emBreakCooldown)
	}
	originalBackup := hostInDomain(strings.TrimSuffix(strings.ToLower(parsedURL.Hostname()), "."), emBackupHost)
	if originalBackup && !route.probe {
		route.backup = true
	}
	outcome := emRequestCanceled
	defer func() { adapter.br.complete(family, route, outcome) }()
	if route.backup {
		parsedURL.Host = emBackupHost
	} else if originalBackup {
		// 直接传备用 URL 时，到期也应试探主域，不把备用成功误记为主域恢复。
		parsedURL.Host = "push2.eastmoney.com"
	}
	body, status, err := adapter.fetch(ctx, parsedURL.String(), headers)
	if canceled := ctx.Err(); canceled != nil {
		return body, status, canceled
	}
	if emRequestInterrupted(ctx, err) {
		return body, status, err
	}
	err = validateEMFetch(body, status, err)
	if err == nil {
		outcome = emPrimarySucceeded
		if route.backup {
			outcome = emBackupSucceeded
		}
		return body, status, nil
	}
	outcome = emRequestRejected
	if errors.Is(err, ErrNoData) && family == "push2ex" && len(body) > 0 {
		// 旧池解析器自行区分首页空池与尾页结束；继续交给它解析，但绝不记恢复成功。
		return body, status, nil
	}
	if !emRateLimited(ctx, err, status) {
		return body, status, err
	}
	outcome = emRequestFailed
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.retryAt.After(time.Now()) {
		return body, status, err // 不用备用域绕过尚未结束的 Retry-After。
	}
	if hasBackup && !route.backup {
		parsedURL.Host = emBackupHost
		body, status, err = adapter.fetch(ctx, parsedURL.String(), headers)
		if canceled := ctx.Err(); canceled != nil {
			outcome = emRequestCanceled
			return body, status, canceled
		}
		if emRequestInterrupted(ctx, err) {
			outcome = emRequestCanceled
			return body, status, err
		}
		err = validateEMFetch(body, status, err)
		if err == nil {
			outcome = emBackupSucceeded
		}
	}
	return body, status, err
}
