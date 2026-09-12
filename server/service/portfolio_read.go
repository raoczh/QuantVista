package service

import (
	"context"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

// 风险读取的账本输入必须来自同一个数据库快照。行情在事务结束后获取，
// 压力测试、风险权重和再平衡复用这份输入，不能再单独读取新现金或新股数。
type portfolioRiskLedger struct {
	account       model.PortfolioAccount
	asOf          string
	realHoldings  []model.Position
	paperHoldings []model.PaperHolding
	valuationGaps map[string]string
	cash          RiskMetric
	points        []EquityPoint
	partial       int
	reasons       []string
}

func (s *PortfolioRiskService) readRiskLedger(ctx context.Context, userID, accountID int64, asOf string, historyDays int) (*portfolioRiskLedger, error) {
	if userID <= 0 || accountID <= 0 {
		return nil, gorm.ErrRecordNotFound
	}
	if asOf == "" {
		asOf = time.Now().Format("2006-01-02")
	}
	state := &portfolioRiskLedger{asOf: asOf, valuationGaps: map[string]string{}}
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", accountID, userID).First(&state.account).Error; err != nil {
			return err
		}
		if state.account.Kind == model.PortfolioKindPaper {
			var cash model.PaperAccount
			if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).First(&cash).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Find(&state.paperHoldings).Error; err != nil {
				return err
			}
			gap, err := portfolioCurrencyGapFor(tx, userID, accountID, state.account.Kind)
			if err != nil {
				return err
			}
			state.cash = available(cash.Cash, 1)
			if gap.affects(asOf) {
				state.cash = unavailable(gap.Reason, 0)
			}
			gaps, err := paperPortfolioValuationGaps(tx, state.paperHoldings, asOf)
			if err != nil {
				return err
			}
			for _, h := range state.paperHoldings {
				if reason := gaps[h.ID]; reason != "" {
					state.valuationGaps[QuoteKey(h.Market, h.Symbol)] = reason
				}
			}
		} else {
			if err := tx.Where("user_id = ? AND account_id = ? AND status = ?", userID, accountID, model.PositionStatusHolding).Order("id ASC").Find(&state.realHoldings).Error; err != nil {
				return err
			}
			cash, reason, err := realCashBalance(tx, userID, accountID, asOf)
			if err != nil {
				return err
			}
			state.cash = available(cash, 1)
			if reason != "" {
				state.cash = unavailable(reason, 0)
			}
			gaps, err := realPortfolioValuationGaps(tx, state.realHoldings, asOf)
			if err != nil {
				return err
			}
			for _, p := range state.realHoldings {
				if reason := gaps[p.ID]; reason != "" {
					state.valuationGaps[QuoteKey(p.Market, p.Symbol)] = reason
				}
			}
		}
		if historyDays > 0 {
			var err error
			state.points, state.partial, state.reasons, err = s.equityPoints(tx, &state.account, historyDays, asOf)
			return err
		}
		return nil
	})
	return state, err
}

func (s *PortfolioRiskService) overviewWithHoldings(ctx context.Context, userID, accountID int64) (*PortfolioOverviewView, []RebalanceHolding, error) {
	state, err := s.readRiskLedger(ctx, userID, accountID, "", 0)
	if err != nil {
		return nil, nil, err
	}
	holdings, rebalance, exposure, err := s.currentHoldings(ctx, state)
	if err != nil {
		return nil, nil, err
	}
	return portfolioOverviewFromLedger(state, holdings, exposure), rebalance, nil
}
