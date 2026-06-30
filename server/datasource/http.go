package datasource

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 适配层共享 HTTP 客户端：带超时，避免上游卡死拖垮整个请求。
// 禁用 HTTP/2：东财等公开接口在 H2 下偶发连接被对端重置(EOF)，强制 HTTP/1.1 更稳。
var httpClient = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: false,
		// 置空 TLSNextProto 即关闭 HTTP/2 协商，连接固定走 HTTP/1.1。
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     60 * time.Second,
	},
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// doGet 统一 GET：注入浏览器头 + 调用方附加头，对网络瞬时错误(含 EOF)重试一次。
// 返回原始字节（GBK 等解码交给调用方）。
func doGet(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Connection", "keep-alive")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue // 网络瞬时错误（EOF/超时/连接重置）重试一次
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		return body, resp.StatusCode, nil
	}
	return nil, 0, lastErr
}

// cnSecid 把 A 股代码转换为东财 secid（沪市 1.、深市 0.）。
// 规则（社区惯例）：6/5/9 开头 -> 沪市；0/2/3 开头 -> 深市。
func cnSecid(symbol string) (string, bool) {
	s := strings.TrimSpace(symbol)
	if len(s) != 6 {
		return "", false
	}
	switch s[0] {
	case '6', '5', '9':
		return "1." + s, true
	case '0', '2', '3':
		return "0." + s, true
	default:
		return "", false
	}
}

// sinaCNSymbol 把 A 股代码转换为新浪前缀代码（sh/sz）。
func sinaCNSymbol(symbol string) (string, bool) {
	s := strings.TrimSpace(symbol)
	if len(s) != 6 {
		return "", false
	}
	switch s[0] {
	case '6', '5', '9':
		return "sh" + s, true
	case '0', '2', '3':
		return "sz" + s, true
	default:
		return "", false
	}
}
