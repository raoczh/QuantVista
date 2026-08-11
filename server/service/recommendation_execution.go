package service

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

const (
	recommendationPreferenceSnapshotVersion = "pref1"
	executionPlanVersion                    = "ep2"
	riskBudgetVersion                       = "rb1"

	executionReady       = "ready"
	executionWait        = "wait"
	executionNotSuitable = "not_suitable"
)

// riskBudgetParams 是风险偏好唯一允许影响推荐执行适配的参数：缩放现有程序化仓位预算。
// 它不设置收益门槛，不改写 AI 动作/置信度，也不参与候选排序。
type riskBudgetParams struct {
	Version                string  `json:"version"`
	ConservativeMultiplier float64 `json:"conservative_multiplier"`
	BalancedMultiplier     float64 `json:"balanced_multiplier"`
	AggressiveMultiplier   float64 `json:"aggressive_multiplier"`
}

func defaultRiskBudgetParams() riskBudgetParams {
	return riskBudgetParams{
		Version:                riskBudgetVersion,
		ConservativeMultiplier: 0.75,
		BalancedMultiplier:     1,
		AggressiveMultiplier:   1.15,
	}
}

func (p riskBudgetParams) multiplier(risk string) (float64, bool) {
	switch risk {
	case "conservative":
		return p.ConservativeMultiplier, true
	case "balanced":
		return p.BalancedMultiplier, true
	case "aggressive":
		return p.AggressiveMultiplier, true
	default:
		return 1, false
	}
}

// recommendationPreferenceSnapshot 是生成时点不可变偏好事实。PreferenceFound 单独保存，
// 防止用数据库默认值掩盖偏好行缺失；风险映射参数随批次固化，后续参数调整不改历史。
type recommendationPreferenceSnapshot struct {
	Version                string           `json:"version"`
	CapturedAt             time.Time        `json:"captured_at"`
	PreferenceFound        bool             `json:"preference_found"`
	PreferenceUpdatedAt    *time.Time       `json:"preference_updated_at,omitempty"`
	HorizonPref            string           `json:"horizon_pref"`
	RiskLevel              string           `json:"risk_level"`
	TotalCapital           float64          `json:"total_capital"`
	DefaultRecCount        int              `json:"default_rec_count"`
	InvestmentGuideVersion int              `json:"investment_guide_version"`
	InvestmentGuideStatus  string           `json:"investment_guide_status"`
	RiskBudget             riskBudgetParams `json:"risk_budget"`
}

func captureRecommendationPreferenceSnapshot(userID int64) (recommendationPreferenceSnapshot, error) {
	snap := recommendationPreferenceSnapshot{
		Version:    recommendationPreferenceSnapshotVersion,
		CapturedAt: time.Now(),
		RiskBudget: defaultRiskBudgetParams(),
	}
	var pref model.UserPreference
	err := common.DB.Where("user_id = ?", userID).First(&pref).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return snap, nil
	}
	if err != nil {
		return snap, err
	}
	snap.PreferenceFound = true
	snap.PreferenceUpdatedAt = &pref.UpdatedAt
	snap.HorizonPref = pref.HorizonPref
	snap.RiskLevel = pref.RiskLevel
	snap.TotalCapital = pref.TotalCapital
	snap.DefaultRecCount = pref.DefaultRecCount
	snap.InvestmentGuideVersion = pref.InvestmentGuideVersion
	snap.InvestmentGuideStatus = pref.InvestmentGuideStatus
	return snap, nil
}

func marshalPreferenceSnapshot(snap recommendationPreferenceSnapshot) string {
	b, err := json.Marshal(snap)
	if err != nil {
		return ""
	}
	return string(b)
}

func preferenceSnapshotCompleted(snap recommendationPreferenceSnapshot) bool {
	return snap.PreferenceFound &&
		snap.InvestmentGuideVersion == InvestmentGuideCurrentVersion &&
		snap.InvestmentGuideStatus == InvestmentGuideCompleted
}

// applyRiskPreferenceSizing 在现有目标波动仓位结果上应用偏好预算乘数，并继续服从
// regime 总仓位预算。只改程序生成的 PositionPct/Why，不接触模型字段。
func applyRiskPreferenceSizing(picks []recPick, snap recommendationPreferenceSnapshot, regime string) {
	multiplier, ok := snap.RiskBudget.multiplier(snap.RiskLevel)
	if !preferenceSnapshotCompleted(snap) || !ok || multiplier <= 0 {
		return
	}
	var sum float64
	for i := range picks {
		if picks[i].Action != model.RecActionBuy || picks[i].PositionPct <= 0 {
			continue
		}
		picks[i].PositionPct *= multiplier
		picks[i].PositionWhy += "；" + riskLevelLabel(snap.RiskLevel) + "风险偏好预算系数×" + formatMultiplier(multiplier)
		sum += picks[i].PositionPct
	}
	if budget := regimeBudgetPct(regime); sum > budget {
		scale := budget / sum
		for i := range picks {
			if picks[i].Action == model.RecActionBuy && picks[i].PositionPct > 0 {
				picks[i].PositionPct *= scale
				picks[i].PositionWhy += "；风险偏好调整后仍按市场总预算收敛"
			}
		}
	}
	for i := range picks {
		picks[i].PositionPct = round2(picks[i].PositionPct)
	}
}

