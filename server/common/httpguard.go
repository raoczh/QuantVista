package common

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// SafeHTTPClient 返回一个 HTTP 客户端，对用户提供的 URL 发起请求时（如 LLM 测试连接）
// 防 SSRF：解析 DNS 后拦截内网/回环/链路本地/CGNAT 目标地址，并限制重定向次数。
// allowPrivate=true 时放行内网（供受信任的管理员访问本地/内网自建模型，如 Ollama/LM Studio）。
func SafeHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		// Control 在实际连接前拿到 DNS 解析后的目标 IP，可同时防御 DNS rebinding 与
		// 跳转到内网（每次重定向都会重新 dial → 重新校验）。
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || isBlockedIP(ip) {
				return fmt.Errorf("目标地址不允许（内网/回环/链路本地）: %s", host)
			}
			return nil
		}
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("重定向次数过多")
			}
			return nil
		},
	}
}

// isBlockedIP 判定是否为不允许对外请求触达的地址段。
func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// CGNAT 100.64.0.0/10
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
}
