package service

import (
	"encoding/json"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func executionTestSnapshot(risk, horizon string, capital float64) recommendationPreferenceSnapshot {
	return recommendationPreferenceSnapshot{
		Version:                recommendationPreferenceSnapshotVersion,
		PreferenceFound:        true,
		RiskLevel:              risk,
		HorizonPref:            horizon,
		TotalCapital:           capital,
		DefaultRecCount:        3,
		InvestmentGuideVersion: InvestmentGuideCurrentVersion,
		InvestmentGuideStatus:  InvestmentGuideCompleted,
		RiskBudget:             defaultRiskBudgetParams(),
	}
}

func executionTestShortPick() recPick {
	return recPick{
		Symbol: "600000", Action: model.RecActionBuy, Confidence: 78, PositionPct: 10,
		BuyZoneLow: 9, BuyZoneHigh: 11, TakeProfit: 13, StopLoss: 8,
	}
}

func executionTestCandidate() candidate {
	return candidate{Symbol: "600000", Market: "cn", Price: 10, QuoteAsOf: "2026-08-07 14:55"}
}

func hasExecutionReason(plan *executionPlan, part string) bool {
	for _, reason := range plan.UnavailableReasons {
		if strings.Contains(reason, part) {
			return true
		}
	}
	return false
}

func TestBuildExecutionPlanShortReady(t *testing.T) {
	plan := buildExecutionPlan(model.RecTypeShortTerm, executionTestShortPick(), executionTestCandidate(),
		executionTestSnapshot("balanced", HorizonShortTerm, 100000), false, true, "fresh")
	if plan.Status != executionReady {
		t.Fatalf("有效短线计划应 ready: %+v", plan)
	}
	if plan.PlannedCapital != 10000 || plan.PlannedPrice != 11 || plan.Quantity != 900 || plan.EstimatedCapital != 9905 {
		t.Fatalf("研究预算与整手计算错误: %+v", plan)
	}
	if plan.MaxPlannedLoss == nil || *plan.MaxPlannedLoss != 2713.6 {
		t.Fatalf("最大计划亏损应包含买卖费用与卖出印花税: %+v", plan)
	}
	if plan.BudgetBasis != "research_budget" || plan.Version != executionPlanVersion {
		t.Fatalf("预算口径/版本未固化: %+v", plan)
	}
}

func TestBuildExecutionPlanPreferenceAndCapitalGuards(t *testing.T) {
	pick, cand := executionTestShortPick(), executionTestCandidate()

	missing := executionTestSnapshot("", "", 100000)
	missing.PreferenceFound = false
	if plan := buildExecutionPlan(model.RecTypeShortTerm, pick, cand, missing, false, true, "fresh"); plan.Status != executionNotSuitable || !hasExecutionReason(plan, "偏好缺失") {
		t.Fatalf("偏好缺失不得可执行: %+v", plan)
	}

	unconfirmed := executionTestSnapshot("balanced", HorizonShortTerm, 100000)
	unconfirmed.InvestmentGuideVersion = 0
	unconfirmed.InvestmentGuideStatus = InvestmentGuideNotStarted
	if plan := buildExecutionPlan(model.RecTypeShortTerm, pick, cand, unconfirmed, false, true, "fresh"); plan.Status != executionNotSuitable || !hasExecutionReason(plan, "尚未完成") {
		t.Fatalf("不得根据偏好默认值猜测用户完成过向导: %+v", plan)
	}

	skipped := executionTestSnapshot("balanced", HorizonShortTerm, 100000)
	skipped.InvestmentGuideStatus = InvestmentGuideSkipped
	if plan := buildExecutionPlan(model.RecTypeShortTerm, pick, cand, skipped, false, true, "fresh"); plan.Status != executionNotSuitable || !hasExecutionReason(plan, "已跳过") {
		t.Fatalf("跳过状态应被明确保存且不得冒充已确认偏好: %+v", plan)
	}

	zero := executionTestSnapshot("balanced", HorizonShortTerm, 0)
	if plan := buildExecutionPlan(model.RecTypeShortTerm, pick, cand, zero, false, true, "fresh"); plan.Status != executionNotSuitable || !hasExecutionReason(plan, "总投资资金未设置") {
		t.Fatalf("资金为 0 不得可执行: %+v", plan)
	}

	small := executionTestSnapshot("balanced", HorizonShortTerm, 5000) // 10%=500 < 11*100
	if plan := buildExecutionPlan(model.RecTypeShortTerm, pick, cand, small, false, true, "fresh"); plan.Status != executionNotSuitable || plan.Quantity != 0 || !hasExecutionReason(plan, "不足买入一手") {
		t.Fatalf("不足一手不得可执行: %+v", plan)
	}
}

func TestBuildExecutionPlanWaitInvalidAndExisting(t *testing.T) {
	snap := executionTestSnapshot("balanced", HorizonShortTerm, 100000)
	pick, cand := executionTestShortPick(), executionTestCandidate()

	if plan := buildExecutionPlan(model.RecTypeShortTerm, pick, cand, snap, false, true, "stale"); plan.Status != executionWait || !hasExecutionReason(plan, "行情时效") {
		t.Fatalf("stale 行情只能等待: %+v", plan)
	}
	if plan := buildExecutionPlan(model.RecTypeShortTerm, pick, cand, snap, true, true, "fresh"); plan.Status != executionNotSuitable || !hasExecutionReason(plan, "已有持仓") {
		t.Fatalf("已有持仓不得重复给新建仓可执行状态: %+v", plan)
	}

	invalid := pick
	invalid.StopLoss = 12
	if plan := buildExecutionPlan(model.RecTypeShortTerm, invalid, cand, snap, false, true, "fresh"); plan.Status != executionNotSuitable || !hasExecutionReason(plan, "价位关系无效") {
		t.Fatalf("无效价位关系不得可执行: %+v", plan)
	}

	watch := pick
	watch.Action, watch.PositionPct = model.RecActionWatch, 0
	if plan := buildExecutionPlan(model.RecTypeShortTerm, watch, cand, snap, false, true, "fresh"); plan.Status != executionWait || !hasExecutionReason(plan, "原推荐动作为观察") {
		t.Fatalf("watch 只能等待且不得凭空给仓位: %+v", plan)
	}
}

func TestBuildExecutionPlanLongTerm(t *testing.T) {
	pick := recPick{
		Symbol: "600000", Action: model.RecActionBuy, Confidence: 70, PositionPct: 10,
		ValuationLow: 8, ValuationHigh: 12,
	}
	cand := executionTestCandidate()
	snap := executionTestSnapshot("balanced", HorizonLongTerm, 100000)
	plan := buildExecutionPlan(model.RecTypeLongTerm, pick, cand, snap, false, true, "fresh")
	if plan.Status != executionReady || plan.Quantity != 900 || plan.EstimatedCapital != 9005 {
		t.Fatalf("有效长线估值区间应可形成整手计划: %+v", plan)
	}
	cand.Price = 13
	if plan = buildExecutionPlan(model.RecTypeLongTerm, pick, cand, snap, false, true, "fresh"); plan.Status != executionWait || !hasExecutionReason(plan, "高于研究估值区间") {
		t.Fatalf("长线价格超区间应等待: %+v", plan)
	}
}

func TestBuildExecutionPlanExplainsTemporaryHorizonOverride(t *testing.T) {
	plan := buildExecutionPlan(model.RecTypeLongTerm, recPick{
		Symbol: "600000", Action: model.RecActionBuy, PositionPct: 10, ValuationLow: 8, ValuationHigh: 12,
	}, executionTestCandidate(), executionTestSnapshot("balanced", HorizonShortTerm, 100000), false, true, "fresh")
	joined := strings.Join(plan.PreferenceExplanation, "；")
	if !strings.Contains(joined, "临时选择长线") || !strings.Contains(joined, "默认短线") {
		t.Fatalf("临时覆盖周期时不得冒充默认偏好: %+v", plan.PreferenceExplanation)
	}
}

func TestRiskPreferenceSizingVersionedAndDoesNotRewriteAI(t *testing.T) {
	p := defaultRiskBudgetParams()
	if p.Version != riskBudgetVersion || p.ConservativeMultiplier != 0.75 || p.BalancedMultiplier != 1 || p.AggressiveMultiplier != 1.15 {
		t.Fatalf("风险预算映射须集中且版本化: %+v", p)
	}
	rawAction, rawConfidence := model.RecActionBuy, FlexInt(81)
	picks := []recPick{{Symbol: "600000", Action: rawAction, Confidence: rawConfidence, PositionPct: 20}}
	applyRiskPreferenceSizing(picks, executionTestSnapshot("conservative", HorizonLongTerm, 100000), RegimeNeutral)
	if picks[0].PositionPct != 15 {
		t.Fatalf("保守风险预算应把现有仓位 20%% 缩为 15%%: %+v", picks[0])
	}
	if picks[0].Action != rawAction || picks[0].Confidence != rawConfidence {
		t.Fatalf("风险适配不得改写 AI action/confidence: %+v", picks[0])
	}
	unconfirmed := []recPick{{Symbol: "600001", Action: rawAction, Confidence: rawConfidence, PositionPct: 20}}
	skipped := executionTestSnapshot("conservative", HorizonLongTerm, 100000)
	skipped.InvestmentGuideStatus = InvestmentGuideSkipped
	applyRiskPreferenceSizing(unconfirmed, skipped, RegimeNeutral)
	if unconfirmed[0].PositionPct != 20 {
		t.Fatalf("未完成向导时不得把默认风险值当作用户选择: %+v", unconfirmed[0])
	}
}

func TestHoldingSymbolSetUserIsolation(t *testing.T) {
	setupTestDB(t)
	const userID, otherUserID = int64(91004), int64(91005)
	common.DB.Where("user_id IN ?", []int64{userID, otherUserID}).Delete(&model.Position{})
	t.Cleanup(func() { common.DB.Where("user_id IN ?", []int64{userID, otherUserID}).Delete(&model.Position{}) })
	common.DB.Create(&model.Position{
		UserID: otherUserID, Symbol: "600000", Market: "cn", Status: model.PositionStatusHolding, Quantity: 100,
	})
	set1, err := loadHoldingSymbolSet(userID)
	if err != nil || set1["cn:600000"] {
		t.Fatalf("不得读取其他用户持仓: set=%v err=%v", set1, err)
	}
	common.DB.Create(&model.Position{
		UserID: userID, Symbol: "600000", Market: "cn", Status: model.PositionStatusHolding, Quantity: 100,
	})
	set1, err = loadHoldingSymbolSet(userID)
	if err != nil || !set1["cn:600000"] {
		t.Fatalf("本人持仓应被识别: set=%v err=%v", set1, err)
	}
}

func TestPreferenceSnapshotAndExecutionPlanHistoricalReplay(t *testing.T) {
	setupTestDB(t)
	const userID, otherUserID = int64(91001), int64(91002)
	cleanup := func() {
		common.DB.Where("user_id = ?", userID).Delete(&model.Recommendation{})
		common.DB.Where("user_id = ?", userID).Delete(&model.RecommendationBatch{})
		common.DB.Where("user_id = ?", userID).Delete(&model.UserPreference{})
	}
	cleanup()
	t.Cleanup(cleanup)
	pref := model.UserPreference{
		UserID: userID, RiskLevel: "conservative", DefaultMarket: "cn", HorizonPref: HorizonShortTerm,
		DefaultRecCount: 3, TotalCapital: 100000, InvestmentGuideVersion: 1,
		InvestmentGuideStatus: InvestmentGuideCompleted,
	}
	if err := common.DB.Create(&pref).Error; err != nil {
		t.Fatal(err)
	}
	snap, err := captureRecommendationPreferenceSnapshot(userID)
	if err != nil {
		t.Fatal(err)
	}
	pick, cand := executionTestShortPick(), executionTestCandidate()
	pick.ExecutionPlan = buildExecutionPlan(model.RecTypeShortTerm, pick, cand, snap, false, true, "fresh")
	detail, _ := json.Marshal(pick)
	batch := model.RecommendationBatch{
		UserID: userID, Type: model.RecTypeShortTerm, Market: "cn", Strategy: "momentum",
		Status: model.RecStatusSuccess, PreferenceSnapshot: marshalPreferenceSnapshot(snap),
	}
	if err := common.DB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	rec := model.Recommendation{
		BatchID: batch.ID, UserID: userID, Symbol: cand.Symbol, Market: cand.Market,
		Action: pick.Action, Confidence: int(pick.Confidence), RefPrice: cand.Price, DetailJSON: string(detail),
	}
	if err := common.DB.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	common.DB.Model(&model.UserPreference{}).Where("user_id = ?", userID).Updates(map[string]any{
		"risk_level": "aggressive", "horizon_pref": HorizonLongTerm, "total_capital": 1000000,
	})

	view, err := (&RecommendationService{}).Get(userID, batch.ID)
	if err != nil || len(view.Items) != 1 || view.Items[0].Detail == nil || view.Items[0].Detail.ExecutionPlan == nil {
		t.Fatalf("历史批次读取失败: view=%+v err=%v", view, err)
	}
	if view.Items[0].Detail.ExecutionPlan.PlannedCapital != 10000 {
		t.Fatalf("历史执行计划不得随当前资金变化: %+v", view.Items[0].Detail.ExecutionPlan)
	}
	var stored recommendationPreferenceSnapshot
	if err := json.Unmarshal([]byte(view.PreferenceSnapshot), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.RiskLevel != "conservative" || stored.HorizonPref != HorizonShortTerm || stored.TotalCapital != 100000 {
		t.Fatalf("历史偏好快照应复现生成时事实: %+v", stored)
	}
	if _, err := (&RecommendationService{}).Get(otherUserID, batch.ID); err == nil {
		t.Fatal("其他用户不得读取历史偏好快照与执行计划")
	}
}

func TestOldRecommendationWithoutExecutionPlanCompatible(t *testing.T) {
	setupTestDB(t)
	const userID = int64(91003)
	cleanup := func() {
		common.DB.Where("user_id = ?", userID).Delete(&model.Recommendation{})
		common.DB.Where("user_id = ?", userID).Delete(&model.RecommendationBatch{})
	}
	cleanup()
	t.Cleanup(cleanup)
	batch := model.RecommendationBatch{
		UserID: userID, Type: model.RecTypeLongTerm, Market: "cn", Strategy: "value", Status: model.RecStatusSuccess,
	}
	common.DB.Create(&batch)
	old := recPick{Symbol: "600000", Action: model.RecActionWatch, Confidence: 50, Thesis: "旧记录"}
	detail, _ := json.Marshal(old)
	common.DB.Create(&model.Recommendation{
		BatchID: batch.ID, UserID: userID, Symbol: old.Symbol, Market: "cn", Action: old.Action,
		Confidence: int(old.Confidence), DetailJSON: string(detail),
	})
	view, err := (&RecommendationService{}).Get(userID, batch.ID)
	if err != nil || len(view.Items) != 1 || view.Items[0].Detail == nil {
		t.Fatalf("旧记录应继续可读: view=%+v err=%v", view, err)
	}
	if view.PreferenceSnapshot != "" || view.Items[0].Detail.ExecutionPlan != nil {
		t.Fatalf("旧记录不得用当前偏好伪造历史快照/执行计划: %+v", view)
	}
}
