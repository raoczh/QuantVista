package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	sinaSpotPageSize = 100
	sinaSpotMaxPages = 80
	sinaSpotPageGap  = 200 * time.Millisecond
	sinaSpotBaseURL  = "https://vip.stock.finance.sina.com.cn/quotes_service/api/json_v2.php/Market_Center."
)

type sinaSpotEntry struct {
	Code         string          `json:"code"`
	Symbol       string          `json:"symbol"`
	Symbols      string          `json:"symbols"`
	Name         string          `json:"name"`
	TurnoverRate json.RawMessage `json:"turnoverratio"`
	PE           json.RawMessage `json:"per"`
	PB           json.RawMessage `json:"pb"`
}

// GetCNSpotSnapshot returns a complete Shanghai/Shenzhen A-share dictionary and
// dated quotes. Undated node-page prices are deliberately not used: attaching a
// newer quote timestamp to an older page price would misrepresent its freshness.
// Enumeration and quote batches are sequential; any missing quote fails the round.
func (adapter *SinaAdapter) GetCNSpotSnapshot(ctx context.Context) ([]SpotRow, error) {
	expectedTotal, err := fetchSinaSpotCount(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]sinaSpotEntry, 0, expectedTotal)
	seenSymbols := make(map[string]struct{}, expectedTotal)
	pageCount := (expectedTotal + sinaSpotPageSize - 1) / sinaSpotPageSize
	for pageNumber := 1; pageNumber <= pageCount; pageNumber++ {
		if err := waitSpotSnapshotInterval(ctx, sinaSpotPageGap); err != nil {
			return nil, err
		}
		endpoint := fmt.Sprintf("%sgetHQNodeData?node=hs_a&sort=code&asc=1&num=%d&page=%d", sinaSpotBaseURL, sinaSpotPageSize, pageNumber)
		body, err := getSinaSpotBody(ctx, endpoint)
		if err != nil {
			return nil, fmt.Errorf("sina snapshot page %d: %w", pageNumber, err)
		}
		pageEntries, err := parseSinaSpotPage(body)
		if err != nil {
			return nil, fmt.Errorf("sina snapshot page %d: %w", pageNumber, err)
		}
		expectedPageRows := min(sinaSpotPageSize, expectedTotal-len(entries))
		if len(pageEntries) != expectedPageRows {
			return nil, fmt.Errorf("%w: sina snapshot page %d incomplete: %d/%d", ErrUpstream, pageNumber, len(pageEntries), expectedPageRows)
		}
		for _, entry := range pageEntries {
			if _, exists := seenSymbols[entry.Code]; exists {
				return nil, fmt.Errorf("%w: sina snapshot duplicate code %s", ErrUpstream, entry.Code)
			}
			if len(entries) > 0 && entry.Code < entries[len(entries)-1].Code {
				return nil, fmt.Errorf("%w: sina snapshot code ordering changed", ErrUpstream)
			}
			seenSymbols[entry.Code] = struct{}{}
			entries = append(entries, entry)
		}
	}
	if len(entries) != expectedTotal || len(seenSymbols) != expectedTotal {
		return nil, fmt.Errorf("%w: sina snapshot unique-code count incomplete: %d/%d", ErrUpstream, len(seenSymbols), expectedTotal)
	}

	// Filter only AFTER checking the raw universe, including unsupported exchanges.
	rows := make([]SpotRow, 0, len(entries))
	for startIndex := 0; startIndex < len(entries); startIndex += sinaSpotPageSize {
		pageEntries := entries[startIndex:min(startIndex+sinaSpotPageSize, len(entries))]
		supportedEntries := make([]sinaSpotEntry, 0, len(pageEntries))
		quoteCodes := make([]string, 0, len(pageEntries))
		for _, entry := range pageEntries {
			if !cnAShareCode(entry.Code) {
				continue
			}
			quoteCode, valid := sinaCNSymbol(entry.Code)
			if !valid {
				return nil, fmt.Errorf("%w: sina snapshot cannot map A-share code", ErrUpstream)
			}
			supportedEntries = append(supportedEntries, entry)
			quoteCodes = append(quoteCodes, quoteCode)
		}
		if len(quoteCodes) == 0 {
			continue
		}
		if err := waitSpotSnapshotInterval(ctx, sinaSpotPageGap); err != nil {
			return nil, err
		}
		body, err := getSinaSpotBody(ctx, "https://hq.sinajs.cn/list="+strings.Join(quoteCodes, ","))
		if err != nil {
			return nil, err
		}
		batchRows, err := parseSinaSpotQuotes(body, supportedEntries, time.Now())
		if err != nil {
			return nil, err
		}
		rows = append(rows, batchRows...)
	}
	if err := waitSpotSnapshotInterval(ctx, sinaSpotPageGap); err != nil {
		return nil, err
	}
	finalTotal, err := fetchSinaSpotCount(ctx)
	if err != nil {
		return nil, err
	}
	if finalTotal != expectedTotal {
		return nil, fmt.Errorf("%w: sina snapshot count changed: %d -> %d", ErrUpstream, expectedTotal, finalTotal)
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	return rows, nil
}

