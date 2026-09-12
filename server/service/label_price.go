package service

import (
	"context"
	"errors"
	"fmt"

	"quantvista/common"
	"quantvista/model"
)

var errLabelPriceBasis = errors.New("标签价格口径待核验")

// actual_position 保存的是原始成交/持仓价格，不能套用推荐生成时的复权锚点。
// 旧记录没有实际入场时的独立价格版本；公司行动后停止直接比较，不重写用户买入价。
func checkActualLabelPriceBasis(ctx context.Context, label *model.RecommendationLabel, today string) error {
	if label.Market != "" && label.Market != "cn" {
		return fmt.Errorf("%w：缺少该市场实际成交价的复权依据", errLabelPriceBasis)
	}
	var actions []model.CorporateAction
	if err := common.DB.WithContext(ctx).Where("market = ? AND symbol = ? AND progress = ? AND ex_date > ? AND ex_date <= ?",
		"cn", label.Symbol, model.CorpActionProgressImplemented, label.EntryDate, today).Find(&actions).Error; err != nil {
		return err
	}
	for _, action := range actions {
		if action.HasAdjustment() {
			return fmt.Errorf("%w：实际建仓之后存在分红送转，缺少该笔成交的独立复权锚点", errLabelPriceBasis)
		}
	}
	return nil
}
