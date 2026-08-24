package service

import (
	"errors"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// 本文件统管「持仓 ↔ 推荐」血缘的两侧读写：
//   - 写：LinkRecommendation 事后补关联/解除关联（建仓时的即时血缘走 PositionService.Create）
//   - 读：RecommendationLinkCandidates 建仓/编辑时的候选推荐；positionRecLinksFor 持仓列表富化
//
// 口径：血缘是 user 级单值关联——一笔持仓最多关联一条推荐，一条推荐可对多笔持仓
// （视图与追踪统一取 id 最小的最早一笔，见 recommendation.go assembleView 与 tracking.go）。
//
// **事后补关联与建仓时即时记录的血缘在库里不作区分**（用户 2026-08-24 定夺）。代价：
// 两者一起进 S0-4 买入胜率分母，日后无法分辨哪些是「已知结果才补上」的后视偏差样本。
// 若日后需要区分，加一列 link_mode 即可（AutoMigrate 自动加列，不需要回填）。

// PositionRecLink 持仓视图里的来源推荐摘要（血缘可见性；无血缘时为 nil）。
type PositionRecLink struct {
	RecommendationID int64     `json:"recommendation_id"`
	BatchID          int64     `json:"batch_id"`
	Type             string    `json:"type"`   // short_term / long_term（批次级）
	Action           string    `json:"action"` // buy / watch
	RefPrice         float64   `json:"ref_price"`
	CreatedAt        time.Time `json:"created_at"`
}

// RecLinkCandidate 可关联的推荐候选（按标的过滤，供建仓/编辑时选择）。
type RecLinkCandidate struct {
	RecommendationID int64     `json:"recommendation_id"`
	BatchID          int64     `json:"batch_id"`
	Type             string    `json:"type"`
	Action           string    `json:"action"`
	Summary          string    `json:"summary"`
	RefPrice         float64   `json:"ref_price"`
	CreatedAt        time.Time `json:"created_at"`
	// LinkedPositionID 该推荐已被哪笔持仓关联（0=尚未被关联）。前端据此标注「已占用」，
	// 但不禁止改指——一条推荐允许对多笔持仓（如分批建仓）。
	LinkedPositionID int64 `json:"linked_position_id"`
}

// LinkRecommendation 事后补/改/解除持仓的推荐血缘。recID=0 表示解除关联。
//
// 校验比建仓路径的 resolveRecommendationLink 更严：除归属外**还要求标的一致**——
// 建仓时血缘由推荐卡片带入，标的天然对得上；事后补关联是人工选择，选错标的会直接
// 污染「AI 推荐 vs 实际买入」的对比事实。
func (s *PositionService) LinkRecommendation(userID, positionID, recID int64) (*model.Position, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var out model.Position
	var oldRecID int64
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var p model.Position
		if err := lockedPosition(tx, userID, positionID, &p); err != nil {
			return errors.New("持仓不存在")
		}
		oldRecID = p.RecommendationID
		if recID > 0 {
			var rec model.Recommendation
			if err := tx.Where("id = ? AND user_id = ?", recID, userID).First(&rec).Error; err != nil {
				return errors.New("推荐记录不存在")
			}
			if !strings.EqualFold(rec.Symbol, p.Symbol) || !strings.EqualFold(rec.Market, p.Market) {
				return errors.New("该推荐的标的与本笔持仓不一致，不能关联")
			}
		}
		if err := tx.Model(&model.Position{}).Where("id = ? AND user_id = ?", positionID, userID).
			Update("recommendation_id", recID).Error; err != nil {
			return err
		}
		p.RecommendationID = recID
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 追踪状态里的用户执行事实必须在这里同步维护，**不能指望后台刷新**：
	// refreshBatches 对 take_profit/stop_loss/expired 终态行直接 continue（终态冻结，
	// 见 tracking.go frozenTerminal），已结算推荐的 actual_* 永远等不到刷新补上。
	// 先清旧血缘（改指/解除都需要），再回填新血缘。
	if oldRecID > 0 && oldRecID != recID {
		clearActualExecutionFact(userID, oldRecID)
	}
	if recID > 0 {
		syncActualExecutionFact(userID, recID, &out)
	}
	return &out, nil
}

// syncActualExecutionFact 按血缘回填/清零追踪状态的用户执行事实（actual_buy_price /
// actual_return_pct）。pos=nil 时按 recID 清零；recID<=0 时按 pos 原血缘清零。
//
// 只动这两列，不碰任何模拟口径字段（return_pct/outcome/…）——用户执行事实与模拟
// 口径并列不混算是 S0-4 的既定口径。终点价复用已落库的 CurrentPrice，不发起上游请求。
func syncActualExecutionFact(userID, recID int64, pos *model.Position) {
	if common.DB == nil {
		return
	}
	if pos == nil || recID <= 0 || pos.BuyPrice <= 0 {
		return
	}
	var st model.RecommendationStatus
	if err := common.DB.Where("recommendation_id = ? AND user_id = ?", recID, userID).
		First(&st).Error; err != nil {
		// 尚无追踪状态行（还没评估过）——无需回填：该条目不是终态，
		// 下一轮 refreshBatches 会正常带上血缘计算。
		return
	}
	updates := map[string]any{"actual_buy_price": pos.BuyPrice, "updated_at": time.Now()}
	if endPrice := actualReturnEndPrice(*pos, st.CurrentPrice); endPrice > 0 {
		updates["actual_return_pct"] = round2((endPrice - pos.BuyPrice) / pos.BuyPrice * 100)
	} else {
		updates["actual_return_pct"] = nil
	}
	if err := common.DB.Model(&model.RecommendationStatus{}).
		Where("id = ? AND user_id = ?", st.ID, userID).Updates(updates).Error; err != nil {
		common.SysWarn("补关联回填执行事实失败 rec=%d pos=%d: %v", recID, pos.ID, err)
	}
}

// clearActualExecutionFact 解除血缘时清零执行事实（否则旧的实际买入价会挂在推荐上，
// 看起来像「仍有持仓」）。
func clearActualExecutionFact(userID, recID int64) {
	if common.DB == nil || recID <= 0 {
		return
	}
	if err := common.DB.Model(&model.RecommendationStatus{}).
		Where("recommendation_id = ? AND user_id = ?", recID, userID).
		Updates(map[string]any{
			"actual_buy_price":  0,
			"actual_return_pct": nil,
			"updated_at":        time.Now(),
		}).Error; err != nil {
		common.SysWarn("解除血缘清零执行事实失败 rec=%d: %v", recID, err)
	}
}

// RecommendationLinkCandidates 某标的近 trackWindowDays 天内可关联的推荐条目。
// 与追踪口径一致只取 success/degraded 批次（processing/failed 无可信条目）。
func (s *RecommendationService) RecommendationLinkCandidates(userID int64, symbol, market string) ([]RecLinkCandidate, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	sym, mkt, err := normalizeSymbolMarket(symbol, market)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -trackWindowDays)
	var recs []model.Recommendation
	if err := common.DB.Where("user_id = ? AND symbol = ? AND market = ? AND created_at >= ?",
		userID, sym, mkt, cutoff).Order("id DESC").Find(&recs).Error; err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return []RecLinkCandidate{}, nil
	}
	batchIDs := make([]int64, 0, len(recs))
	recIDs := make([]int64, 0, len(recs))
	for _, r := range recs {
		batchIDs = append(batchIDs, r.BatchID)
		recIDs = append(recIDs, r.ID)
	}
	// 批次类型 + status 过滤（一次查完，避免逐条查批次）。
	var batches []model.RecommendationBatch
	common.DB.Select("id", "type", "status").
		Where("id IN ? AND user_id = ? AND status IN ?", batchIDs, userID,
			[]string{model.RecStatusSuccess, model.RecStatusDegraded}).Find(&batches)
	batchType := make(map[int64]string, len(batches))
	for _, b := range batches {
		batchType[b.ID] = b.Type
	}
	// 已被关联的持仓（标注「已占用」，不禁止改指）。
	var linked []model.Position
	common.DB.Select("id", "recommendation_id").
		Where("user_id = ? AND recommendation_id IN ?", userID, recIDs).Order("id").Find(&linked)
	linkedBy := make(map[int64]int64, len(linked))
	for _, p := range linked {
		if _, ok := linkedBy[p.RecommendationID]; !ok {
			linkedBy[p.RecommendationID] = p.ID
		}
	}

	out := make([]RecLinkCandidate, 0, len(recs))
	for _, r := range recs {
		t, ok := batchType[r.BatchID]
		if !ok {
			continue // 批次非 success/degraded
		}
		out = append(out, RecLinkCandidate{
			RecommendationID: r.ID,
			BatchID:          r.BatchID,
			Type:             t,
			Action:           r.Action,
			Summary:          r.Summary,
			RefPrice:         r.RefPrice,
			CreatedAt:        r.CreatedAt,
			LinkedPositionID: linkedBy[r.ID],
		})
	}
	return out, nil
}

