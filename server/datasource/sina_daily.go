package datasource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SinaMaxDailyBars bounds the raw daily endpoint; it is not full listing history.
const SinaMaxDailyBars = 1023

type sinaQFQFactor struct {
	date  string
	value float64
}

// GetDailyBars applies Sina's ratio-based forward adjustment. Its price basis
// differs from Tencent's cash adjustment, so consumers must rebase the entire
// series on a source-family switch. Never use legacy "sina" for these bars.
func (adapter *SinaAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, valid := sinaCNSymbol(symbol)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	// The verified factor and two-decimal price contract is for A shares.
	// ETFs need their own adjustment/three-decimal validation, not stock rules.
	if !cnAShareCode(symbol) {
		return nil, ErrNotSupported
	}
	if limit <= 0 {
		limit = 120
	}
	limit = min(limit, SinaMaxDailyBars)
	// Both dependent requests share a total budget and the common Sina limiter.
	ctx, cancel := context.WithTimeout(ctx, httpRequestBudget)
	defer cancel()
	bars, err := adapter.fetchRawDailyBars(ctx, code, limit)
	if err != nil {
		return nil, err
	}
	endpoint := "https://finance.sina.com.cn/realstock/company/" + code + "/qfq.js"
	body, status, err := doGet(ctx, endpoint, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, fmt.Errorf("%w: sina adjustment request: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: sina adjustment HTTP %d", ErrUpstream, status)
	}
	factors, err := parseSinaQFQFactors(body, code)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return applySinaForwardAdjustment(bars, factors)
}

// fetchRawDailyBars is private: unadjusted equity bars must never leak into the
// public daily route. Benchmark indices retain their separate raw-price path.
func (adapter *SinaAdapter) fetchRawDailyBars(ctx context.Context, code string, limit int) ([]Bar, error) {
	endpoint := fmt.Sprintf("https://money.finance.sina.com.cn/quotes_service/api/json_v2.php/CN_MarketData.getKLineData?symbol=%s&scale=240&ma=no&datalen=%d", code, limit)
	body, status, err := doGet(ctx, endpoint, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, fmt.Errorf("%w: sina raw daily request: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: sina raw daily HTTP %d", ErrUpstream, status)
	}
	bars, err := parseSinaRawDailyResponse(body, time.Now())
	if err != nil {
		return nil, err
	}
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}
	return bars, nil
}

