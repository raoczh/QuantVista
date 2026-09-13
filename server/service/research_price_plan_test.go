package service

import (
	"encoding/json"
	"reflect"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func researchFixture() (candidate, []datasource.Bar) {
	bars := genTrendBars(250, 10, .006)
	for i := range bars {
		c := 10 + float64(i)*.006
		setupTestCandle(bars, i, c-.02, c+.1, c-.1, c, 1000000)
	}
	last := bars[len(bars)-1]
	return candidate{Symbol: "600100", Market: "cn", Price: last.Close, QuoteAsOf: last.TradeDate + " 15:10"}, bars
}

func TestResearchPriceSingleSourceAcrossModules(t *testing.T) {
	c, bars := researchFixture()
	strat := shortStrategies[1] // 强势回踩
	pc := researchPriceContextFor(model.RecTypeShortTerm, &strat)
	plan := buildResearchPricePlan(c, bars, pc)
	if !researchPlanValid(plan) {
		t.Fatalf("完整行情应有共同计划：%+v", plan)
	}
	shortWindow := buildResearchPricePlan(c, bars[len(bars)-120:], pc)
	if !reflect.DeepEqual(plan, shortWindow) {
		t.Fatal("输入尾窗相同，不能因模块拉取根数不同而改变价格")
	}
	c.PricePlan = plan
	proposed := recPick{Action: model.RecActionBuy, BuyZoneLow: 99, BuyZoneHigh: 100, TakeProfit: 150, StopLoss: 90, Confidence: 80}
	pick := normalizePick(proposed, c.Symbol, c)
	result := &AnalysisResult{Rating: model.AnalysisRatingBullish, KillSwitches: []string{"跌破计划支撑后复核"}}
	snapshot := map[string]any{"freshness_status": freshStatusFresh, "quote": map[string]any{"price": c.Price}, "price_plan": plan, "recent_bars": compactBars(bars, 30)}
	usage, run := (&AnalysisService{}).attachTradePlan(t.Context(), 1, nil, "", false, AnalyzeRequest{Module: "stock", Market: "cn"}, snapshot, result, "test", "")
	if run != nil || !reflect.DeepEqual(usage, chatUsage{}) {
		t.Fatal("A股价格无需再调用AI定价")
	}
	got := result.TradePlan
	if got == nil || got.NoPlan || got.BuyLow != pick.BuyZoneLow || got.BuyHigh != pick.BuyZoneHigh || got.TargetPrice != pick.TakeProfit || got.StopPrice != pick.StopLoss {
		t.Fatalf("两个模块必须共用价格：pick=%+v analysis=%+v", pick, got)
	}
	if pick.PriceProposal == nil || pick.PriceProposal.Target != 150 || pick.TakeProfit == 150 {
		t.Fatal("AI提案须保留审计，但不能覆盖共同计划")
	}
	proposed.DegradedSource = "quant_fallback"
	if fallback := normalizePick(proposed, c.Symbol, c); fallback.PriceProposal != nil {
		t.Fatal("规则降级结果不能冒充AI原始价位提案")
	}
	forged := normalizePick(recPick{PricePlan: plan}, c.Symbol, candidate{})
	if forged.PricePlan != nil {
		t.Fatal("模型自附的程序价格计划必须剥除")
	}
	pick.ExecutionPlan = &executionPlan{Quantity: 500, PlannedPrice: plan.BuyHigh}
	attachRecommendationExitPlan(t.Context(), model.RecTypeShortTerm, &pick, c, pc.Profile)
	if pick.ExecutionPlan.ExitPlan.TargetPrice != pick.TakeProfit || pick.ExecutionPlan.ExitPlan.StopPrice != pick.StopLoss {
		t.Fatal("推荐卡/执行规划不能再生成另一套退出价")
	}
	if pick.ExecutionPlan.ExitPlan.Quantity != 500 || plan.Exit.Quantity == 500 {
		t.Fatal("按预算重算费用不能污染冻结的共同价格输入")
	}
	// 旧记录不经实时重算，缺失共同计划的历史 JSON 原样保留。
	var historical recPick
	if err := json.Unmarshal([]byte(`{"symbol":"600100","buy_zone_low":9,"buy_zone_high":10,"take_profit":12,"stop_loss":8}`), &historical); err != nil {
		t.Fatal(err)
	}
	if historical.PricePlan != nil || historical.TakeProfit != 12 {
		t.Fatal("不得重写历史价位")
	}
}

func TestResearchPriceWaitsForEntryWithoutInflatingTargets(t *testing.T) {
	c, bars := researchFixture()
	pc := researchPriceContextFor(model.RecTypeShortTerm, &shortStrategies[0])
	baseline := buildResearchPricePlan(c, bars, pc)
	c.Price += 2
	extended := buildResearchPricePlan(c, bars, pc)
	if !researchPlanValid(extended) || extended.Status != "wait" || extended.BuyHigh >= c.Price {
		t.Fatalf("延伸后应等待原区间：%+v", extended)
	}
	if extended.BuyHigh != baseline.BuyHigh || extended.Exit.TargetPrice != baseline.Exit.TargetPrice {
		t.Fatal("同一突破结构下，报价上涨不能推高买点与卖出目标")
	}
	long := buildResearchPricePlan(c, bars, researchPriceContextFor(model.RecTypeLongTerm, &shortStrategies[0]))
	if long.Exit == nil || long.Context.Horizon == extended.Context.Horizon || long.Exit.StopPrice == extended.Exit.StopPrice {
		t.Fatal("周期不同的风险容忍度应可解释地不同")
	}
	bad := append([]datasource.Bar{}, bars...)
	bad[len(bad)-1].Source = "sina"
	if got := buildResearchPricePlan(c, bad, pc); got.Status != "unavailable" || got.BuyHigh != 0 {
		t.Fatal("复权口径不可信时不得给出精确价位")
	}
}

func TestResearchPriceResistanceConstrainsTarget(t *testing.T) {
	c, bars := researchFixture()
	// 确認高点位于拟买价附近；两侧各两根都更低，不借未来数据确认末根。
	for i := len(bars) - 15; i < len(bars); i++ {
		setupTestCandle(bars, i, 11.30, 11.38, 11.20, 11.32, 1000000)
	}
	setupTestCandle(bars, len(bars)-7, 11.33, 11.43, 11.21, 11.35, 1000000)
	c.Price = 11.32
	plan := buildResearchPricePlan(c, bars, researchPriceContextFor(model.RecTypeShortTerm, &shortStrategies[1]))
	if !researchPlanValid(plan) {
		t.Fatalf("阻力附近仍应如实给研究规划：%+v", plan)
	}
	if plan.Exit.Resistance > 0 && plan.Exit.TargetPrice > plan.Exit.Resistance+.01 {
		t.Fatalf("不得抬高目标越过近端阻力来凑盈亏比：%+v", plan.Exit)
	}
	if plan.Exit.NetRewardRisk < 1.2 && plan.Status != "wait" {
		t.Fatal("低净风险收益不得标为就绪")
	}
}

func TestResearchPricePersistenceFeedsDetailAndTrackingBarriers(t *testing.T) {
	setupTestDB(t)
	c, bars := researchFixture()
	c.PricePlan = buildResearchPricePlan(c, bars, researchPriceContextFor(model.RecTypeShortTerm, &shortStrategies[1]))
	pick := normalizePick(recPick{Action: model.RecActionBuy, BuyZoneLow: 99, BuyZoneHigh: 100, TakeProfit: 150, StopLoss: 90, ValidDays: 5}, c.Symbol, c)
	if !researchPlanValid(pick.PricePlan) {
		t.Fatal("测试需要有效的程序价格计划")
	}
	batch, rec := seedLinkFixture(t, 8877, c.Symbol, model.RecTypeShortTerm, model.RecStatusSuccess)
	payload, err := json.Marshal(pick)
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&rec).Update("detail_json", string(payload)).Error; err != nil {
		t.Fatal(err)
	}
	view, err := (&RecommendationService{}).Get(rec.UserID, batch.ID)
	if err != nil || view == nil || len(view.Items) != 1 {
		t.Fatalf("读取冻结推荐失败：%v", err)
	}
	detail := view.Items[0].Detail
	if detail == nil || !reflect.DeepEqual(detail.PricePlan, pick.PricePlan) || detail.TakeProfit != pick.TakeProfit || detail.StopLoss != pick.StopLoss || detail.BuyZoneLow != pick.BuyZoneLow || detail.BuyZoneHigh != pick.BuyZoneHigh {
		t.Fatalf("页面明细必须读取同一份冻结程序价位：%+v", detail)
	}
	tp, sl, err := labelBarriers(t.Context(), &model.RecommendationLabel{RecommendationID: rec.ID, EntryMode: model.EntryModeNextOpen})
	if err != nil || tp != pick.PricePlan.Exit.TargetPrice || sl != pick.PricePlan.Exit.StopPrice {
		t.Fatalf("标签障碍不能使用 AI 未生效提案：tp=%v sl=%v err=%v", tp, sl, err)
	}
	out := evaluateTracking(trackInput{RefPrice: pick.BuyZoneHigh, TakeProfit: detail.TakeProfit, StopLoss: detail.StopLoss, IsShort: true, ValidDays: 5, ElapsedTradeDays: 1,
		Bars: []datasource.Bar{{TradeDate: "2026-09-14", Open: pick.BuyZoneHigh, High: tp + .01, Low: pick.BuyZoneHigh, Close: tp}}})
	if !out.HitTakeProfit || out.HitStopLoss {
		t.Fatalf("追踪应使用同一有效价位判定触达：%+v", out)
	}
	var after model.Recommendation
	if err := common.DB.First(&after, rec.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.DetailJSON != string(payload) {
		t.Fatal("读取、标签与追踪不得重写冻结价位")
	}
}
