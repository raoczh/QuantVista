package datasource

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 适配层共享 HTTP 客户端：带超时，避免上游卡死拖垮整个请求。
// 禁用 HTTP/2：东财等公开接口在 H2 下偶发连接被对端重置(EOF)，强制 HTTP/1.1 更稳。
var httpClient = &http.Client{
	Timeout: 8 * time.Second,
	// API 重定向通常是登录/验证页；不让隐式跳转绕过节流和状态校验。
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: false,
		// 置空 TLSNextProto 即关闭 HTTP/2 协商，连接固定走 HTTP/1.1。
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       60 * time.Second,
	},
}

const (
	httpMaxAttempts      = 3
	httpRequestBudget    = 20 * time.Second
	httpRetryBackoff     = 250 * time.Millisecond
	maxHTTPResponseBytes = 4 << 20
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// Legacy URL builders still include numbered nodes. EastMoneyAdapter.get
// normalizes them to the canonical host; these are not independent sources.
var emNodeCounter uint32

// waitSourceInterval 为共享数据源逐次放行请求。限速与可选冷却共用同一把锁，
// 唤醒后重新检查，避免并发等待者同时通过或绕过刚收到的 Retry-After。
func waitSourceInterval(ctx context.Context, mu *sync.Mutex, last *time.Time, interval time.Duration, notBefore *time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return observeLocalSourceWait(ctx, err)
		}
		mu.Lock()
		wait := interval - time.Since(*last)
		if notBefore != nil {
			wait = max(wait, time.Until(*notBefore))
		}
		if wait <= 0 {
			*last = time.Now()
			mu.Unlock()
			return nil
		}
		mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return observeLocalSourceWait(ctx, ctx.Err())
		case <-timer.C:
		}
	}
}

// sourceRequestActivity 按一次能力调用跟踪真实尝试。它也能穿过旧适配器的
// 文本错误包装，且不会把翻页/重试中已有 HTTP 尝试的失败误判为纯本地排队。
type sourceRequestActivity struct {
	started   atomic.Bool
	mu        sync.Mutex
	localWait error
}

type sourceRequestActivityKey struct{}

func trackSourceRequests(ctx context.Context) (context.Context, *sourceRequestActivity) {
	activity := &sourceRequestActivity{}
	return context.WithValue(ctx, sourceRequestActivityKey{}, activity), activity
}

func (activity *sourceRequestActivity) localWaitFailure(err error) error {
	if err == nil || activity.started.Load() {
		return nil
	}
	activity.mu.Lock()
	defer activity.mu.Unlock()
	return activity.localWait
}

type localSourceWaitError struct{ cause error }

func (failure *localSourceWaitError) Error() string {
	return fmt.Sprintf("local source wait: %v", failure.cause)
}

func (failure *localSourceWaitError) Unwrap() error { return failure.cause }

func observeLocalSourceWait(ctx context.Context, cause error) error {
	activity, _ := ctx.Value(sourceRequestActivityKey{}).(*sourceRequestActivity)
	if activity != nil && activity.started.Load() {
		return cause
	}
	failure := &localSourceWaitError{cause: cause}
	if activity != nil {
		activity.mu.Lock()
		activity.localWait = failure
		activity.mu.Unlock()
	}
	return failure
}

func emNode() int {
	return int(atomic.AddUint32(&emNodeCounter, 1)%99) + 1
}

// httpRequestPolicy 只扩展逐次放行、共享冷却和退避，不允许再嵌套重试循环。
type httpRequestPolicy struct {
	beforeAttempt func(context.Context) error
	onRetryAfter  func(time.Time)
	retryBackoff  time.Duration
}

// doGet 返回原始字节（GBK 解码仍由调用方负责）。非 2xx、HTML 验证页、显式业务
// 错误及超限响应均返回错误；状态码在已收到响应时保留，取消原因可用 errors.Is 检查。
func doGet(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
	return doGetWithPolicy(ctx, rawURL, headers, httpRequestPolicy{retryBackoff: httpRetryBackoff})
}

func doGetWithPolicy(ctx context.Context, rawURL string, headers map[string]string, policy httpRequestPolicy) ([]byte, int, error) {
	return doHTTPRequest(ctx, http.MethodGet, rawURL, "", headers, policy)
}

