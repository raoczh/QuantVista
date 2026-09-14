package service

import (
	"fmt"
	"math"
	"strings"
	"time"

	"quantvista/datasource"
	"quantvista/model"
)

const researchPricePlanVersion = "rp2"

type ResearchPriceContext struct {
	StrategyKey        string `json:"strategy_key"`
	StrategyName       string `json:"strategy_name"`
	StrategyRevisionID int64  `json:"strategy_revision_id,omitempty"`
	Profile            string `json:"profile"`
	Intent             string `json:"intent"`
	Horizon            string `json:"horizon"`
}

type researchPriceBuildContext struct {
	Context  ResearchPriceContext
	Strategy *strategyTemplate
}

func analysisPriceContext(userID int64, req AnalyzeRequest) (researchPriceBuildContext, error) {
	horizon := req.PriceHorizon
	if horizon == "" {
		horizon = model.RecTypeShortTerm
	}
	if horizon != model.RecTypeShortTerm && horizon != model.RecTypeLongTerm {
		return researchPriceBuildContext{}, fmt.Errorf("价格计划周期必须为短线或长线")
	}
	strat, err := resolveRecStrategyRevision(userID, horizon, req.PriceStrategy, req.PriceStrategyRevisionID)
	if err != nil {
		return researchPriceBuildContext{}, err
	}
	return researchPriceBuildContext{Context: researchPriceContextFor(horizon, strat), Strategy: strat}, nil
}

type ResearchPriceProposal struct {
	BuyLow  float64 `json:"buy_low"`
	BuyHigh float64 `json:"buy_high"`
	Target  float64 `json:"target_price"`
	Stop    float64 `json:"stop_price"`
}

func applyResearchPriceToPick(p *recPick, plan *ResearchPricePlan) {
	p.PricePlan, p.PriceProposal = nil, nil
	if plan == nil {
		return
	}
	copy := *plan
	p.PricePlan = &copy
	if p.DegradedSource == "" && (p.BuyZoneLow > 0 || p.BuyZoneHigh > 0 || p.TakeProfit > 0 || p.StopLoss > 0) {
		p.PriceProposal = &ResearchPriceProposal{p.BuyZoneLow, p.BuyZoneHigh, p.TakeProfit, p.StopLoss}
	}
	p.BuyZoneLow, p.BuyZoneHigh, p.TakeProfit, p.StopLoss = 0, 0, 0, 0
	if !researchPlanValid(plan) {
		p.Action = model.RecActionWatch
		p.Risks = append(p.Risks, "统一程序价位依据不足，原模型价位不作为可执行买卖价")
		return
	}
	p.BuyZoneLow, p.BuyZoneHigh = plan.BuyLow, plan.BuyHigh
	p.TakeProfit, p.StopLoss, p.ValidDays = plan.Exit.TargetPrice, plan.Exit.StopPrice, plan.EntryValidDays
}

// ResearchPricePlan 是个股分析与推荐共同的研究价位事实。AI 解释和否决计划，
// 不再各自生成一套可生效的买卖价；实际持仓仍按真实成本建立独立保护状态。
type ResearchPricePlan struct {
	Version        string               `json:"version"`
	InputHash      string               `json:"input_hash"`
	Context        ResearchPriceContext `json:"context"`
	QuoteAsOf      string               `json:"quote_as_of"`
	BarsAsOf       string               `json:"bars_as_of"`
	ReferencePrice float64              `json:"reference_price"`
	Status         string               `json:"status"` // ready / wait / unavailable
	BuyLow         float64              `json:"buy_low"`
	BuyHigh        float64              `json:"buy_high"`
	EntryAnchor    float64              `json:"entry_anchor"`
	EntryATR       float64              `json:"entry_atr"`
	HorizonDays    int                  `json:"horizon_days"`
	EntryValidDays int                  `json:"entry_valid_days"`
	Reasons        []string             `json:"reasons"`
	SetupReasons   []string             `json:"setup_reasons,omitempty"`
	Evidence       []string             `json:"evidence"`
	Exit           *ExitPlanSeed        `json:"exit,omitempty"`
}

