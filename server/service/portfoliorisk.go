package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PortfolioRiskService struct {
	market    *MarketService
	positions *PositionService
}

func NewPortfolioRiskService(market *MarketService, positions *PositionService) *PortfolioRiskService {
	return &PortfolioRiskService{market: market, positions: positions}
}

type PortfolioHoldingWeight struct {
	Symbol         string  `json:"symbol"`
	Market         string  `json:"market"`
	Name           string  `json:"name"`
	Industry       string  `json:"industry,omitempty"`
	Quantity       float64 `json:"quantity"`
	Price          float64 `json:"price,omitempty"`
	Value          float64 `json:"value,omitempty"`
	WeightPct      float64 `json:"weight_pct,omitempty"`
	Status         string  `json:"status"`
	Reason         string  `json:"reason,omitempty"`
	PlanStopLoss   float64 `json:"plan_stop_loss,omitempty"`
	ValuationKnown bool    `json:"valuation_known"`
}

type PortfolioOverviewView struct {
	Account        *model.PortfolioAccount  `json:"account"`
	AsOf           string                   `json:"as_of"`
	TotalAssets    RiskMetric               `json:"total_assets"`
	MarketValue    float64                  `json:"market_value"`
	Cash           RiskMetric               `json:"cash"`
	HoldingCount   int                      `json:"holding_count"`
	PricedCount    int                      `json:"priced_count"`
	CoveragePct    float64                  `json:"coverage_pct"`
	TopNWeightPct  float64                  `json:"top_n_weight_pct"`
	Holdings       []PortfolioHoldingWeight `json:"holdings"`
	Exposure       *PortfolioExposure       `json:"exposure,omitempty"`
	PartialReasons []string                 `json:"partial_reasons"`
	DataVersion    string                   `json:"data_version"`
}

type PortfolioRiskView struct {
	AccountID            int64                  `json:"account_id"`
	AsOf                 string                 `json:"as_of"`
	WindowDays           int                    `json:"window_days"`
	ParameterHash        string                 `json:"parameter_hash"`
	Parameters           RiskParameters         `json:"parameters"`
	TWR                  RiskMetric             `json:"twr_pct"`
	AnnualizedVolatility RiskMetric             `json:"annualized_volatility_pct"`
	DownsideVolatility   RiskMetric             `json:"downside_volatility_pct"`
	Sharpe               RiskMetric             `json:"sharpe"`
	Sortino              RiskMetric             `json:"sortino"`
	Beta                 RiskMetric             `json:"beta"`
	Alpha                RiskMetric             `json:"alpha_pct"`
	MaxDrawdown          DrawdownResult         `json:"max_drawdown"`
	Curve                []EquityPoint          `json:"curve"`
	Correlation          CorrelationMatrix      `json:"correlation"`
	Exposure             *PortfolioExposure     `json:"exposure,omitempty"`
	RiskContribution     RiskContributionResult `json:"risk_contribution"`
	PartialCount         int                    `json:"partial_count"`
	UnknownReasons       []string               `json:"unknown_reasons"`
	DataVersion          string                 `json:"data_version"`
}