func parseSinaRawDailyResponse(body []byte, observedAt time.Time) ([]Bar, error) {
	var rows []struct {
		Day    string `json:"day"`
		Open   string `json:"open"`
		High   string `json:"high"`
		Low    string `json:"low"`
		Close  string `json:"close"`
		Volume string `json:"volume"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: sina daily JSON: %v", ErrUpstream, err)
	}
	if len(rows) > SinaMaxDailyBars {
		return nil, fmt.Errorf("%w: sina daily response exceeds row limit", ErrUpstream)
	}
	// Keep shares when detecting conflicts: 101 and 199 both truncate to one lot.
	seenShares := make(map[string]float64, len(rows))
	bars := make([]Bar, 0, len(rows))
	for rowIndex, row := range rows {
		if _, valid := parseDailyFactDate(row.Day, observedAt); !valid {
			return nil, fmt.Errorf("%w: sina daily row %d has an invalid date", ErrUpstream, rowIndex+1)
		}
		values := [5]float64{}
		for fieldIndex, text := range []string{row.Open, row.High, row.Low, row.Close, row.Volume} {
			value, valid := parseSinaFiniteNumber(text)
			if !valid {
				return nil, fmt.Errorf("%w: sina daily row %d field %d is not finite numeric data", ErrUpstream, rowIndex+1, fieldIndex+1)
			}
			values[fieldIndex] = value
		}
		shares := values[4]
		if shares < 0 || shares >= float64(math.MaxInt64) || math.Trunc(shares) != shares {
			return nil, fmt.Errorf("%w: sina daily row %d has invalid shares", ErrUpstream, rowIndex+1)
		}
		if previous, exists := seenShares[row.Day]; exists && previous != shares {
			return nil, fmt.Errorf("%w: sina daily has conflicting share volumes", ErrUpstream)
		}
		seenShares[row.Day] = shares
		bars = append(bars, Bar{
			TradeDate: row.Day, Open: values[0], High: values[1], Low: values[2], Close: values[3],
			Volume: int64(shares) / 100, Source: "sina_raw",
		})
	}
	return ValidateDailyBars(bars)
}

// parseSinaQFQFactors treats the assignment as inert data, never JavaScript.
// Only the exact requested variable, JSON and a harmless suffix are accepted.
func parseSinaQFQFactors(body []byte, code string) ([]sinaQFQFactor, error) {
	text := strings.TrimSpace(string(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))))
	if !strings.HasPrefix(text, "var") || len(text) <= 3 || !strings.ContainsRune(" \t\r\n", rune(text[3])) {
		return nil, fmt.Errorf("%w: sina adjustment assignment missing", ErrUpstream)
	}
	text = strings.TrimSpace(text[3:])
	variable := code + "qfq"
	if !strings.HasPrefix(text, variable) {
		return nil, fmt.Errorf("%w: sina adjustment symbol mismatch", ErrUpstream)
	}
	text = strings.TrimSpace(text[len(variable):])
	if !strings.HasPrefix(text, "=") {
		return nil, fmt.Errorf("%w: sina adjustment assignment invalid", ErrUpstream)
	}
	text = strings.TrimSpace(text[1:])
	var response struct {
		Total *int `json:"total"`
		Data  []struct {
			Date   string `json:"d"`
			Factor string `json:"f"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("%w: sina adjustment JSON: %v", ErrUpstream, err)
	}
	if !validSinaAssignmentSuffix(text[decoder.InputOffset():]) {
		return nil, fmt.Errorf("%w: sina adjustment contains executable or invalid suffix", ErrUpstream)
	}
	if response.Total == nil || *response.Total <= 0 || *response.Total != len(response.Data) || len(response.Data) > SinaMaxDailyBars {
		return nil, fmt.Errorf("%w: sina adjustment factors missing or incomplete", ErrUpstream)
	}
	factors := make([]sinaQFQFactor, 0, len(response.Data))
	seen := make(map[string]float64, len(response.Data))
	today := time.Now().In(ChinaMarketLocation()).Format(time.DateOnly)
	for _, row := range response.Data {
		date, err := time.ParseInLocation(time.DateOnly, row.Date, ChinaMarketLocation())
		value, valid := parseSinaFiniteNumber(row.Factor)
		// The sentinel is a real coverage anchor, not a guessed factor of one.
		if err != nil || date.Format(time.DateOnly) != row.Date || row.Date > today || (date.Year() < 1990 && row.Date != "1900-01-01") || !valid || value <= 0 {
			return nil, fmt.Errorf("%w: sina adjustment has invalid date or factor", ErrUpstream)
		}
		if previous, exists := seen[row.Date]; exists {
			if previous != value {
				return nil, fmt.Errorf("%w: sina adjustment has conflicting duplicate factors", ErrUpstream)
			}
			continue
		}
		seen[row.Date] = value
		factors = append(factors, sinaQFQFactor{date: row.Date, value: value})
	}
	sort.Slice(factors, func(leftIndex, rightIndex int) bool { return factors[leftIndex].date < factors[rightIndex].date })
	return factors, nil
}

func validSinaAssignmentSuffix(suffix string) bool {
	semicolonSeen := false
	for {
		suffix = strings.TrimSpace(suffix)
		switch {
		case suffix == "":
			return true
		case strings.HasPrefix(suffix, ";") && !semicolonSeen:
			semicolonSeen = true
			suffix = suffix[1:]
		case strings.HasPrefix(suffix, "/*"):
			end := strings.Index(suffix[2:], "*/")
			if end < 0 {
				return false
			}
			suffix = suffix[end+4:]
		case strings.HasPrefix(suffix, "//"):
			end := strings.IndexAny(suffix, "\r\n\u2028\u2029")
			if end < 0 {
				return true
			}
			suffix = suffix[end:]
		default:
			return false
		}
	}
}

func applySinaForwardAdjustment(bars []Bar, factors []sinaQFQFactor) ([]Bar, error) {
	adjusted := append([]Bar(nil), bars...)
	for index := range adjusted {
		bar := &adjusted[index]
		// The largest effective date <= the candle date includes the NEW factor
		// on an ex-date, even if a factor became effective over a weekend.
		factorIndex := sort.Search(len(factors), func(index int) bool { return factors[index].date > bar.TradeDate }) - 1
		if factorIndex < 0 {
			return nil, fmt.Errorf("%w: sina adjustment does not cover %s", ErrUpstream, bar.TradeDate)
		}
		factor := factors[factorIndex].value
		if factor <= 0 || math.IsNaN(factor) || math.IsInf(factor, 0) {
			return nil, fmt.Errorf("%w: sina adjustment factor is not positive and finite", ErrUpstream)
		}
		bar.Open = math.RoundToEven(bar.Open/factor*100) / 100
		bar.High = math.RoundToEven(bar.High/factor*100) / 100
		bar.Low = math.RoundToEven(bar.Low/factor*100) / 100
		bar.Close = math.RoundToEven(bar.Close/factor*100) / 100
		bar.Source = "sina_qfq"
	}
	return ValidateDailyBars(adjusted)
}
