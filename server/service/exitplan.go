package service

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"quantvista/model"
)

const exitPlanVersion = "xp1"

// ExitPlanSeed 固定建仓依据。后续的止盈阶段和保护线不能重定义初始风险。
type ExitPlanSeed struct {
	Version                 string         `json:"version"`
	Hash                    string         `json:"hash"`
	BasisHash               string         `json:"basis_hash"`
	Source                  string         `json:"source"`
	Profile                 string         `json:"profile"`
	PositionType            string         `json:"position_type"`
	GeneratedAt             string         `json:"generated_at"`
	AnchorDate              string         `json:"anchor_date"`
	BarsAsOf                string         `json:"bars_as_of"`
	DataStatus              string         `json:"data_status"`
	EntryPrice              float64        `json:"entry_price"`
	StopPrice               float64        `json:"stop_price"`
	TargetPrice             float64        `json:"target_price"`
	ExtendedTarget          float64        `json:"extended_target"`
	InitialRisk             float64        `json:"initial_risk"`
	ATR14                   float64        `json:"atr14"`
	Support                 float64        `json:"support"`
	Resistance              float64        `json:"resistance"`
	TrailATR                float64        `json:"trail_atr"`
	BreakevenR              float64        `json:"breakeven_r"`
	LockFraction            float64        `json:"lock_fraction"`
	ReviewDays              int            `json:"review_days"`
	Quantity                float64        `json:"quantity"`
	Cost                    float64        `json:"cost"`
	EstimatedRisk           float64        `json:"estimated_risk"`
	EstimatedReward         float64        `json:"estimated_reward"`
	EstimatedExtendedReward float64        `json:"estimated_extended_reward"`
	NetRewardRisk           float64        `json:"net_reward_risk"`
	SlippageBPS             float64        `json:"slippage_bps"`
	Evidence                []string       `json:"evidence"`
	DataGaps                []string       `json:"data_gaps"`
	Carry                   *ExitPlanCarry `json:"carry,omitempty"`
}

// 公司行动只改变价格刻度，承接已经建立的保护与已触达阶段。
type ExitPlanCarry struct {
	Stop          float64 `json:"stop"`
	Peak          float64 `json:"peak"`
	ATR           float64 `json:"atr"`
	StopSince     string  `json:"stop_since"`
	FirstAt       string  `json:"first_at"`
	SecondAt      string  `json:"second_at"`
	FirstHandled  bool    `json:"first_handled"`
	SecondHandled bool    `json:"second_handled"`
	StopActive    bool    `json:"stop_active"`
	StopEpisode   int     `json:"stop_episode"`
	StopAt        string  `json:"stop_at"`
	Quantity      float64 `json:"quantity"`
}

type ExitPlan struct {
	Initial             ExitPlanSeed `json:"initial"`
	CurrentStop         float64      `json:"current_stop"`
	StopEffectiveAt     string       `json:"stop_effective_at"`
	BreakevenPrice      float64      `json:"breakeven_price"`
	PeakPrice           float64      `json:"peak_price"`
	CurrentATR          float64      `json:"current_atr"`
	Stage               string       `json:"stage"`
	FirstTargetAt       string       `json:"first_target_at,omitempty"`
	SecondTargetAt      string       `json:"second_target_at,omitempty"`
	FirstTargetHandled  bool         `json:"first_target_handled"`
	SecondTargetHandled bool         `json:"second_target_handled"`
	TimeReviewAt        string       `json:"time_review_at,omitempty"`
	StopActive          bool         `json:"stop_active"`
	StopEpisode         int          `json:"stop_episode"`
	StopTriggeredAt     string       `json:"stop_triggered_at,omitempty"`
	ObservedQuantity    float64      `json:"observed_quantity"`
	SuggestedQuantity   float64      `json:"suggested_quantity"`
	SellableQuantity    *float64     `json:"sellable_quantity,omitempty"`
	EstimatedStopNet    float64      `json:"estimated_stop_net"`
	ExecutionNotes      []string     `json:"execution_notes"`
	DataStatus          string       `json:"data_status"`
	ProtectionSuspended bool         `json:"protection_suspended,omitempty"`
	DataGaps            []string     `json:"data_gaps"`
	Evidence            []string     `json:"evidence"`
}

