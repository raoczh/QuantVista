package datasource

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// TencentAdapter 腾讯财经 qt.gtimg.cn 实时行情。
// 实现行情、估值、指数及前复权日线；指数日线使用明确的原始 day 口径。
type TencentAdapter struct{}

func NewTencentAdapter() *TencentAdapter { return &TencentAdapter{} }

func (t *TencentAdapter) Name() string { return "tencent" }

// fetchFields 请求 qt.gtimg.cn 并切出 ~ 分隔的字段数组（GBK 转码）。
func (adapter *TencentAdapter) fetchFields(ctx context.Context, market, symbol string) ([]string, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, valid := sinaCNSymbol(symbol) // 腾讯与新浪同为 sh/sz 前缀
	if !valid {
		return nil, ErrSymbolInvalid
	}
	records, err := fetchTencentQuotes(ctx, []string{code})
	if err != nil {
		return nil, err
	}
	fields, exists := records[code]
	if !exists {
		return nil, ErrNoData
	}
	return fields, nil
}

// fetchTencentQuotes preserves the exchange-qualified assignment key. A missing
// or reordered response cannot be attached to a different stock or index.
func fetchTencentQuotes(ctx context.Context, codes []string) (map[string][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestURL := "https://qt.gtimg.cn/q=" + strings.Join(codes, ",")
	raw, status, err := doGet(ctx, requestURL, map[string]string{"Referer": "https://gu.qq.com/"})
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, fmt.Errorf("%w: tencent quotes: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 转码失败 %v", ErrUpstream, err)
	}
	requested := make(map[string]bool, len(codes))
	for _, code := range codes {
		requested[code] = true
	}
	records := make(map[string][]string, len(codes))
	for _, assignment := range strings.Split(string(decoded), ";") {
		variable, value, found := strings.Cut(strings.TrimSpace(assignment), "=")
		if !found {
			continue
		}
		variable = strings.TrimPrefix(strings.TrimSpace(variable), "var ")
		code, hasPrefix := strings.CutPrefix(variable, "v_")
		if !hasPrefix || !requested[code] {
			continue
		}
		fields, parseErr := parseTencentFields(value)
		if parseErr != nil {
			continue
		}
		if len(code) != 8 || strings.TrimSpace(fields[2]) != code[2:] {
			return nil, fmt.Errorf("%w: tencent quote identity mismatch", ErrUpstream)
		}
		if previous, exists := records[code]; exists && !slices.Equal(previous, fields) {
			return nil, fmt.Errorf("%w: tencent quote has conflicting duplicates", ErrUpstream)
		}
		records[code] = fields
	}
	if len(records) == 0 {
		return nil, ErrNoData
	}
	return records, nil
}

// parseTencentFields 从 v_shXXXXXX="1~名称~代码~..." 中切出字段（独立函数便于 fixture 单测）。
func parseTencentFields(text string) ([]string, error) {
	startQuote := strings.Index(text, "\"")
	endQuote := strings.LastIndex(text, "\"")
	if startQuote < 0 || endQuote <= startQuote {
		return nil, ErrNoData
	}
	fields := strings.Split(text[startQuote+1:endQuote], "~")
	if len(fields) < 38 {
		return nil, ErrNoData // 停牌/非法代码字段不全
	}
	return fields, nil
}

// tencentAtof 越界安全的字段转 float。
func tencentAtof(fields []string, fieldIndex int) float64 {
	if fieldIndex < 0 || fieldIndex >= len(fields) {
		return 0
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(fields[fieldIndex]), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

func tencentScaledField(fields []string, fieldIndex int, scale float64) float64 {
	value := tencentAtof(fields, fieldIndex) * scale
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

// tencentDataTime 解析字段 30 的行情时间（yyyyMMddHHmmss）。解析失败返回零值
// （timestamp_unknown）：回填当前时间会把旧价伪装成实时价，零值由新鲜度判定恒判 stale。
func tencentDataTime(fields []string) time.Time {
	if len(fields) > 30 {
		timestamp := strings.TrimSpace(fields[30])
		parsed, err := time.ParseInLocation("20060102150405", timestamp, ChinaMarketLocation())
		if err == nil && len(timestamp) == 14 && parsed.Format("20060102150405") == timestamp {
			return parsed
		}
	}
	return time.Time{}
}

func (adapter *TencentAdapter) GetQuote(ctx context.Context, market, symbol string) (*Quote, error) {
	fields, err := adapter.fetchFields(ctx, market, symbol)
	if err != nil {
		return nil, err
	}

	price := tencentAtof(fields, 3)
	if price <= 0 {
		return nil, ErrNoData
	}
	// 成交额优先取 "价/量/额"（字段 35）的第三段（单位元），更精确。
	amount := 0.0
	if segments := strings.Split(fields[35], "/"); len(segments) >= 3 {
		amount = math.Max(0, tencentAtof(segments, 2))
	}
	volume := tencentAtof(fields, 6)
	if volume < 0 || volume >= float64(math.MaxInt64) {
		return nil, fmt.Errorf("%w: tencent quote has invalid volume", ErrUpstream)
	}

	return &Quote{
		Symbol:    symbol,
		Market:    market,
		Name:      strings.TrimSpace(fields[1]),
		Price:     price,
		ChangePct: tencentAtof(fields, 32),
		Open:      tencentAtof(fields, 5),
		High:      tencentAtof(fields, 33),
		Low:       tencentAtof(fields, 34),
		PrevClose: tencentAtof(fields, 4),
		Volume:    int64(volume), // 手
		Amount:    amount,
		Source:    adapter.Name(),
		DataTime:  tencentDataTime(fields),
	}, nil
}

// GetValuation 估值/盘面扩展快照：同一行情串的 38~53 号字段（换手率/PE-TTM/振幅/
// 流通市值/总市值/PB/涨停价/跌停价/量比/PE 动/PE 静），市值源单位为亿、统一转元。
func (adapter *TencentAdapter) GetValuation(ctx context.Context, market, symbol string) (*Valuation, error) {
	fields, err := adapter.fetchFields(ctx, market, symbol)
	if err != nil {
		return nil, err
	}
	return parseTencentValuation(fields, market, symbol, adapter.Name())
}

// parseTencentValuation 从字段数组组装估值快照（独立函数便于 fixture 单测）。
func parseTencentValuation(fields []string, market, symbol, source string) (*Valuation, error) {
	price := tencentAtof(fields, 3)
	if price <= 0 {
		return nil, ErrNoData
	}
	name := strings.TrimSpace(fields[1])
	return &Valuation{
		Symbol:       symbol,
		Market:       market,
		Name:         name,
		TurnoverRate: tencentAtof(fields, 38),
		PETTM:        tencentAtof(fields, 39),
		Amplitude:    tencentAtof(fields, 43),
		FloatCap:     tencentScaledField(fields, 44, 1e8), // 源单位：亿
		TotalCap:     tencentScaledField(fields, 45, 1e8),
		PB:           tencentAtof(fields, 46),
		LimitUp:      tencentAtof(fields, 47),
		LimitDown:    tencentAtof(fields, 48),
		VolumeRatio:  tencentAtof(fields, 49),
		PEDynamic:    tencentAtof(fields, 52),
		PEStatic:     tencentAtof(fields, 53),
		IsST:         strings.Contains(strings.ToUpper(name), "ST"),
		Source:       source,
		DataTime:     tencentDataTime(fields),
	}, nil
}