func researchPriceContextFor(recType string, strat *strategyTemplate) ResearchPriceContext {
	if strat == nil {
		return ResearchPriceContext{StrategyKey: "balanced", StrategyName: "均衡研究", Profile: "balanced", Intent: "custom", Horizon: recType}
	}
	public := publicStrategy(*strat)
	return ResearchPriceContext{StrategyKey: strat.Key, StrategyName: strat.Name, StrategyRevisionID: strat.StrategyRevisionID,
		Profile: strat.baseKey, Intent: public.Intent, Horizon: recType}
}

func researchDailyBars(symbol, market string, bars []datasource.Bar) []model.DailyBar {
	rows := make([]model.DailyBar, len(bars))
	for i, b := range bars {
		rows[i] = model.DailyBar{Symbol: symbol, Market: market, TradeDate: b.TradeDate, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Source: b.Source}
	}
	return rows
}

func researchPlanValid(p *ResearchPricePlan) bool {
	return p != nil && p.Version == researchPricePlanVersion && p.Status != "unavailable" &&
		p.Exit != nil && validExitSeed(*p.Exit) && p.Exit.DataStatus != "unavailable" &&
		p.BuyLow > p.Exit.StopPrice && p.BuyHigh > p.BuyLow && p.Exit.TargetPrice > p.BuyHigh
}

