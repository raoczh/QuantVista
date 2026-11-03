package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const sinaBoardPrefix = "sina:"

var sinaBoardNodePattern = regexp.MustCompile(`^(new_[a-z0-9_]{1,32}|gn_[A-Za-z0-9_]{1,32})$`)

func sinaBoardNode(code string) (string, bool) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, sinaBoardPrefix) {
		return "", false
	}
	node := strings.TrimPrefix(code, sinaBoardPrefix)
	return node, sinaBoardNodePattern.MatchString(node)
}

func (adapter *SinaAdapter) GetBoardHeat(ctx context.Context, kind string) ([]BoardHeat, error) {
	return adapter.GetBoardHeatLimited(ctx, kind, boardHeatPageSize)
}

// The aggregate endpoint is a single bounded request, not a scan of every stock.
// It has no source timestamp or advance/decline counts; neither is manufactured.
func (adapter *SinaAdapter) GetBoardHeatLimited(ctx context.Context, kind string, limit int) ([]BoardHeat, error) {
	endpoint := ""
	switch kind {
	case "industry":
		endpoint = "https://vip.stock.finance.sina.com.cn/q/view/newSinaHy.php"
	case "concept":
		endpoint = "https://vip.stock.finance.sina.com.cn/q/view/newFLJK.php?param=class"
	default:
		return nil, ErrSymbolInvalid
	}
	body, err := getSinaSpotBody(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(body)
	if err != nil {
		return nil, fmt.Errorf("%w: sina board GBK decoding failed", ErrUpstream)
	}
	rows, err := parseSinaBoardHeat(decoded, kind)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > boardHeatPageSize {
		limit = boardHeatPageSize
	}
	return rows[:min(limit, len(rows))], nil
}

func parseSinaBoardHeat(body []byte, kind string) ([]BoardHeat, error) {
	assignment, payload, found := strings.Cut(strings.TrimSpace(string(body)), "=")
	expectedAssignment, nodePrefix := "", ""
	switch kind {
	case "industry":
		expectedAssignment, nodePrefix = "var S_Finance_bankuai_sinaindustry", "new_"
	case "concept":
		expectedAssignment, nodePrefix = "var S_Finance_bankuai_class", "gn_"
	default:
		return nil, ErrSymbolInvalid
	}
	if !found || strings.TrimSpace(assignment) != expectedAssignment {
		return nil, fmt.Errorf("%w: unexpected sina board assignment", ErrUpstream)
	}
	payload = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(payload), ";"))
	var records map[string]string
	if err := json.Unmarshal([]byte(payload), &records); err != nil {
		return nil, fmt.Errorf("%w: invalid sina board JSON", ErrUpstream)
	}
	if len(records) > 1000 {
		return nil, fmt.Errorf("%w: sina board list exceeds safety limit", ErrUpstream)
	}
	rows := make([]BoardHeat, 0, len(records))
	for node, record := range records {
		fields := strings.Split(record, ",")
		if !sinaBoardNodePattern.MatchString(node) || !strings.HasPrefix(node, nodePrefix) ||
			len(fields) != 13 || fields[0] != node || strings.TrimSpace(fields[1]) == "" {
			return nil, fmt.Errorf("%w: invalid sina board identity or fields", ErrUpstream)
		}
		memberCount, countErr := strconv.Atoi(fields[2])
		changePercent, changeValid := parseSinaFiniteNumber(fields[5])
		amount, amountValid := parseSinaFiniteNumber(fields[7])
		if countErr != nil || memberCount < 0 || memberCount > 10000 ||
			!changeValid || !amountValid || amount < 0 {
			return nil, fmt.Errorf("%w: invalid sina board numeric fields", ErrUpstream)
		}
		if memberCount == 0 {
			continue
		}
		leaderCode := ""
		if len(fields[8]) == 8 {
			symbol := fields[8][2:]
			if mapped, valid := sinaCNSymbol(symbol); valid && cnAShareCode(symbol) && mapped == fields[8] {
				leaderCode = symbol
			}
		}
		rows = append(rows, BoardHeat{
			Code: sinaBoardPrefix + node, Name: strings.TrimSpace(fields[1]),
			ChangePct: changePercent, Amount: amount, MemberCount: memberCount,
			Leader: strings.TrimSpace(fields[12]), LeaderCode: leaderCode,
			Source: "sina", CountsAvailable: false,
		})
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	sort.Slice(rows, func(first, second int) bool {
		if rows[first].Amount == rows[second].Amount {
			return rows[first].Code < rows[second].Code
		}
		return rows[first].Amount > rows[second].Amount
	})
	return rows, nil
}

