package service

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"quantvista/model"

	"gorm.io/gorm"
)

func paperHoldingCost(h model.PaperHolding) float64 {
	if h.RemainingCost != nil {
		return round2(*h.RemainingCost)
	}
	return round2(h.AvgCost * h.Quantity)
}

func paperSoldCost(cost, quantity, sold float64) float64 {
	if quantity-sold <= positionQtyEps {
		return cost
	}
	return round2(cost * sold / quantity)
}

// 回放原始成交金额，四位展示均价不参与成本结转；历史已实现字段仅用于审计差异。
func replayPaperCost(rows []model.PaperTrade) (quantity, cost float64, mismatches int, err error) {
	trades := append([]model.PaperTrade(nil), rows...)
	sort.SliceStable(trades, func(i, j int) bool {
		a, b := effectiveTradeDate(trades[i].TradeDate, trades[i].CreatedAt), effectiveTradeDate(trades[j].TradeDate, trades[j].CreatedAt)
		if a == b {
			return trades[i].ID < trades[j].ID
		}
		return a < b
	})
	for _, t := range trades {
		amount := t.Amount
		if amount <= 0 && (t.Side == model.PaperSideBuy || t.Side == model.PaperSideSell) {
			amount = round2(t.Price * t.Quantity)
		}
		if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 || math.IsNaN(t.Quantity) || math.IsInf(t.Quantity, 0) {
			return 0, 0, mismatches, errors.New("模拟流水数量或金额无效")
		}
		switch t.Side {
		case model.PaperSideBuy:
			if t.Quantity <= 0 || amount <= 0 {
				return 0, 0, mismatches, errors.New("模拟买入流水缺少有效数量或金额")
			}
			quantity, cost = round4(quantity+t.Quantity), round2(cost+amount+t.Fee+t.Tax)
		case model.PaperSideSell:
			if t.Quantity <= 0 || t.Quantity > quantity+positionQtyEps {
				return 0, 0, mismatches, errors.New("模拟卖出流水缺少可回放的买入成本")
			}
			consumed := paperSoldCost(cost, quantity, t.Quantity)
			if math.Abs(round2(amount-t.Fee-t.Tax-consumed)-t.RealizedPnl) > 0.005 {
				mismatches++
			}
			quantity, cost = round4(quantity-t.Quantity), round2(cost-consumed)
		case model.PaperSideAdjust:
			quantity = round4(quantity + t.Quantity)
			if quantity < -positionQtyEps {
				return 0, 0, mismatches, errors.New("模拟调整流水数量不一致")
			}
		default:
			return 0, 0, mismatches, errors.New("模拟流水方向无法识别")
		}
	}
	return quantity, cost, mismatches, nil
}

// 不写库。新持仓直接读取余额；旧持仓有完整流水时恢复精确余额，无流水保留原均价估算。
func restorePaperHoldingCost(db *gorm.DB, h *model.PaperHolding) (string, error) {
	if h.RemainingCost != nil {
		if math.IsNaN(*h.RemainingCost) || math.IsInf(*h.RemainingCost, 0) || *h.RemainingCost < 0 {
			return "", errors.New("模拟持仓剩余成本无效")
		}
		if h.CostBasisEstimated {
			return "存量持仓缺少成交明细，成本沿用原均价估算", nil
		}
		return "", nil
	}
	var rows []model.PaperTrade
	if err := db.Where("user_id = ? AND account_id = ? AND market = ? AND symbol = ?", h.UserID, h.AccountID, h.Market, h.Symbol).Find(&rows).Error; err != nil {
		return "", err
	}
	cost := paperHoldingCost(*h)
	note := ""
	if len(rows) == 0 {
		note = "存量持仓缺少成交明细，成本沿用原均价估算"
		h.CostBasisEstimated = true
	} else {
		quantity, recovered, _, err := replayPaperCost(rows)
		if err != nil {
			return "", err
		}
		if math.Abs(quantity-h.Quantity) > positionQtyEps {
			return "", errors.New("模拟持仓与流水数量不一致，成本需核验")
		}
		cost = recovered
		h.CostBasisEstimated = false
	}
	h.RemainingCost = &cost
	if h.Quantity > 0 {
		h.AvgCost = round4(cost / h.Quantity)
	}
	return note, nil
}

func paperRealizedAccountingNote(db *gorm.DB, userID, accountID int64) (string, error) {
	var rows []model.PaperTrade
	if err := db.Where("user_id = ? AND account_id = ? AND market IN ?", userID, accountID, []string{"", "cn"}).Find(&rows).Error; err != nil {
		return "", err
	}
	groups := map[string][]model.PaperTrade{}
	for _, row := range rows {
		key := QuoteKey(row.Market, row.Symbol)
		groups[key] = append(groups[key], row)
	}
	mismatches := 0
	for _, trades := range groups {
		_, _, n, err := replayPaperCost(trades)
		if err != nil {
			return "历史模拟流水无法完整回放，累计已实现盈亏待核验", nil
		}
		mismatches += n
	}
	if mismatches > 0 {
		return fmt.Sprintf("%d 笔历史卖出盈亏与成交金额回放不一致，原记录保留，累计已实现盈亏待核验", mismatches), nil
	}
	return "", nil
}
