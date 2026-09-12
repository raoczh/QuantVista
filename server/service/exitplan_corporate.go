package service

import (
	"encoding/json"
	"errors"
	"math"

	"gorm.io/gorm"
	"quantvista/model"
)

type exitPlanCorporateSnapshot struct {
	Stop float64 `json:"stop"`
	Take float64 `json:"take"`
	Seed string  `json:"seed"`
}

func exitCarryFromPlan(plan ExitPlan) *ExitPlanCarry {
	return &ExitPlanCarry{Stop: plan.CurrentStop, Peak: plan.PeakPrice, ATR: plan.CurrentATR, StopSince: plan.StopEffectiveAt,
		FirstAt: plan.FirstTargetAt, SecondAt: plan.SecondTargetAt, FirstHandled: plan.FirstTargetHandled, SecondHandled: plan.SecondTargetHandled,
		StopActive: plan.StopActive, StopEpisode: plan.StopEpisode, StopAt: plan.StopTriggeredAt, Quantity: plan.ObservedQuantity}
}

// 在修改账本前保存已经建立的保护；评估的原始行仍原样保留。
func captureExitCorporateBefore(tx *gorm.DB, p model.Position) (exitPlanCorporateSnapshot, error) {
	snapshot := exitPlanCorporateSnapshot{Stop: p.PlanStopLoss, Take: p.PlanTakeProfit, Seed: p.ExitPlanSeedJSON}
	var previous model.PositionExitAssessment
	err := tx.Where("user_id = ? AND position_id = ?", p.UserID, p.ID).Order("evaluated_at DESC, id DESC").First(&previous).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return snapshot, err
	}
	if plan := decodeExitPlan(previous.PlanJSON, previous.PlanHash); plan != nil && plan.Initial.BasisHash == exitPlanBasis(p) {
		seed := plan.Initial
		seed.Carry = exitCarryFromPlan(*plan)
		sealExitSeed(&seed)
		snapshot.Seed = mustPositionExitJSON(seed)
	}
	return snapshot, nil
}

func applyExitCorporatePrices(p *model.Position, before exitPlanCorporateSnapshot, adj *model.PositionCorpAdjust) {
	ratio := 1 + (adj.BonusRatio+adj.TransferRatio)/10
	cash := adj.DividendPretax / 10
	tick := exitPriceTick(p.Symbol)
	price := func(v float64) float64 {
		if v <= 0 {
			return 0
		}
		return math.Max(tick, exitFloor((v-cash)/ratio, tick))
	}
	p.PlanStopLoss, p.PlanTakeProfit = price(before.Stop), price(before.Take)
	if seed := decodeExitSeed(before.Seed); seed != nil {
		seed.StopPrice, seed.TargetPrice, seed.ExtendedTarget = price(seed.StopPrice), price(seed.TargetPrice), price(seed.ExtendedTarget)
		seed.Support, seed.Resistance = price(seed.Support), price(seed.Resistance)
		seed.EntryPrice = p.BuyPrice
		seed.InitialRisk = round4(math.Max(0, seed.EntryPrice-seed.StopPrice))
		seed.ATR14 = round4(seed.ATR14 / ratio)
		seed.Quantity, seed.Cost = p.Quantity, round4(exitRemainingCost(*p))
		seed.Source = "corporate_adjustment"
		if c := seed.Carry; c != nil {
			copy := *c
			seed.Carry = &copy
			copy.Stop, copy.Peak, copy.ATR = price(copy.Stop), price(copy.Peak), round4(copy.ATR/ratio)
			copy.Quantity = p.Quantity
		}
		seed.EstimatedRisk = round4(math.Max(0, seed.Cost-exitNetProceeds(*p, seed.StopPrice, p.Quantity, seed.SlippageBPS)))
		seed.EstimatedReward = round4(exitNetProceeds(*p, seed.TargetPrice, p.Quantity, seed.SlippageBPS) - seed.Cost)
		seed.EstimatedExtendedReward = round4(exitNetProceeds(*p, seed.ExtendedTarget, p.Quantity, seed.SlippageBPS) - seed.Cost)
		seed.NetRewardRisk = 0
		if seed.EstimatedRisk > 0 {
			seed.NetRewardRisk = round4(seed.EstimatedReward / seed.EstimatedRisk)
		}
		seed.Evidence = append(seed.Evidence, "已按确认的公司行动转换价位刻度并承接既有保护；现金分红在账本单列")
		seed.BasisHash = exitPlanBasis(*p)
		if seed.StopPrice >= seed.EntryPrice || seed.TargetPrice <= seed.EntryPrice || seed.ExtendedTarget <= seed.TargetPrice {
			seed.DataStatus = "unavailable"
			seed.DataGaps = append(seed.DataGaps, "折算后初始风险关系已变化，需要重新核对规划")
		}
		sealExitSeed(seed)
		p.ExitPlanSeedJSON = mustPositionExitJSON(seed)
	}
	adj.ExitPlanBeforeJSON = mustPositionExitJSON(before)
	adj.ExitPlanAfterJSON = mustPositionExitJSON(exitPlanCorporateSnapshot{p.PlanStopLoss, p.PlanTakeProfit, p.ExitPlanSeedJSON})
}

func restoreExitCorporatePrices(p *model.Position, adj model.PositionCorpAdjust) error {
	if adj.ExitPlanBeforeJSON == "" && adj.ExitPlanAfterJSON == "" {
		return nil
	}
	var before, after exitPlanCorporateSnapshot
	if json.Unmarshal([]byte(adj.ExitPlanBeforeJSON), &before) != nil || json.Unmarshal([]byte(adj.ExitPlanAfterJSON), &after) != nil {
		return errors.New("退出规划折算记录损坏，不能猜测撤销前的价位")
	}
	if p.PlanStopLoss != after.Stop || p.PlanTakeProfit != after.Take || p.ExitPlanSeedJSON != after.Seed {
		return errors.New("折算后退出规划已经修改，不能撤销并覆盖当前规划")
	}
	p.PlanStopLoss, p.PlanTakeProfit, p.ExitPlanSeedJSON = before.Stop, before.Take, before.Seed
	return nil
}
