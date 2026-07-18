package datasource

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// TencentAdapter 腾讯财经 qt.gtimg.cn 实时行情。
// 独立于东财/新浪的第三源，稳定性好；实现实时快照与估值扩展（行情串 38~53 号
// 字段自带换手率/PE/振幅/市值/PB/涨跌停价/量比），日线回退到新浪。
type TencentAdapter struct{}

func NewTencentAdapter() *TencentAdapter { return &TencentAdapter{} }

func (t *TencentAdapter) Name() string { return "tencent" }

// fetchFields 请求 qt.gtimg.cn 并切出 ~ 分隔的字段数组（GBK 转码）。
func (t *TencentAdapter) fetchFields(ctx context.Context, market, symbol string) ([]string, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, ok := sinaCNSymbol(symbol) // 腾讯与新浪同为 sh/sz 前缀
	if !ok {
		return nil, ErrSymbolInvalid
	}

	url := "https://qt.gtimg.cn/q=" + code
	raw, status, err := doGet(ctx, url, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 转码失败 %v", ErrUpstream, err)
	}
	return parseTencentFields(string(decoded))
}

// parseTencentFields 从 v_shXXXXXX="1~名称~代码~..." 中切出字段（独立函数便于 fixture 单测）。
func parseTencentFields(text string) ([]string, error) {
	a := strings.Index(text, "\"")
	b := strings.LastIndex(text, "\"")
	if a < 0 || b <= a {
		return nil, ErrNoData
	}
	f := strings.Split(text[a+1:b], "~")
	if len(f) < 38 {
		return nil, ErrNoData // 停牌/非法代码字段不全
	}
	return f, nil
}

// tencentAtof 越界安全的字段转 float。
func tencentAtof(f []string, i int) float64 {
	if i >= len(f) {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(f[i]), 64)
	return v
}

// tencentDataTime 解析字段 30 的行情时间（yyyyMMddHHmmss）。解析失败返回零值
// （timestamp_unknown）：回填当前时间会把旧价伪装成实时价，零值由新鲜度判定恒判 stale。
func tencentDataTime(f []string) time.Time {
	if len(f) > 30 {
		if tm, err := time.ParseInLocation("20060102150405", f[30], time.Local); err == nil {
			return tm
		}
	}
	return time.Time{}
}

func (t *TencentAdapter) GetQuote(ctx context.Context, market, symbol string) (*Quote, error) {
	f, err := t.fetchFields(ctx, market, symbol)
	if err != nil {
		return nil, err
	}

	price := tencentAtof(f, 3)
	if price == 0 {
		return nil, ErrNoData
	}
	// 成交额优先取 "价/量/额"（字段 35）的第三段（单位元），更精确。
	amount := 0.0
	if seg := strings.Split(f[35], "/"); len(seg) >= 3 {
		amount, _ = strconv.ParseFloat(seg[2], 64)
	}

	return &Quote{
		Symbol:    symbol,
		Market:    market,
		Name:      strings.TrimSpace(f[1]),
		Price:     price,
		ChangePct: tencentAtof(f, 32),
		Open:      tencentAtof(f, 5),
		High:      tencentAtof(f, 33),
		Low:       tencentAtof(f, 34),
		PrevClose: tencentAtof(f, 4),
		Volume:    int64(tencentAtof(f, 6)), // 手
		Amount:    amount,
		Source:    t.Name(),
		DataTime:  tencentDataTime(f),
	}, nil
}

// GetValuation 估值/盘面扩展快照：同一行情串的 38~53 号字段（换手率/PE-TTM/振幅/
// 流通市值/总市值/PB/涨停价/跌停价/量比/PE 动/PE 静），市值源单位为亿、统一转元。
func (t *TencentAdapter) GetValuation(ctx context.Context, market, symbol string) (*Valuation, error) {
	f, err := t.fetchFields(ctx, market, symbol)
	if err != nil {
		return nil, err
	}
	return parseTencentValuation(f, market, symbol, t.Name())
}

// parseTencentValuation 从字段数组组装估值快照（独立函数便于 fixture 单测）。
func parseTencentValuation(f []string, market, symbol, source string) (*Valuation, error) {
	price := tencentAtof(f, 3)
	if price == 0 {
		return nil, ErrNoData
	}
	name := strings.TrimSpace(f[1])
	return &Valuation{
		Symbol:       symbol,
		Market:       market,
		Name:         name,
		TurnoverRate: tencentAtof(f, 38),
		PETTM:        tencentAtof(f, 39),
		Amplitude:    tencentAtof(f, 43),
		FloatCap:     tencentAtof(f, 44) * 1e8, // 源单位：亿
		TotalCap:     tencentAtof(f, 45) * 1e8,
		PB:           tencentAtof(f, 46),
		LimitUp:      tencentAtof(f, 47),
		LimitDown:    tencentAtof(f, 48),
		VolumeRatio:  tencentAtof(f, 49),
		PEDynamic:    tencentAtof(f, 52),
		PEStatic:     tencentAtof(f, 53),
		IsST:         strings.Contains(strings.ToUpper(name), "ST"),
		Source:       source,
		DataTime:     tencentDataTime(f),
	}, nil
}

// GetDailyBars 腾讯日线接口字段不稳，骨架不实现，由 manager 回退到新浪日线。
func (t *TencentAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	return nil, ErrNotSupported
}
