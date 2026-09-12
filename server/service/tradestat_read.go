package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

type tradeStatCalendar struct {
	index      map[string]int
	openPrefix []int
}

func tradeStatMarket(raw string) string {
	market := strings.ToLower(strings.TrimSpace(raw))
	if market == "" {
		return "cn"
	}
	return market
}

func (c tradeStatCalendar) count(from, to string) (int, bool) {
	start, startErr := time.Parse("2006-01-02", from)
	end, endErr := time.Parse("2006-01-02", to)
	if startErr != nil || endErr != nil || end.Before(start) {
		return 0, false
	}
	i, hasStart := c.index[from]
	j, hasEnd := c.index[to]
	// 连休也必须有明确的 is_open=false，不能把日历缺口算成休市。
	if !hasStart || !hasEnd || j-i+1 != int(end.Sub(start).Hours()/24)+1 {
		return 0, false
	}
	return c.openPrefix[j+1] - c.openPrefix[i+1], true
}

type tradeStatFacts struct {
	positions       []model.Position
	industries      map[string]string
	calendars       map[string]tradeStatCalendar
	legacyHasTrades map[int64]bool
	truncated       bool
}

// 统计只读取同一时点的平仓、行业和日历；不在 GET 过程中补写历史成交。
func readTradeStatFacts(ctx context.Context, userID, accountID int64, from, through string) (*tradeStatFacts, error) {
	out := &tradeStatFacts{industries: map[string]string{}, calendars: map[string]tradeStatCalendar{}, legacyHasTrades: map[int64]bool{}}
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		if _, err := portfolioAccountByIDDB(tx, userID, accountID, model.PortfolioKindReal); err != nil {
			return err
		}
		q := tx.Where("user_id = ? AND account_id = ? AND status = ? AND (sell_date = '' OR sell_date <= ?)", userID, accountID, model.PositionStatusClosed, through)
		if from != "" {
			q = q.Where("sell_date >= ?", from)
		}
		if err := q.Order("sell_date DESC, id DESC").Limit(tradeStatMaxRows + 1).Find(&out.positions).Error; err != nil {
			return err
		}
		if len(out.positions) > tradeStatMaxRows {
			out.truncated = true
			out.positions = out.positions[:tradeStatMaxRows]
		}
		if len(out.positions) == 0 {
			return nil
		}
		var symbols []string
		var legacyIDs []int64
		type dateRange struct{ from, to string }
		ranges := map[string]dateRange{}
		for _, p := range out.positions {
			if positionCurrencyIssue(p, "CNY") != "" {
				continue
			}
			symbols = append(symbols, p.Symbol)
			if p.TotalBuyCost <= 0 {
				legacyIDs = append(legacyIDs, p.ID)
			}
			start, startErr := time.Parse("2006-01-02", p.BuyDate)
			end, endErr := time.Parse("2006-01-02", p.SellDate)
			if startErr != nil || endErr != nil || end.Before(start) {
				continue
			}
			market := tradeStatMarket(p.Market)
			r := ranges[market]
			if r.from == "" || p.BuyDate < r.from {
				r.from = p.BuyDate
			}
			if p.SellDate > r.to {
				r.to = p.SellDate
			}
			ranges[market] = r
		}
		if len(legacyIDs) > 0 {
			var ids []int64
			if err := tx.Model(&model.PositionTrade{}).Where("user_id = ? AND account_id = ? AND position_id IN ?", userID, accountID, legacyIDs).
				Distinct().Pluck("position_id", &ids).Error; err != nil {
				return err
			}
			for _, id := range ids {
				out.legacyHasTrades[id] = true
			}
		}
		var err error
		out.industries, err = industriesForDB(tx, symbols, through)
		if err != nil {
			return fmt.Errorf("读取复盘行业失败: %w", err)
		}
		for market, r := range ranges {
			var days []model.TradingCalendar
			if err := tx.Where("market = ? AND trade_date >= ? AND trade_date <= ?", market, r.from, r.to).
				Order("trade_date").Find(&days).Error; err != nil {
				return fmt.Errorf("读取复盘交易日历失败: %w", err)
			}
			calendar := tradeStatCalendar{index: map[string]int{}, openPrefix: []int{0}}
			for _, day := range days {
				if _, err := time.Parse("2006-01-02", day.TradeDate); err != nil {
					continue
				}
				if _, exists := calendar.index[day.TradeDate]; exists {
					continue
				}
				index := len(calendar.openPrefix) - 1
				calendar.index[day.TradeDate] = index
				count := calendar.openPrefix[index]
				if day.IsOpen {
					count++
				}
				calendar.openPrefix = append(calendar.openPrefix, count)
			}
			out.calendars[market] = calendar
		}
		return ctx.Err()
	})
	return out, err
}
