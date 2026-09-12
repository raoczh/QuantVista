package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"quantvista/common"
	"quantvista/model"
)

func exitPlanFixture() (model.Position, ExitPlanSeed, time.Time) {
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.Local)
	p := model.Position{ID: 8801, UserID: 880, Symbol: "600901", Market: "cn", Currency: "CNY", PositionType: model.PositionTypeShortTerm,
		Status: model.PositionStatusHolding, BuyPrice: 10, BuyDate: "2026-09-01", Quantity: 1000, BuyFee: 5, RemainingCost: 10005,
		TotalBuyCost: 10005, TotalBuyQty: 1000, PeakPrice: 10, PeakFrom: "2026-09-01", PeakDate: "2026-09-01"}
	entryTime := now.AddDate(0, 0, -10)
	seed := buildExitPlanSeed(p, exitTestBars(entryTime, 10, 10.2, 9.8, 70), "momentum", "entry", p.BuyDate, entryTime, nil)
	return p, seed, now
}

func TestExitPlanInitialRiskProfilesFeesAndMissingData(t *testing.T) {
	p, s, now := exitPlanFixture()
	if s.DataStatus != "ready" || !(s.StopPrice < p.BuyPrice && s.TargetPrice > p.BuyPrice && s.ExtendedTarget > s.TargetPrice) {
		t.Fatalf("规划不完整: %+v", s)
	}
	if s.EstimatedRisk <= p.Quantity*(p.BuyPrice-s.StopPrice) || s.NetRewardRisk >= 1.5 {
		t.Fatal("初始风险必须包含费用与滑点")
	}
	pullback := buildExitPlanSeed(p, exitTestBars(now, 10, 10.2, 9.8, 70), "pullback", "entry", p.BuyDate, now, nil)
	if pullback.StopPrice <= s.StopPrice || pullback.TrailATR >= s.TrailATR {
		t.Fatal("策略退出容忍度应有明确区别")
	}
	p.PositionType = model.PositionTypeLongTerm
	long := buildExitPlanSeed(p, exitTestBars(now, 10, 10.2, 9.8, 70), "momentum", "entry", p.BuyDate, now, nil)
	if long.StopPrice >= s.StopPrice || long.ReviewDays <= s.ReviewDays {
		t.Fatal("长线不能复用短线风险距离与复核周期")
	}
	missing := buildExitPlanSeed(p, nil, "balanced", "entry", p.BuyDate, now, nil)
	if missing.DataStatus != "unavailable" || missing.StopPrice != 0 || missing.TargetPrice != 0 {
		t.Fatalf("缺日线不能编造默认价位: %+v", missing)
	}
	if decodeExitSeed(mustPositionExitJSON(s)) == nil {
		t.Fatal("合法规划应可重放")
	}
	s.StopPrice += 0.01
	if decodeExitSeed(mustPositionExitJSON(s)) != nil {
		t.Fatal("修改冻结价位必须使摘要失效")
	}
}

func TestExitPlanNearResistanceReducesRewardInsteadOfInventingTarget(t *testing.T) {
	p, _, now := exitPlanFixture()
	bars := exitTestBars(now, 10, 10.1, 9.9, 70)
	bars[64].High = 10.3
	s := buildExitPlanSeed(p, bars, "momentum", "entry", p.BuyDate, now, nil)
	if s.Resistance != 10.3 || s.TargetPrice >= 10.3 || s.NetRewardRisk >= 1.2 {
		t.Fatalf("近端阻力必须降低预期空间并显示风险收益不足: %+v", s)
	}
	if !strings.Contains(strings.Join(s.Evidence, " "), "不足 1.2") {
		t.Fatal("不合适的风险收益不能隐瞒")
	}
}