func (s *PortfolioRiskService) currentHoldings(ctx context.Context, account *model.PortfolioAccount) ([]PortfolioHoldingWeight, []RebalanceHolding, *PortfolioExposure, error) {
	if account.Kind == model.PortfolioKindReal {
		var rows []model.Position
		if err := common.DB.Where("user_id = ? AND account_id = ? AND status = ?", account.UserID, account.ID, model.PositionStatusHolding).Order("id ASC").Find(&rows).Error; err != nil {
			return nil, nil, nil, err
		}
		refs := make([]QuoteRef, 0, len(rows))
		symbols := make([]string, 0, len(rows))
		seenRefs := map[string]bool{}
		for _, p := range rows {
			key := QuoteKey(p.Market, p.Symbol)
			if !seenRefs[key] {
				seenRefs[key] = true
				refs = append(refs, QuoteRef{Market: p.Market, Symbol: p.Symbol})
			}
			if p.Market == "cn" {
				symbols = append(symbols, p.Symbol)
			}
		}
		quotes := map[string]FreshQuoteResult{}
		valuations := map[string]*datasource.Valuation{}
		if s.market != nil && s.market.mgr != nil {
			quotes = s.market.FreshQuotesFor(ctx, refs)
			valuations = s.market.ValuationsFor(ctx, refs)
		}
		industries := industriesFor(symbols)
		total := 0.0
		agg := map[string]*PortfolioHoldingWeight{}
		reb := map[string]*RebalanceHolding{}
		exposureViews := make([]PositionView, 0, len(rows))
		for _, p := range rows {
			key := QuoteKey(p.Market, p.Symbol)
			fq, hasQuote := quotes[key]
			fresh := hasQuote && fq.Quote != nil && fq.Quote.Price > 0 && fq.Fresh.Status == freshStatusFresh
			reason := "缺少 fresh 价格"
			if hasQuote && fq.Quote != nil && !fresh {
				reason = "行情已过期"
				if note, _ := stockFreshnessNote(fq.Fresh, fq.Quote.DataTime); note != "" {
					reason = note
				}
			}
			if agg[key] == nil {
				valuation := valuations[key]
				agg[key] = &PortfolioHoldingWeight{Symbol: p.Symbol, Market: p.Market, Name: p.Name, Industry: industries[p.Symbol], Status: RiskStatusUnavailable, Reason: reason, PlanStopLoss: p.PlanStopLoss, ValuationKnown: valuation != nil && valuation.PETTM != 0}
				reb[key] = &RebalanceHolding{Symbol: p.Symbol, Name: p.Name, Market: p.Market, Industry: industries[p.Symbol], Fresh: fresh, FreshnessReason: reason, Suspended: strings.Contains(reason, "停牌")}
			}
			agg[key].Quantity += p.Quantity
			reb[key].Quantity += p.Quantity
			exposureView := PositionView{Position: p, FreshnessStatus: fq.Fresh.Status, StaleReason: reason}
			if fresh {
				value := round2(fq.Quote.Price * p.Quantity)
				agg[key].Status = RiskStatusAvailable
				agg[key].Reason = ""
				agg[key].Price = fq.Quote.Price
				agg[key].Value += value
				reb[key].Price = fq.Quote.Price
				reb[key].Value += value
				total += value
				exposureView.CurrentPrice, exposureView.MarketValue, exposureView.QuoteOK = fq.Quote.Price, value, true
				exposureView.FreshnessStatus, exposureView.StaleReason = freshStatusFresh, ""
				if valuation := valuations[key]; valuation != nil && valuation.LimitUp > 0 && fq.Quote.Price >= valuation.LimitUp-0.005 {
					reb[key].LimitUp = true
				}
			}
			exposureViews = append(exposureViews, exposureView)
		}
		out := make([]PortfolioHoldingWeight, 0, len(agg))
		rb := make([]RebalanceHolding, 0, len(reb))
		for k, v := range agg {
			if total > 0 {
				v.WeightPct = round2(v.Value / total * 100)
			}
			out = append(out, *v)
			rb = append(rb, *reb[k])
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
		sort.Slice(rb, func(i, j int) bool { return rb[i].Value > rb[j].Value })
		skipped := 0
		for _, v := range out {
			if v.Status != RiskStatusAvailable {
				skipped++
			}
		}
		return out, rb, computeExposure(exposureViews, industries, valuations, skipped), nil
	}
	var rows []model.PaperHolding
	if err := common.DB.Where("user_id = ? AND account_id = ?", account.UserID, account.ID).Find(&rows).Error; err != nil {
		return nil, nil, nil, err
	}
	refs := make([]QuoteRef, 0, len(rows))
	symbols := make([]string, 0, len(rows))
	for _, h := range rows {
		refs = append(refs, QuoteRef{Market: h.Market, Symbol: h.Symbol})
		if h.Market == "cn" {
			symbols = append(symbols, h.Symbol)
		}
	}
	quotes := map[string]FreshQuoteResult{}
	valuations := map[string]*datasource.Valuation{}
	if s.market != nil && s.market.mgr != nil {
		quotes = s.market.FreshQuotesFor(ctx, refs)
		valuations = s.market.ValuationsFor(ctx, refs)
	}
	industries := industriesFor(symbols)
	out := make([]PortfolioHoldingWeight, 0, len(rows))
	rb := make([]RebalanceHolding, 0, len(rows))
	exposureViews := make([]PositionView, 0, len(rows))
	total := 0.0
	for _, h := range rows {
		fq, ok := quotes[QuoteKey(h.Market, h.Symbol)]
		valuation := valuations[QuoteKey(h.Market, h.Symbol)]
		v := PortfolioHoldingWeight{Symbol: h.Symbol, Market: h.Market, Name: h.Name, Industry: industries[h.Symbol], Quantity: h.Quantity, Status: RiskStatusUnavailable, Reason: "缺少 fresh 价格", ValuationKnown: valuation != nil && valuation.PETTM != 0}
		r := RebalanceHolding{Symbol: h.Symbol, Name: h.Name, Market: h.Market, Industry: industries[h.Symbol], Quantity: h.Quantity, FreshnessReason: "缺少 fresh 价格"}
		exposureView := PositionView{Position: model.Position{UserID: account.UserID, AccountID: account.ID, Symbol: h.Symbol, Market: h.Market, Name: h.Name, Quantity: h.Quantity, BuyPrice: h.AvgCost, Status: model.PositionStatusHolding}}
		if ok && fq.Quote != nil && fq.Quote.Price > 0 && fq.Fresh.Status == freshStatusFresh {
			v.Status = RiskStatusAvailable
			v.Reason = ""
			v.Price = fq.Quote.Price
			v.Value = round2(v.Price * h.Quantity)
			r.Price = v.Price
			r.Value = v.Value
			r.Fresh = true
			total += v.Value
			exposureView.CurrentPrice, exposureView.MarketValue, exposureView.QuoteOK = v.Price, v.Value, true
			exposureView.FreshnessStatus = freshStatusFresh
			if valuation := valuations[QuoteKey(h.Market, h.Symbol)]; valuation != nil && valuation.LimitUp > 0 && v.Price >= valuation.LimitUp-0.005 {
				r.LimitUp = true
			}
		} else if ok && fq.Quote != nil {
			r.FreshnessReason = "行情已过期"
			if note, _ := stockFreshnessNote(fq.Fresh, fq.Quote.DataTime); note != "" {
				r.FreshnessReason = note
			}
			r.Suspended = strings.Contains(r.FreshnessReason, "停牌")
			v.Reason = r.FreshnessReason
			exposureView.FreshnessStatus, exposureView.StaleReason = fq.Fresh.Status, r.FreshnessReason
		}
		out = append(out, v)
		rb = append(rb, r)
		exposureViews = append(exposureViews, exposureView)
	}
	for i := range out {
		if total > 0 {
			out[i].WeightPct = round2(out[i].Value / total * 100)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	skipped := 0
	for _, v := range out {
		if v.Status != RiskStatusAvailable {
			skipped++
		}
	}
	return out, rb, computeExposure(exposureViews, industries, valuations, skipped), nil
}

func (s *PortfolioRiskService) Overview(ctx context.Context, userID, accountID int64) (*PortfolioOverviewView, error) {
	account, err := PortfolioAccountByID(userID, accountID, "")
	if err != nil {
		return nil, err
	}
	holdings, _, exposure, err := s.currentHoldings(ctx, account)
	if err != nil {
		return nil, err
	}
	out := &PortfolioOverviewView{Account: account, AsOf: time.Now().Format("2006-01-02"), Holdings: holdings, Exposure: exposure, PartialReasons: []string{}, DataVersion: portfolioRiskVersion, HoldingCount: len(holdings)}
	for _, h := range holdings {
		if h.Status == RiskStatusAvailable {
			out.PricedCount++
			out.MarketValue += h.Value
		} else {
			out.PartialReasons = append(out.PartialReasons, h.Symbol+": "+h.Reason)
		}
	}
	out.MarketValue = round2(out.MarketValue)
	if out.HoldingCount > 0 {
		out.CoveragePct = round2(float64(out.PricedCount) / float64(out.HoldingCount) * 100)
	} else {
		out.CoveragePct = 100
	}
	if account.Kind == model.PortfolioKindPaper {
		var cash model.PaperAccount
		if err := common.DB.Where("user_id = ? AND account_id = ?", userID, accountID).First(&cash).Error; err != nil {
			return nil, err
		}
		out.Cash = available(cash.Cash, 1)
		out.TotalAssets = available(round2(cash.Cash+out.MarketValue), out.PricedCount)
	} else {
		cash, reason, err := realCashBalance(common.DB, userID, accountID, out.AsOf)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			out.Cash = unavailable(reason, 0)
			out.TotalAssets = unavailable("真实账户缺少完整现金流事实，不能给出完整总资产", out.PricedCount)
			out.PartialReasons = append(out.PartialReasons, reason)
		} else {
			out.Cash = available(cash, 1)
			out.TotalAssets = available(round2(cash+out.MarketValue), out.PricedCount)
		}
	}
	if out.PricedCount < out.HoldingCount {
		out.TotalAssets = unavailable("部分持仓缺少 fresh 价格，完整总资产不可用", out.PricedCount)
	}
	if out.TotalAssets.Status == RiskStatusAvailable && out.TotalAssets.Value > 0 {
		for i := range holdings {
			if holdings[i].Status == RiskStatusAvailable {
				holdings[i].WeightPct = round2(holdings[i].Value / out.TotalAssets.Value * 100)
			}
		}
	}
	for i := 0; i < len(holdings) && i < 5; i++ {
		out.TopNWeightPct += holdings[i].WeightPct
	}
	out.TopNWeightPct = round2(out.TopNWeightPct)
	out.Holdings = holdings
	return out, nil
}

func realCashBalance(db *gorm.DB, userID, accountID int64, asOf string) (float64, string, error) {
	var flows []model.PortfolioCashFlow
	if err := db.Where("user_id = ? AND account_id = ? AND trade_date <= ?", userID, accountID, asOf).Find(&flows).Error; err != nil {
		return 0, "", err
	}
	hasDeposit := false
	cash := 0.0
	for _, f := range flows {
		cash += f.Amount
		if f.Type == model.CashFlowDeposit && !isReversedFlow(flows, f.ID) {
			hasDeposit = true
		}
	}
	if !hasDeposit {
		return 0, "缺少有效初始入金现金流", nil
	}
	var trades []model.PositionTrade
	if err := db.Where("user_id = ? AND account_id = ? AND trade_date <= ?", userID, accountID, asOf).Find(&trades).Error; err != nil {
		return 0, "", err
	}
	for _, t := range trades {
		switch t.Side {
		case model.PositionTradeBuy:
			cash -= t.Price*t.Quantity + t.Fee + t.Tax
		case model.PositionTradeSell:
			cash += t.Price*t.Quantity - t.Fee - t.Tax
		case model.PositionTradeAdjust:
			cash += t.RealizedPnl
		}
	}
	return round2(cash), "", nil
}
func isReversedFlow(flows []model.PortfolioCashFlow, id int64) bool {
	for _, f := range flows {
		if f.ReversalOfID != nil && *f.ReversalOfID == id {
			return true
		}
	}
	return false
}

func riskWindowStart(asOf string, days int) string {
	end, err := time.ParseInLocation("2006-01-02", asOf, time.Local)
	if err != nil {
		end = time.Now()
	}
	return end.AddDate(0, 0, -days).Format("2006-01-02")
}

func (s *PortfolioRiskService) equityPoints(account *model.PortfolioAccount, days int, asOf string) ([]EquityPoint, int, []string, error) {
	from := riskWindowStart(asOf, days)
	var snaps []model.PortfolioSnapshot
	if err := common.DB.Where("user_id = ? AND account_id = ? AND trade_date >= ? AND trade_date <= ?", account.UserID, account.ID, from, asOf).Order("trade_date ASC").Find(&snaps).Error; err != nil {
		return nil, 0, nil, err
	}
	points := make([]EquityPoint, 0, len(snaps))
	partial := 0
	reasons := []string{}
	var flows []model.PortfolioCashFlow
	if account.Kind == model.PortfolioKindReal {
		if err := common.DB.Where("user_id = ? AND account_id = ? AND trade_date >= ?", account.UserID, account.ID, from).Find(&flows).Error; err != nil {
			return nil, 0, nil, err
		}
	}
	for i, snap := range snaps {
		p := EquityPoint{TradeDate: snap.TradeDate, Partial: snap.Partial}
		if account.Kind == model.PortfolioKindReal && i > 0 {
			previousDate := snaps[i-1].TradeDate
			for _, flow := range flows {
				if flow.TradeDate > previousDate && flow.TradeDate <= snap.TradeDate {
					p.CashFlow += flow.Amount
				}
			}
			p.CashFlow = round2(p.CashFlow)
		}
		if snap.Partial {
			reasons = append(reasons, snap.TradeDate+": partial 快照未参与完整指标")
		}
		if account.Kind == model.PortfolioKindPaper {
			p.Assets = round2(snap.MarketValue + snap.Cash)
		} else {
			cash, reason, err := realCashBalance(common.DB, account.UserID, account.ID, snap.TradeDate)
			if err != nil {
				return nil, 0, nil, err
			}
			if reason != "" {
				p.Partial = true
				reasons = append(reasons, snap.TradeDate+": "+reason)
			} else {
				p.Assets = round2(snap.MarketValue + cash)
			}
		}
		if p.Partial {
			partial++
		}
		points = append(points, p)
	}
	return points, partial, uniqueRiskStrings(reasons), nil
}
func uniqueRiskStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func returnsByDate(points []EquityPoint) map[string]float64 {
	out := map[string]float64{}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if a.Partial || b.Partial || a.Assets <= 0 {
			continue
		}
		r := (b.Assets-b.CashFlow)/a.Assets - 1
		if r > -1 {
			out[b.TradeDate] = r
		}
	}
	return out
}
func localBarReturns(symbol, market, from, asOf string) (map[string]float64, error) {
	var bars []model.DailyBar
	if err := common.DB.Where("symbol = ? AND market = ? AND trade_date >= ? AND trade_date <= ?", symbol, market, from, asOf).Order("trade_date ASC").Find(&bars).Error; err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for i := 1; i < len(bars); i++ {
		if bars[i-1].Close > 0 && bars[i].Close > 0 {
			out[bars[i].TradeDate] = bars[i].Close/bars[i-1].Close - 1
		}
	}
	return out, nil
}

func (s *PortfolioRiskService) Risk(ctx context.Context, userID, accountID int64, params RiskParameters) (*PortfolioRiskView, error) {
	account, err := PortfolioAccountByID(userID, accountID, "")
	if err != nil {
		return nil, err
	}
	params = params.normalized()
	if params.AsOf == "" {
		params.AsOf = time.Now().Format("2006-01-02")
	}
	points, partial, reasons, err := s.equityPoints(account, params.WindowDays, params.AsOf)
	if err != nil {
		return nil, err
	}
	returns, pointsWithReturns := DailyReturns(points)
	dd, pointsWithDD := MaxDrawdown(pointsWithReturns)
	for i := range pointsWithDD {
		if i < len(pointsWithReturns) {
			pointsWithDD[i].Return = pointsWithReturns[i].Return
		}
	}
	out := &PortfolioRiskView{AccountID: accountID, AsOf: params.AsOf, WindowDays: params.WindowDays, Parameters: params, ParameterHash: stableHash(params), TWR: ComputeTWR(points), AnnualizedVolatility: AnnualizedVolatility(returns, params.Annualization), DownsideVolatility: DownsideVolatility(returns, params.Annualization), Sharpe: SharpeRatio(returns, params.Annualization, params.RiskFreeRatePct/100), Sortino: SortinoRatio(returns, params.Annualization, params.RiskFreeRatePct/100), MaxDrawdown: dd, Curve: pointsWithDD, PartialCount: partial, UnknownReasons: reasons, DataVersion: portfolioRiskVersion}
	portfolioReturns := returnsByDate(points)
	if params.BenchmarkCode == "" {
		out.Beta = unavailable("未指定基准代码", 0)
		out.Alpha = unavailable("未指定基准代码", 0)
	} else {
		from := riskWindowStart(params.AsOf, params.WindowDays+10)
		bench, err := localBarReturns(params.BenchmarkCode, "cn", from, params.AsOf)
		if err != nil {
			return nil, err
		}
		p, b, _ := alignReturnsByDate(portfolioReturns, bench)
		out.Beta, out.Alpha = BetaAlpha(p, b, params.Annualization, params.RiskFreeRatePct/100)
	}
	holdings, _, exposure, err := s.currentHoldings(ctx, account)
	if err != nil {
		return nil, err
	}
	series := map[string]map[string]float64{}
	weights := map[string]float64{}
	marketValue := 0.0
	totalAssets := 0.0
	weightsComplete := true
	weightReason := ""
	from := riskWindowStart(params.AsOf, params.WindowDays+10)
	for _, h := range holdings {
		r, err := localBarReturns(h.Symbol, h.Market, from, params.AsOf)
		if err != nil {
			return nil, err
		}
		series[h.Symbol] = r
		if h.Status != RiskStatusAvailable {
			weightsComplete = false
		} else {
			marketValue += h.Value
		}
	}
	out.Correlation = CorrelationFromReturns(series, params.WindowDays, params.AsOf)
	if exposure != nil {
		exposure.WindowDays = params.WindowDays
		exposure.SampleCount = len(returns)
		exposure.AsOf = params.AsOf
		exposure.FactorVersion = portfolioFactorVersion
		exposure.DataVersion = portfolioRiskVersion
		out.Exposure = exposure
	}
	if weightsComplete && marketValue > 0 {
		if account.Kind == model.PortfolioKindPaper {
			var cash model.PaperAccount
			if err := common.DB.Where("user_id = ? AND account_id = ?", userID, accountID).First(&cash).Error; err != nil {
				return nil, err
			}
			totalAssets = marketValue + cash.Cash
		} else {
			cash, reason, err := realCashBalance(common.DB, userID, accountID, params.AsOf)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				weightsComplete = false
				weightReason = reason + "，风险贡献不可用"
			} else {
				totalAssets = marketValue + cash
			}
		}
		if totalAssets <= 0 {
			weightsComplete = false
			weightReason = "组合总资产非正数，风险贡献不可用"
		}
	}
	if weightsComplete && marketValue > 0 && totalAssets > 0 {
		for _, h := range holdings {
			weights[h.Symbol] = h.Value / totalAssets
		}
		out.RiskContribution = ComputeRiskContributions(series, weights, params.Annualization, params.WindowDays, params.AsOf)
	} else {
		reason := "持仓价格覆盖不完整，风险贡献不可用"
		if weightReason != "" {
			reason = weightReason
		}
		if len(holdings) == 0 {
			reason = "当前账户没有持仓，风险贡献不可用"
		}
		out.RiskContribution = RiskContributionResult{PredictedVolatility: unavailable(reason, 0), Items: []RiskContributionItem{}, WindowDays: params.WindowDays, AsOf: params.AsOf, DataVersion: "daily-bars-covariance-v1"}
	}
	return out, nil
}

