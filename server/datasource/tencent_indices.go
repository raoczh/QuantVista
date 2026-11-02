package datasource

import "context"

const tencentBenchmarkCode = "sh000001"

// GetIndices uses the exchange-qualified CNIndices codes, not the stock symbol
// mapper: stock 000001 is Shenzhen's Ping An Bank, not the Shanghai Composite.
func (adapter *TencentAdapter) GetIndices(ctx context.Context, market string) ([]Index, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	codes := make([]string, 0, len(CNIndices))
	for _, index := range CNIndices {
		codes = append(codes, index.Sina)
	}
	records, err := fetchTencentQuotes(ctx, codes)
	if err != nil {
		return nil, err
	}
	indices := make([]Index, 0, len(CNIndices))
	for _, index := range CNIndices {
		fields, exists := records[index.Sina]
		if !exists {
			continue
		}
		price := tencentAtof(fields, 3)
		if price <= 0 {
			continue
		}
		indices = append(indices, Index{
			Code:      index.Code,
			Name:      index.Name,
			Price:     price,
			ChangePct: tencentAtof(fields, 32),
			Open:      tencentAtof(fields, 5),
			High:      tencentAtof(fields, 33),
			Low:       tencentAtof(fields, 34),
			PrevClose: tencentAtof(fields, 4),
			Source:    adapter.Name(),
			DataTime:  tencentDataTime(fields),
		})
	}
	if len(indices) == 0 {
		return nil, ErrNoData
	}
	return indices, nil
}

// GetBenchmarkBars returns the unadjusted Shanghai Composite series. An index
// has no stock adjustment factor; qfqday is never substituted for day here.
func (adapter *TencentAdapter) GetBenchmarkBars(ctx context.Context, market string, limit int) (string, []Bar, error) {
	if market != "cn" {
		return "", nil, ErrNotSupported
	}
	bars, err := adapter.fetchDailyBars(ctx, tencentBenchmarkCode, "day", tencentDailyLimit(limit, 250))
	if err != nil {
		return "", nil, err
	}
	return "上证指数", bars, nil
}

// GetTradingDays only exposes dates present in real index bars. A short upstream
// window remains short; this is not a forward calendar or a holiday calculator.
func (adapter *TencentAdapter) GetTradingDays(ctx context.Context, market string, limit int) ([]string, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	bars, err := adapter.fetchDailyBars(ctx, tencentBenchmarkCode, "day", tencentDailyLimit(limit, TencentMaxDailyBars))
	if err != nil {
		return nil, err
	}
	dates := make([]string, len(bars))
	for barIndex, bar := range bars {
		dates[barIndex] = bar.TradeDate
	}
	return dates, nil
}
