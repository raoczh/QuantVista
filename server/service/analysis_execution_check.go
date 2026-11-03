package service

import (
	"context"
	"fmt"
	"time"

	"quantvista/datasource"
	"quantvista/model"
)

// Recheck only execution readiness after all model calls. The frozen research
// prices and input snapshot remain unchanged; a rally never raises the buy zone.
func (service *AnalysisService) recheckAnalysisTradePlan(ctx context.Context, request AnalyzeRequest, result *AnalysisResult) {
	if request.Module != model.AnalysisModuleStock || request.AsOf != "" || result.TradePlan == nil || result.TradePlan.NoPlan {
		return
	}
	plan := result.TradePlan
	var quote *datasource.Quote
	reason := "分析完成时未能确认有效报价，暂不采用此计划建仓，请刷新后重新核对"
	if service.market != nil {
		current, freshness, err := service.market.GetFreshQuote(ctx, request.Market, request.Symbol)
		if err == nil && current != nil && freshness.Status == freshStatusFresh && current.Price > 0 {
			quote = current
		}
	}
	if quote != nil {
		checked := fmt.Sprintf("分析完成前核对：%s，行情截至 %s，价格 %g；原研究区间不随行情追抬",
			quote.Source, quote.DataTime.In(datasource.ChinaMarketLocation()).Format(time.DateTime), quote.Price)
		plan.Discipline = append(plan.Discipline, checked)
		if quote.Price >= plan.BuyLow && quote.Price <= plan.BuyHigh && quote.Price > plan.StopPrice && quote.Price < plan.TargetPrice {
			return
		}
		reason = "分析完成时价格已不在原买入区间或计划已失效，请等待或重新分析，不按旧状态建仓"
	}
	plan.NoPlan, plan.NoPlanReason = true, reason
	if plan.Position != nil {
		position := *plan.Position
		position.PositionPct, position.Note = 0, reason
		plan.Position = &position
	}
	if plan.PricePlan != nil {
		pricePlan := *plan.PricePlan
		pricePlan.Status = "unavailable"
		pricePlan.Reasons = append(append([]string{}, pricePlan.Reasons...), reason)
		plan.PricePlan = &pricePlan
	}
}
