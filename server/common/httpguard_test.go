package common

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // 回环
		"10.0.0.1", "192.168.1.1", "172.16.0.1", // 私网
		"169.254.169.254", // 云元数据/链路本地
		"0.0.0.0",         // 未指定
		"100.64.0.1",      // CGNAT
		"fc00::1",         // IPv6 ULA
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("isBlockedIP(%s) 应为 true（内网/回环类）", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::1"}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("isBlockedIP(%s) 应为 false（公网）", s)
		}
	}
}

// TestSafeHTTPClientBlocksLoopback 验证守护客户端拒绝连回环地址（SSRF 防护生效）。
func TestSafeHTTPClientBlocksLoopback(t *testing.T) {
	srv := &http.Server{Addr: "127.0.0.1:0"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无法监听本地端口")
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Close()

	url := "http://" + ln.Addr().String() + "/"

	// 非管理员（allowPrivate=false）应被拦截。
	guarded := SafeHTTPClient(3*time.Second, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if _, err := guarded.Do(req); err == nil {
		t.Fatal("守护客户端应拒绝连回环地址")
	}

	// 管理员（allowPrivate=true）应放行（能连上本地服务）。
	open := SafeHTTPClient(3*time.Second, true)
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if _, err := open.Do(req2); err != nil {
		t.Fatalf("放行模式应能连本地服务，却失败: %v", err)
	}
}