func exitPriceTick(symbol string) float64 {
	if isCNFund(symbol) {
		return 0.001
	}
	return 0.01
}

func exitFinite(v float64) bool         { return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) < 1e14 }
func exitFloor(v, tick float64) float64 { return round4(math.Floor(v/tick+1e-8) * tick) }
func exitCeil(v, tick float64) float64  { return round4(math.Ceil(v/tick-1e-8) * tick) }

// 分批卖出不改变初始风险依据；成本修正、加仓、周期、计划或价格刻度变化会使其失效。
func exitPlanBasis(p model.Position) string {
	buyQty, buyCost := p.TotalBuyQty, p.TotalBuyCost
	if buyQty <= 0 {
		buyQty = p.Quantity
	}
	if buyCost <= 0 {
		buyCost = round4(p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax)
	}
	return stablePositionExitHash(struct {
		Symbol, Market, Currency, Type, BuyDate, PeakFrom string
		Price, BuyCost, BuyQty, Stop, Take                float64
	}{p.Symbol, p.Market, p.Currency, p.PositionType, p.BuyDate, p.PeakFrom,
		p.BuyPrice, buyCost, buyQty, p.PlanStopLoss, p.PlanTakeProfit})
}

func sealExitSeed(seed *ExitPlanSeed) {
	seed.Hash = ""
	seed.Hash = stablePositionExitHash(*seed)
}

func decodeExitSeed(raw string) *ExitPlanSeed {
	var seed ExitPlanSeed
	if json.Unmarshal([]byte(raw), &seed) != nil || seed.Version != exitPlanVersion || seed.Hash == "" {
		return nil
	}
	hash := seed.Hash
	sealExitSeed(&seed)
	if seed.Hash != hash || !validExitSeed(seed) {
		return nil
	}
	return &seed
}

func validExitSeed(s ExitPlanSeed) bool {
	for _, v := range []float64{s.EntryPrice, s.StopPrice, s.TargetPrice, s.ExtendedTarget, s.ATR14, s.InitialRisk, s.Cost, s.Quantity, s.TrailATR, s.BreakevenR, s.LockFraction} {
		if !exitFinite(v) || v < 0 {
			return false
		}
	}
	if s.Carry != nil {
		for _, v := range []float64{s.Carry.Stop, s.Carry.Peak, s.Carry.ATR, s.Carry.Quantity} {
			if !exitFinite(v) || v < 0 {
				return false
			}
		}
	}
	if s.DataStatus == "unavailable" {
		return true
	}
	return s.EntryPrice > 0 && s.StopPrice > 0 && s.StopPrice < s.EntryPrice && s.TargetPrice > s.EntryPrice &&
		s.ExtendedTarget > s.TargetPrice && s.InitialRisk > 0 && s.TrailATR > 0 && s.LockFraction > 0 && s.LockFraction < 1
}

func decodeExitPlan(raw, hash string) *ExitPlan {
	var plan ExitPlan
	if raw == "" || json.Unmarshal([]byte(raw), &plan) != nil || !validExitSeed(plan.Initial) || plan.Initial.Version != exitPlanVersion {
		return nil
	}
	if decodeExitSeed(mustPositionExitJSON(plan.Initial)) == nil {
		return nil
	}
	if hash != "" && stablePositionExitHash(plan) != hash {
		return nil
	}
	for _, v := range []float64{plan.CurrentStop, plan.PeakPrice, plan.BreakevenPrice, plan.CurrentATR, plan.ObservedQuantity} {
		if !exitFinite(v) || v < 0 {
			return nil
		}
	}
	return &plan
}

