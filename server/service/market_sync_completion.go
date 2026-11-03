package service

import (
	"quantvista/model"

	"gorm.io/gorm"
)

func hasCompletedMarketWideSync(database *gorm.DB, tradeDate string) (bool, error) {
	var count int64
	err := database.Model(&model.DataSyncLog{}).
		Where("task = ? AND market = ? AND status = ? AND succeeded > 0 AND failed = 0 AND range_summary = ?",
			JobKindSyncMarketWide, "cn", "success", tradeDate+".."+tradeDate).
		Count(&count).Error
	return count > 0, err
}
