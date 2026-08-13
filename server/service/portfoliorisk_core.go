package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"quantvista/model"
)

const (
	RiskStatusAvailable    = "available"
	RiskStatusPartial      = "partial"
	RiskStatusUnavailable  = "unavailable"
	portfolioRiskVersion   = "pr1"
	portfolioFactorVersion = "position-exposure-v1"
)

type RiskMetric struct {
	Status      string  `json:"status"`
	Value       float64 `json:"value,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	SampleCount int     `json:"sample_count,omitempty"`
}

type EquityPoint struct {
	TradeDate   string   `json:"trade_date"`
	Assets      float64  `json:"assets"`
	CashFlow    float64  `json:"cash_flow,omitempty"`
	Return      *float64 `json:"return,omitempty"`
	DrawdownPct *float64 `json:"drawdown_pct,omitempty"`
	Partial     bool     `json:"partial"`
}

type DrawdownResult struct {
	Metric       RiskMetric `json:"metric"`
	PeakDate     string     `json:"peak_date,omitempty"`
	TroughDate   string     `json:"trough_date,omitempty"`
	RecoveryDate string     `json:"recovery_date,omitempty"`
}

type RiskParameters struct {
	Annualization   int     `json:"annualization"`
	RiskFreeRatePct float64 `json:"risk_free_rate_pct"`
	WindowDays      int     `json:"window_days"`
	BenchmarkCode   string  `json:"benchmark_code"`
	AsOf            string  `json:"as_of"`
	Version         string  `json:"version"`
}

func (p RiskParameters) normalized() RiskParameters {
	if p.Annualization <= 0 {
		p.Annualization = 252
	}
	if p.WindowDays <= 0 {
		p.WindowDays = 252
	}
	if p.WindowDays > 730 {
		p.WindowDays = 730
	}
	p.BenchmarkCode = strings.TrimSpace(p.BenchmarkCode)
	if p.Version == "" {
		p.Version = portfolioRiskVersion
	}
	return p
}

func stableHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func unavailable(reason string, n int) RiskMetric {
	return RiskMetric{Status: RiskStatusUnavailable, Reason: reason, SampleCount: n}
}

func available(value float64, n int) RiskMetric {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return unavailable("计算结果无效", n)
	}
	return RiskMetric{Status: RiskStatusAvailable, Value: value, SampleCount: n}
}

// ComputeTWR 按完整净值点和点间外部资金流计算链式时间加权收益率。
// cashFlow 记在区间终点：r=(V_end-flow)/V_start-1；调用方需保证流向符号入金为正。
func ComputeTWR(points []EquityPoint) RiskMetric {
	if len(points) < 2 {
		return unavailable("完整资产快照不足 2 个", len(points))
	}
	product := 1.0
	n := 0
	for i := 1; i < len(points); i++ {
		prev, cur := points[i-1], points[i]
		if prev.Partial || cur.Partial || prev.Assets <= 0 {
			continue
		}
		r := (cur.Assets-cur.CashFlow)/prev.Assets - 1
		if r <= -1 || math.IsNaN(r) || math.IsInf(r, 0) {
			return unavailable("资产或资金流无法形成有效区间收益", n)
		}
		product *= 1 + r
		n++
	}
	if n == 0 {
		return unavailable("没有相邻的完整资产快照", 0)
	}
	return available((product-1)*100, n)
}

// DailyReturns 不补交易日，只计算输入中相邻的完整净值点。
func DailyReturns(points []EquityPoint) ([]float64, []EquityPoint) {
	returns := make([]float64, 0, len(points)-1)
	out := append([]EquityPoint(nil), points...)
	for i := 1; i < len(out); i++ {
		if out[i-1].Partial || out[i].Partial || out[i-1].Assets <= 0 {
			continue
		}
		r := (out[i].Assets-out[i].CashFlow)/out[i-1].Assets - 1
		if r <= -1 || math.IsNaN(r) || math.IsInf(r, 0) {
			continue
		}
		v := r * 100
		out[i].Return = &v
		returns = append(returns, r)
	}
	return returns, out
}

func mean(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func sampleStd(values []float64) float64 {
	if len(values) < 2 {
		return math.NaN()
	}
	m := mean(values)
	var sum float64
	for _, v := range values {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(values)-1))
}

func AnnualizedVolatility(returns []float64, annualization int) RiskMetric {
	if len(returns) < 2 {
		return unavailable("日收益样本不足 2 个", len(returns))
	}
	if annualization <= 0 {
		annualization = 252
	}
	return available(sampleStd(returns)*math.Sqrt(float64(annualization))*100, len(returns))
}

func DownsideVolatility(returns []float64, annualization int) RiskMetric {
	if len(returns) < 2 {
		return unavailable("日收益样本不足 2 个", len(returns))
	}
	if annualization <= 0 {
		annualization = 252
	}
	var sum float64
	for _, r := range returns {
		if r < 0 {
			sum += r * r
		}
	}
	return available(math.Sqrt(sum/float64(len(returns)))*math.Sqrt(float64(annualization))*100, len(returns))
}

func SharpeRatio(returns []float64, annualization int, riskFreeRate float64) RiskMetric {
	if len(returns) < 2 {
		return unavailable("日收益样本不足 2 个", len(returns))
	}
	if annualization <= 0 {
		annualization = 252
	}
	std := sampleStd(returns)
	if std <= 0 || math.IsNaN(std) {
		return unavailable("收益波动为零，Sharpe 不可定义", len(returns))
	}
	dailyRF := riskFreeRate / float64(annualization)
	return available((mean(returns)-dailyRF)/std*math.Sqrt(float64(annualization)), len(returns))
}

func SortinoRatio(returns []float64, annualization int, riskFreeRate float64) RiskMetric {
	if len(returns) < 2 {
		return unavailable("日收益样本不足 2 个", len(returns))
	}
	if annualization <= 0 {
		annualization = 252
	}
	dailyRF := riskFreeRate / float64(annualization)
	var downside float64
	for _, r := range returns {
		d := r - dailyRF
		if d < 0 {
			downside += d * d
		}
	}
	dev := math.Sqrt(downside / float64(len(returns)))
	if dev <= 0 {
		return unavailable("没有下行波动，Sortino 不可定义", len(returns))
	}
	return available((mean(returns)-dailyRF)/dev*math.Sqrt(float64(annualization)), len(returns))
}

func MaxDrawdown(points []EquityPoint) (DrawdownResult, []EquityPoint) {
	out := append([]EquityPoint(nil), points...)
	peak, maxDD, peakDate, troughDate := 0.0, 0.0, "", ""
	activePeakDate, recovery := "", ""
	peakAtMax := 0.0
	segment, maxSegment := 0, -1
	previous := -1
	nav := 1.0
	intervals := 0
	for i := range out {
		if out[i].Partial || out[i].Assets <= 0 {
			previous = -1
			peak, activePeakDate = 0, ""
			segment++
			continue
		}
		if previous < 0 {
			nav, peak, activePeakDate = 1, 1, out[i].TradeDate
			dd := 0.0
			out[i].DrawdownPct = &dd
			previous = i
			continue
		}
		r := (out[i].Assets-out[i].CashFlow)/out[previous].Assets - 1
		if r <= -1 || math.IsNaN(r) || math.IsInf(r, 0) {
			previous = i
			nav, peak, activePeakDate = 1, 1, out[i].TradeDate
			segment++
			dd := 0.0
			out[i].DrawdownPct = &dd
			continue
		}
		nav *= 1 + r
		intervals++
		if nav > peak {
			peak, activePeakDate = nav, out[i].TradeDate
		}
		dd := (nav/peak - 1) * 100
		out[i].DrawdownPct = &dd
		if dd < maxDD {
			maxDD, peakDate, troughDate = dd, activePeakDate, out[i].TradeDate
			peakAtMax, maxSegment, recovery = peak, segment, ""
		} else if recovery == "" && troughDate != "" && segment == maxSegment && out[i].TradeDate > troughDate && nav >= peakAtMax {
			recovery = out[i].TradeDate
		}
		previous = i
	}
	if intervals == 0 {
		return DrawdownResult{Metric: unavailable("相邻完整资产快照不足 2 个", 0)}, out
	}
	return DrawdownResult{Metric: available(maxDD, intervals), PeakDate: peakDate, TroughDate: troughDate, RecoveryDate: recovery}, out
}

// BetaAlpha 对已经按共同交易日对齐的收益序列计算 beta 与年化 Jensen alpha。
func BetaAlpha(portfolio, benchmark []float64, annualization int, riskFreeRate float64) (RiskMetric, RiskMetric) {
	if len(portfolio) != len(benchmark) || len(portfolio) < 2 {
		n := len(portfolio)
		if len(benchmark) < n {
			n = len(benchmark)
		}
		return unavailable("组合与基准共同收益样本不足 2 个", n), unavailable("组合与基准共同收益样本不足 2 个", n)
	}
	if annualization <= 0 {
		annualization = 252
	}
	mp, mb := mean(portfolio), mean(benchmark)
	var cov, variance float64
	for i := range portfolio {
		dp, db := portfolio[i]-mp, benchmark[i]-mb
		cov += dp * db
		variance += db * db
	}
	if variance <= 0 {
		return unavailable("基准收益方差为零，Beta 不可定义", len(portfolio)), unavailable("基准收益方差为零，Alpha 不可定义", len(portfolio))
	}
	beta := cov / variance
	dailyRF := riskFreeRate / float64(annualization)
	alpha := (mp - (dailyRF + beta*(mb-dailyRF))) * float64(annualization) * 100
	return available(beta, len(portfolio)), available(alpha, len(portfolio))
}

func alignReturnsByDate(portfolio map[string]float64, benchmark map[string]float64) ([]float64, []float64, []string) {
	dates := make([]string, 0)
	for d := range portfolio {
		if _, ok := benchmark[d]; ok {
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)
	p, b := make([]float64, 0, len(dates)), make([]float64, 0, len(dates))
	for _, d := range dates {
		p = append(p, portfolio[d])
		b = append(b, benchmark[d])
	}
	return p, b, dates
}

type CorrelationCell struct {
	Status      string  `json:"status"`
	Value       float64 `json:"value,omitempty"`
	SampleCount int     `json:"sample_count"`
	Reason      string  `json:"reason,omitempty"`
}

type CorrelationMatrix struct {
	Symbols     []string            `json:"symbols"`
	Cells       [][]CorrelationCell `json:"cells"`
	WindowDays  int                 `json:"window_days"`
	AsOf        string              `json:"as_of"`
	DataVersion string              `json:"data_version"`
}

type RiskContributionItem struct {
	Symbol                 string  `json:"symbol"`
	WeightPct              float64 `json:"weight_pct"`
	MarginalVolatilityPct  float64 `json:"marginal_volatility_pct,omitempty"`
	ComponentVolatilityPct float64 `json:"component_volatility_pct,omitempty"`
	RiskContributionPct    float64 `json:"risk_contribution_pct,omitempty"`
	Status                 string  `json:"status"`
	Reason                 string  `json:"reason,omitempty"`
}

type RiskContributionResult struct {
	PredictedVolatility RiskMetric             `json:"predicted_volatility_pct"`
	Items               []RiskContributionItem `json:"items"`
	SampleCount         int                    `json:"sample_count"`
	WindowDays          int                    `json:"window_days"`
	AsOf                string                 `json:"as_of"`
	DataVersion         string                 `json:"data_version"`
}

// ComputeRiskContributions 用共同交易日收益协方差计算组合预测波动及成分风险贡献。
// weights 使用小数权重；不补交易日，也不对缺失标的重新归一化。
func ComputeRiskContributions(series map[string]map[string]float64, weights map[string]float64, annualization, window int, asOf string) RiskContributionResult {
	symbols := make([]string, 0, len(weights))
	for symbol, weight := range weights {
		if weight > 0 {
			symbols = append(symbols, symbol)
		}
	}
	sort.Strings(symbols)
	out := RiskContributionResult{Items: make([]RiskContributionItem, 0, len(symbols)), WindowDays: window, AsOf: asOf, DataVersion: "daily-bars-covariance-v1"}
	if annualization <= 0 {
		annualization = 252
	}
	for _, symbol := range symbols {
		out.Items = append(out.Items, RiskContributionItem{Symbol: symbol, WeightPct: weights[symbol] * 100, Status: RiskStatusUnavailable})
	}
	if len(symbols) == 0 {
		out.PredictedVolatility = unavailable("没有可计算风险贡献的持仓权重", 0)
		return out
	}
	dates := make([]string, 0)
	for date := range series[symbols[0]] {
		common := true
		for _, symbol := range symbols[1:] {
			if _, ok := series[symbol][date]; !ok {
				common = false
				break
			}
		}
		if common {
			dates = append(dates, date)
		}
	}
	sort.Strings(dates)
	out.SampleCount = len(dates)
	if len(dates) < 2 {
		reason := "全部持仓共同交易日收益样本不足 2 个"
		out.PredictedVolatility = unavailable(reason, len(dates))
		for i := range out.Items {
			out.Items[i].Reason = reason
		}
		return out
	}
	n := len(symbols)
	means := make([]float64, n)
	for i, symbol := range symbols {
		values := make([]float64, 0, len(dates))
		for _, date := range dates {
			values = append(values, series[symbol][date])
		}
		means[i] = mean(values)
	}
	covariance := make([][]float64, n)
	for i := range covariance {
		covariance[i] = make([]float64, n)
		for j := range covariance[i] {
			var sum float64
			for _, date := range dates {
				sum += (series[symbols[i]][date] - means[i]) * (series[symbols[j]][date] - means[j])
			}
			covariance[i][j] = sum / float64(len(dates)-1)
		}
	}
	covarianceWeight := make([]float64, n)
	variance := 0.0
	for i := range symbols {
		for j, symbol := range symbols {
			covarianceWeight[i] += covariance[i][j] * weights[symbol]
		}
		variance += weights[symbols[i]] * covarianceWeight[i]
	}
	if variance <= 0 || math.IsNaN(variance) || math.IsInf(variance, 0) {
		reason := "组合收益方差为零，风险贡献不可定义"
		out.PredictedVolatility = unavailable(reason, len(dates))
		for i := range out.Items {
			out.Items[i].Reason = reason
		}
		return out
	}
	dailyVolatility := math.Sqrt(variance)
	annualScale := math.Sqrt(float64(annualization)) * 100
	out.PredictedVolatility = available(dailyVolatility*annualScale, len(dates))
	for i, symbol := range symbols {
		marginal := covarianceWeight[i] / dailyVolatility
		component := weights[symbol] * marginal
		out.Items[i].MarginalVolatilityPct = marginal * annualScale
		out.Items[i].ComponentVolatilityPct = component * annualScale
		out.Items[i].RiskContributionPct = component / dailyVolatility * 100
		out.Items[i].Status = RiskStatusAvailable
	}
	return out
}

func CorrelationFromReturns(series map[string]map[string]float64, window int, asOf string) CorrelationMatrix {
	symbols := make([]string, 0, len(series))
	for s := range series {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	out := CorrelationMatrix{Symbols: symbols, WindowDays: window, AsOf: asOf, DataVersion: "daily-bars-v1", Cells: make([][]CorrelationCell, len(symbols))}
	for i, a := range symbols {
		out.Cells[i] = make([]CorrelationCell, len(symbols))
		for j, b := range symbols {
			x, y, _ := alignReturnsByDate(series[a], series[b])
			if len(x) < 2 {
				out.Cells[i][j] = CorrelationCell{Status: RiskStatusUnavailable, SampleCount: len(x), Reason: "共同交易日收益样本不足 2 个"}
				continue
			}
			sx, sy := sampleStd(x), sampleStd(y)
			if sx <= 0 || sy <= 0 {
				out.Cells[i][j] = CorrelationCell{Status: RiskStatusUnavailable, SampleCount: len(x), Reason: "收益方差为零"}
				continue
			}
			mx, my := mean(x), mean(y)
			var cov float64
			for k := range x {
				cov += (x[k] - mx) * (y[k] - my)
			}
			corr := cov / float64(len(x)-1) / sx / sy
			out.Cells[i][j] = CorrelationCell{Status: RiskStatusAvailable, Value: corr, SampleCount: len(x)}
		}
	}
	return out
}

type StressHolding struct {
	Symbol         string  `json:"symbol"`
	Name           string  `json:"name"`
	Industry       string  `json:"industry,omitempty"`
	Value          float64 `json:"value"`
	Quantity       float64 `json:"quantity"`
	Price          float64 `json:"price"`
	PlanStopLoss   float64 `json:"plan_stop_loss,omitempty"`
	Known          bool    `json:"known"`
	ValuationKnown bool    `json:"valuation_known"`
}
type StressScenario struct {
	Type     string  `json:"type"`
	ShockPct float64 `json:"shock_pct"`
	Symbol   string  `json:"symbol,omitempty"`
	Industry string  `json:"industry,omitempty"`
}
type StressContribution struct {
	Symbol     string  `json:"symbol"`
	Name       string  `json:"name"`
	LossAmount float64 `json:"loss_amount"`
	LossPct    float64 `json:"loss_pct"`
}
type StressResult struct {
	Scenario            StressScenario       `json:"scenario"`
	EstimatedLossAmount float64              `json:"estimated_loss_amount"`
	EstimatedLossPct    float64              `json:"estimated_loss_pct"`
	Contributions       []StressContribution `json:"contributions"`
	Unknown             []string             `json:"unknown"`
	BaseValue           float64              `json:"base_value"`
	GeneratedAt         string               `json:"generated_at"`
	ReadOnly            bool                 `json:"read_only"`
}

func ComputeStress(holdings []StressHolding, scenario StressScenario, generatedAt time.Time) StressResult {
	out := StressResult{Scenario: scenario, Contributions: []StressContribution{}, Unknown: []string{}, GeneratedAt: generatedAt.UTC().Format(time.RFC3339), ReadOnly: true}
	for _, h := range holdings {
		if h.Known && h.Value > 0 {
			out.BaseValue += h.Value
		}
	}
	for _, h := range holdings {
		if !h.Known || h.Value <= 0 {
			out.Unknown = append(out.Unknown, h.Symbol+": 缺少当前有效价格")
			continue
		}
		if !h.ValuationKnown {
			out.Unknown = append(out.Unknown, h.Symbol+": 缺少估值数据（不影响本场景价格冲击计算）")
		}
		shock, applies := 0.0, false
		switch scenario.Type {
		case "market":
			shock, applies = scenario.ShockPct, true
		case "industry":
			if h.Industry == "" {
				out.Unknown = append(out.Unknown, h.Symbol+": 行业未知")
			} else if h.Industry == scenario.Industry {
				shock, applies = scenario.ShockPct, true
			}
		case "symbol":
			if h.Symbol == scenario.Symbol {
				shock, applies = scenario.ShockPct, true
			}
		case "plan_stop_loss":
			if h.PlanStopLoss > 0 && h.Price > 0 {
				shock = (h.PlanStopLoss/h.Price - 1) * 100
				if shock > 0 {
					shock = 0
				}
				applies = true
			} else {
				out.Unknown = append(out.Unknown, h.Symbol+": 缺少计划止损价")
			}
		}
		if !applies {
			continue
		}
		loss := h.Value * shock / 100
		out.EstimatedLossAmount += loss
		out.Contributions = append(out.Contributions, StressContribution{Symbol: h.Symbol, Name: h.Name, LossAmount: round2(loss), LossPct: shock})
	}
	out.BaseValue = round2(out.BaseValue)
	out.EstimatedLossAmount = round2(out.EstimatedLossAmount)
	if out.BaseValue > 0 {
		out.EstimatedLossPct = round2(out.EstimatedLossAmount / out.BaseValue * 100)
	}
	sort.Slice(out.Contributions, func(i, j int) bool {
		return math.Abs(out.Contributions[i].LossAmount) > math.Abs(out.Contributions[j].LossAmount)
	})
	return out
}

type RebalanceHolding struct {
	Symbol          string
	Name            string
	Market          string
	Industry        string
	Value           float64
	Quantity        float64
	Price           float64
	Fresh           bool
	FreshnessReason string
	Suspended       bool
	LimitUp         bool
}
type TargetAllocationItem struct {
	Type            string  `json:"type"`
	Key             string  `json:"key"`
	TargetWeightPct float64 `json:"target_weight_pct"`
	MinWeightPct    float64 `json:"min_weight_pct"`
	MaxWeightPct    float64 `json:"max_weight_pct"`
	Enabled         bool    `json:"enabled"`
}
type RebalanceDraftItem struct {
	Type             string  `json:"type"`
	Key              string  `json:"key"`
	Name             string  `json:"name"`
	CurrentWeightPct float64 `json:"current_weight_pct"`
	TargetWeightPct  float64 `json:"target_weight_pct"`
	MinWeightPct     float64 `json:"min_weight_pct"`
	MaxWeightPct     float64 `json:"max_weight_pct"`
	DeviationPct     float64 `json:"deviation_pct"`
	AmountChange     float64 `json:"amount_change"`
	QuantityChange   float64 `json:"quantity_change"`
	EstimatedFee     float64 `json:"estimated_fee"`
	EstimatedTax     float64 `json:"estimated_tax"`
	Status           string  `json:"status"`
	Reason           string  `json:"reason,omitempty"`
}

func BuildRebalanceDraft(holdings []RebalanceHolding, targets []TargetAllocationItem, totalAssets float64) []RebalanceDraftItem {
	bySymbol := map[string]RebalanceHolding{}
	byIndustry := map[string]float64{}
	for _, h := range holdings {
		old := bySymbol[h.Symbol]
		old.Value += h.Value
		old.Quantity += h.Quantity
		if old.Symbol == "" {
			old = h
		}
		bySymbol[h.Symbol] = old
		byIndustry[h.Industry] += h.Value
	}
	out := make([]RebalanceDraftItem, 0, len(targets))
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		row := RebalanceDraftItem{Type: t.Type, Key: t.Key, TargetWeightPct: t.TargetWeightPct,
			MinWeightPct: t.MinWeightPct, MaxWeightPct: t.MaxWeightPct, Status: RiskStatusAvailable}
		current := 0.0
		var h RebalanceHolding
		if t.Type == "symbol" {
			h = bySymbol[t.Key]
			current = h.Value
			row.Name = h.Name
		} else {
			current = byIndustry[t.Key]
			row.Name = t.Key
		}
		if totalAssets > 0 {
			row.CurrentWeightPct = round2(current / totalAssets * 100)
		}
		row.DeviationPct = round2(row.CurrentWeightPct - row.TargetWeightPct)
		hasBand := t.MinWeightPct > 0 || (t.MaxWeightPct > 0 && t.MaxWeightPct < 100)
		maxWeight := t.MaxWeightPct
		if maxWeight <= 0 {
			maxWeight = 100
		}
		if hasBand && row.CurrentWeightPct >= t.MinWeightPct && row.CurrentWeightPct <= maxWeight {
			row.Reason = "当前权重在目标区间内，无需调整"
			out = append(out, row)
			continue
		}
		row.AmountChange = round2(totalAssets*t.TargetWeightPct/100 - current)
		if t.Type == "industry" {
			row.Status = RiskStatusUnavailable
			row.Reason = "行业目标仅展示资金缺口，无法自动映射到具体股票"
			out = append(out, row)
			continue
		}
		if h.Symbol == "" && row.AmountChange < 0 {
			row.Status = RiskStatusUnavailable
			row.Reason = "当前没有可减持仓位"
		} else if h.Suspended {
			row.Status = RiskStatusUnavailable
			row.Reason = "标的停牌"
		} else if !h.Fresh || h.Price <= 0 {
			row.Status = RiskStatusUnavailable
			row.Reason = h.FreshnessReason
			if row.Reason == "" {
				row.Reason = "缺少 fresh 价格"
			}
		} else if row.AmountChange > 0 && h.LimitUp {
			row.Status = RiskStatusUnavailable
			row.Reason = "标的涨停，买入草案不可执行"
		} else {
			qty := row.AmountChange / h.Price
			if h.Market == "cn" {
				if qty > 0 {
					qty = math.Floor(qty/100) * 100
				} else {
					qty = math.Ceil(qty/100) * 100
				}
			}
			if qty < 0 && -qty > h.Quantity {
				qty = -h.Quantity
			}
			row.QuantityChange = round4(qty)
			amount := math.Abs(qty * h.Price)
			side := model.PaperSideBuy
			if qty < 0 {
				side = model.PaperSideSell
			}
			row.EstimatedFee, row.EstimatedTax = tradeFee(h.Market, side, h.Symbol, amount)
		}
		out = append(out, row)
	}
	return out
}