// doPostForm 仅供只读巨潮查询使用：表单可重放，复用 GET 的预算、限流和重试治理。
// 不得用于下单、写入等有副作用的 POST；不会修改调用方的 values 或 headers。
func doPostForm(ctx context.Context, rawURL string, values url.Values, headers map[string]string) ([]byte, int, error) {
	return doHTTPRequest(ctx, http.MethodPost, rawURL, values.Encode(), headers,
		httpRequestPolicy{retryBackoff: httpRetryBackoff})
}

func doHTTPRequest(ctx context.Context, method, rawURL, payload string, headers map[string]string, policy httpRequestPolicy) ([]byte, int, error) {
	activity, _ := ctx.Value(sourceRequestActivityKey{}).(*sourceRequestActivity)
	if activity == nil {
		ctx, activity = trackSourceRequests(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, httpRequestBudget)
	defer cancel()
	var body []byte
	var status int
	var lastErr error
	var attempts atomic.Int32
	for attempt := 0; attempt < httpMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, status, err
		}
		request, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(payload))
		if err != nil {
			return nil, status, err
		}
		if (request.URL.Scheme != "http" && request.URL.Scheme != "https") || request.URL.Hostname() == "" {
			return nil, status, fmt.Errorf("%w: invalid upstream HTTP URL", ErrUpstream)
		}
		request.Header.Set("User-Agent", browserUA)
		request.Header.Set("Accept", "*/*")
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}

		var attemptStatus int
		attempts.Add(1)
		body, attemptStatus, lastErr = performHTTPAttempt(request, policy, &attempts)
		if attemptStatus != 0 {
			status = attemptStatus
		}
		if localWait := activity.localWaitFailure(lastErr); localWait != nil {
			return nil, status, localWait
		}
		if err := ctx.Err(); err != nil {
			return nil, status, err
		}
		if lastErr == nil || attempts.Load() >= httpMaxAttempts || !retryableHTTPError(lastErr) {
			return body, status, lastErr
		}
		delay := time.Duration(attempt+1) * policy.retryBackoff
		var statusErr *httpStatusError
		if errors.As(lastErr, &statusErr) {
			if retryDelay := time.Until(statusErr.retryAt); retryDelay > delay {
				delay = retryDelay
			}
		}
		// 不截短 Retry-After。预算装不下完整等待时保留上次失败，直接停止。
		if deadline, ok := ctx.Deadline(); ok && delay >= time.Until(deadline) {
			return body, status, lastErr
		}
		if err := waitHTTPDelay(ctx, delay); err != nil {
			return nil, status, err
		}
	}
	return body, status, lastErr
}

func performHTTPAttempt(request *http.Request, policy httpRequestPolicy, attempts *atomic.Int32) ([]byte, int, error) {
	limiter := publicSourceLimiter(request.URL.Hostname())
	if limiter != nil {
		release, err := limiter.acquire(request.Context())
		if err != nil {
			return nil, 0, err
		}
		defer release()
	}
	if policy.beforeAttempt != nil {
		if err := policy.beforeAttempt(request.Context()); err != nil {
			return nil, 0, err
		}
	}
	if err := request.Context().Err(); err != nil {
		return nil, 0, err
	}
	// Transport 会透明重放失效 keep-alive 连接上的 GET；GotConn 对每次重放都会
	// 回调，在写请求前重新节流并扣减同一尝试预算，避免隐藏的物理重试绕过 DC QPS。
	attemptContext, cancelAttempt := context.WithCancelCause(request.Context())
	defer cancelAttempt(nil)
	var connections atomic.Int32
	request = request.WithContext(httptrace.WithClientTrace(attemptContext, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			if connections.Add(1) == 1 {
				return
			}
			if attempts.Add(1) > httpMaxAttempts {
				cancelAttempt(fmt.Errorf("%w: HTTP attempt budget exhausted", ErrUpstream))
				return
			}
			if limiter != nil {
				if err := limiter.wait(attemptContext); err != nil {
					cancelAttempt(err)
					return
				}
			}
			if policy.beforeAttempt != nil {
				if err := policy.beforeAttempt(attemptContext); err != nil {
					cancelAttempt(err)
				}
			}
		},
	}))
	if activity, ok := request.Context().Value(sourceRequestActivityKey{}).(*sourceRequestActivity); ok {
		activity.started.Store(true)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		if cause := context.Cause(attemptContext); cause != nil {
			return nil, 0, cause
		}
		return nil, 0, fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	defer response.Body.Close()

	var statusErr *httpStatusError
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		statusErr = &httpStatusError{statusCode: response.StatusCode}
		if delay, ok := parseHTTPRetryAfter(response.Header.Get("Retry-After"), time.Now()); ok {
			statusErr.retryAt = time.Now().Add(delay)
			if retryableHTTPStatus(response.StatusCode) {
				if limiter != nil {
					limiter.deferUntil(statusErr.retryAt)
				}
				if policy.onRetryAfter != nil {
					policy.onRetryAfter(statusErr.retryAt)
				}
			}
		}
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxHTTPResponseBytes+1))
	if len(body) > maxHTTPResponseBytes {
		return nil, response.StatusCode, fmt.Errorf("%w: response exceeds %d bytes", ErrUpstream, maxHTTPResponseBytes)
	}
	if statusErr != nil {
		return body, response.StatusCode, statusErr
	}
	if readErr != nil {
		return nil, response.StatusCode, fmt.Errorf("%w: read response: %w", ErrUpstream, readErr)
	}
	if err := validateHTTPBody(body); err != nil {
		if errors.Is(err, ErrNoData) {
			return body, response.StatusCode, err
		}
		return nil, response.StatusCode, err
	}
	return body, response.StatusCode, nil
}

