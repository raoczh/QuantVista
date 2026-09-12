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

func (s *PortfolioRiskService) currentHoldings(ctx context.Context, state *portfolioRiskLedger) ([]PortfolioHoldingWeight, []RebalanceHolding, *PortfolioExposure, error) {
	account := &state.account
	if account.Kind == model.PortfolioKindReal {
		rows := state.realHoldings
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
		currencyIssues := map[string]string{}
		for _, p := range rows {
			if reason := positionCurrencyIssue(p, account.Currency); reason != "" {
				currencyIssues[QuoteKey(p.Market, p.Symbol)] = reason
			}
		}
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
			if issue := currencyIssues[key]; issue != "" {
				fresh, reason = false, issue
			}
			if issue := state.valuationGaps[key]; issue != "" {
				fresh, reason = false, issue
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
	rows := state.paperHoldings
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
		currencyIssue := positionCurrencyIssue(model.Position{Market: h.Market}, account.Currency)
		if issue := state.valuationGaps[QuoteKey(h.Market, h.Symbol)]; issue != "" {
			currencyIssue = issue
		}
		valuation := valuations[QuoteKey(h.Market, h.Symbol)]
		v := PortfolioHoldingWeight{Symbol: h.Symbol, Market: h.Market, Name: h.Name, Industry: industries[h.Symbol], Quantity: h.Quantity, Status: RiskStatusUnavailable, Reason: "缺少 fresh 价格", ValuationKnown: valuation != nil && valuation.PETTM != 0}
		r := RebalanceHolding{Symbol: h.Symbol, Name: h.Name, Market: h.Market, Industry: industries[h.Symbol], Quantity: h.Quantity, FreshnessReason: "缺少 fresh 价格"}
		exposureView := PositionView{Position: model.Position{UserID: account.UserID, AccountID: account.ID, Symbol: h.Symbol, Market: h.Market, Name: h.Name, Quantity: h.Quantity, BuyPrice: h.AvgCost, Status: model.PositionStatusHolding}}
		if currencyIssue != "" {
			v.Reason, r.FreshnessReason = currencyIssue, currencyIssue
		} else if ok && fq.Quote != nil && fq.Quote.Price > 0 && fq.Fresh.Status == freshStatusFresh {
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
	out, _, err := s.overviewWithHoldings(ctx, userID, accountID)
	return out, err
}

func portfolioOverviewFromLedger(state *portfolioRiskLedger, holdings []PortfolioHoldingWeight, exposure *PortfolioExposure) *PortfolioOverviewView {
	out := &PortfolioOverviewView{Account: &state.account, AsOf: state.asOf, Holdings: holdings, Exposure: exposure, PartialReasons: []string{}, DataVersion: portfolioRiskVersion, HoldingCount: len(holdings)}
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
	out.Cash = state.cash
	if out.Cash.Status == RiskStatusAvailable {
		out.TotalAssets = available(round2(out.Cash.Value+out.MarketValue), out.PricedCount)
	} else {
		out.TotalAssets = unavailable(out.Cash.Reason, out.PricedCount)
		out.PartialReasons = append(out.PartialReasons, out.Cash.Reason)
	}
	if out.PricedCount < out.HoldingCount {
		out.TotalAssets = unavailable("部分持仓价格或币种口径不可用，无法计算完整总资产", out.PricedCount)
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
	return out
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
	tradePositionIDs := make(map[int64]struct{}, len(trades))
	for _, t := range trades {
		tradePositionIDs[t.PositionID] = struct{}{}
		switch t.Side {
		case model.PositionTradeBuy:
			cash -= t.Price*t.Quantity + t.Fee + t.Tax
		case model.PositionTradeSell:
			cash += t.Price*t.Quantity - t.Fee - t.Tax
		case model.PositionTradeAdjust:
			cash += t.RealizedPnl
		}
	}
	// 旧持仓在账本升级前没有 position_trades。风险计算必须保持只读，不能像持仓
	// 列表那样惰性补建；这里仅对可由现有字段无歧义重建的买入现金做等价回放。
	var positions []model.Position
	if err := db.Where("user_id = ? AND account_id = ? AND (buy_date = '' OR buy_date <= ?)", userID, accountID, asOf).Find(&positions).Error; err != nil {
		return 0, "", err
	}
	knownPositions := make(map[int64]bool, len(positions))
	for _, p := range positions {
		knownPositions[p.ID] = true
		if reason := positionCurrencyIssue(p, "CNY"); reason != "" {
			return 0, reason, nil
		}
		if _, ok := tradePositionIDs[p.ID]; ok {
			continue
		}
		if p.Status == model.PositionStatusHolding && p.Quantity > positionQtyEps {
			if p.TotalSellNet != 0 {
				return 0, "旧持仓缺少可回放的分批卖出流水", nil
			}
			cash -= p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax
			continue
		}
		if p.Status == model.PositionStatusClosed && p.TotalBuyCost > 0 {
			cash -= p.TotalBuyCost
			if p.SellDate != "" && p.SellDate <= asOf {
				if p.TotalSellNet != 0 {
					cash += p.TotalSellNet
				} else if p.SellPrice > 0 && p.TotalBuyQty > 0 {
					cash += p.SellPrice*p.TotalBuyQty - p.SellFee - p.SellTax
				} else {
					return 0, "旧平仓记录缺少可回放的卖出事实", nil
				}
			}
			continue
		}
		if p.Status == model.PositionStatusClosed && p.Quantity > positionQtyEps && p.BuyPrice > 0 {
			cash -= p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax
			if p.SellDate != "" && p.SellDate <= asOf {
				if p.SellPrice <= 0 {
					return 0, "旧平仓记录缺少可回放的卖出事实", nil
				}
				cash += p.SellPrice*p.Quantity - p.SellFee - p.SellTax
			}
			continue
		}
		return 0, "存在无法由旧持仓字段重建的历史现金影响", nil
	}
	for id := range tradePositionIDs {
		if !knownPositions[id] {
			return 0, "交易缺少对应持仓，无法核验现金币种口径", nil
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

func (s *PortfolioRiskService) equityPoints(db *gorm.DB, account *model.PortfolioAccount, days int, asOf string) ([]EquityPoint, int, []string, error) {
	currencyGap, err := portfolioCurrencyGapFor(db, account.UserID, account.ID, account.Kind)
	if err != nil {
		return nil, 0, nil, err
	}
	var snaps []model.PortfolioSnapshot
	if err := db.Where("user_id = ? AND account_id = ? AND trade_date <= ?", account.UserID, account.ID, asOf).
		Order("trade_date DESC").Limit(days + 1).Find(&snaps).Error; err != nil {
		return nil, 0, nil, err
	}
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
	}
	dates := make([]string, len(snaps))
	for i, snap := range snaps {
		dates[i] = snap.TradeDate
	}
	gaps, err := riskDailyIntervalGaps(db, "cn", dates)
	if err != nil {
		return nil, 0, nil, err
	}
	points := make([]EquityPoint, 0, len(snaps))
	partial := 0
	reasons := []string{}
	var flows []model.PortfolioCashFlow
	if account.Kind == model.PortfolioKindReal {
		from := asOf
		if len(snaps) > 0 {
			from = snaps[0].TradeDate
		}
		if err := db.Where("user_id = ? AND account_id = ? AND trade_date >= ? AND trade_date <= ?", account.UserID, account.ID, from, asOf).Find(&flows).Error; err != nil {
			return nil, 0, nil, err
		}
	}
	for i, snap := range snaps {
		p := EquityPoint{TradeDate: snap.TradeDate, Partial: snap.Partial, ReturnUnavailableReason: gaps[i]}
		if currencyGap.affects(snap.TradeDate) {
			p.Partial = true
			reasons = append(reasons, snap.TradeDate+": "+currencyGap.Reason)
		}
		if gaps[i] != "" {
			reasons = append(reasons, snap.TradeDate+": "+gaps[i])
		}
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
			cash, reason, err := realCashBalance(db, account.UserID, account.ID, snap.TradeDate)
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

// riskDailyIntervalGaps 核对相邻记录是否相隔一个交易日，防止把多日变化当作日收益。
// A 股周末确定休市；其他缺失日历不能用工作日近似，也不补造任何价格或快照。
func riskDailyIntervalGaps(db *gorm.DB, market string, dates []string) ([]string, error) {
	gaps := make([]string, len(dates))
	if len(dates) < 2 {
		return gaps, nil
	}
	var rows []model.TradingCalendar
	if err := db.Where("market = ? AND trade_date >= ? AND trade_date <= ?", market, dates[0], dates[len(dates)-1]).Find(&rows).Error; err != nil {
		return nil, err
	}
	calendar := make(map[string]bool, len(rows))
	for _, row := range rows {
		calendar[row.TradeDate] = row.IsOpen
	}
	for i := 1; i < len(dates); i++ {
		from, fromErr := time.Parse("2006-01-02", dates[i-1])
		to, toErr := time.Parse("2006-01-02", dates[i])
		if fromErr != nil || toErr != nil || !from.Before(to) {
			gaps[i] = "日期无效，无法核验日收益区间"
			continue
		}
		if !calendar[dates[i-1]] || !calendar[dates[i]] {
			gaps[i] = "交易日历缺失或记录日期非交易日，无法核验日收益"
			continue
		}
		openDays := 0
		for date := from.AddDate(0, 0, 1); !date.After(to); date = date.AddDate(0, 0, 1) {
			isOpen, known := calendar[date.Format("2006-01-02")]
			if !known && market == "cn" && (date.Weekday() == time.Saturday || date.Weekday() == time.Sunday) {
				continue
			}
			if !known {
				gaps[i] = "区间内交易日历不完整，无法核验日收益"
				break
			}
			if isOpen {
				openDays++
			}
		}
		if gaps[i] == "" && openDays != 1 {
			gaps[i] = fmt.Sprintf("相邻记录跨越 %d 个交易日，缺少中间日收益", openDays)
		}
	}
	return gaps, nil
}

func returnsByDate(points []EquityPoint) map[string]float64 {
	out := map[string]float64{}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if a.Partial || b.Partial || a.Assets <= 0 || b.ReturnUnavailableReason != "" {
			continue
		}
		r := (b.Assets-b.CashFlow)/a.Assets - 1
		if r > -1 {
			out[b.TradeDate] = r
		}
	}
	return out
}
func localBarReturns(symbol, market string, limit int, asOf string) (map[string]float64, error) {
	var bars []model.DailyBar
	if limit < 2 {
		limit = 2
	}
	if err := common.DB.Where("symbol = ? AND market = ? AND trade_date <= ?", symbol, market, asOf).
		Order("trade_date DESC").Limit(limit + 1).Find(&bars).Error; err != nil {
		return nil, err
	}
	if err := validateLocalAdjustedBars(market, bars); err != nil {
		return nil, err
	}
	for i, j := 0, len(bars)-1; i < j; i, j = i+1, j-1 {
		bars[i], bars[j] = bars[j], bars[i]
	}
	dates := make([]string, len(bars))
	for i, bar := range bars {
		dates[i] = bar.TradeDate
	}
	gaps, err := riskDailyIntervalGaps(common.DB, market, dates)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for i := 1; i < len(bars); i++ {
		if gaps[i] == "" && bars[i-1].Close > 0 && bars[i].Close > 0 {
			out[bars[i].TradeDate] = bars[i].Close/bars[i-1].Close - 1
		}
	}
	return out, nil
}

func (s *PortfolioRiskService) Risk(ctx context.Context, userID, accountID int64, params RiskParameters) (*PortfolioRiskView, error) {
	params = params.normalized()
	if params.AsOf == "" {
		params.AsOf = time.Now().Format("2006-01-02")
	}
	state, err := s.readRiskLedger(ctx, userID, accountID, params.AsOf, params.WindowDays)
	if err != nil {
		return nil, err
	}
	points, partial, reasons := state.points, state.partial, state.reasons
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
		bench, err := localBarReturns(params.BenchmarkCode, "cn", params.WindowDays, params.AsOf)
		if err != nil {
			return nil, err
		}
		p, b, _ := alignReturnsByDate(portfolioReturns, bench)
		out.Beta, out.Alpha = BetaAlpha(p, b, params.Annualization, params.RiskFreeRatePct/100)
	}
	if len(returns) < len(points)-1 {
		for _, metric := range []*RiskMetric{&out.AnnualizedVolatility, &out.DownsideVolatility, &out.Sharpe, &out.Sortino, &out.Beta, &out.Alpha} {
			if metric.Status == RiskStatusAvailable {
				metric.Status = RiskStatusPartial
				metric.Reason = "仅基于可核验的相邻交易日日收益，窗口内存在缺失区间"
			}
		}
	}
	if params.AsOf < time.Now().Format("2006-01-02") {
		reason := "历史 as_of 缺少逐标的持仓快照，相关性、暴露和风险贡献不可复现"
		out.UnknownReasons = uniqueRiskStrings(append(out.UnknownReasons, reason))
		out.Correlation = CorrelationMatrix{Symbols: []string{}, Cells: [][]CorrelationCell{}, WindowDays: params.WindowDays, AsOf: params.AsOf, DataVersion: "daily-bars-v1"}
		out.RiskContribution = RiskContributionResult{PredictedVolatility: unavailable(reason, 0), Items: []RiskContributionItem{}, WindowDays: params.WindowDays, AsOf: params.AsOf, DataVersion: "daily-bars-covariance-v1"}
		return out, nil
	}
	holdings, _, exposure, err := s.currentHoldings(ctx, state)
	if err != nil {
		return nil, err
	}
	series := map[string]map[string]float64{}
	weights := map[string]float64{}
	marketValue := 0.0
	totalAssets := 0.0
	weightsComplete := true
	weightReason := ""
	for _, h := range holdings {
		r, err := localBarReturns(h.Symbol, h.Market, params.WindowDays, params.AsOf)
		if err != nil {
			return nil, err
		}
		series[QuoteKey(h.Market, h.Symbol)] = r
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
		if state.cash.Status != RiskStatusAvailable {
			weightsComplete = false
			weightReason = state.cash.Reason + "，风险贡献不可用"
		} else {
			totalAssets = marketValue + state.cash.Value
		}
		if weightsComplete && totalAssets <= 0 {
			weightsComplete = false
			weightReason = "组合总资产非正数，风险贡献不可用"
		}
	}
	if weightsComplete && marketValue > 0 && totalAssets > 0 {
		for _, h := range holdings {
			weights[QuoteKey(h.Market, h.Symbol)] = h.Value / totalAssets
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
	if math.IsNaN(scenario.ShockPct) || math.IsInf(scenario.ShockPct, 0) || scenario.ShockPct > 0 || scenario.ShockPct < -100 {
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
	state, err := s.readRiskLedger(ctx, userID, accountID, "", 0)
	if err != nil {
		return nil, err
	}
	holdings, _, exposure, err := s.currentHoldings(ctx, state)
	if err != nil {
		return nil, err
	}
	overview := portfolioOverviewFromLedger(state, holdings, exposure)
	inputs := make([]StressHolding, 0, len(holdings))
	if scenario.Type == "plan_stop_loss" && state.account.Kind == model.PortfolioKindReal {
		// 同一标的可以有多笔成本和计划，不能把聚合后的第一笔止损价套给全部数量。
		bySymbol := make(map[string]PortfolioHoldingWeight, len(holdings))
		for _, h := range holdings {
			bySymbol[QuoteKey(h.Market, h.Symbol)] = h
		}
		for _, p := range state.realHoldings {
			h := bySymbol[QuoteKey(p.Market, p.Symbol)]
			inputs = append(inputs, StressHolding{Symbol: p.Symbol, Name: p.Name, Industry: h.Industry,
				Value: round2(p.Quantity * h.Price), Quantity: p.Quantity, Price: h.Price, PlanStopLoss: p.PlanStopLoss,
				Known: h.Status == RiskStatusAvailable, ValuationKnown: h.ValuationKnown})
		}
	} else {
		for _, h := range holdings {
			inputs = append(inputs, StressHolding{Symbol: h.Symbol, Name: h.Name, Industry: h.Industry, Value: h.Value, Quantity: h.Quantity, Price: h.Price, PlanStopLoss: h.PlanStopLoss, Known: h.Status == RiskStatusAvailable, ValuationKnown: h.ValuationKnown})
		}
	}
	out := ComputeStress(inputs, scenario, time.Now())
	if overview.TotalAssets.Status == RiskStatusAvailable && overview.TotalAssets.Value > 0 {
		out.BaseValue = overview.TotalAssets.Value
		out.EstimatedLossPct = round2(out.EstimatedLossAmount / out.BaseValue * 100)
	} else if overview.TotalAssets.Reason != "" {
		out.Unknown = append(out.Unknown, overview.TotalAssets.Reason+"，损失比例仅基于已知持仓市值")
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
		if math.IsNaN(items[i].TargetWeightPct) || math.IsInf(items[i].TargetWeightPct, 0) ||
			math.IsNaN(items[i].MinWeightPct) || math.IsInf(items[i].MinWeightPct, 0) ||
			math.IsNaN(items[i].MaxWeightPct) || math.IsInf(items[i].MaxWeightPct, 0) ||
			items[i].TargetWeightPct < 0 || items[i].TargetWeightPct > 100 || items[i].MinWeightPct < 0 ||
			items[i].MaxWeightPct < 0 || items[i].MaxWeightPct > 100 || items[i].MinWeightPct > items[i].TargetWeightPct ||
			(items[i].MaxWeightPct > 0 && (items[i].MinWeightPct > items[i].MaxWeightPct || items[i].TargetWeightPct > items[i].MaxWeightPct)) {
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
	return s.SaveTargetsContext(context.Background(), userID, accountID, items)
}

func (s *PortfolioRiskService) SaveTargetsContext(ctx context.Context, userID, accountID int64, items []TargetAllocationItem) (*model.TargetAllocationRevision, error) {
	db := common.DB.WithContext(ctx)
	if _, err := activePortfolioAccountByIDDB(db, userID, accountID, ""); err != nil {
		return nil, err
	}
	items, err := normalizeTargets(items)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(items)
	row := model.TargetAllocationRevision{UserID: userID, AccountID: accountID, ItemsJSON: string(b), ContentHash: stableHash(items)}
	err = db.Transaction(func(tx *gorm.DB) error {
		// revision 通过账户行锁串行化；首个 revision 没有可锁的历史行，
		// 锁账户本身可避免两个空账户同时生成 revision=1。
		if err := lockActivePortfolioAccount(tx, userID, accountID, ""); err != nil {
			return err
		}
		var latest model.TargetAllocationRevision
		e := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Order("revision DESC").First(&latest).Error
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		row.Revision = latest.Revision + 1
		return tx.Create(&row).Error
	})
	return &row, err
}
func LoadTargetRevision(userID, accountID int64, revision int) (*model.TargetAllocationRevision, []TargetAllocationItem, error) {
	return LoadTargetRevisionContext(context.Background(), userID, accountID, revision)
}

func LoadTargetRevisionContext(ctx context.Context, userID, accountID int64, revision int) (*model.TargetAllocationRevision, []TargetAllocationItem, error) {
	if revision < 0 {
		return nil, nil, errors.New("目标配置版本不能为负数")
	}
	db := common.DB.WithContext(ctx)
	if _, err := portfolioAccountByIDDB(db, userID, accountID, ""); err != nil {
		return nil, nil, err
	}
	q := db.Where("user_id = ? AND account_id = ?", userID, accountID)
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
	rev, targets, err := LoadTargetRevisionContext(ctx, userID, accountID, revision)
	if err != nil {
		return nil, err
	}
	ov, holdings, err := s.overviewWithHoldings(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	out := &RebalanceDraftView{AccountID: accountID, Revision: rev.Revision, RevisionHash: rev.ContentHash, AsOf: ov.AsOf, TotalAssets: ov.TotalAssets, ReadOnly: true, Note: "只读研究草案，不创建成交流水、不自动下单"}
	if ov.TotalAssets.Status != RiskStatusAvailable {
		out.Items = []RebalanceDraftItem{}
		return out, nil
	}
	holdings = s.addUnheldTargetQuotes(ctx, holdings, targets)
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
	d, err := time.ParseInLocation("2006-01-02", asOf, time.Local)
	if err != nil || d.Format("2006-01-02") != asOf {
		return fmt.Errorf("as_of 格式应为 YYYY-MM-DD")
	}
	if asOf > time.Now().Format("2006-01-02") {
		return fmt.Errorf("as_of 不能晚于今天")
	}
	return nil
}

var _ = math.Abs