func formatMultiplier(v float64) string {
	// 整数倍数也要如实显示（2.0 → "2"）；曾硬编码返回 "1.00"，默认参数下恰好只有
	// v=1 命中才没暴露，一旦某档位调成整数就会把错误标签写进历史批次解释。
	return strings.TrimRight(strings.TrimRight(formatFloat(v, 2), "0"), ".")
}

func formatFloat(v float64, precision int) string {
	return strconv.FormatFloat(v, 'f', precision, 64)
}

// executionPlan 是面向用户研究执行的程序化适配结果，不是预测，也不代表券商现金。
type executionPlan struct {
	Status                string   `json:"status"`
	PreferenceExplanation []string `json:"preference_explanation"`
	PlannedCapital        float64  `json:"planned_capital"`
	PlannedPrice          float64  `json:"planned_price"`
	Quantity              int      `json:"quantity"`
	EstimatedCapital      float64  `json:"estimated_capital"`
	MaxPlannedLoss        *float64 `json:"max_planned_loss,omitempty"`
	UnavailableReasons    []string `json:"unavailable_reasons"`
	DataAsOf              string   `json:"data_as_of"`
	DataStatus            string   `json:"data_status"`
	BudgetBasis           string   `json:"budget_basis"`
	Version               string   `json:"version"`
}