func (s *PortfolioRiskService) Stress(ctx context.Context, userID, accountID int64, scenario StressScenario) (*StressResult, error) {
	account, err := PortfolioAccountByID(userID, accountID, "")
	if err != nil {
		return nil, err
	}
	if scenario.ShockPct > 0 || scenario.ShockPct < -100 {
		return nil, errors.New("冲击比例须在 -100% 到 0% 之间")
	}
	allowed := map[string]bool{"market": true, "industry": true, "symbol": true, "plan_stop_loss": true}
	if !allowed[scenario.Type] {
		return nil, errors.New("压力场景类型不支持")
	}
	if scenario.Type == "industry" && strings.TrimSpace(scenario.Industry) == "" {
		return nil, errors.New("行业冲击必须指定行业")
	}
	if scenario.Type == "symbol" && strings.TrimSpace(scenario.Symbol) == "" {
		return nil, errors.New("单票冲击必须指定股票代码")
	}
	holdings, _, _, err := s.currentHoldings(ctx, account)
	if err != nil {
		return nil, err
	}
	inputs := make([]StressHolding, 0, len(holdings))
	for _, h := range holdings {
		inputs = append(inputs, StressHolding{Symbol: h.Symbol, Name: h.Name, Industry: h.Industry, Value: h.Value, Quantity: h.Quantity, Price: h.Price, PlanStopLoss: h.PlanStopLoss, Known: h.Status == RiskStatusAvailable, ValuationKnown: h.ValuationKnown})
	}
	out := ComputeStress(inputs, scenario, time.Now())
	if overview, overviewErr := s.Overview(ctx, userID, accountID); overviewErr == nil && overview.TotalAssets.Status == RiskStatusAvailable && overview.TotalAssets.Value > 0 {
		out.BaseValue = overview.TotalAssets.Value
		out.EstimatedLossPct = round2(out.EstimatedLossAmount / out.BaseValue * 100)
	}
	return &out, nil
}

