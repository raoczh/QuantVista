package datasource

import (
	"net"
	"net/http"
	"strings"
	"time"
)

// 适配层共享 HTTP 客户端：带超时，避免上游卡死拖垮整个请求。
var httpClient = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     60 * time.Second,
	},
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

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
