package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var sinaMarketLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// A single sh600000 request returned 800 bars on 2026-11-02.
const sinaMin5MaxCount = 800

// GetMin5Bars returns actual, unadjusted five-minute bars, never one-minute data.
// Volume is converted from shares to lots; missing history is not synthesized.
// count defaults to 60 and is bounded by the verified Sina request size.
func (adapter *SinaAdapter) GetMin5Bars(ctx context.Context, market, symbol string, count int) ([]Min5Bar, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	quoteCode, valid := sinaCNSymbol(symbol)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if count <= 0 {
		count = 60
	}
	count = min(count, sinaMin5MaxCount)
	endpoint := fmt.Sprintf("https://money.finance.sina.com.cn/quotes_service/api/json_v2.php/CN_MarketData.getKLineData?symbol=%s&scale=5&ma=no&datalen=%d", quoteCode, count)
	body, status, err := doGet(ctx, endpoint, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, fmt.Errorf("%w: sina min5 request: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: sina min5 HTTP %d", ErrUpstream, status)
	}
	bars, err := parseSinaMin5Response(body, time.Now())
	if err != nil {
		return nil, err
	}
	if len(bars) > count {
		bars = bars[len(bars)-count:]
	}
	return bars, nil
}

func parseSinaMin5Response(body []byte, observedAt time.Time) ([]Min5Bar, error) {
	var rows []struct {
		Day    string `json:"day"`
		Open   string `json:"open"`
		High   string `json:"high"`
		Low    string `json:"low"`
		Close  string `json:"close"`
		Volume string `json:"volume"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: sina min5 JSON: %v", ErrUpstream, err)
	}
	// Keep raw share volume for duplicate checks: 101 and 199 shares both round to one lot.
	type minuteSample struct {
		bar          Min5Bar
		volumeShares float64
	}
	seen := make(map[string]minuteSample, len(rows))
	bars := make([]Min5Bar, 0, len(rows))
	for rowIndex, row := range rows {
		barTime, validTime := parseSinaDateTime(row.Day)
		if !validTime || !isSinaMin5End(barTime) {
			return nil, fmt.Errorf("%w: sina min5 row %d has an invalid five-minute timestamp", ErrUpstream, rowIndex+1)
		}
		if barTime.Format("2006-01-02") > observedAt.In(sinaMarketLocation).Format("2006-01-02") {
			return nil, fmt.Errorf("%w: sina min5 contains a future date", ErrUpstream)
		}
		openPrice, validOpen := parseSinaFiniteNumber(row.Open)
		highPrice, validHigh := parseSinaFiniteNumber(row.High)
		lowPrice, validLow := parseSinaFiniteNumber(row.Low)
		closePrice, validClose := parseSinaFiniteNumber(row.Close)
		volumeShares, validVolume := parseSinaFiniteNumber(row.Volume)
		if !validOpen || !validHigh || !validLow || !validClose ||
			!validSinaOHLC(openPrice, highPrice, lowPrice, closePrice) ||
			!validVolume || volumeShares < 0 || volumeShares/100 >= float64(math.MaxInt64) {
			return nil, fmt.Errorf("%w: sina min5 row %d has invalid OHLC or volume", ErrUpstream, rowIndex+1)
		}
		bar := Min5Bar{
			Time: barTime.Format("200601021504"), Open: openPrice, High: highPrice,
			Low: lowPrice, Close: closePrice, Volume: int64(volumeShares / 100), Source: "sina",
		}
		sample := minuteSample{bar: bar, volumeShares: volumeShares}
		if previous, exists := seen[bar.Time]; exists {
			if previous != sample {
				return nil, fmt.Errorf("%w: sina min5 has conflicting duplicate timestamps", ErrUpstream)
			}
			continue
		}
		seen[bar.Time] = sample
		if barTime.After(observedAt) {
			continue // An intraday response may contain a still-open, end-stamped bar.
		}
		bars = append(bars, bar)
	}
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	sort.Slice(bars, func(leftIndex, rightIndex int) bool { return bars[leftIndex].Time < bars[rightIndex].Time })
	return bars, nil
}

func parseSinaFiniteNumber(value string) (float64, bool) {
	number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func validSinaOHLC(openPrice, highPrice, lowPrice, closePrice float64) bool {
	return openPrice > 0 && highPrice > 0 && lowPrice > 0 && closePrice > 0 &&
		highPrice >= openPrice && highPrice >= closePrice && lowPrice <= openPrice && lowPrice <= closePrice
}

func parseSinaDateTime(value string) (time.Time, bool) {
	const layout = "2006-01-02 15:04:05"
	value = strings.TrimSpace(value)
	parsed, err := time.ParseInLocation(layout, value, sinaMarketLocation)
	return parsed, err == nil && parsed.Year() >= 1990 && parsed.Format(layout) == value
}

func isSinaMin5End(timestamp time.Time) bool {
	if timestamp.Second() != 0 || timestamp.Minute()%5 != 0 ||
		timestamp.Weekday() == time.Saturday || timestamp.Weekday() == time.Sunday {
		return false
	}
	minuteOfDay := timestamp.Hour()*60 + timestamp.Minute()
	return minuteOfDay >= 9*60+35 && minuteOfDay <= 11*60+30 ||
		minuteOfDay >= 13*60+5 && minuteOfDay <= 15*60
}