func exitNetProceeds(p model.Position, price, quantity, slippageBPS float64) float64 {
	if price <= 0 || quantity <= 0 {
		return 0
	}
	amount := price * quantity * (1 - slippageBPS/10000)
	fee, tax := tradeFee(p.Market, model.PaperSideSell, p.Symbol, amount)
	return amount - fee - tax
}

func exitRemainingCost(p model.Position) float64 {
	if p.RemainingCost > 0 {
		return p.RemainingCost
	}
	// 旧余额尚未补齐时沿用当前均价和未结转费用；累计买入均价含已卖出的仓位，
	// 在先减仓、再以不同价格加仓后不能代表剩余成本。
	return p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax
}

func exitBreakeven(p model.Position, slippage float64) float64 {
	if p.Quantity <= 0 {
		return 0
	}
	cost := exitRemainingCost(p)
	lo, hi := cost/p.Quantity, cost/p.Quantity*1.1+20/p.Quantity
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if exitNetProceeds(p, mid, p.Quantity, slippage) >= cost {
			hi = mid
		} else {
			lo = mid
		}
	}
	return exitCeil(hi, exitPriceTick(p.Symbol))
}

// 只使用已确认的两侧转折，不借用未来日线确认最后两根的支撑/阻力。
func exitStructure(bars []model.DailyBar, entry float64, lookback int) (support, resistance, upper float64) {
	start := len(bars) - lookback
	if start < 2 {
		start = 2
	}
	var levels []float64
	for i := start; i+2 < len(bars); i++ {
		b := bars[i]
		low := b.Low < bars[i-1].Low && b.Low <= bars[i-2].Low && b.Low < bars[i+1].Low && b.Low <= bars[i+2].Low
		high := b.High > bars[i-1].High && b.High >= bars[i-2].High && b.High > bars[i+1].High && b.High >= bars[i+2].High
		if low && b.Low < entry && b.Low > support {
			support = b.Low
		}
		if high && b.High > entry {
			levels = append(levels, b.High)
		}
	}
	sort.Float64s(levels)
	if len(levels) > 0 {
		resistance = levels[0]
	}
	for _, v := range levels {
		if v > resistance*1.005 {
			upper = v
			break
		}
	}
	return
}