func normalizeTargets(items []TargetAllocationItem) ([]TargetAllocationItem, error) {
	if len(items) > 200 {
		return nil, errors.New("目标配置最多 200 项")
	}
	seen := map[string]bool{}
	sum := 0.0
	for i := range items {
		items[i].Type = strings.ToLower(strings.TrimSpace(items[i].Type))
		items[i].Key = strings.TrimSpace(items[i].Key)
		if (items[i].Type != "symbol" && items[i].Type != "industry") || items[i].Key == "" {
			return nil, errors.New("目标配置类型或标识无效")
		}
		if items[i].TargetWeightPct < 0 || items[i].TargetWeightPct > 100 || items[i].MinWeightPct < 0 || items[i].MaxWeightPct > 100 || items[i].MinWeightPct > items[i].TargetWeightPct || (items[i].MaxWeightPct > 0 && items[i].TargetWeightPct > items[i].MaxWeightPct) {
			return nil, errors.New("目标权重或上下限无效")
		}
		key := items[i].Type + ":" + items[i].Key
		if seen[key] {
			return nil, errors.New("目标配置存在重复项")
		}
		seen[key] = true
		if items[i].Enabled {
			sum += items[i].TargetWeightPct
		}
	}
	if sum > 100.000001 {
		return nil, errors.New("启用目标权重合计不能超过 100%")
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type == items[j].Type {
			return items[i].Key < items[j].Key
		}
		return items[i].Type < items[j].Type
	})
	return items, nil
}
func (s *PortfolioRiskService) SaveTargets(userID, accountID int64, items []TargetAllocationItem) (*model.TargetAllocationRevision, error) {
	if _, err := ActivePortfolioAccountByID(userID, accountID, ""); err != nil {
		return nil, err
	}
	items, err := normalizeTargets(items)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(items)
	row := model.TargetAllocationRevision{UserID: userID, AccountID: accountID, ItemsJSON: string(b), ContentHash: stableHash(items)}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		var latest model.TargetAllocationRevision
		e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND account_id = ?", userID, accountID).Order("revision DESC").First(&latest).Error
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		row.Revision = latest.Revision + 1
		return tx.Create(&row).Error
	})
	return &row, err
}
func LoadTargetRevision(userID, accountID int64, revision int) (*model.TargetAllocationRevision, []TargetAllocationItem, error) {
	if _, err := PortfolioAccountByID(userID, accountID, ""); err != nil {
		return nil, nil, err
	}
	q := common.DB.Where("user_id = ? AND account_id = ?", userID, accountID)
	if revision > 0 {
		q = q.Where("revision = ?", revision)
	} else {
		q = q.Order("revision DESC")
	}
	var row model.TargetAllocationRevision
	if err := q.First(&row).Error; err != nil {
		return nil, nil, err
	}
	var items []TargetAllocationItem
	if err := json.Unmarshal([]byte(row.ItemsJSON), &items); err != nil {
		return nil, nil, err
	}
	return &row, items, nil
}

