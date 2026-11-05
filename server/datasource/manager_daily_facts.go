package datasource

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

const dailyFactsEnrichmentBudget = 3 * time.Second

// EnrichDailyTradingFacts best-effort fills only zero amount/turnover fields on
// the same symbol and original dates. Pass the daily route's budgeted context
// so optional facts cannot extend its remaining budget. Cancellation handling
// belongs to the price caller: any failure here returns its valid bars intact.
//
// At most one 400-calendar-day tail window is requested per provider, including
// when the input is historical. No universe scan, history paging or price merge
// is performed. A successful merge returns a copy and never mutates the input.
func (manager *Manager) EnrichDailyTradingFacts(ctx context.Context, market, symbol string, bars []Bar) []Bar {
	if len(bars) == 0 || market != "cn" || ctx.Err() != nil {
		return bars
	}
	symbol = strings.TrimSpace(symbol)
	if !validSohuStockSymbol(symbol) {
		return bars
	}
	observedAt := time.Now()
	var latestDate time.Time
	for _, bar := range bars {
		if date, valid := parseDailyFactDate(bar.TradeDate, observedAt); valid && date.After(latestDate) {
			latestDate = date
		}
	}
	if latestDate.IsZero() {
		return bars
	}
	cutoff := latestDate.AddDate(0, 0, 1-SohuMaxDailyFactsDays).Format(time.DateOnly)
	from, to := "", ""
	for _, bar := range bars {
		if bar.Amount != 0 && bar.TurnoverRate != 0 {
			continue
		}
		if _, valid := parseDailyFactDate(bar.TradeDate, observedAt); !valid || bar.TradeDate < cutoff {
			continue
		}
		if from == "" || bar.TradeDate < from {
			from = bar.TradeDate
		}
		if bar.TradeDate > to {
			to = bar.TradeDate
		}
	}
	if from == "" {
		return bars
	}
	totalBudget, sourceBudget := manager.budgets("daily_facts")
	ctx, cancel := context.WithTimeout(ctx, min(dailyFactsEnrichmentBudget, totalBudget, sourceBudget))
	defer cancel()
	facts, err := routeCap(manager, ctx, market, "daily_facts", func(sourceContext context.Context, adapter Adapter) ([]DailyTradingFact, error) {
		provider, supported := adapter.(DailyTradingFactsProvider)
		if !supported {
			return nil, ErrNotSupported
		}
		facts, err := provider.GetDailyTradingFacts(sourceContext, market, symbol, from, to)
		if err != nil {
			return nil, err
		}
		return validateDailyTradingFacts(facts, market, symbol, from, to, observedAt)
	})
	if err != nil || ctx.Err() != nil {
		return bars
	}
	byDate := make(map[string]DailyTradingFact, len(facts))
	for _, fact := range facts {
		byDate[fact.TradeDate] = fact
	}
	var enriched []Bar
	for index, bar := range bars {
		fact, exists := byDate[bar.TradeDate]
		if !exists {
			continue
		}
		fillAmount := bar.Amount == 0 && fact.Amount > 0
		fillTurnover := bar.TurnoverRate == 0 && fact.TurnoverRate > 0
		if !fillAmount && !fillTurnover {
			continue
		}
		if enriched == nil {
			enriched = append([]Bar(nil), bars...)
		}
		if fillAmount {
			enriched[index].Amount = fact.Amount
			enriched[index].AmountSource = fact.Source
		}
		if fillTurnover {
			enriched[index].TurnoverRate = fact.TurnoverRate
			enriched[index].TurnoverSource = fact.Source
		}
	}
	if enriched == nil {
		return bars
	}
	return enriched
}

func validateDailyTradingFacts(facts []DailyTradingFact, market, symbol, from, to string, observedAt time.Time) ([]DailyTradingFact, error) {
	if len(facts) == 0 {
		return nil, ErrNoData
	}
	if len(facts) > SohuMaxDailyFactsDays {
		return nil, fmt.Errorf("%w: daily facts exceeds row limit", ErrUpstream)
	}
	seen := make(map[string]DailyTradingFact, len(facts))
	validated := make([]DailyTradingFact, 0, len(facts))
	for _, fact := range facts {
		_, validDate := parseDailyFactDate(fact.TradeDate, observedAt)
		if fact.Market != market || fact.Symbol != symbol || !validDate || fact.TradeDate < from || fact.TradeDate > to ||
			strings.TrimSpace(fact.Source) == "" || fact.Volume < 0 || fact.Amount < 0 || fact.TurnoverRate < 0 ||
			math.IsNaN(fact.Amount) || math.IsInf(fact.Amount, 0) || math.IsNaN(fact.TurnoverRate) || math.IsInf(fact.TurnoverRate, 0) {
			return nil, fmt.Errorf("%w: daily facts contains an invalid identity, date or value", ErrUpstream)
		}
		if previous, exists := seen[fact.TradeDate]; exists {
			if previous != fact {
				return nil, fmt.Errorf("%w: daily facts has conflicting duplicates", ErrUpstream)
			}
			continue
		}
		seen[fact.TradeDate] = fact
		validated = append(validated, fact)
	}
	return validated, nil
}
