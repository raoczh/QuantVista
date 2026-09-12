package service

import (
	"fmt"
	"strings"

	"quantvista/model"

	"gorm.io/gorm"
)

// 当前账户只支持 CNY，持仓可保留原币种记录；没有汇率事实就不能合计不同单位。
func positionCurrencyIssue(p model.Position, base string) string {
	market := strings.ToLower(strings.TrimSpace(p.Market))
	if market == "" {
		market = "cn"
	}
	if market != "cn" && market != "hk" && market != "us" {
		return "持仓市场的计价币种无法确认"
	}
	native := defaultCurrencyFor(market)
	currency := strings.ToUpper(strings.TrimSpace(p.Currency))
	if currency == "" {
		currency = native
	}
	if currency != native {
		return fmt.Sprintf("持仓币种 %s 与市场报价币种 %s 不一致，金额口径待核验", currency, native)
	}
	if base == "" {
		base = "CNY"
	}
	if currency != base {
		return fmt.Sprintf("缺少 %s 到 %s 的汇率事实，跨币种金额不能合计", currency, base)
	}
	return ""
}

type portfolioCurrencyGap struct {
	FromDate string
	Reason   string
}

func (g portfolioCurrencyGap) affects(date string) bool {
	return g.Reason != "" && (g.FromDate == "" || date == "" || date >= g.FromDate)
}

// 只读标记历史影响起点。保留存量快照原值，但不能再当作完整 CNY 净值使用。
func portfolioCurrencyGapFor(db *gorm.DB, userID, accountID int64, kind string) (portfolioCurrencyGap, error) {
	gap := portfolioCurrencyGap{}
	add := func(date, reason string) {
		if reason == "" {
			return
		}
		if gap.Reason == "" {
			gap = portfolioCurrencyGap{FromDate: date, Reason: reason}
		} else if date == "" || (gap.FromDate != "" && date < gap.FromDate) {
			gap.FromDate = date
		}
	}
	if kind == model.PortfolioKindReal {
		var positions []model.Position
		if err := db.Select("id", "market", "currency", "buy_date", "created_at").Where("user_id = ? AND account_id = ?", userID, accountID).Find(&positions).Error; err != nil {
			return gap, err
		}
		var ids []int64
		for _, p := range positions {
			if reason := positionCurrencyIssue(p, "CNY"); reason != "" {
				ids = append(ids, p.ID)
				add(effectiveTradeDate(p.BuyDate, p.CreatedAt), reason)
			}
		}
		if len(ids) > 0 {
			var trades []model.PositionTrade
			if err := db.Select("trade_date", "created_at").Where("user_id = ? AND account_id = ? AND position_id IN ?", userID, accountID, ids).Find(&trades).Error; err != nil {
				return gap, err
			}
			for _, trade := range trades {
				add(effectiveTradeDate(trade.TradeDate, trade.CreatedAt), gap.Reason)
			}
		}
		return gap, nil
	}
	var trades []model.PaperTrade
	if err := db.Select("market", "trade_date", "created_at").Where("user_id = ? AND account_id = ? AND market NOT IN ?", userID, accountID, []string{"", "cn"}).Find(&trades).Error; err != nil {
		return gap, err
	}
	for _, trade := range trades {
		add(effectiveTradeDate(trade.TradeDate, trade.CreatedAt), positionCurrencyIssue(model.Position{Market: trade.Market}, "CNY"))
	}
	var holdings []model.PaperHolding
	if err := db.Select("market", "created_at").Where("user_id = ? AND account_id = ? AND market NOT IN ?", userID, accountID, []string{"", "cn"}).Find(&holdings).Error; err != nil {
		return gap, err
	}
	for _, holding := range holdings {
		add(effectiveTradeDate("", holding.CreatedAt), positionCurrencyIssue(model.Position{Market: holding.Market}, "CNY"))
	}
	return gap, nil
}