func TestExitPlanRatchetAndIntradayNoRetroactiveTouch(t *testing.T) {
	p, s, now := exitPlanFixture()
	sellable := p.Quantity
	o := exitPlanObservation{Position: p, Seed: s, Price: 14, High: 14, Low: 11, Peak: 10, ATR: 0.4, TradeDate: "2026-09-11", Now: now, PriceOK: true, TechnicalOK: true, Sellable: &sellable}
	first := evolveExitPlan(o)
	if first.CurrentStop < 12 || first.StopActive {
		t.Fatalf("盘中上移的保护不能回溯触发旧低点: %+v", first)
	}
	o.Previous = &first
	o.Now = now.Add(time.Minute)
	o.Price = 13.5
	o.ATR = 2
	second := evolveExitPlan(o)
	if second.CurrentStop < first.CurrentStop || second.StopActive {
		t.Fatalf("波动扩大不能放宽保护或借旧低点触发: %+v", second)
	}
	o.Previous = &second
	o.Price = second.CurrentStop - 0.01
	o.Now = now.Add(2 * time.Minute)
	third := evolveExitPlan(o)
	if !third.StopActive || third.StopEpisode != 1 || !hasPositionExitSignal(exitPlanSignals(third), "profit_protection", 0) {
		t.Fatalf("当前价格真正穿过保护应提醒: %+v", third)
	}
	o.Previous = &third
	o.Now = now.Add(3 * time.Minute)
	fourth := evolveExitPlan(o)
	key, date := exitPlanActionIdentity(third, "profit_protection")
	key2, date2 := exitPlanActionIdentity(fourth, "profit_protection")
	if key != key2 || date != date2 {
		t.Fatal("同一持续触发不能按轮次产生新提醒")
	}
	o.Previous = &fourth
	o.TechnicalOK = false
	o.ATR = 0
	o.Price = 15
	stale := evolveExitPlan(o)
	if stale.CurrentStop != fourth.CurrentStop || stale.DataStatus != "partial" {
		t.Fatal("技术数据缺口时只能保持已建立的保护")
	}
}

func TestExitPlanTargetsPartialSellAndTPlusOne(t *testing.T) {
	p, s, now := exitPlanFixture()
	zero := 0.0
	o := exitPlanObservation{Position: p, Seed: s, Price: s.TargetPrice, High: s.TargetPrice, Low: s.TargetPrice, ATR: s.ATR14,
		TradeDate: "2026-09-11", Now: now, PriceOK: true, TechnicalOK: true, Sellable: &zero}
	first := evolveExitPlan(o)
	if first.FirstTargetAt == "" || first.SuggestedQuantity != 0 || !hasPositionExitSignal(exitPlanSignals(first), "target_first", 0) {
		t.Fatal("T+1 只约束可执行数量，不能吞掉目标触价提醒")
	}
	oldStop := first.CurrentStop
	p.Quantity = 500
	p.RemainingCost = 5002.5
	if exitPlanBasis(p) != s.BasisHash {
		t.Fatal("减仓不应改变初始风险依据")
	}
	available := 500.0
	o.Position = p
	o.Previous = &first
	o.Sellable = &available
	o.Now = now.Add(time.Hour)
	reduced := evolveExitPlan(o)
	if !reduced.FirstTargetHandled || reduced.CurrentStop < oldStop || hasPositionExitSignal(exitPlanSignals(reduced), "target_first", 0) {
		t.Fatal("实际减仓后保留保护，不再要求处理已兑现的第一阶段")
	}
	o.Previous = &reduced
	o.Price = s.ExtendedTarget
	o.High = o.Price
	o.Low = o.Price
	extended := evolveExitPlan(o)
	if extended.SecondTargetAt == "" || !hasPositionExitSignal(exitPlanSignals(extended), "target_extended", 0) || extended.SuggestedQuantity != 500 {
		t.Fatal("剩余仓位应有独立的延伸目标提醒")
	}
}

func TestExitPlanBreakevenMinimumCommissionAndFundTick(t *testing.T) {
	p, _, _ := exitPlanFixture()
	p.Quantity = 100
	p.RemainingCost = 1005
	price := exitBreakeven(p, 10)
	if exitNetProceeds(p, price, p.Quantity, 10) < p.RemainingCost || exitNetProceeds(p, price-0.01, p.Quantity, 10) >= p.RemainingCost {
		t.Fatal("净保本必须覆盖最低佣金、税及滑点，且按最小价格单位取整")
	}
	p.Symbol = "510300"
	p.BuyPrice = 4.001
	p.Quantity = 1000
	p.RemainingCost = 4006
	price = exitBreakeven(p, 10)
	if math.Abs(price*1000-math.Round(price*1000)) > 1e-6 || exitNetProceeds(p, price, p.Quantity, 10) < p.RemainingCost {
		t.Fatal("基金使用三位价格精度与免印花税口径")
	}
}