func getSinaSpotBody(ctx context.Context, endpoint string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, status, err := doGet(ctx, endpoint, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, fmt.Errorf("%w: sina snapshot request: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: sina snapshot HTTP %d", ErrUpstream, status)
	}
	return body, nil
}

func fetchSinaSpotCount(ctx context.Context) (int, error) {
	// Verified on 2026-11-02: getHQNodeCount returns a Service not found error;
	// getHQNodeStockCount returns the hs_a total as a quoted integer.
	body, err := getSinaSpotBody(ctx, sinaSpotBaseURL+"getHQNodeStockCount?node=hs_a")
	if err != nil {
		return 0, err
	}
	var count json.Number
	if err := json.Unmarshal(body, &count); err != nil {
		return 0, fmt.Errorf("%w: sina snapshot invalid count JSON", ErrUpstream)
	}
	total, err := count.Int64()
	if err != nil || total < 0 || total > sinaSpotPageSize*sinaSpotMaxPages {
		return 0, fmt.Errorf("%w: sina snapshot count invalid or exceeds page limit", ErrUpstream)
	}
	if total == 0 {
		return 0, ErrNoData
	}
	return int(total), nil
}

func parseSinaSpotPage(body []byte) ([]sinaSpotEntry, error) {
	var entries []sinaSpotEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("%w: sina snapshot page JSON: %v", ErrUpstream, err)
	}
	for entryIndex := range entries {
		entry := &entries[entryIndex]
		entry.Code = strings.TrimSpace(entry.Code)
		entry.Name = strings.TrimSpace(entry.Name)
		if !isSixDigitSymbol(entry.Code) || entry.Name == "" {
			return nil, fmt.Errorf("%w: sina snapshot invalid code or name", ErrUpstream)
		}
		for _, alias := range []string{entry.Symbol, entry.Symbols} {
			if alias == "" {
				continue
			}
			if len(alias) != 8 || alias[2:] != entry.Code ||
				(alias[:2] != "sh" && alias[:2] != "sz" && alias[:2] != "bj") {
				return nil, fmt.Errorf("%w: sina snapshot code/alias mismatch", ErrUpstream)
			}
			expectedAlias, mapped := sinaCNSymbol(entry.Code)
			if entry.Code[0] == '4' || entry.Code[0] == '8' || strings.HasPrefix(entry.Code, "920") {
				expectedAlias, mapped = "bj"+entry.Code, true
			}
			if mapped && alias != expectedAlias {
				return nil, fmt.Errorf("%w: sina snapshot exchange/alias mismatch", ErrUpstream)
			}
		}
	}
	return entries, nil
}