func buildExitPlanSeed(p model.Position, bars []model.DailyBar, profile, source, anchorDate string, now time.Time, gaps []string) ExitPlanSeed {
	s := ExitPlanSeed{Version: exitPlanVersion, BasisHash: exitPlanBasis(p), Source: source, Profile: profile,
		PositionType: p.PositionType, GeneratedAt: now.In(time.Local).Format(time.RFC3339Nano), AnchorDate: anchorDate,
		EntryPrice: p.BuyPrice, Quantity: p.Quantity, Cost: round4(exitRemainingCost(p)), DataStatus: "unavailable",
		SlippageBPS: 10, DataGaps: append([]string{}, gaps...), Evidence: []string{}}
	if p.Market != "cn" || p.Currency != "" && p.Currency != "CNY" {
		s.DataGaps = append(s.DataGaps, "当前退出算法只支持人民币计价的 A 股，不套用到港美持仓")
		sealExitSeed(&s)
		return s
	}
	if !exitFinite(p.BuyPrice) || p.BuyPrice <= 0 || !exitFinite(p.Quantity) || p.Quantity <= 0 || !exitFinite(s.Cost) || s.Cost <= 0 {
		s.EntryPrice, s.Quantity, s.Cost = 0, 0, 0
		s.DataGaps = append(s.DataGaps, "买入价格、数量或剩余成本无效")
		sealExitSeed(&s)
		return s
	}
	if p.TotalBuyCost > 0 && p.RemainingCost <= 0 {
		s.DataGaps = append(s.DataGaps, "旧账本尚未补齐精确剩余成本，暂按当前均价和未结转费用估算")
	}
	stopATR := 2.3
	s.TrailATR, s.BreakevenR, s.LockFraction, s.ReviewDays = 2.8, 1.0, 0.5, 10
	switch profile {
	case "momentum":
		stopATR, s.TrailATR = 2.5, 3.0
	case "pullback":
		stopATR, s.TrailATR = 2.0, 2.5
	case "active":
		stopATR, s.TrailATR, s.ReviewDays = 2.2, 2.5, 5
	case "value", "leader":
		stopATR, s.TrailATR = 2.8, 3.2
	case "growth":
		stopATR, s.TrailATR = 2.6, 3.0
	default:
		s.Profile = "balanced"
	}
	lookback := 40
	if p.PositionType == model.PositionTypeLongTerm {
		stopATR += 0.7
		s.TrailATR += 0.7
		s.BreakevenR, s.LockFraction, s.ReviewDays = 1.25, 0.35, 60
		lookback = 90
	}
	session := model.PositionExitSessionIntraday
	if now.In(time.Local).Hour() >= 15 {
		session = model.PositionExitSessionClose
	}
	completed, barGaps := completedPositionExitBars(bars, now.In(time.Local).Format("2006-01-02"), session)
	if err := validateLocalAdjustedBars(p.Market, completed); err != nil {
		completed = nil
		barGaps = append(barGaps, err.Error())
	}
	bars = completed
	s.DataGaps = append(s.DataGaps, barGaps...)
	if len(bars) > 0 {
		s.BarsAsOf = bars[len(bars)-1].TradeDate
	}
	atr, ok := positionExitATR14(bars, 14)
	if len(bars) < 21 || !ok || !exitFinite(atr) || atr <= 0 {
		s.DataGaps = append(s.DataGaps, "至少需要 21 根有效完整日线及非零 ATR；不编造默认百分比价位")
		// 明确的人工价位仍能独立保护；不能把未知 ATR 伪造成可动态调整。
		if p.PlanStopLoss > 0 && p.PlanStopLoss < p.BuyPrice && p.PlanTakeProfit > p.BuyPrice {
			s.StopPrice, s.TargetPrice = p.PlanStopLoss, p.PlanTakeProfit
			s.InitialRisk = p.BuyPrice - s.StopPrice
			s.ExtendedTarget = exitCeil(math.Max(s.TargetPrice+s.InitialRisk, s.EntryPrice+3*s.InitialRisk), exitPriceTick(p.Symbol))
			s.DataStatus = "partial"
			s.EstimatedRisk = round4(math.Max(0, s.Cost-exitNetProceeds(p, s.StopPrice, p.Quantity, s.SlippageBPS)))
			s.EstimatedReward = round4(exitNetProceeds(p, s.TargetPrice, p.Quantity, s.SlippageBPS) - s.Cost)
			s.EstimatedExtendedReward = round4(exitNetProceeds(p, s.ExtendedTarget, p.Quantity, s.SlippageBPS) - s.Cost)
			if s.EstimatedRisk > 0 {
				s.NetRewardRisk = round4(s.EstimatedReward / s.EstimatedRisk)
			}
		}
		sealExitSeed(&s)
		return s
	}
	s.ATR14 = round4(atr)
	var upperResistance float64
	s.Support, s.Resistance, upperResistance = exitStructure(bars, p.BuyPrice, lookback)
	tick := exitPriceTick(p.Symbol)
	stop := p.BuyPrice - stopATR*atr
	if s.Support > 0 {
		structural := s.Support - 0.35*atr
		distance := p.BuyPrice - structural
		if distance >= 1.2*atr && distance <= 4*atr {
			stop = structural
			s.Evidence = append(s.Evidence, "初始止损位于已确认支撑下方，预留 0.35 ATR 缓冲")
		} else {
			s.Evidence = append(s.Evidence, "支撑过近或过远，采用策略对应的 ATR 风险距离")
		}
	} else {
		s.Evidence = append(s.Evidence, "窗口内没有已确认支撑，使用 ATR 备选止损")
	}
	if p.PlanStopLoss > 0 && p.PlanStopLoss < p.BuyPrice {
		stop = p.PlanStopLoss
		s.Evidence = append(s.Evidence, "初始止损采用已填写的计划价，后续保护单独上移")
	}
	s.StopPrice = exitFloor(stop, tick)
	if s.StopPrice <= 0 || s.StopPrice >= s.EntryPrice {
		s.DataGaps = append(s.DataGaps, "波动风险距离无法形成有效正数止损价")
		sealExitSeed(&s)
		return s
	}
	s.InitialRisk = round4(s.EntryPrice - s.StopPrice)
	target := s.EntryPrice + 1.5*s.InitialRisk
	if s.Resistance > s.EntryPrice+tick {
		target = math.Min(target, s.Resistance-0.15*atr)
	}
	if p.PlanTakeProfit > s.EntryPrice {
		target = p.PlanTakeProfit
		s.Evidence = append(s.Evidence, "第一目标采用已填写的计划止盈价")
	}
	s.TargetPrice = exitCeil(math.Max(s.EntryPrice+tick, target), tick)
	s.ExtendedTarget = exitCeil(math.Max(s.EntryPrice+3*s.InitialRisk, s.TargetPrice+s.InitialRisk), tick)
	if upperResistance > s.TargetPrice+0.5*s.InitialRisk {
		s.ExtendedTarget = exitCeil(math.Min(s.ExtendedTarget, upperResistance-0.15*atr), tick)
	}
	s.EstimatedRisk = round4(math.Max(0, s.Cost-exitNetProceeds(p, s.StopPrice, p.Quantity, s.SlippageBPS)))
	s.EstimatedReward = round4(exitNetProceeds(p, s.TargetPrice, p.Quantity, s.SlippageBPS) - s.Cost)
	s.EstimatedExtendedReward = round4(exitNetProceeds(p, s.ExtendedTarget, p.Quantity, s.SlippageBPS) - s.Cost)
	if s.EstimatedRisk > 0 {
		s.NetRewardRisk = round4(s.EstimatedReward / s.EstimatedRisk)
	}
	s.Evidence = append(s.Evidence, fmt.Sprintf("初始风险每股 %.4f；第一目标用于分批兑现，延伸目标参考 3R 与远端阻力", s.InitialRisk),
		"费用沿用统一佣金/印花税模型，另按 10 基点卖出滑点估算；触发价不是保证成交价")
	if s.NetRewardRisk < 1.2 {
		s.Evidence = append(s.Evidence, "扣费后第一目标风险收益比不足 1.2，应重新核对买入价格与近端阻力")
	}
	if s.InitialRisk/s.EntryPrice > 0.12 {
		s.Evidence = append(s.Evidence, "初始风险距离超过成本的 12%，应降低风险暴露并检查买入依据")
	}
	s.DataStatus = "ready"
	if len(s.DataGaps) > 0 {
		s.DataStatus = "partial"
	}
	sealExitSeed(&s)
	return s
}