func TestExitPlanCannotRaiseBreakevenAboveObservedPeak(t *testing.T) {
	p, _, now := exitPlanFixture()
	p.Quantity, p.TotalBuyQty, p.BuyFee, p.RemainingCost, p.TotalBuyCost = 1, 1, 5, 15, 15
	s := buildExitPlanSeed(p, exitTestBars(now, 10, 10.2, 9.8, 70), "momentum", "entry", p.BuyDate, now, nil)
	plan := evolveExitPlan(exitPlanObservation{Position: p, Seed: s, Price: 12, High: 12, Low: 12, ATR: s.ATR14, Now: now, TradeDate: "2026-09-11", PriceOK: true, TechnicalOK: true})
	if plan.CurrentStop >= plan.PeakPrice || plan.StopActive {
		t.Fatalf("佣金导致的高保本价不能凭空制造已经触发的保护: %+v", plan)
	}
	for _, signal := range exitPlanSignals(plan) {
		if strings.Contains(signal.Label, "止盈") {
			t.Fatal("扣费后仍亏损的价格不能称作止盈")
		}
	}
}

func TestExitPlanLegacyRemainingCostExcludesPreviouslySoldLots(t *testing.T) {
	p, _, now := exitPlanFixture()
	// 买 1000 股 @10，卖 500 股后再买 500 股 @20；每次买入费 5 元。
	// 未补齐余额的新旧混合账本，剩余成本是 15007.5，累计均价摊回会低估成 13340。
	p.BuyPrice, p.Quantity, p.BuyFee = 15, 1000, 7.5
	p.TotalBuyCost, p.TotalBuyQty, p.RemainingCost = 20010, 1500, 0
	seed := buildExitPlanSeed(p, exitTestBars(now, 15, 15.2, 14.8, 70), "balanced", "first_assessment", p.BuyDate, now, nil)
	if seed.Cost != 15007.5 || seed.DataStatus != "partial" || exitBreakeven(p, seed.SlippageBPS) <= 15 {
		t.Fatalf("已结转仓位不能压低当前净保本价，且应声明精确余额缺口: %+v", seed)
	}
}

func TestExitPlanFutureBarsAndLegacyLedgerHydration(t *testing.T) {
	p, _, now := exitPlanFixture()
	bars := exitTestBars(now, 10, 10.2, 9.8, 70)
	base := buildExitPlanSeed(p, bars, "momentum", "entry", p.BuyDate, now, nil)
	future := model.DailyBar{TradeDate: now.AddDate(0, 0, 1).Format("2006-01-02"), Open: math.NaN(), High: 1000, Low: 1, Close: 900}
	next := buildExitPlanSeed(p, append(bars, future), "momentum", "entry", p.BuyDate, now, nil)
	if next.Hash != base.Hash {
		t.Fatal("未来根的内容不能改变当时的退出规划")
	}
	p.TotalBuyCost, p.TotalBuyQty, p.RemainingCost = 0, 0, 0
	legacy := exitPlanBasis(p)
	p.TotalBuyCost, p.TotalBuyQty, p.RemainingCost = 10005, 1000, 10005
	if legacy != exitPlanBasis(p) {
		t.Fatal("等价账本惰性补齐不能改变经济依据并重置保护")
	}
}

func TestExitPlanQuarantinesUnverifiedPeakAndRebuildsAfterRepair(t *testing.T) {
	p, s, now := exitPlanFixture()
	prior := evolveExitPlan(exitPlanObservation{Position: p, Seed: s, Price: 14, High: 14, Low: 14, ATR: s.ATR14, Now: now, TradeDate: "2026-09-11", PriceOK: true, TechnicalOK: true})
	p.PeakDataQuality = model.FactorQualityUnverifiedAdjustment
	bars := exitTestBars(now, 10, 10.2, 9.8, 70)
	row := evaluatePositionExit(positionExitInput{position: p, quote: freshExitQuote(now, 10, 10, 10), barRows: bars, now: now,
		session: model.PositionExitSessionIntraday, planSeed: &s, previousPlan: &prior}, defaultPositionExitParams)
	quarantined := decodeExitPlan(row.PlanJSON, row.PlanHash)
	if quarantined == nil || !quarantined.ProtectionSuspended || quarantined.DataStatus != "unavailable" || hasPositionExitSignal(secondRowSignals(row), "profit_protection", 0) {
		t.Fatal("峰值失去价格依据后，不能继续用旧保护制造假卖出信号")
	}
	p.PeakDataQuality = ""
	repaired := positionExitSeedFor(nil, p, quarantined, bars, now, nil)
	if repaired.Source != "data_recovery" || repaired.Hash == s.Hash {
		t.Fatal("数据修正必须有可追溯的新规划依据")
	}
}

