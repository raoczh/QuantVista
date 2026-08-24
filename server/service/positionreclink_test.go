package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// seedLinkFixture 建一批「批次 + 条目」并返回，供血缘用例复用。
func seedLinkFixture(t *testing.T, userID int64, symbol, recType, status string) (model.RecommendationBatch, model.Recommendation) {
	t.Helper()
	batch := model.RecommendationBatch{
		UserID: userID, Type: recType, Market: "cn", Strategy: "momentum",
		Status: status, CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	if err := common.DB.Create(&batch).Error; err != nil {
		t.Fatalf("建批次失败: %v", err)
	}
	rec := model.Recommendation{
		BatchID: batch.ID, UserID: userID, Symbol: symbol, Market: "cn", Name: symbol,
		Action: model.RecActionBuy, Confidence: 70, RefPrice: 10, CreatedAt: batch.CreatedAt,
	}
	if err := common.DB.Create(&rec).Error; err != nil {
		t.Fatalf("建推荐条目失败: %v", err)
	}
	return batch, rec
}

// TestLinkRecommendationBackfillsFrozenTerminalStatus 补关联对**已终态冻结**的推荐也必须
// 回填实际执行事实。
//
// 这是补关联的核心：refreshBatches 对 take_profit/stop_loss/expired 直接 continue
// （frozenTerminal 终态冻结防收益漂移），所以已结算推荐的 actual_buy_price /
// actual_return_pct 永远等不到后台刷新补上——必须在 LinkRecommendation 里同步写。
func TestLinkRecommendationBackfillsFrozenTerminalStatus(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8801

	for _, outcome := range []string{
		model.RecOutcomeTakeProfit, model.RecOutcomeStopLoss, model.RecOutcomeExpired,
	} {
		t.Run(outcome, func(t *testing.T) {
			batch, rec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
			// 已结算的追踪状态：终态 + 已落库最新价，但没有任何执行事实。
			st := model.RecommendationStatus{
				RecommendationID: rec.ID, BatchID: batch.ID, UserID: userID,
				Symbol: rec.Symbol, Market: "cn", Type: batch.Type, Action: rec.Action,
				RefPrice: 10, CurrentPrice: 12, ReturnPct: 20, Outcome: outcome,
			}
			if err := common.DB.Create(&st).Error; err != nil {
				t.Fatalf("建追踪状态失败: %v", err)
			}
			// 手动录入的持仓：recommendation_id=0，血缘查不出来。
			pos := seedHoldingWithLedger(t, userID, "600519", 11, 100, 0, 0, "2026-08-20")

			svc := NewPositionService(nil)
			if _, err := svc.LinkRecommendation(userID, pos.ID, rec.ID); err != nil {
				t.Fatalf("补关联失败: %v", err)
			}

			var after model.Position
			common.DB.First(&after, pos.ID)
			if after.RecommendationID != rec.ID {
				t.Fatalf("血缘未落库: got %d want %d", after.RecommendationID, rec.ID)
			}
			var stAfter model.RecommendationStatus
			common.DB.First(&stAfter, st.ID)
			if stAfter.ActualBuyPrice != 11 {
				t.Fatalf("终态推荐的实际买入价未回填（终态冻结绕过失效）: got %v want 11", stAfter.ActualBuyPrice)
			}
			// 未平仓按最新价：(12-11)/11*100 = 9.09
			if stAfter.ActualReturnPct == nil {
				t.Fatal("终态推荐的实际收益未回填")
			}
			if got := *stAfter.ActualReturnPct; got < 9.08 || got > 9.1 {
				t.Fatalf("实际收益算错: got %v want ~9.09", got)
			}
			// 模拟口径不得被执行事实污染。
			if stAfter.ReturnPct != 20 || stAfter.Outcome != outcome {
				t.Fatalf("补关联改写了模拟口径: return_pct=%v outcome=%s", stAfter.ReturnPct, stAfter.Outcome)
			}
		})
	}
}

// TestLinkRecommendationClosedPositionUsesSellPrice 已平仓持仓按真实卖出价定格实际收益，
// 不随最新行情漂移（与 actualReturnEndPrice 同口径）。
func TestLinkRecommendationClosedPositionUsesSellPrice(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8802
	batch, rec := seedLinkFixture(t, userID, "000001", model.RecTypeShortTerm, model.RecStatusSuccess)
	st := model.RecommendationStatus{
		RecommendationID: rec.ID, BatchID: batch.ID, UserID: userID,
		Symbol: rec.Symbol, Market: "cn", Type: batch.Type, Action: rec.Action,
		RefPrice: 10, CurrentPrice: 20, Outcome: model.RecOutcomeTakeProfit,
	}
	common.DB.Create(&st)
	pos := seedHoldingWithLedger(t, userID, "000001", 10, 100, 0, 0, "2026-08-01")
	// 平仓：卖出价 13（最新价 20 不该被用上）。
	common.DB.Model(&model.Position{}).Where("id = ?", pos.ID).Updates(map[string]any{
		"status": model.PositionStatusClosed, "sell_price": 13, "sell_date": "2026-08-10",
	})

	svc := NewPositionService(nil)
	if _, err := svc.LinkRecommendation(userID, pos.ID, rec.ID); err != nil {
		t.Fatalf("补关联失败: %v", err)
	}
	var stAfter model.RecommendationStatus
	common.DB.First(&stAfter, st.ID)
	if stAfter.ActualReturnPct == nil || *stAfter.ActualReturnPct != 30 {
		t.Fatalf("已平仓应按卖出价 13 定格 30%%: got %v", stAfter.ActualReturnPct)
	}
}

// TestLinkRecommendationRejectsSymbolMismatch 标的不一致必须拒绝——事后补关联是人工
// 选择，选错标的会直接污染「AI 推荐 vs 实际买入」的对比事实。
func TestLinkRecommendationRejectsSymbolMismatch(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8803
	_, rec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	pos := seedHoldingWithLedger(t, userID, "000002", 11, 100, 0, 0, "2026-08-20")

	svc := NewPositionService(nil)
	if _, err := svc.LinkRecommendation(userID, pos.ID, rec.ID); err == nil {
		t.Fatal("标的不一致应拒绝关联")
	}
	var after model.Position
	common.DB.First(&after, pos.ID)
	if after.RecommendationID != 0 {
		t.Fatalf("拒绝后不得落血缘: got %d", after.RecommendationID)
	}
}

// TestLinkRecommendationCrossUserIsolation 他人的推荐不可关联（全链路隔离铁律）。
func TestLinkRecommendationCrossUserIsolation(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const owner, intruder int64 = 8804, 8805
	_, rec := seedLinkFixture(t, owner, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	pos := seedHoldingWithLedger(t, intruder, "600519", 11, 100, 0, 0, "2026-08-20")

	svc := NewPositionService(nil)
	if _, err := svc.LinkRecommendation(intruder, pos.ID, rec.ID); err == nil {
		t.Fatal("不得关联他人的推荐")
	}
}

// TestUnlinkRecommendationClearsActualFact 解除关联要清零执行事实，否则旧的实际买入价
// 会继续挂在推荐上，看起来像「仍然持有」。
func TestUnlinkRecommendationClearsActualFact(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8806
	batch, rec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	st := model.RecommendationStatus{
		RecommendationID: rec.ID, BatchID: batch.ID, UserID: userID,
		Symbol: rec.Symbol, Market: "cn", Type: batch.Type, Action: rec.Action,
		RefPrice: 10, CurrentPrice: 12, Outcome: model.RecOutcomeExpired,
	}
	common.DB.Create(&st)
	pos := seedHoldingWithLedger(t, userID, "600519", 11, 100, 0, 0, "2026-08-20")

	svc := NewPositionService(nil)
	if _, err := svc.LinkRecommendation(userID, pos.ID, rec.ID); err != nil {
		t.Fatalf("补关联失败: %v", err)
	}
	if _, err := svc.LinkRecommendation(userID, pos.ID, 0); err != nil {
		t.Fatalf("解除关联失败: %v", err)
	}
	var after model.Position
	common.DB.First(&after, pos.ID)
	if after.RecommendationID != 0 {
		t.Fatalf("解除后血缘应为 0: got %d", after.RecommendationID)
	}
	var stAfter model.RecommendationStatus
	common.DB.First(&stAfter, st.ID)
	if stAfter.ActualBuyPrice != 0 || stAfter.ActualReturnPct != nil {
		t.Fatalf("解除后执行事实应清零: buy=%v ret=%v", stAfter.ActualBuyPrice, stAfter.ActualReturnPct)
	}
}

// TestRecommendationLinkCandidatesFiltersBatchStatus 候选只含 success/degraded 批次，
// 且限近 trackWindowDays 天内、同标的、本人的条目。
func TestRecommendationLinkCandidatesFiltersBatchStatus(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8807
	_, okRec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	_, degRec := seedLinkFixture(t, userID, "600519", model.RecTypeLongTerm, model.RecStatusDegraded)
	_, failRec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusFailed)
	_, otherSymbol := seedLinkFixture(t, userID, "000002", model.RecTypeShortTerm, model.RecStatusSuccess)
	_, otherUser := seedLinkFixture(t, 8808, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	// 超出 90 天窗口的旧条目。
	oldBatch, oldRec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	stale := time.Now().AddDate(0, 0, -(trackWindowDays + 5))
	common.DB.Model(&model.RecommendationBatch{}).Where("id = ?", oldBatch.ID).Update("created_at", stale)
	common.DB.Model(&model.Recommendation{}).Where("id = ?", oldRec.ID).Update("created_at", stale)

	svc := NewRecommendationService(nil, nil, nil)
	list, err := svc.RecommendationLinkCandidates(userID, "600519", "cn")
	if err != nil {
		t.Fatalf("查候选失败: %v", err)
	}
	got := map[int64]bool{}
	for _, c := range list {
		got[c.RecommendationID] = true
	}
	if !got[okRec.ID] || !got[degRec.ID] {
		t.Fatalf("success/degraded 条目应入选: %+v", list)
	}
	for name, id := range map[string]int64{
		"failed 批次": failRec.ID, "其它标的": otherSymbol.ID, "他人条目": otherUser.ID, "超窗口": oldRec.ID,
	} {
		if got[id] {
			t.Fatalf("%s 不应入选: %+v", name, list)
		}
	}
}

// TestRecommendationLinkCandidatesMarksOccupied 已被其它持仓关联的候选要标出占用者
// （前端提示用；不禁止改指——一条推荐允许对多笔持仓）。
func TestRecommendationLinkCandidatesMarksOccupied(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8809
	_, rec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	pos := seedHoldingWithLedger(t, userID, "600519", 11, 100, 0, 0, "2026-08-20")
	common.DB.Model(&model.Position{}).Where("id = ?", pos.ID).Update("recommendation_id", rec.ID)

	svc := NewRecommendationService(nil, nil, nil)
	list, err := svc.RecommendationLinkCandidates(userID, "600519", "cn")
	if err != nil || len(list) != 1 {
		t.Fatalf("应有 1 条候选: err=%v list=%+v", err, list)
	}
	if list[0].LinkedPositionID != pos.ID {
		t.Fatalf("未标出占用持仓: got %d want %d", list[0].LinkedPositionID, pos.ID)
	}
}

// TestUnlinkedHoldingsSoftMatch 无血缘条目按标的软匹配到 holding 持仓；有血缘时不提示，
// 已平仓不提示（软匹配只用于「你买了但没登记」这一种情况）。
func TestUnlinkedHoldingsSoftMatch(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8810
	_, recA := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	_, recB := seedLinkFixture(t, userID, "000002", model.RecTypeShortTerm, model.RecStatusSuccess)
	_, recC := seedLinkFixture(t, userID, "600036", model.RecTypeShortTerm, model.RecStatusSuccess)

	// A：手动录入的同标的持仓（应被软匹配到）
	posA := seedHoldingWithLedger(t, userID, "600519", 11, 100, 0, 0, "2026-08-20")
	// B：已有血缘（不该出现在软匹配结果里）
	posB := seedHoldingWithLedger(t, userID, "000002", 12, 200, 0, 0, "2026-08-19")
	common.DB.Model(&model.Position{}).Where("id = ?", posB.ID).Update("recommendation_id", recB.ID)
	// C：已平仓（不提示——追踪的是当前持有事实）
	posC := seedHoldingWithLedger(t, userID, "600036", 13, 300, 0, 0, "2026-08-18")
	common.DB.Model(&model.Position{}).Where("id = ?", posC.ID).Update("status", model.PositionStatusClosed)

	items := []model.Recommendation{
		{ID: recA.ID, Symbol: "600519", Market: "cn"},
		{ID: recB.ID, Symbol: "000002", Market: "cn"},
		{ID: recC.ID, Symbol: "600036", Market: "cn"},
	}
	linked := map[int64]RecPositionLink{recB.ID: {PositionID: posB.ID}}
	out := unlinkedHoldingsFor(userID, items, linked)

	if got, ok := out[recA.ID]; !ok || got.PositionID != posA.ID {
		t.Fatalf("无血缘的同标的持仓应被软匹配: %+v", out)
	}
	if _, ok := out[recB.ID]; ok {
		t.Fatal("已有血缘的条目不该进软匹配结果")
	}
	if _, ok := out[recC.ID]; ok {
		t.Fatal("已平仓持仓不该被软匹配")
	}
}

// TestGetExposesUnlinkedPositionOnlyWithoutLineage 详情视图里 position 与 unlinked_position
// 互斥：有血缘只给 position，无血缘才给软匹配提示。
func TestGetExposesUnlinkedPositionOnlyWithoutLineage(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8811
	batch, rec := seedLinkFixture(t, userID, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	pos := seedHoldingWithLedger(t, userID, "600519", 11, 100, 0, 0, "2026-08-20")

	svc := NewRecommendationService(nil, nil, nil)
	// 无血缘：应给 unlinked_position，不给 position。
	v, err := svc.Get(userID, batch.ID)
	if err != nil || len(v.Items) != 1 {
		t.Fatalf("取详情失败: err=%v items=%d", err, len(v.Items))
	}
	if v.Items[0].Position != nil {
		t.Fatal("无血缘时不该有 position")
	}
	if v.Items[0].UnlinkedPosition == nil || v.Items[0].UnlinkedPosition.PositionID != pos.ID {
		t.Fatalf("无血缘时应给软匹配提示: %+v", v.Items[0].UnlinkedPosition)
	}

	// 补关联后：应给 position，不再给 unlinked_position。
	if _, err := NewPositionService(nil).LinkRecommendation(userID, pos.ID, rec.ID); err != nil {
		t.Fatalf("补关联失败: %v", err)
	}
	v, err = svc.Get(userID, batch.ID)
	if err != nil {
		t.Fatalf("取详情失败: %v", err)
	}
	if v.Items[0].Position == nil || v.Items[0].Position.PositionID != pos.ID {
		t.Fatalf("有血缘时应给 position: %+v", v.Items[0].Position)
	}
	if v.Items[0].UnlinkedPosition != nil {
		t.Fatal("有血缘时不该再提示补关联")
	}
}

// TestPositionRecLinksForBatch 持仓列表的血缘富化：有血缘才有摘要，血缘指向已删除的
// 推荐时按无血缘处理（不能让徽章指向一条不存在的推荐）。
func TestPositionRecLinksForBatch(t *testing.T) {
	setupTestDB(t)
	cleanLedgerTables(t)
	const userID int64 = 8812
	batch, rec := seedLinkFixture(t, userID, "600519", model.RecTypeLongTerm, model.RecStatusSuccess)
	linkedPos := seedHoldingWithLedger(t, userID, "600519", 11, 100, 0, 0, "2026-08-20")
	common.DB.Model(&model.Position{}).Where("id = ?", linkedPos.ID).Update("recommendation_id", rec.ID)
	manualPos := seedHoldingWithLedger(t, userID, "000002", 12, 200, 0, 0, "2026-08-19")
	danglingPos := seedHoldingWithLedger(t, userID, "600036", 13, 300, 0, 0, "2026-08-18")
	common.DB.Model(&model.Position{}).Where("id = ?", danglingPos.ID).Update("recommendation_id", 999999)

	var rows []model.Position
	common.DB.Where("user_id = ?", userID).Find(&rows)
	out := positionRecLinksFor(userID, rows)

	link, ok := out[linkedPos.ID]
	if !ok {
		t.Fatalf("有血缘的持仓应有摘要: %+v", out)
	}
	if link.RecommendationID != rec.ID || link.BatchID != batch.ID ||
		link.Type != model.RecTypeLongTerm || link.RefPrice != 10 {
		t.Fatalf("血缘摘要字段不对: %+v", link)
	}
	if _, ok := out[manualPos.ID]; ok {
		t.Fatal("手动建仓不该有血缘摘要")
	}
	if _, ok := out[danglingPos.ID]; ok {
		t.Fatal("血缘指向已删除推荐时应按无血缘处理")
	}
}