// GetSectorRanking makes the same independent industry source available to the
// market overview and AI market context. Its codes retain the Sina namespace.
func (adapter *SinaAdapter) GetSectorRanking(ctx context.Context, market string, limit int) ([]SectorRank, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	rows, err := adapter.GetBoardHeat(ctx, "industry")
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(first, second int) bool {
		if rows[first].ChangePct == rows[second].ChangePct {
			return rows[first].Code < rows[second].Code
		}
		return rows[first].ChangePct > rows[second].ChangePct
	})
	if limit <= 0 || limit > boardHeatPageSize {
		limit = 10
	}
	result := make([]SectorRank, 0, min(limit, len(rows)))
	for _, row := range rows[:min(limit, len(rows))] {
		result = append(result, SectorRank{Code: row.Code, Name: row.Name,
			ChangePct: row.ChangePct, Leader: row.Leader, Source: row.Source})
	}
	return result, nil
}

func (adapter *SinaAdapter) GetBoardConstituents(ctx context.Context, code string, limit int) ([]BoardStock, error) {
	node, valid := sinaBoardNode(code)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	endpoint := fmt.Sprintf("%sgetHQNodeData?node=%s&sort=amount&asc=0&num=%d&page=1",
		sinaSpotBaseURL, url.QueryEscape(node), limit)
	body, err := getSinaSpotBody(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	entries, err := parseSinaSpotPage(body)
	if err != nil {
		return nil, err
	}
	if len(entries) > limit {
		return nil, fmt.Errorf("%w: sina board constituents exceed requested limit", ErrUpstream)
	}
	var valuations []struct {
		Code     string          `json:"code"`
		TotalCap json.RawMessage `json:"mktcap"`
		FloatCap json.RawMessage `json:"nmc"`
	}
	if err := json.Unmarshal(body, &valuations); err != nil {
		return nil, fmt.Errorf("%w: invalid sina board valuation fields", ErrUpstream)
	}
	marketCaps := make(map[string][2]float64, len(valuations))
	for _, valuation := range valuations {
		totalCap, _ := emNum(valuation.TotalCap)
		floatCap, _ := emNum(valuation.FloatCap)
		if math.IsInf(totalCap*1e4, 0) || math.IsInf(floatCap*1e4, 0) {
			return nil, fmt.Errorf("%w: invalid sina board market cap", ErrUpstream)
		}
		marketCaps[valuation.Code] = [2]float64{max(totalCap, 0) * 1e4, max(floatCap, 0) * 1e4}
	}

	// Node pages only expose time-of-day. Fetch real dated quotes instead of
	// attaching today's date to those undated prices.
	supported := make([]sinaSpotEntry, 0, len(entries))
	quoteCodes := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if seen[entry.Code] {
			return nil, fmt.Errorf("%w: duplicate sina board constituent", ErrUpstream)
		}
		seen[entry.Code] = true
		if !cnAShareCode(entry.Code) {
			continue
		}
		quoteCode, _ := sinaCNSymbol(entry.Code)
		supported = append(supported, entry)
		quoteCodes = append(quoteCodes, quoteCode)
	}
	if len(supported) == 0 {
		return nil, ErrNoData
	}
	quoteBody, err := getSinaSpotBody(ctx, "https://hq.sinajs.cn/list="+strings.Join(quoteCodes, ","))
	if err != nil {
		return nil, err
	}
	quotes, err := parseSinaSpotQuotes(quoteBody, supported, time.Now())
	if err != nil {
		return nil, err
	}
	stocks := make([]BoardStock, 0, len(quotes))
	for _, quote := range quotes {
		caps := marketCaps[quote.Symbol]
		stocks = append(stocks, BoardStock{
			Symbol: quote.Symbol, Name: quote.Name, Price: quote.Price, ChangePct: quote.ChangePct,
			Amount: quote.Amount, TurnoverRate: quote.TurnoverRate, TotalCap: caps[0], FloatCap: caps[1],
			Source: adapter.Name(), DataTime: time.Unix(quote.DataTime, 0).In(ChinaMarketLocation()),
		})
	}
	sort.Slice(stocks, func(first, second int) bool { return stocks[first].Amount > stocks[second].Amount })
	return stocks, nil
}