func TestExitPlanCorporatePreservesProtectionAndExactRevert(t *testing.T) {
	p, s, now := exitPlanFixture()
	o := exitPlanObservation{Position: p, Seed: s, Price: 14, High: 14, Low: 14, ATR: s.ATR14, PriceOK: true, TechnicalOK: true, TradeDate: "2026-09-11", Now: now}
	plan := evolveExitPlan(o)
	s.Carry = exitCarryFromPlan(plan)
	sealExitSeed(&s)
	before := exitPlanCorporateSnapshot{9, 11.5, mustPositionExitJSON(s)}
	p.PlanStopLoss, p.PlanTakeProfit = 9, 11.5
	p.Quantity *= 2
	p.BuyPrice /= 2
	adj := model.PositionCorpAdjust{BonusRatio: 10}
	applyExitCorporatePrices(&p, before, &adj)
	adjusted := decodeExitSeed(p.ExitPlanSeedJSON)
	if adjusted == nil || p.PlanStopLoss != 4.5 || p.PlanTakeProfit != 5.75 || adjusted.Carry == nil || adjusted.Carry.Stop != plan.CurrentStop/2 {
		t.Fatalf("除权必须同时折算固定价位和已建立保护: %+v", adjusted)
	}
	if err := restoreExitCorporatePrices(&p, adj); err != nil || p.PlanStopLoss != before.Stop || p.PlanTakeProfit != before.Take || p.ExitPlanSeedJSON != before.Seed {
		t.Fatalf("撤销应还原审计原值: %v", err)
	}
	p.PlanStopLoss = 4.6
	if restoreExitCorporatePrices(&p, adj) == nil {
		t.Fatal("不得覆盖折算后手工修改的计划")
	}
}

func TestExitPlanNoticeAtomicityAndStaleParent(t *testing.T) {
	setupTestDB(t)
	p, s, now := exitPlanFixture()
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	in := positionExitInput{position: p, quote: freshExitQuote(now, 8, 8, 8), now: now, session: model.PositionExitSessionIntraday, planSeed: &s, planParentID: &zero}
	row := evaluatePositionExit(in, defaultPositionExitParams)
	callback := "test_exit_notice_failure"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*model.PositionExitNotice); ok {
			tx.AddError(errors.New("injected queue failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := persistPositionExitAssessment(context.Background(), &row)
	common.DB.Callback().Create().Remove(callback)
	if err == nil {
		t.Fatal("通知事件落库失败必须回滚评估，不能留下永远无法投递的事实")
	}
	var count int64
	common.DB.Model(&model.PositionExitAssessment{}).Count(&count)
	if count != 0 {
		t.Fatal("事务未回滚")
	}
	if inserted, _, err := persistPositionExitAssessment(context.Background(), &row); err != nil || !inserted {
		t.Fatalf("正常落库失败: %v", err)
	}
	stale := evaluatePositionExit(in, defaultPositionExitParams)
	stale.EvaluatedAt = now.Add(time.Minute)
	stale.FactHash = "obsolete_plan"
	if inserted, _, err := persistPositionExitAssessment(context.Background(), &stale); err != nil || inserted {
		t.Fatal("父事实改变后不能提交旧规划覆盖新保护")
	}
	common.DB.Model(&model.PositionExitNotice{}).Count(&count)
	if count != 1 {
		t.Fatalf("同一事件只能排队一次，got %d", count)
	}
	if decodePositionExitAssessment(row).ExitPlan == nil {
		t.Fatal("已保存规划应能校验并展示")
	}
	var damaged map[string]any
	json.Unmarshal([]byte(row.PlanJSON), &damaged)
	damaged["current_stop"] = 1.0
	data, _ := json.Marshal(damaged)
	row.PlanJSON = string(data)
	if decodePositionExitAssessment(row).ExitPlan != nil {
		t.Fatal("损坏的规划不能显示成有效保护")
	}
}
