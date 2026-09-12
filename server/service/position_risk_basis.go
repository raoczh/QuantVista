package service

import (
	"errors"
	"sort"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errPositionRiskChanged = errors.New("评估使用的持仓状态已变化")

// 仅固定参与价格风险判断的持仓输入，备注等无关编辑不废弃有效评估。
func positionRiskBasisHash(p model.Position) string {
	return stablePositionExitHash(struct {
		ID, UserID, AccountID            int64
		Symbol, Market, Status, Currency string
		Cost, Quantity, Stop, Take, Peak float64
		PeakFrom, PeakDate, PeakQuality  string
	}{p.ID, p.UserID, p.AccountID, p.Symbol, p.Market, p.Status, p.Currency,
		p.BuyPrice, p.Quantity, p.PlanStopLoss, p.PlanTakeProfit, p.PeakPrice,
		p.PeakFrom, p.PeakDate, p.PeakDataQuality})
}

func verifyPositionRiskBasesTx(tx *gorm.DB, userID int64, snapshots []model.Position) error {
	if len(snapshots) == 0 {
		return nil
	}
	if err := lockPositionRiskAccountsTx(tx, userID, snapshots); err != nil {
		return err
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(snapshots))
	for _, p := range snapshots {
		if !seen[p.ID] {
			ids = append(ids, p.ID)
			seen[p.ID] = true
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var current []model.Position
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ? AND user_id = ? AND status = ?", ids, userID, model.PositionStatusHolding).
		Order("id").Find(&current).Error; err != nil {
		return err
	}
	byID := make(map[int64]model.Position, len(current))
	for _, p := range current {
		byID[p.ID] = p
	}
	for _, snapshot := range snapshots {
		p, ok := byID[snapshot.ID]
		if !ok || positionRiskBasisHash(p) != positionRiskBasisHash(snapshot) {
			return errPositionRiskChanged
		}
	}
	return nil
}

func sameAlertRuleDefinition(a, b model.AlertRule) bool {
	return a.Symbol == b.Symbol && a.Market == b.Market && a.Name == b.Name &&
		a.Kind == b.Kind && a.Op == b.Op && a.Threshold == b.Threshold && a.Period == b.Period &&
		a.Once == b.Once && a.Note == b.Note
}
