package service

import (
	"sort"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 未归属账户的旧持仓仍按迁移前语义处理；一旦已归属账户，只允许本人活动真实账户。
// 历史列表不使用此 scope，归档数据继续可查。
func withActivePositionAccount(db *gorm.DB) *gorm.DB {
	return db.Where(`(positions.account_id = 0 OR positions.account_id IS NULL OR EXISTS (
		SELECT 1 FROM portfolio_accounts pa
		WHERE pa.id = positions.account_id AND pa.user_id = positions.user_id
		  AND pa.kind = ? AND pa.status = ?
	))`, model.PortfolioKindReal, model.PortfolioStatusActive)
}

func activePositionSnapshotIDs(db *gorm.DB, positions []model.Position) (map[int64]bool, error) {
	active := map[int64]bool{}
	if len(positions) == 0 {
		return active, nil
	}
	ids := make([]int64, 0, len(positions))
	for _, p := range positions {
		ids = append(ids, p.ID)
	}
	var current []model.Position
	if err := db.Model(&model.Position{}).Scopes(withActivePositionAccount).
		Select("id", "user_id", "account_id").Where("id IN ? AND status = ?", ids, model.PositionStatusHolding).
		Find(&current).Error; err != nil {
		return nil, err
	}
	byID := make(map[int64]model.Position, len(current))
	for _, p := range current {
		byID[p.ID] = p
	}
	for _, p := range positions {
		if row, ok := byID[p.ID]; ok && row.UserID == p.UserID && row.AccountID == p.AccountID {
			active[p.ID] = true
		}
	}
	return active, nil
}

// 所有评估写入都先按 ID 升序锁账户，再锁持仓，与交易/归档使用同一顺序。
func lockPositionRiskAccountsTx(tx *gorm.DB, userID int64, positions []model.Position) error {
	seen := map[int64]bool{}
	var ids []int64
	for _, p := range positions {
		if p.AccountID > 0 && !seen[p.AccountID] {
			ids = append(ids, p.AccountID)
			seen[p.AccountID] = true
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var accounts []model.PortfolioAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ? AND user_id = ? AND kind = ? AND status = ?", ids, userID, model.PortfolioKindReal, model.PortfolioStatusActive).
		Order("id").Find(&accounts).Error; err != nil {
		return err
	}
	if len(accounts) != len(ids) {
		return errPositionRiskChanged
	}
	return nil
}