func buildExecutionPlan(recType string, p recPick, c candidate, snap recommendationPreferenceSnapshot,
	hasHolding, holdingKnown bool, quoteStatus string) *executionPlan {
	plan := &executionPlan{
		Status:                executionReady,
		PreferenceExplanation: []string{},
		DataAsOf:              c.QuoteAsOf,
		DataStatus:            quoteStatus,
		BudgetBasis:           "research_budget",
		Version:               executionPlanVersion,
		UnavailableReasons:    []string{},
	}
	if plan.DataStatus == "" {
		plan.DataStatus = "unknown"
	}
	if preferenceSnapshotCompleted(snap) {
		if label := horizonLabel(snap.HorizonPref); label != "" {
			if RecommendationTypeForHorizon(snap.HorizonPref) == recType {
				plan.PreferenceExplanation = append(plan.PreferenceExplanation, "本批按你的"+label+"持有偏好展示")
			} else {
				plan.PreferenceExplanation = append(plan.PreferenceExplanation,
					"本批临时选择"+recommendationTypeLabel(recType)+"，与默认"+label+"持有偏好不同")
			}
		}
		if multiplier, ok := snap.RiskBudget.multiplier(snap.RiskLevel); ok {
			plan.PreferenceExplanation = append(plan.PreferenceExplanation,
				"你的风险偏好为"+riskLevelLabel(snap.RiskLevel)+"，现有程序仓位预算系数为×"+formatMultiplier(multiplier))
		}
	}
	if snap.TotalCapital > 0 {
		plan.PreferenceExplanation = append(plan.PreferenceExplanation,
			"以你设置的总投资资金作为研究预算估算，不代表券商账户可用现金")
	}

	notSuitable := make([]string, 0, 4)
	wait := make([]string, 0, 3)
	if !snap.PreferenceFound || !validHorizon[snap.HorizonPref] || !validRisk[snap.RiskLevel] {
		notSuitable = append(notSuitable, "投资偏好缺失，无法做用户适配")
	} else if !preferenceSnapshotCompleted(snap) {
		if snap.InvestmentGuideStatus == InvestmentGuideSkipped {
			notSuitable = append(notSuitable, "投资偏好向导已跳过，完成三问后才能生成可执行计划")
		} else {
			notSuitable = append(notSuitable, "投资偏好向导尚未完成，不能根据默认值推断用户偏好")
		}
	}
	if snap.TotalCapital <= 0 {
		notSuitable = append(notSuitable, "总投资资金未设置，无法估算整手计划")
	}
	if !holdingKnown {
		wait = append(wait, "当前持仓核验失败，暂不生成可执行状态")
	} else if hasHolding {
		notSuitable = append(notSuitable, "该股票已有持仓，不重复生成新建仓计划")
	}
	if p.Action != model.RecActionBuy {
		wait = append(wait, "原推荐动作为观察，等待研究条件改善")
	}
	if plan.DataStatus != "fresh" || c.QuoteAsOf == "" {
		wait = append(wait, "行情时效为"+plan.DataStatus+"，需刷新为有效行情后再评估")
	}

	plannedPrice := c.Price
	switch recType {
	case model.RecTypeShortTerm:
		if !shortPlanPricesValid(p, c.Price) {
			notSuitable = append(notSuitable, "买入区间、止盈与止损的价位关系无效")
		} else {
			plannedPrice = p.BuyZoneHigh // 按区间上沿估算，确保整手金额不超过研究预算。
			if c.Price < p.BuyZoneLow {
				wait = append(wait, "当前价格尚未进入买入区间")
			} else if c.Price > p.BuyZoneHigh {
				wait = append(wait, "当前价格已高于买入区间，按计划不追价")
			}
		}
	case model.RecTypeLongTerm:
		switch {
		case p.ValuationLow == 0 && p.ValuationHigh == 0:
			wait = append(wait, "长期估值区间缺失，暂不能标记为可执行")
		case p.ValuationLow <= 0 || p.ValuationHigh <= p.ValuationLow:
			notSuitable = append(notSuitable, "长期估值区间关系无效")
		case c.Price < p.ValuationLow:
			wait = append(wait, "当前价格低于研究估值区间，需先核验下跌原因")
		case c.Price > p.ValuationHigh:
			wait = append(wait, "当前价格高于研究估值区间，等待估值回落")
		}
	default:
		notSuitable = append(notSuitable, "推荐周期类型无效")
	}

	if p.Action == model.RecActionBuy {
		if p.PositionPct <= 0 {
			notSuitable = append(notSuitable, "现有仓位模型缺少有效预算，无法计算计划金额")
		} else if snap.TotalCapital > 0 {
			plan.PlannedCapital = round2(snap.TotalCapital * p.PositionPct / 100)
		}
		if plannedPrice <= 0 {
			notSuitable = append(notSuitable, "计划价格无效，无法计算整手数量")
		} else if plan.PlannedCapital > 0 {
			lots := int(math.Floor(plan.PlannedCapital / (plannedPrice * 100)))
			if lots <= 0 {
				notSuitable = append(notSuitable, "计划资金不足买入一手（100股）")
			} else {
				plan.PlannedPrice = round2(plannedPrice)
				plan.Quantity = affordableBoardLotQuantity(c.Market, p.Symbol, plannedPrice, plan.PlannedCapital, lots*100)
				if plan.Quantity <= 0 {
					notSuitable = append(notSuitable, "计划资金计入买入费用后不足一手（100股）")
				} else {
					buyAmount := float64(plan.Quantity) * plannedPrice
					buyFee, buyTax := tradeFee(c.Market, model.PaperSideBuy, p.Symbol, buyAmount)
					plan.EstimatedCapital = round2(buyAmount + buyFee + buyTax)
					if p.StopLoss > 0 {
						if p.StopLoss >= plannedPrice {
							notSuitable = append(notSuitable, "止损价不低于计划价格，价位关系无效")
						} else {
							sellAmount := p.StopLoss * float64(plan.Quantity)
							sellFee, sellTax := tradeFee(c.Market, model.PaperSideSell, p.Symbol, sellAmount)
							loss := round2(plan.EstimatedCapital - (sellAmount - sellFee - sellTax))
							plan.MaxPlannedLoss = &loss
						}
					}
				}
			}
		}
	}

	if len(notSuitable) > 0 {
		plan.Status = executionNotSuitable
		plan.UnavailableReasons = append(plan.UnavailableReasons, notSuitable...)
		plan.UnavailableReasons = append(plan.UnavailableReasons, wait...)
	} else if len(wait) > 0 {
		plan.Status = executionWait
		plan.UnavailableReasons = append(plan.UnavailableReasons, wait...)
	}
	return plan
}

func affordableBoardLotQuantity(market, symbol string, price, budget float64, initial int) int {
	for quantity := initial; quantity >= 100; quantity -= 100 {
		amount := float64(quantity) * price
		fee, tax := tradeFee(market, model.PaperSideBuy, symbol, amount)
		if round2(amount+fee+tax) <= budget {
			return quantity
		}
	}
	return 0
}

func recommendationTypeLabel(recType string) string {
	if recType == model.RecTypeShortTerm {
		return "短线"
	}
	if recType == model.RecTypeLongTerm {
		return "长线"
	}
	return "未知周期"
}

func loadHoldingSymbolSet(userID int64) (map[string]bool, error) {
	var rows []model.Position
	if err := common.DB.Select("market", "symbol").
		Where("user_id = ? AND status = ? AND quantity > 0", userID, model.PositionStatusHolding).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[marketSymbolKey(row.Market, row.Symbol)] = true
	}
	return out, nil
}

func marketSymbolKey(market, symbol string) string {
	return strings.ToLower(strings.TrimSpace(market)) + ":" + strings.ToLower(strings.TrimSpace(symbol))
}

func horizonLabel(v string) string {
	switch v {
	case HorizonShortTerm:
		return "短线"
	case HorizonMidTerm:
		return "中线"
	case HorizonLongTerm:
		return "长线"
	default:
		return ""
	}
}

func riskLevelLabel(v string) string {
	switch v {
	case "conservative":
		return "保守"
	case "aggressive":
		return "激进"
	default:
		return "均衡"
	}
}
