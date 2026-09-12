package service

import (
	"errors"

	"quantvista/model"

	"gorm.io/gorm"
)

// 只核验与本次读取相同的持仓和公司行动快照，不生成建议、不自动改账。
func realPortfolioValuationGaps(db *gorm.DB, positions []model.Position, date string) (map[int64]string, error) {
	gaps := map[int64]string{}
	for _, p := range positions {
		if p.Status != model.PositionStatusHolding {
			continue
		}
		if err := ensurePositionShareActionsProcessedTx(db, p, date); err != nil {
			if !errors.Is(err, errPositionShareActionPending) {
				return nil, err
			}
			gaps[p.ID] = "送转尚未确认，数量与估值待核验；请先处理持仓页的除权调整"
		}
	}
	return gaps, nil
}

func paperPortfolioValuationGaps(db *gorm.DB, holdings []model.PaperHolding, date string) (map[int64]string, error) {
	gaps := map[int64]string{}
	for _, h := range holdings {
		if err := verifyPaperCorpAdjustBeforeTradeTx(db, h.UserID, h.AccountID, h.Symbol, h.Market, date); err != nil {
			if !errors.Is(err, errPaperCorpActionPending) {
				return nil, err
			}
			gaps[h.ID] = err.Error()
		}
	}
	return gaps, nil
}