func parseSinaSpotQuotes(body []byte, entries []sinaSpotEntry, observedAt time.Time) ([]SpotRow, error) {
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(body)
	if err != nil {
		return nil, fmt.Errorf("%w: sina snapshot GBK decoding failed", ErrUpstream)
	}
	expectedCodes := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		quoteCode, valid := sinaCNSymbol(entry.Code)
		if !valid {
			return nil, fmt.Errorf("%w: sina snapshot invalid requested quote code", ErrUpstream)
		}
		expectedCodes[quoteCode] = struct{}{}
	}
	quotes := make(map[string][]string, len(entries))
	for _, statement := range strings.Split(string(decoded), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		assignment, quotedValue, found := strings.Cut(statement, "=")
		if !found || !strings.HasPrefix(assignment, "var hq_str_") {
			return nil, fmt.Errorf("%w: sina snapshot malformed quote assignment", ErrUpstream)
		}
		quoteCode := strings.TrimSpace(strings.TrimPrefix(assignment, "var hq_str_"))
		if _, expected := expectedCodes[quoteCode]; !expected {
			return nil, fmt.Errorf("%w: sina snapshot unexpected quote identity", ErrUpstream)
		}
		if _, duplicate := quotes[quoteCode]; duplicate {
			return nil, fmt.Errorf("%w: sina snapshot duplicate quote identity", ErrUpstream)
		}
		value, err := strconv.Unquote(strings.TrimSpace(quotedValue))
		if err != nil {
			return nil, fmt.Errorf("%w: sina snapshot malformed quote string", ErrUpstream)
		}
		fields := strings.Split(value, ",")
		if len(fields) < 32 {
			return nil, fmt.Errorf("%w: sina snapshot missing or truncated quote %s", ErrUpstream, quoteCode)
		}
		quotes[quoteCode] = fields
	}
	if len(quotes) != len(expectedCodes) {
		return nil, fmt.Errorf("%w: sina snapshot missing batch quotes: %d/%d", ErrUpstream, len(quotes), len(expectedCodes))
	}
	rows := make([]SpotRow, 0, len(entries))
	for _, entry := range entries {
		quoteCode, _ := sinaCNSymbol(entry.Code)
		row, err := buildSinaSpotRow(entry, quotes[quoteCode], observedAt)
		if err != nil {
			return nil, fmt.Errorf("sina snapshot %s: %w", entry.Code, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func buildSinaSpotRow(entry sinaSpotEntry, fields []string, observedAt time.Time) (SpotRow, error) {
	row := SpotRow{Symbol: entry.Code, Name: strings.TrimSpace(fields[0]), Source: "sina_spot"}
	if row.Name == "" {
		return SpotRow{}, fmt.Errorf("%w: empty quote name", ErrUpstream)
	}
	var volumeShares float64
	for _, field := range []struct {
		index       int
		destination *float64
	}{
		{1, &row.Open}, {2, &row.PrevClose}, {3, &row.Price}, {4, &row.High},
		{5, &row.Low}, {8, &volumeShares}, {9, &row.Amount},
	} {
		value, valid := parseSinaFiniteNumber(fields[field.index])
		if !valid || value < 0 {
			return SpotRow{}, fmt.Errorf("%w: invalid quote numeric field %d", ErrUpstream, field.index)
		}
		*field.destination = value
	}
	if volumeShares/100 >= float64(math.MaxInt64) {
		return SpotRow{}, fmt.Errorf("%w: quote volume overflow", ErrUpstream)
	}
	row.Volume = int64(volumeShares / 100)
	// A suspended listing can retain its last price while open/high/low and
	// all trading activity are zero (601326 observed on 2026-11-02). Preserve
	// that quote without inventing OHLC; zero volume prevents a daily-bar write.
	noTradingActivity := row.Open == 0 && row.High == 0 && row.Low == 0 && volumeShares == 0 && row.Amount == 0
	if row.Price > 0 {
		if !noTradingActivity && !validSinaOHLC(row.Open, row.High, row.Low, row.Price) {
			return SpotRow{}, fmt.Errorf("%w: inconsistent quote OHLC", ErrUpstream)
		}
		if row.PrevClose > 0 {
			row.ChangePct = (row.Price - row.PrevClose) / row.PrevClose * 100
			if math.IsNaN(row.ChangePct) || math.IsInf(row.ChangePct, 0) {
				return SpotRow{}, fmt.Errorf("%w: quote change percentage overflow", ErrUpstream)
			}
		}
	} else if row.Open != 0 || row.High != 0 || row.Low != 0 || volumeShares != 0 || row.Amount != 0 {
		return SpotRow{}, fmt.Errorf("%w: zero-price quote has active trading fields", ErrUpstream)
	}
	quoteTime, validTime := parseSinaDateTime(strings.TrimSpace(fields[30]) + " " + strings.TrimSpace(fields[31]))
	if validTime && !quoteTime.After(observedAt) {
		row.DataTime = quoteTime.Unix()
	} else if row.Price > 0 && !noTradingActivity {
		return SpotRow{}, fmt.Errorf("%w: active quote has no valid source timestamp", ErrUpstream)
	}
	// No TTM or industry data is supplied by this endpoint. Keep both fields empty.
	for _, field := range []struct {
		value       json.RawMessage
		destination *float64
	}{
		{entry.TurnoverRate, &row.TurnoverRate}, {entry.PE, &row.PE}, {entry.PB, &row.PB},
	} {
		value, err := parseSinaSpotOptionalNumber(field.value)
		if err != nil {
			return SpotRow{}, err
		}
		*field.destination = value
	}
	if row.TurnoverRate < 0 {
		return SpotRow{}, fmt.Errorf("%w: negative turnover rate", ErrUpstream)
	}
	return row, nil
}

func parseSinaSpotOptionalNumber(raw json.RawMessage) (float64, error) {
	text := strings.TrimSpace(string(raw))
	if len(text) > 0 && text[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, fmt.Errorf("%w: invalid valuation JSON", ErrUpstream)
		}
		text = strings.TrimSpace(text)
	}
	if text == "" || text == "null" || text == "-" {
		return 0, nil
	}
	value, valid := parseSinaFiniteNumber(text)
	if !valid {
		return 0, fmt.Errorf("%w: invalid valuation number", ErrUpstream)
	}
	return value, nil
}
