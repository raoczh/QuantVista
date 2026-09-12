package service

import (
	"context"
	"strings"

	"quantvista/common"
	"quantvista/model"
)

// 生成时冻结的名称决定当时 ST 语义，不能用今天戴帽/摘帽后的字典倒改历史入场。
// 仅接受用户、市场与代码均匹配的来源；缺少旧快照的标签由调用方沿用字典兼容路径。
func labelSnapshotNames(ctx context.Context, labels []model.RecommendationLabel) (map[int64]string, error) {
	var recIDs, eventIDs []int64
	for _, label := range labels {
		if label.RecommendationID > 0 {
			recIDs = append(recIDs, label.RecommendationID)
		}
		if label.CandidateEventID > 0 {
			eventIDs = append(eventIDs, label.CandidateEventID)
		}
	}
	type identity struct {
		ID, UserID           int64
		Symbol, Market, Name string
	}
	recs, events := map[int64]identity{}, map[int64]identity{}
	if len(recIDs) > 0 {
		var rows []identity
		if err := common.DB.WithContext(ctx).Model(&model.Recommendation{}).Select("id", "user_id", "symbol", "market", "name").Where("id IN ?", recIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			recs[row.ID] = row
		}
	}
	if len(eventIDs) > 0 {
		var rows []identity
		if err := common.DB.WithContext(ctx).Model(&model.RecommendationCandidateEvent{}).Select("id", "user_id", "symbol", "market", "name").Where("id IN ?", eventIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			events[row.ID] = row
		}
	}
	out := map[int64]string{}
	for _, label := range labels {
		row := recs[label.RecommendationID]
		if label.RecommendationID <= 0 {
			row = events[label.CandidateEventID]
		}
		if row.ID > 0 && row.UserID == label.UserID && row.Symbol == label.Symbol && row.Market == label.Market {
			out[label.ID] = strings.TrimSpace(row.Name)
		}
	}
	return out, nil
}