// positionRecLinksFor 批量取持仓的来源推荐摘要，按 position_id 索引（列表富化用）。
// 一次查推荐 + 一次查批次类型，不做 N+1。
func positionRecLinksFor(userID int64, positions []model.Position) map[int64]*PositionRecLink {
	out := map[int64]*PositionRecLink{}
	if common.DB == nil {
		return out
	}
	recIDs := make([]int64, 0, len(positions))
	for _, p := range positions {
		if p.RecommendationID > 0 {
			recIDs = append(recIDs, p.RecommendationID)
		}
	}
	if len(recIDs) == 0 {
		return out
	}
	var recs []model.Recommendation
	if err := common.DB.Select("id", "batch_id", "action", "ref_price", "created_at").
		Where("id IN ? AND user_id = ?", recIDs, userID).Find(&recs).Error; err != nil {
		common.SysWarn("读取持仓血缘推荐失败: %v", err)
		return out
	}
	byRec := make(map[int64]model.Recommendation, len(recs))
	batchIDs := make([]int64, 0, len(recs))
	for _, r := range recs {
		byRec[r.ID] = r
		batchIDs = append(batchIDs, r.BatchID)
	}
	batchType := map[int64]string{}
	if len(batchIDs) > 0 {
		var batches []model.RecommendationBatch
		common.DB.Select("id", "type").Where("id IN ? AND user_id = ?", batchIDs, userID).Find(&batches)
		for _, b := range batches {
			batchType[b.ID] = b.Type
		}
	}
	for _, p := range positions {
		r, ok := byRec[p.RecommendationID]
		if !ok {
			continue // 血缘指向的推荐已被删除（批次删除会清条目）——视为无血缘
		}
		out[p.ID] = &PositionRecLink{
			RecommendationID: r.ID,
			BatchID:          r.BatchID,
			Type:             batchType[r.BatchID],
			Action:           r.Action,
			RefPrice:         r.RefPrice,
			CreatedAt:        r.CreatedAt,
		}
	}
	return out
}