type RebalanceDraftView struct {
	AccountID    int64                `json:"account_id"`
	Revision     int                  `json:"revision"`
	RevisionHash string               `json:"revision_hash"`
	AsOf         string               `json:"as_of"`
	TotalAssets  RiskMetric           `json:"total_assets"`
	Items        []RebalanceDraftItem `json:"items"`
	ReadOnly     bool                 `json:"read_only"`
	Note         string               `json:"note"`
}

func (s *PortfolioRiskService) Rebalance(ctx context.Context, userID, accountID int64, revision int) (*RebalanceDraftView, error) {
	account, err := PortfolioAccountByID(userID, accountID, "")
	if err != nil {
		return nil, err
	}
	rev, targets, err := LoadTargetRevision(userID, accountID, revision)
	if err != nil {
		return nil, err
	}
	ov, err := s.Overview(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	_, holdings, _, err := s.currentHoldings(ctx, account)
	if err != nil {
		return nil, err
	}
	holdings = s.addUnheldTargetQuotes(ctx, holdings, targets)
	out := &RebalanceDraftView{AccountID: accountID, Revision: rev.Revision, RevisionHash: rev.ContentHash, AsOf: ov.AsOf, TotalAssets: ov.TotalAssets, ReadOnly: true, Note: "只读研究草案，不创建成交流水、不自动下单"}
	if ov.TotalAssets.Status != RiskStatusAvailable {
		out.Items = []RebalanceDraftItem{}
		return out, nil
	}
	out.Items = BuildRebalanceDraft(holdings, targets, ov.TotalAssets.Value)
	return out, nil
}

// addUnheldTargetQuotes 为尚未持有的 symbol 目标取当前 A 股行情。它只富化内存草案，
// 不创建持仓或流水；无 fresh 价格时由 BuildRebalanceDraft fail-closed 标不可执行。
func (s *PortfolioRiskService) addUnheldTargetQuotes(ctx context.Context, holdings []RebalanceHolding, targets []TargetAllocationItem) []RebalanceHolding {
	existing := make(map[string]bool, len(holdings))
	for _, holding := range holdings {
		existing[holding.Symbol] = true
	}
	refs := make([]QuoteRef, 0)
	for _, target := range targets {
		if target.Enabled && target.Type == "symbol" && !existing[target.Key] {
			existing[target.Key] = true
			refs = append(refs, QuoteRef{Market: "cn", Symbol: target.Key})
		}
	}
	if len(refs) == 0 {
		return holdings
	}
	quotes := map[string]FreshQuoteResult{}
	valuations := map[string]*datasource.Valuation{}
	if s.market != nil && s.market.mgr != nil {
		quotes = s.market.FreshQuotesFor(ctx, refs)
		valuations = s.market.ValuationsFor(ctx, refs)
	}
	for _, ref := range refs {
		key := QuoteKey(ref.Market, ref.Symbol)
		holding := RebalanceHolding{Symbol: ref.Symbol, Market: ref.Market, FreshnessReason: "缺少 fresh 价格"}
		if valuation := valuations[key]; valuation != nil {
			holding.Name = valuation.Name
		}
		if fq, ok := quotes[key]; ok && fq.Quote != nil {
			if holding.Name == "" {
				holding.Name = fq.Quote.Name
			}
			if fq.Quote.Price > 0 && fq.Fresh.Status == freshStatusFresh {
				holding.Price, holding.Fresh = fq.Quote.Price, true
				if valuation := valuations[key]; valuation != nil && valuation.LimitUp > 0 && fq.Quote.Price >= valuation.LimitUp-0.005 {
					holding.LimitUp = true
				}
			} else {
				holding.FreshnessReason = "行情已过期"
				if note, _ := stockFreshnessNote(fq.Fresh, fq.Quote.DataTime); note != "" {
					holding.FreshnessReason = note
				}
				holding.Suspended = strings.Contains(holding.FreshnessReason, "停牌")
			}
		}
		holdings = append(holdings, holding)
	}
	return holdings
}

func NewPortfolioRiskParameters(window int, annualization int, riskFree float64, benchmark, asOf string) RiskParameters {
	return RiskParameters{Annualization: annualization, RiskFreeRatePct: riskFree, WindowDays: window, BenchmarkCode: benchmark, AsOf: asOf, Version: portfolioRiskVersion}.normalized()
}
func ValidatePortfolioRiskAsOf(asOf string) error {
	if asOf == "" {
		return nil
	}
	d, err := time.Parse("2006-01-02", asOf)
	if err != nil || d.Format("2006-01-02") != asOf {
		return fmt.Errorf("as_of 格式应为 YYYY-MM-DD")
	}
	return nil
}

var _ = math.Abs