type httpStatusError struct {
	statusCode int
	retryAt    time.Time
}

func (failure *httpStatusError) Error() string {
	return fmt.Sprintf("%v: http %d", ErrUpstream, failure.statusCode)
}

func (failure *httpStatusError) Unwrap() error { return ErrUpstream }

type upstreamBusinessError struct{ field string }

func (failure *upstreamBusinessError) Error() string {
	return fmt.Sprintf("%v: upstream business error (%s)", ErrUpstream, failure.field)
}

func (failure *upstreamBusinessError) Unwrap() error { return ErrUpstream }

// 只识别通用的显式失败字段；各接口的 code 成功口径不同，仍由对应解析器校验。
// 新浪等接口会把正常 JSON/JS 标成 text/html，因此按正文前缀识别 HTML，而非只看 MIME。
func validateHTTPBody(body []byte) error {
	payload := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(body), []byte("\xef\xbb\xbf")))
	if len(payload) == 0 {
		return nil
	}
	if payload[0] == '<' {
		return fmt.Errorf("%w: unexpected HTML/XML response", ErrUpstream)
	}
	if payload[0] != '{' && payload[0] != '[' {
		return nil // GBK 行情文本和 JSONP 留给各自解析器。
	}
	if payload[0] == '[' {
		if !json.Valid(payload) {
			return fmt.Errorf("%w: invalid JSON response", ErrUpstream)
		}
		return nil
	}
	var envelope struct {
		Success json.RawMessage `json:"success"`
		Code    json.RawMessage `json:"code"`
		RC      json.RawMessage `json:"rc"`
		Errno   json.RawMessage `json:"errno"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("%w: invalid JSON response", ErrUpstream)
	}
	success := strings.Trim(string(envelope.Success), "\"")
	if success == "false" || success == "0" {
		if code, ok := responseInteger(envelope.Code); ok && code == dcNoDataCode {
			return ErrNoData
		}
		return &upstreamBusinessError{field: "success"}
	}
	for _, field := range []struct {
		name  string
		value json.RawMessage
	}{{"rc", envelope.RC}, {"errno", envelope.Errno}} {
		if code, ok := responseInteger(field.value); ok && code != 0 {
			if field.name == "rc" && code == emEmptyPoolCode && bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
				return ErrNoData
			}
			return &upstreamBusinessError{field: field.name}
		}
	}
	return nil
}

func responseInteger(raw json.RawMessage) (int64, bool) {
	value, err := strconv.ParseInt(strings.Trim(string(raw), "\""), 10, 64)
	return value, err == nil
}

func retryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryableHTTPError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return retryableHTTPStatus(statusErr.statusCode)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary()) {
		return true
	}
	var operationErr *net.OpError
	return errors.As(err, &operationErr) &&
		(operationErr.Op == "dial" || operationErr.Op == "read" || operationErr.Op == "write")
}

func parseHTTPRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		const longestDelay = time.Duration(1<<63 - 1)
		if err != nil || seconds > uint64(longestDelay/time.Second) {
			return longestDelay, true // 巨大秒数不能溢出成短等待。
		}
		return time.Duration(seconds) * time.Second, true
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return max(deadline.Sub(now), 0), true
	}
	return 0, false
}

func waitHTTPDelay(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(max(delay, 0))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// 数字负载节点共享同一额度，不以换 host 增加并发。非白名单（含 httptest 本地地址）
// 不限流；datacenter/F10 的逐次限流由其显式 policy 提供，避免重复等待。
var (
	emQuoteLimiter = newHTTPSourceLimiter(2, 250*time.Millisecond)
	tencentLimiter = newHTTPSourceLimiter(2, 250*time.Millisecond)
	sinaLimiter    = newHTTPSourceLimiter(2, 500*time.Millisecond)
	cninfoLimiter  = newHTTPSourceLimiter(1, 500*time.Millisecond)
)

func publicSourceLimiter(host string) *httpSourceLimiter {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if family, _ := emHostFamily(host); family != "" {
		return emQuoteLimiter
	}
	if hostInDomain(host, "gtimg.cn") || hostInDomain(host, "finance.qq.com") {
		return tencentLimiter
	}
	if hostInDomain(host, "sinajs.cn") || hostInDomain(host, "finance.sina.com.cn") {
		return sinaLimiter
	}
	if hostInDomain(host, "cninfo.com.cn") {
		return cninfoLimiter
	}
	return nil
}

func hostInDomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

type httpSourceLimiter struct {
	slots       chan struct{}
	mu          sync.Mutex
	nextAllowed time.Time
	interval    time.Duration
}

func newHTTPSourceLimiter(concurrency int, interval time.Duration) *httpSourceLimiter {
	return &httpSourceLimiter{slots: make(chan struct{}, concurrency), interval: interval}
}

func (limiter *httpSourceLimiter) acquire(ctx context.Context) (func(), error) {
	select {
	case limiter.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, observeLocalSourceWait(ctx, ctx.Err())
	}
	release := func() { <-limiter.slots }
	if err := limiter.wait(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (limiter *httpSourceLimiter) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return observeLocalSourceWait(ctx, err)
		}
		limiter.mu.Lock()
		delay := time.Until(limiter.nextAllowed)
		if delay <= 0 {
			limiter.nextAllowed = time.Now().Add(limiter.interval)
			limiter.mu.Unlock()
			return nil
		}
		limiter.mu.Unlock()
		if err := waitHTTPDelay(ctx, delay); err != nil {
			return observeLocalSourceWait(ctx, err)
		}
	}
}

func (limiter *httpSourceLimiter) deferUntil(deadline time.Time) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if deadline.After(limiter.nextAllowed) {
		limiter.nextAllowed = deadline
	}
}

// cnSecid 把 A 股代码转换为东财 secid（沪市 1.、深市 0.）。
// 规则（社区惯例）：6/5/9 开头 -> 沪市；0/2/3 开头 -> 深市。
// 深市基金（15x ETF / 16x LOF / 18x 封闭基金）也以 '1' 开头，单独放行 -> 深市；
// 不放开整个 '1' 前缀：10x/11x 为沪深可转债，代码规则不同，须排除（否则误当基金查行情）。
func cnSecid(symbol string) (string, bool) {
	s := strings.TrimSpace(symbol)
	if !isSixDigitSymbol(s) {
		return "", false
	}
	switch s[0] {
	case '6', '5', '9':
		return "1." + s, true
	case '0', '2', '3':
		return "0." + s, true
	case '1':
		if p := s[:2]; p == "15" || p == "16" || p == "18" {
			return "0." + s, true // 深市基金
		}
		return "", false
	default:
		return "", false
	}
}

// sinaCNSymbol 把 A 股代码转换为新浪前缀代码（sh/sz）。
// 深市基金 15x/16x/18x 同 cnSecid：放行为深市（sz），排除 10x/11x 可转债。
func sinaCNSymbol(symbol string) (string, bool) {
	s := strings.TrimSpace(symbol)
	if !isSixDigitSymbol(s) {
		return "", false
	}
	switch s[0] {
	case '6', '5', '9':
		return "sh" + s, true
	case '0', '2', '3':
		return "sz" + s, true
	case '1':
		if p := s[:2]; p == "15" || p == "16" || p == "18" {
			return "sz" + s, true // 深市基金
		}
		return "", false
	default:
		return "", false
	}
}

func isSixDigitSymbol(symbol string) bool {
	if len(symbol) != 6 {
		return false
	}
	for i := range symbol {
		if symbol[i] < '0' || symbol[i] > '9' {
			return false
		}
	}
	return true
}