type exitPlanObservation struct {
	Position                    model.Position
	Seed                        ExitPlanSeed
	Previous                    *ExitPlan
	Price, High, Low, Peak, ATR float64
	TradeDate                   string
	Now                         time.Time
	PriceOK, TechnicalOK        bool
	Sellable                    *float64
	ExecutionNotes              []string
	Gaps                        []string
	HeldDays                    *int
}

func evolveExitPlan(o exitPlanObservation) ExitPlan {
	p, s := o.Position, o.Seed
	plan := ExitPlan{Initial: s, CurrentStop: s.StopPrice, StopEffectiveAt: s.GeneratedAt, PeakPrice: s.EntryPrice,
		ObservedQuantity: p.Quantity, Stage: "initial", DataStatus: s.DataStatus, DataGaps: append([]string{}, s.DataGaps...)}
	if c := s.Carry; c != nil {
		plan.CurrentStop, plan.PeakPrice, plan.CurrentATR, plan.StopEffectiveAt = c.Stop, c.Peak, c.ATR, c.StopSince
		plan.FirstTargetAt, plan.SecondTargetAt, plan.FirstTargetHandled, plan.SecondTargetHandled = c.FirstAt, c.SecondAt, c.FirstHandled, c.SecondHandled
		plan.StopActive, plan.StopEpisode, plan.StopTriggeredAt, plan.ObservedQuantity = c.StopActive, c.StopEpisode, c.StopAt, c.Quantity
	}
	if o.Previous != nil && o.Previous.Initial.Hash == s.Hash {
		plan = *o.Previous
		plan.DataGaps = append([]string{}, s.DataGaps...)
	}
	plan.Evidence = append([]string{}, s.Evidence...)
	plan.ExecutionNotes = append([]string{}, o.ExecutionNotes...)
	plan.SellableQuantity = o.Sellable
	plan.SuggestedQuantity = 0
	plan.DataGaps = append(plan.DataGaps, o.Gaps...)
	if plan.ObservedQuantity > p.Quantity+positionQtyEps {
		if plan.FirstTargetAt != "" {
			plan.FirstTargetHandled = true
		}
		if plan.SecondTargetAt != "" {
			plan.SecondTargetHandled = true
		}
	}
	plan.ObservedQuantity = p.Quantity
	plan.BreakevenPrice = exitBreakeven(p, s.SlippageBPS)
	plan.Stage = "initial"
	if plan.BreakevenPrice > 0 && plan.CurrentStop >= plan.BreakevenPrice {
		plan.Stage = "breakeven"
	}
	if plan.BreakevenPrice > 0 && plan.CurrentStop > plan.BreakevenPrice+exitPriceTick(p.Symbol) {
		plan.Stage = "profit_lock"
	}
	if !o.PriceOK || s.DataStatus == "unavailable" {
		if plan.FirstTargetAt != "" {
			plan.Stage = "first_target"
		}
		if plan.SecondTargetAt != "" {
			plan.Stage = "extended_target"
		}
		if plan.StopActive {
			plan.Stage = "stop_triggered"
		}
		plan.DataStatus = "partial"
		if s.DataStatus == "unavailable" {
			plan.DataStatus = "unavailable"
		}
		return plan
	}
	nowText := o.Now.In(time.Local).Format(time.RFC3339Nano)
	if !exitFinite(o.High) {
		o.High = o.Price
		plan.DataGaps = append(plan.DataGaps, "当日最高价无效，只使用当前有效价")
	}
	if !exitFinite(o.Low) {
		o.Low = o.Price
		plan.DataGaps = append(plan.DataGaps, "当日最低价无效，只使用当前有效价")
	}
	if !exitFinite(o.Peak) {
		o.Peak = 0
		plan.DataGaps = append(plan.DataGaps, "持仓峰值无效，不参与保护价更新")
	}
	tick := exitPriceTick(p.Symbol)
	oldStop, oldSince := plan.CurrentStop, plan.StopEffectiveAt
	if o.TechnicalOK && o.ATR > 0 {
		plan.CurrentATR = round4(o.ATR)
	}
	// 峰值属于持仓期；当日建仓只能使用买入后的当前观测价，不能借用整日最高。
	plan.PeakPrice = math.Max(plan.PeakPrice, math.Max(o.Price, o.Peak))
	if p.PeakFrom != "" && p.PeakFrom < o.TradeDate {
		plan.PeakPrice = math.Max(plan.PeakPrice, o.High)
	}
	peakR := (plan.PeakPrice - s.EntryPrice) / s.InitialRisk
	if o.TechnicalOK && s.InitialRisk > 0 {
		nextStop := oldStop
		if peakR >= s.BreakevenR && plan.PeakPrice-plan.BreakevenPrice >= math.Max(tick, 0.25*plan.CurrentATR) {
			nextStop = math.Max(nextStop, plan.BreakevenPrice)
			plan.Stage = "breakeven"
		}
		if peakR >= 2 {
			nextStop = math.Max(nextStop, s.EntryPrice+(plan.PeakPrice-s.EntryPrice)*s.LockFraction)
			plan.Stage = "profit_lock"
		}
		if peakR >= 0.75 && plan.CurrentATR > 0 {
			nextStop = math.Max(nextStop, plan.PeakPrice-s.TrailATR*plan.CurrentATR)
		}
		nextStop = exitFloor(nextStop, tick)
		if nextStop > oldStop+tick/2 {
			plan.CurrentStop = nextStop
			plan.StopEffectiveAt = nowText
		}
		if plan.CurrentStop < plan.BreakevenPrice && plan.CurrentStop > s.StopPrice {
			plan.Stage = "tightened"
		}
	} else {
		plan.DataGaps = append(plan.DataGaps, "技术数据暂不可用，保留已经建立的保护价，不放宽也不猜测新价")
	}
	low := o.Price
	// 旧保护价在今日开盘前已生效时，才可以用全日最低检查它；新价只能检查当前价。
	oldActiveBeforeToday := len(oldSince) >= 10 && oldSince[:10] < o.TradeDate && p.PeakFrom < o.TradeDate
	observedStopToday := len(plan.StopTriggeredAt) >= 10 && plan.StopTriggeredAt[:10] == o.TradeDate
	if oldActiveBeforeToday && !observedStopToday && o.Low > 0 {
		low = math.Min(low, o.Low)
	}
	stopHit := oldStop > 0 && low <= oldStop || plan.CurrentStop > 0 && o.Price <= plan.CurrentStop
	if stopHit && !plan.StopActive {
		plan.StopActive = true
		plan.StopEpisode++
		plan.StopTriggeredAt = nowText
	}
	if !stopHit && plan.StopActive && o.Price > plan.CurrentStop+math.Max(2*tick, 0.35*plan.CurrentATR) {
		plan.StopActive = false
	}
	high := o.Price
	if len(s.GeneratedAt) >= 10 && s.GeneratedAt[:10] < o.TradeDate && p.PeakFrom < o.TradeDate {
		high = math.Max(high, o.High)
	}
	if s.TargetPrice > 0 && high >= s.TargetPrice && plan.FirstTargetAt == "" {
		plan.FirstTargetAt = nowText
	}
	if s.ExtendedTarget > 0 && high >= s.ExtendedTarget && plan.SecondTargetAt == "" {
		plan.SecondTargetAt = nowText
	}
	if o.HeldDays != nil && *o.HeldDays >= s.ReviewDays && s.ReviewDays > 0 && plan.FirstTargetAt == "" && plan.TimeReviewAt == "" {
		plan.TimeReviewAt = nowText
	}
	if plan.TimeReviewAt != "" && plan.FirstTargetAt == "" {
		plan.Stage = "time_review"
	}
	if plan.FirstTargetAt != "" {
		plan.Stage = "first_target"
	}
	if plan.SecondTargetAt != "" {
		plan.Stage = "extended_target"
	}
	if plan.StopActive {
		plan.Stage = "stop_triggered"
		plan.SuggestedQuantity = p.Quantity
	} else if plan.SecondTargetAt != "" && !plan.SecondTargetHandled {
		plan.SuggestedQuantity = p.Quantity
	} else if plan.FirstTargetAt != "" && !plan.FirstTargetHandled {
		plan.SuggestedQuantity = math.Floor(p.Quantity/200) * 100
		if plan.SuggestedQuantity <= 0 {
			plan.SuggestedQuantity = p.Quantity
		}
	}
	if o.Sellable != nil && plan.SuggestedQuantity > *o.Sellable {
		plan.SuggestedQuantity = *o.Sellable
	}
	plan.EstimatedStopNet = round4(exitNetProceeds(p, plan.CurrentStop, p.Quantity, s.SlippageBPS) - exitRemainingCost(p))
	plan.Evidence = append(plan.Evidence, fmt.Sprintf("当前保护 %.4f，净保本 %.4f；同一规划的保护价只上移", plan.CurrentStop, plan.BreakevenPrice))
	plan.DataStatus = "ready"
	if len(plan.DataGaps) > 0 {
		plan.DataStatus = "partial"
	}
	return plan
}

