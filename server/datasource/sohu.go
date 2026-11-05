package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SohuMaxDailyFactsDays is our inclusive request-window cap, not an upstream
// retention guarantee. Historical windows are supported from 1990 onward;
// larger backfills must be deliberately chunked by their caller, not retried as
// an unbounded history download. Automatic enrichment uses one window only.
const SohuMaxDailyFactsDays = 400

// DailyTradingFact contains independently observed, unadjusted trading facts.
// Prices are intentionally absent: Sohu's raw OHLC cannot be an adjusted source.
type DailyTradingFact struct {
	Market       string  `json:"market"`
	Symbol       string  `json:"symbol"`
	TradeDate    string  `json:"trade_date"`
	Volume       int64   `json:"volume"`        // Lots, never adjustment-scaled.
	Amount       float64 `json:"amount"`        // CNY, not ten-thousand CNY.
	TurnoverRate float64 `json:"turnover_rate"` // Percent, not a fraction.
	Source       string  `json:"source"`
}

// DailyTradingFactsProvider is optional. Dates are inclusive YYYY-MM-DD values.
// Missing fields remain zero; price sources and fact sources are independent.
type DailyTradingFactsProvider interface {
	GetDailyTradingFacts(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error)
}

type SohuAdapter struct{}

func NewSohuAdapter() *SohuAdapter        { return &SohuAdapter{} }
func (adapter *SohuAdapter) Name() string { return "sohu" }

func (adapter *SohuAdapter) GetQuote(context.Context, string, string) (*Quote, error) {
	return nil, ErrNotSupported
}

func (adapter *SohuAdapter) GetDailyBars(context.Context, string, string, int) ([]Bar, error) {
	return nil, ErrNotSupported
}

func (adapter *SohuAdapter) GetDailyTradingFacts(ctx context.Context, market, symbol, from, to string) ([]DailyTradingFact, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	symbol = strings.TrimSpace(symbol)
	if !validSohuStockSymbol(symbol) {
		return nil, ErrSymbolInvalid
	}
	observedAt := time.Now()
	fromDate, toDate, err := validateDailyFactsWindow(from, to, observedAt)
	if err != nil {
		return nil, err
	}
	parameters := url.Values{
		"code": {"cn_" + symbol}, "start": {fromDate.Format("20060102")}, "end": {toDate.Format("20060102")},
		"stat": {"1"}, "order": {"A"}, "period": {"d"}, "rt": {"json"},
	}
	body, status, err := doGet(ctx, "https://q.stock.sohu.com/hisHq?"+parameters.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: sohu daily facts request: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: sohu daily facts HTTP %d", ErrUpstream, status)
	}
	return parseSohuDailyFacts(body, symbol, from, to, observedAt)
}

func validSohuStockSymbol(symbol string) bool {
	// The verified cn_ endpoint is used only for mainland A shares, not funds,
	// B shares or indices whose currency/identity contracts have not been checked.
	return cnAShareCode(symbol)
}

func parseDailyFactDate(value string, observedAt time.Time) (time.Time, bool) {
	date, err := time.ParseInLocation(time.DateOnly, value, ChinaMarketLocation())
	return date, err == nil && date.Year() >= 1990 && date.Format(time.DateOnly) == value &&
		value <= observedAt.In(ChinaMarketLocation()).Format(time.DateOnly)
}

func validateDailyFactsWindow(from, to string, observedAt time.Time) (time.Time, time.Time, error) {
	fromDate, validFrom := parseDailyFactDate(from, observedAt)
	toDate, validTo := parseDailyFactDate(to, observedAt)
	if !validFrom || !validTo || toDate.Before(fromDate) || toDate.After(fromDate.AddDate(0, 0, SohuMaxDailyFactsDays-1)) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: invalid daily facts window (maximum %d calendar days)", ErrNotSupported, SohuMaxDailyFactsDays)
	}
	return fromDate, toDate, nil
}

func parseSohuDailyFacts(body []byte, symbol, from, to string, observedAt time.Time) ([]DailyTradingFact, error) {
	if _, _, err := validateDailyFactsWindow(from, to, observedAt); err != nil {
		return nil, err
	}
	var envelopes []struct {
		Status *int       `json:"status"`
		Code   string     `json:"code"`
		Rows   [][]string `json:"hq"`
	}
	if err := json.Unmarshal(body, &envelopes); err != nil {
		return nil, fmt.Errorf("%w: sohu daily facts JSON: %v", ErrUpstream, err)
	}
	if len(envelopes) == 0 {
		return nil, ErrNoData
	}
	if len(envelopes) != 1 || envelopes[0].Status == nil || *envelopes[0].Status != 0 || envelopes[0].Code != "cn_"+symbol {
		return nil, fmt.Errorf("%w: sohu daily facts status or symbol mismatch", ErrUpstream)
	}
	rows := envelopes[0].Rows
	if len(rows) > SohuMaxDailyFactsDays {
		return nil, fmt.Errorf("%w: sohu daily facts exceeds row limit", ErrUpstream)
	}
	facts := make([]DailyTradingFact, 0, len(rows))
	previousDate := ""
	for rowIndex, row := range rows {
		if len(row) != 10 {
			return nil, fmt.Errorf("%w: sohu daily facts row %d has unexpected columns", ErrUpstream, rowIndex+1)
		}
		tradeDate, validDate := parseDailyFactDate(row[0], observedAt)
		if !validDate || row[0] < from || row[0] > to || row[0] <= previousDate || tradeDate.Weekday() == time.Saturday || tradeDate.Weekday() == time.Sunday {
			return nil, fmt.Errorf("%w: sohu daily facts has invalid dates or ascending order", ErrUpstream)
		}
		previousDate = row[0]
		values := [9]float64{}
		for fieldIndex, field := range row[1:] {
			if fieldIndex == 3 || fieldIndex == 8 {
				// A dash is an explicitly unavailable turnover, not a guessed zero.
				if fieldIndex == 8 && field == "-" {
					continue
				}
				if !strings.HasSuffix(field, "%") {
					return nil, fmt.Errorf("%w: sohu daily facts percent unit missing", ErrUpstream)
				}
				field = strings.TrimSuffix(field, "%")
			}
			value, valid := parseSinaFiniteNumber(field)
			if !valid {
				return nil, fmt.Errorf("%w: sohu daily facts row %d has nonfinite numeric data", ErrUpstream, rowIndex+1)
			}
			values[fieldIndex] = value
		}
		openPrice, closePrice, lowPrice, highPrice := values[0], values[1], values[4], values[5]
		volume, amount, turnover := values[6], values[7]*1e4, values[8]
		if !validSinaOHLC(openPrice, highPrice, lowPrice, closePrice) || volume < 0 || volume >= float64(math.MaxInt64) ||
			math.Trunc(volume) != volume || amount < 0 || math.IsInf(amount, 0) || turnover < 0 {
			return nil, fmt.Errorf("%w: sohu daily facts row %d has invalid OHLC, lots or facts", ErrUpstream, rowIndex+1)
		}
		facts = append(facts, DailyTradingFact{
			Market: "cn", Symbol: symbol, TradeDate: row[0], Volume: int64(volume),
			Amount: amount, TurnoverRate: turnover, Source: "sohu",
		})
	}
	if len(facts) == 0 {
		return nil, ErrNoData
	}
	return facts, nil
}
