package service

import (
	"context"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

// 旧条目没有保存复权锚点。发生分红送转后保留原参照价，明确停止直接比较。
// 查询只标注缺口，不回写历史价格或根据今天的价格重造放弃记录。
func readMissedComparisons(ctx context.Context, userID int64) ([]model.WatchlistItem, map[int64]string, error) {
	var items []model.WatchlistItem
	notes := map[int64]string{}
	today := time.Now().Format("2006-01-02")
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND research_stage = ?", userID, model.StagePassed).Order("stage_at DESC, id DESC").Find(&items).Error; err != nil {
			return err
		}
		var symbols []string
		from := ""
		for _, item := range items {
			if item.PassedPrice <= 0 {
				continue
			}
			if item.StageAt == nil || item.StageAt.IsZero() {
				notes[item.ID] = "历史参照时间缺失，无法核验放弃价与现价的复权口径"
				continue
			}
			date := item.StageAt.In(time.Local).Format("2006-01-02")
			if date > today {
				notes[item.ID] = "参照时间晚于当前日期，暂不能比较价格"
				continue
			}
			if item.Market != "cn" {
				notes[item.ID] = "当前缺少该市场的公司行动核验信息，暂不能直接比较历史价格"
				continue
			}
			if from == "" || date < from {
				from = date
			}
			symbols = append(symbols, item.Symbol)
		}
		if len(symbols) == 0 {
			return nil
		}
		var actions []model.CorporateAction
		if err := tx.Where("market = ? AND symbol IN ? AND ex_date >= ? AND ex_date <= ? AND progress = ?", "cn", symbols, from, today,
			model.CorpActionProgressImplemented).Find(&actions).Error; err != nil {
			return err
		}
		for _, item := range items {
			if item.StageAt == nil || notes[item.ID] != "" {
				continue
			}
			date := item.StageAt.In(time.Local).Format("2006-01-02")
			for _, action := range actions {
				if action.Symbol == item.Symbol && action.ExDate >= date && action.HasAdjustment() {
					notes[item.ID] = "参照期间存在分红送转，旧价与现价口径可能不同；缺少复权锚点，暂不判断错过或回避"
					break
				}
			}
		}
		return nil
	})
	return items, notes, err
}