func exitPlanSignals(plan ExitPlan) []PositionExitSignal {
	if plan.Initial.DataStatus == "unavailable" {
		return nil
	}
	var signals []PositionExitSignal
	if plan.StopActive {
		key, label := "adaptive_stop", "触达持仓保护价"
		if plan.CurrentStop >= plan.BreakevenPrice && plan.BreakevenPrice > 0 {
			key, label = "profit_protection", "触达盈利保护价"
		}
		signals = append(signals, PositionExitSignal{Key: key, Label: label, Detail: fmt.Sprintf("当前保护价 %.4f；先核对可卖数量与成交条件", plan.CurrentStop), Severity: model.PositionExitLevelUrgent, Threshold: plan.CurrentStop, Crossing: true})
	}
	if plan.SecondTargetAt != "" && !plan.SecondTargetHandled {
		label, detail := "触达延伸止盈目标", fmt.Sprintf("目标 %.4f，可复核剩余仓位兑现或继续跟踪保护价", plan.Initial.ExtendedTarget)
		if plan.Initial.EstimatedExtendedReward <= 0 {
			label = "触达延伸价格复核点"
			detail = fmt.Sprintf("价格 %.4f，原规划扣费后仍未盈利，应核对费用和持有逻辑", plan.Initial.ExtendedTarget)
		}
		signals = append(signals, PositionExitSignal{Key: "target_extended", Label: label, Detail: detail, Severity: model.PositionExitLevelReview, Threshold: plan.Initial.ExtendedTarget, Crossing: true})
	} else if plan.FirstTargetAt != "" && !plan.FirstTargetHandled {
		label, detail := "触达第一止盈目标", fmt.Sprintf("目标 %.4f，可按规划分批兑现；触价尚未记作成交", plan.Initial.TargetPrice)
		if plan.Initial.EstimatedReward <= 0 {
			label = "触达第一价格复核点"
			detail = fmt.Sprintf("价格 %.4f，原规划扣费后仍未盈利，应核对费用和持有逻辑，不能视为获利兑现", plan.Initial.TargetPrice)
		}
		signals = append(signals, PositionExitSignal{Key: "target_first", Label: label, Detail: detail, Severity: model.PositionExitLevelReview, Threshold: plan.Initial.TargetPrice, Crossing: true})
	}
	if plan.TimeReviewAt != "" && plan.FirstTargetAt == "" {
		signals = append(signals, PositionExitSignal{Key: "time_review", Label: "持有期到达规划复核点", Detail: fmt.Sprintf("已达到 %d 个交易日复核窗口，第一目标尚未触达，重新核对持有逻辑和资金占用", plan.Initial.ReviewDays), Severity: model.PositionExitLevelReview})
	}
	return signals
}

func exitPlanActionIdentity(plan ExitPlan, primary string) (string, string) {
	stamp := ""
	switch primary {
	case "adaptive_stop", "profit_protection":
		stamp = plan.StopTriggeredAt
	case "target_first":
		stamp = plan.FirstTargetAt
	case "target_extended":
		stamp = plan.SecondTargetAt
	case "time_review":
		stamp = plan.TimeReviewAt
	}
	if stamp == "" {
		return "", ""
	}
	date := stamp
	if len(date) > 10 {
		date = date[:10]
	}
	return stablePositionExitHash([]string{plan.Initial.Hash, primary, stamp}), date
}