// unlinkedHoldingsFor 为「无血缘」的推荐条目找同标的在持仓中的记录（疑似买了但没登记）。
// 返回按 recommendation_id 索引的软匹配结果。
//
// 软匹配只用于**提示补关联**，绝不自动写血缘——同一标的可能被多个批次推荐过，也可能
// 是用户自己决定买的，系统无权替用户认定因果。
//
// positions 表只装真实持仓（模拟盘走 PaperAccount/PaperTrade，另一套表），无需按账户
// kind 过滤；跨真实账户不收窄，与 assembleView 的 posLinks 保持同一 user 级口径。
func unlinkedHoldingsFor(userID int64, items []model.Recommendation, linked map[int64]RecPositionLink) map[int64]RecPositionLink {
	out := map[int64]RecPositionLink{}
	if common.DB == nil || len(items) == 0 {
		return out
	}
	symbols := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		if _, ok := linked[it.ID]; ok {
			continue // 已有血缘，不需要软匹配
		}
		if !seen[it.Symbol] {
			seen[it.Symbol] = true
			symbols = append(symbols, it.Symbol)
		}
	}
	if len(symbols) == 0 {
		return out
	}
	var rows []model.Position
	if err := common.DB.Where("user_id = ? AND status = ? AND symbol IN ?",
		userID, model.PositionStatusHolding, symbols).Order("id").Find(&rows).Error; err != nil {
		common.SysWarn("软匹配未登记持仓失败: %v", err)
		return out
	}
	// 同标的多笔持仓取最早一笔，与血缘视图口径一致。
	byKey := map[string]model.Position{}
	for _, p := range rows {
		k := strings.ToLower(p.Market + ":" + p.Symbol)
		if _, ok := byKey[k]; !ok {
			byKey[k] = p
		}
	}
	for _, it := range items {
		if _, ok := linked[it.ID]; ok {
			continue
		}
		p, ok := byKey[strings.ToLower(it.Market+":"+it.Symbol)]
		if !ok {
			continue
		}
		out[it.ID] = RecPositionLink{
			PositionID: p.ID, BuyPrice: p.BuyPrice, BuyDate: p.BuyDate,
			Quantity: p.Quantity, Status: p.Status,
		}
	}
	return out
}
