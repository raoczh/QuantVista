package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TencentMaxDailyBars bounds both the upstream request and the returned window.
// A 2026-11-02 check with count=640 returned 641 rows, including today's bar.
const TencentMaxDailyBars = 640

var chinaMarketLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// Minimal deployments may not ship tzdata. Modern CN market time is UTC+8.
		return time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	return location
}()

// ChinaMarketLocation is independent of the host's local timezone.
func ChinaMarketLocation() *time.Location {
	return chinaMarketLocation
}

// ValidateDailyBars validates the complete input before sorting a copy by date.
// Exact duplicates are collapsed; conflicting rows or invalid values reject the
// whole series. Source and missing optional fields (zero amount/turnover) are
// preserved. Nonpositive adjusted prices are not repaired into invented prices.
func ValidateDailyBars(bars []Bar) ([]Bar, error) {
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	validated := make([]Bar, 0, len(bars))
	seen := make(map[string]Bar, len(bars))
	for rowIndex, bar := range bars {
		tradeDate, err := time.ParseInLocation(time.DateOnly, bar.TradeDate, ChinaMarketLocation())
		if err != nil || tradeDate.Year() < 1 || tradeDate.Format(time.DateOnly) != bar.TradeDate {
			return nil, fmt.Errorf("%w: daily bar row %d has an invalid trade date", ErrUpstream, rowIndex+1)
		}
		for _, price := range []float64{bar.Open, bar.Close, bar.High, bar.Low} {
			if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
				return nil, fmt.Errorf("%w: daily bar %s has a nonpositive or nonfinite price", ErrUpstream, bar.TradeDate)
			}
		}
		if bar.High < bar.Low || bar.High < bar.Open || bar.High < bar.Close ||
			bar.Low > bar.Open || bar.Low > bar.Close {
			return nil, fmt.Errorf("%w: daily bar %s has inconsistent OHLC prices", ErrUpstream, bar.TradeDate)
		}
		if bar.Volume < 0 || bar.Amount < 0 || bar.TurnoverRate < 0 ||
			math.IsNaN(bar.Amount) || math.IsInf(bar.Amount, 0) ||
			math.IsNaN(bar.TurnoverRate) || math.IsInf(bar.TurnoverRate, 0) {
			return nil, fmt.Errorf("%w: daily bar %s has invalid volume, amount or turnover", ErrUpstream, bar.TradeDate)
		}
		if previous, exists := seen[bar.TradeDate]; exists {
			if previous != bar {
				return nil, fmt.Errorf("%w: daily bar %s has conflicting duplicates", ErrUpstream, bar.TradeDate)
			}
			continue
		}
		seen[bar.TradeDate] = bar
		validated = append(validated, bar)
	}
	sort.Slice(validated, func(leftIndex, rightIndex int) bool {
		return validated[leftIndex].TradeDate < validated[rightIndex].TradeDate
	})
	return validated, nil
}

func tencentDailyLimit(limit, defaultLimit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > TencentMaxDailyBars {
		return TencentMaxDailyBars
	}
	return limit
}

// GetDailyBars only accepts qfqday, never the unadjusted day fallback. Tencent
// supplies volume in lots; amount and turnover are unavailable and remain zero.
func (adapter *TencentAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, valid := sinaCNSymbol(symbol)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	return adapter.fetchDailyBars(ctx, code, "qfqday", tencentDailyLimit(limit, 120))
}

// fetchDailyBars also serves the explicit sh000001 benchmark. Selecting the key
// determines adjustment semantics, rather than trying whichever key is present.
func (adapter *TencentAdapter) fetchDailyBars(ctx context.Context, code, dataKey string, limit int) ([]Bar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	adjustment := ""
	switch dataKey {
	case "qfqday":
		adjustment = "qfq"
	case "day":
	default:
		return nil, ErrNotSupported
	}
	parameters := url.Values{}
	parameters.Set("param", fmt.Sprintf("%s,day,,,%d,%s", code, limit, adjustment))
	requestURL := "https://web.ifzq.gtimg.cn/appstock/app/fqkline/get?" + parameters.Encode()
	body, status, err := doGet(ctx, requestURL, map[string]string{"Referer": "https://gu.qq.com/"})
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, fmt.Errorf("%w: tencent daily request: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: tencent daily http %d", ErrUpstream, status)
	}
	bars, err := parseTencentDailyResponse(body, code, dataKey)
	if err != nil {
		return nil, err
	}
	// The endpoint can append today's incomplete bar beyond the requested count.
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}
	return bars, nil
}

func parseTencentDailyResponse(raw []byte, code, dataKey string) ([]Bar, error) {
	var response struct {
		Code *int                                  `json:"code"`
		Data map[string]map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("%w: tencent daily JSON: %v", ErrUpstream, err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, fmt.Errorf("%w: tencent daily response has no successful code", ErrUpstream)
	}
	series := response.Data[code][dataKey]
	if len(series) == 0 || string(series) == "null" {
		return nil, ErrNoData
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(series, &rows); err != nil {
		return nil, fmt.Errorf("%w: tencent daily rows: %v", ErrUpstream, err)
	}
	bars := make([]Bar, 0, len(rows))
	for rowIndex, row := range rows {
		if len(row) < 6 {
			return nil, fmt.Errorf("%w: tencent daily row %d has fewer than six fields", ErrUpstream, rowIndex+1)
		}
		var tradeDate string
		if err := json.Unmarshal(row[0], &tradeDate); err != nil {
			return nil, fmt.Errorf("%w: tencent daily row %d has an invalid date field", ErrUpstream, rowIndex+1)
		}
		var values [5]float64
		for fieldIndex := range values {
			value, valid := tencentDailyNumber(row[fieldIndex+1])
			if !valid {
				return nil, fmt.Errorf("%w: tencent daily row %d field %d is not finite numeric data", ErrUpstream, rowIndex+1, fieldIndex+1)
			}
			values[fieldIndex] = value
		}
		volume := values[4]
		if volume < 0 || volume >= float64(math.MaxInt64) || math.Trunc(volume) != volume {
			return nil, fmt.Errorf("%w: tencent daily row %d has an invalid lot count", ErrUpstream, rowIndex+1)
		}
		bars = append(bars, Bar{
			TradeDate: tradeDate,
			Open:      values[0],
			Close:     values[1],
			High:      values[2],
			Low:       values[3],
			Volume:    int64(volume),
			Source:    "tencent",
		})
	}
	return ValidateDailyBars(bars)
}

func tencentDailyNumber(raw json.RawMessage) (float64, bool) {
	text := strings.TrimSpace(string(raw))
	if len(text) > 0 && text[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, false
		}
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}