func buildResearchPricePlan(c candidate, bars []datasource.Bar, pc ResearchPriceContext) *ResearchPricePlan {
	p := &ResearchPricePlan{Version: researchPricePlanVersion, Context: pc, QuoteAsOf: c.QuoteAsOf, ReferencePrice: c.Price,
		Status: "unavailable", Reasons: []string{}, Evidence: []string{}, HorizonDays: 10, EntryValidDays: 5}
	if pc.Horizon == model.RecTypeLongTerm {
		p.HorizonDays, p.EntryValidDays = 60, 10
	}
	if pc.Profile == "active" && pc.Horizon == model.RecTypeShortTerm {
		p.HorizonDays = 5
	}
	if c.Market != "cn" || c.Price <= 0 || !finiteRecNumber(c.Price) {
		p.Reasons = append(p.Reasons, "统一研究价位仅支持有效 A 股行情")
		return p
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", c.QuoteAsOf, time.Local)
	if err != nil {
		p.Reasons = append(p.Reasons, "缺少可核验的报价时点")
		return p
	}
	completed, issue := recommendationCompletedBars(bars, c)
	if issue != "" {
		p.Reasons = append(p.Reasons, issue)
		return p
	}
	// 固定尾窗保证两模块不因各自拉取 120/250 根而产生递推种子差异。
	if len(completed) > 120 {
		completed = completed[len(completed)-120:]
	}
	if len(completed) < 22 {
		p.Reasons = append(p.Reasons, "至少需要22根有效完整日线")
		return p
	}
	p.BarsAsOf = completed[len(completed)-1].TradeDate
	p.InputHash = stablePositionExitHash(struct {
		Context   ResearchPriceContext
		Price     float64
		QuoteAsOf string
		Bars      []datasource.Bar
	}{pc, c.Price, c.QuoteAsOf, completed})
	q := computeRecSignalQuality(c.Price, completed)
	if q.ATR == nil || *q.ATR <= 0 {
		p.Reasons = append(p.Reasons, "波动参照不可用")
		return p
	}
	atr := *q.ATR
	p.EntryATR = atr
	closes := make([]float64, len(completed))
	for i, b := range completed {
		closes[i] = b.Close
	}
	ma20, _ := movingAverage(closes, 20)
	ma10, _ := movingAverage(closes, 10)
	rows := researchDailyBars(c.Symbol, c.Market, completed)
	lookback := 40
	if pc.Horizon == model.RecTypeLongTerm {
		lookback = 90
	}
	support, _, _ := exitStructure(rows, c.Price, lookback)
	anchor := c.Price
	low, high := c.Price-0.35*atr, math.Min(c.Price+0.15*atr, ma20+2*atr)
	key := strings.TrimPrefix(pc.StrategyKey, recStrategyScreenPrefix)
	switch {
	case pc.Intent == "reversal":
		anchor = math.Min(c.Price, ma10+0.5*atr)
		if support > 0 && c.Price-support <= atr {
			anchor = support + 0.25*atr
		}
		low, high = anchor-0.3*atr, anchor+0.4*atr
		p.Evidence = append(p.Evidence, "反转采用已知支撑或短均线附近的区间，必须另有完整日线企稳确认")
	case pc.Profile == "pullback" || pc.Profile == "value" || pc.Intent == "pullback" || pc.Intent == "consolidation":
		anchor = ma20
		if key == "limit-up-pullback" {
			anchor = ma10
		}
		if key == "breakout-retest" {
			if level := commonSetupFactors(completed)["breakout_retest_level"]; level > 0 {
				anchor = level
			}
		}
		low, high = anchor-0.15*atr, anchor+0.75*atr
		p.Evidence = append(p.Evidence, "回踩区间锚定均线或已突破平台，上沿限制为锚点上方0.75 ATR")
	case pc.Intent == "breakout":
		anchor = *q.BreakoutLevel
		confirmed := q.BreakoutConfirmed != nil && *q.BreakoutConfirmed
		switch key {
		case "nr7-breakout":
			setup := commonSetupFactors(completed)
			if level := setup["nr7_level"]; level > 0 {
				anchor = level
			}
			confirmed = setup["nr7_break"] == 1
		case "boll-break-up", "boll-squeeze-break":
			up, _, _ := bollSeries(closes, 20, 2)
			anchor = up[len(up)-2]
			confirmed = closes[len(closes)-1] > anchor
		case "yang-through-3ma":
			ma5, _ := movingAverage(closes, 5)
			anchor = math.Max(ma5, math.Max(ma10, ma20))
			confirmed = closes[len(closes)-1] > anchor
		case "donchian-55":
			if len(completed) >= 56 {
				anchor = completed[len(completed)-56].High
				for _, b := range completed[len(completed)-56 : len(completed)-1] {
					anchor = math.Max(anchor, b.High)
				}
				confirmed = closes[len(closes)-1] > anchor
			} else {
				confirmed = false
			}
		}
		breakATR := atr
		if q.BreakoutATR != nil && *q.BreakoutATR > 0 {
			breakATR = math.Min(atr, *q.BreakoutATR)
		}
		low, high = anchor-0.1*breakATR, math.Min(anchor+breakATR, ma20+2.5*atr)
		if !confirmed {
			p.SetupReasons = append(p.SetupReasons, "所选突破尚无完整收盘确认，等待信号成立")
		}
		p.Evidence = append(p.Evidence, "突破上沿受原突破位置和突破前波动约束，连续上涨不逐日追抬入场区间")
	default:
		if low >= high {
			low = high - 0.5*atr
		}
		p.Evidence = append(p.Evidence, "趋势/活跃区间以当前价格附近为起点，上沿不超过MA20上方2 ATR")
	}
	tick := exitPriceTick(c.Symbol)
	if high <= low {
		p.Reasons = append(p.Reasons, "结构锚点与延伸上限冲突，暂不生成精确买点")
		return p
	}
	p.EntryAnchor, p.BuyLow, p.BuyHigh = round4(anchor), exitCeil(math.Max(tick, low), tick), exitFloor(high, tick)
	quantity := float64(cnMinimumBuyQuantity(c.Symbol))
	seedAt := func(price float64) ExitPlanSeed {
		fee, tax := tradeFee("cn", model.PaperSideBuy, c.Symbol, price*quantity)
		position := model.Position{Symbol: c.Symbol, Market: "cn", Currency: "CNY", PositionType: pc.Horizon,
			BuyPrice: price, BuyDate: at.Format("2006-01-02"), Quantity: quantity, BuyFee: fee, BuyTax: tax, RemainingCost: round4(price*quantity + fee + tax)}
		return buildExitPlanSeed(position, rows, pc.Profile, "research", position.BuyDate, at, nil)
	}
	seed := seedAt(p.BuyHigh)
	// 近端阻力压缩收益时，优先收紧可接受买价；不抬目标价凑盈亏比。
	if seed.DataStatus != "unavailable" && seed.NetRewardRisk < 1.2 && p.BuyLow < p.BuyHigh {
		lowerSeed := seedAt(p.BuyLow)
		if lowerSeed.DataStatus != "unavailable" && lowerSeed.NetRewardRisk >= 1.2 {
			lo, hi := p.BuyLow, p.BuyHigh
			for i := 0; i < 18; i++ {
				mid := (lo + hi) / 2
				if seedAt(mid).NetRewardRisk >= 1.2 {
					lo = mid
				} else {
					hi = mid
				}
			}
			p.BuyHigh = exitFloor(lo, tick)
			seed = seedAt(p.BuyHigh)
			p.Evidence = append(p.Evidence, "近端阻力限制收益空间，已下调买入上沿，使扣费后第一目标至少覆盖1.2倍风险")
		}
	}
	p.Exit = &seed
	if !researchPlanGeometryValid(p) {
		p.Reasons = append(p.Reasons, "结构与最小价位不能形成有效的买入、止损及目标关系")
		p.BuyLow, p.BuyHigh = 0, 0
		return p
	}
	p.Status = "ready"
	if c.Price < p.BuyLow {
		p.Reasons = append(p.Reasons, "当前价格低于计划区间，等待回到区间并重新确认支撑")
	}
	if c.Price > p.BuyHigh {
		p.Reasons = append(p.Reasons, "当前价格高于计划上沿，等待回落，不追抬买价")
	}
	if seed.NetRewardRisk < 1.2 {
		p.Reasons = append(p.Reasons, "按上沿买入，扣费后第一目标不足1.2R，等待更合适买点或新的结构证据")
	}
	if pc.Intent == "reversal" || pc.Profile == "pullback" || pc.Profile == "value" {
		if q.Stabilized == nil || !*q.Stabilized {
			p.SetupReasons = append(p.SetupReasons, "最新完整日线低点与收盘尚未同时企稳")
		}
	}
	if isAtLimitUp(c) {
		p.Reasons = append(p.Reasons, "当前接近涨停，不能假定可成交")
	}
	if c.StrategyHit != nil && !c.StrategyHit.Full {
		p.SetupReasons = append(p.SetupReasons, "当前完整日线未满足所选选股策略的全部条件")
	}
	p.Reasons = append(p.Reasons, p.SetupReasons...)
	if len(p.Reasons) > 0 {
		p.Status = "wait"
	}
	p.Evidence = append(p.Evidence, fmt.Sprintf("共同定价版本%s；退出规划%s；费用按%d股示例估算，实际买入后按成本重算", p.Version, seed.Version, int(quantity)),
		"第一目标是分阶段风险规划价，估值区间是AI研究判断，两者不等同；A股T+1、跳空和涨跌停均可能影响成交")
	return p
}

func researchPlanGeometryValid(p *ResearchPricePlan) bool {
	return p.Exit != nil && p.Exit.DataStatus != "unavailable" && validExitSeed(*p.Exit) &&
		p.BuyLow > 0 && p.BuyLow > p.Exit.StopPrice && p.BuyHigh > p.BuyLow && p.Exit.TargetPrice > p.BuyHigh
}

func cnMinimumBuyQuantity(symbol string) int {
	if strings.HasPrefix(symbol, "68") {
		return 200
	}
	return 100
}
