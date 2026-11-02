package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const eastMoneyRankingFields = "f12,f14,f2,f3,f6,f8,f9,f21,f23"

// stock/get has a different field dictionary from clist. The quote website's
// newstatic/build/vendor.js maps PE dynamic/static/TTM to f162/f163/f164 and PB
// to f167. Verified with fltt=2 on 2026-11-02: prices/ratios are already decimal,
// f116/f117 are yuan, not the 100-million-yuan units used by Tencent.
const eastMoneyValuationFields = "f43,f50,f51,f52,f57,f58,f86,f116,f117,f162,f163,f164,f167,f168,f171"

// GetStockRanking uses the same four sort names and default limit as Sina.
// EastMoney po=0 is ascending, po=1 descending (opposite Sina's asc flag).
func (adapter *EastMoneyAdapter) GetStockRanking(ctx context.Context, market, sortField string, ascending bool, limit int) ([]StockRank, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	if limit <= 0 || limit > clistPageSize {
		limit = 10
	}
	fieldID := "f3"
	switch sortField {
	case "amount":
		fieldID = "f6"
	case "turnoverratio":
		fieldID = "f8"
	case "pb":
		fieldID = "f23"
	}
	order := "1"
	if ascending {
		order = "0"
	}
	parameters := url.Values{
		"pn": {"1"}, "pz": {strconv.Itoa(limit)}, "po": {order}, "np": {"1"},
		"fid": {fieldID}, "fltt": {"2"}, "invt": {"2"}, "ut": {clistUT},
		"fs": {clistSpotFS}, "fields": {eastMoneyRankingFields},
	}
	requestURL := fmt.Sprintf("https://%d.push2.eastmoney.com/api/qt/clist/get?%s", emNode(), parameters.Encode())
	body, status, err := adapter.get(ctx, requestURL, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, fmt.Errorf("%w: eastmoney ranking: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: eastmoney ranking http %d", ErrUpstream, status)
	}
	rows, err := parseEastMoneyStockRanking(body)
	if err != nil {
		return nil, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func parseEastMoneyStockRanking(body []byte) ([]StockRank, error) {
	var response struct {
		Code *int `json:"rc"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("%w: eastmoney ranking JSON: %v", ErrUpstream, err)
	}
	if response.Code != nil && *response.Code != 0 {
		return nil, fmt.Errorf("%w: eastmoney ranking rc=%d", ErrUpstream, *response.Code)
	}
	items, err := clistDiffItems(body)
	if err != nil {
		return nil, err
	}
	rows := make([]StockRank, 0, len(items))
	for _, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, fmt.Errorf("%w: eastmoney ranking row: %v", ErrUpstream, err)
		}
		symbol := eastMoneyText(fields["f12"])
		if _, valid := cnSecid(symbol); !valid {
			continue
		}
		price := eastMoneyOptionalNumber(fields["f2"])
		if price <= 0 {
			continue
		}
		rows = append(rows, StockRank{
			Symbol:       symbol,
			Name:         eastMoneyText(fields["f14"]),
			Price:        price,
			ChangePct:    eastMoneyOptionalNumber(fields["f3"]),
			Amount:       math.Max(0, eastMoneyOptionalNumber(fields["f6"])),
			TurnoverRate: math.Max(0, eastMoneyOptionalNumber(fields["f8"])),
			PE:           eastMoneyOptionalNumber(fields["f9"]), // Dynamic PE, not TTM.
			PB:           eastMoneyOptionalNumber(fields["f23"]),
			FloatCap:     math.Max(0, eastMoneyOptionalNumber(fields["f21"])), // Yuan.
			Source:       "eastmoney",
		})
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	return rows, nil
}

// GetValuation returns an independently sourced snapshot, without deriving any
// missing ratio or limit price. Individual missing fields do not erase a useful
// response; a response with no usable valuation fields is ErrNoData.
func (adapter *EastMoneyAdapter) GetValuation(ctx context.Context, market, symbol string) (*Valuation, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	secid, valid := cnSecid(symbol)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	parameters := url.Values{
		"secid": {secid}, "fields": {eastMoneyValuationFields}, "fltt": {"2"}, "invt": {"2"},
	}
	requestURL := fmt.Sprintf("https://%d.push2.eastmoney.com/api/qt/stock/get?%s", emNode(), parameters.Encode())
	body, status, err := adapter.get(ctx, requestURL, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, fmt.Errorf("%w: eastmoney valuation: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: eastmoney valuation http %d", ErrUpstream, status)
	}
	return parseEastMoneyValuation(body, market, strings.TrimSpace(symbol))
}

func parseEastMoneyValuation(body []byte, market, symbol string) (*Valuation, error) {
	var response struct {
		Code *int                       `json:"rc"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("%w: eastmoney valuation JSON: %v", ErrUpstream, err)
	}
	if response.Code != nil && *response.Code != 0 {
		return nil, fmt.Errorf("%w: eastmoney valuation rc=%d", ErrUpstream, *response.Code)
	}
	fields := response.Data
	if len(fields) == 0 {
		return nil, ErrNoData
	}
	if returnedSymbol := eastMoneyText(fields["f57"]); returnedSymbol != "" && returnedSymbol != symbol {
		return nil, fmt.Errorf("%w: eastmoney valuation identity mismatch", ErrUpstream)
	}
	name := eastMoneyText(fields["f58"])
	valuation := &Valuation{
		Symbol: symbol, Market: market, Name: name,
		IsST: strings.Contains(strings.ToUpper(name), "ST"), Source: "eastmoney",
	}
	availableFields := 0
	for _, field := range []struct {
		key           string
		destination   *float64
		allowNegative bool
	}{
		{"f162", &valuation.PEDynamic, true},
		{"f163", &valuation.PEStatic, true},
		{"f164", &valuation.PETTM, true},
		{"f167", &valuation.PB, true},
		{"f116", &valuation.TotalCap, false},
		{"f117", &valuation.FloatCap, false},
		{"f168", &valuation.TurnoverRate, false},
		{"f171", &valuation.Amplitude, false},
		{"f50", &valuation.VolumeRatio, false},
		{"f51", &valuation.LimitUp, false},
		{"f52", &valuation.LimitDown, false},
	} {
		value, valid := emNum(fields[field.key])
		if !valid || math.IsNaN(value) || math.IsInf(value, 0) || (!field.allowNegative && value < 0) {
			continue
		}
		*field.destination = value
		availableFields++
	}
	if availableFields == 0 {
		return nil, ErrNoData
	}
	// Never substitute capture time for an absent upstream timestamp.
	if timestamp, valid := emNum(fields["f86"]); valid && timestamp > 0 &&
		timestamp <= 253402300799 && math.Trunc(timestamp) == timestamp {
		valuation.DataTime = time.Unix(int64(timestamp), 0).In(ChinaMarketLocation())
	}
	return valuation, nil
}

func eastMoneyText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "-" {
		return ""
	}
	return text
}

func eastMoneyOptionalNumber(raw json.RawMessage) float64 {
	value, valid := emNum(raw)
	if !valid || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
