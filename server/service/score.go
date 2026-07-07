package service

import (
	"context"
	"errors"
	"math"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// ScoreService 个股综合评分：基于真实行情与技术指标（趋势/动量/位置/量能/风险）加权打分。
// 纯技术面量化，非投资建议；快照按 symbol+market+交易日落库。
type ScoreService struct {
	market *MarketService
}

func NewScoreService(market *MarketService) *ScoreService {
	return &ScoreService{market: market}
}

const scoreBarLimit = 60

// 各维度权重（合计 1.0）。
const (
	wTrend    = 0.30
	wMomentum = 0.25
	wPosition = 0.15
	wVolume   = 0.15
	wRisk     = 0.15
)

// ScoreResult 评分结果（纯计算产物）。
type ScoreResult struct {
	Total       float64 `json:"total"`
	Trend       float64 `json:"trend"`
	Momentum    float64 `json:"momentum"`
	Position    float64 `json:"position"`
	Volume      float64 `json:"volume"`
	Risk        float64 `json:"risk"`
	Label       string  `json:"label"`
	BarCount    int     `json:"bar_count"`
	DataLimited bool    `json:"data_limited"` // 日线不足 20 根，维度精度受限
}

// clamp01 把值钳制到 [0,100]。
func clamp0100(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// scoreLabel 综合分 → 强弱标签。
func scoreLabel(total float64) string {
	switch {
	case total >= 75:
		return "强"
	case total >= 60:
		return "偏强"
	case total >= 45:
		return "中性"
	case total >= 30:
		return "偏弱"
	default:
		return "弱"
	}
}

// computeScore 纯函数：由现价与升序日线算五维评分与综合分。数据不足时对应维度取中性、不臆测。
func computeScore(price float64, bars []datasource.Bar) ScoreResult {
	r := ScoreResult{BarCount: len(bars)}
	if price <= 0 || len(bars) == 0 {
		r.Trend, r.Momentum, r.Position, r.Volume, r.Risk = 50, 50, 50, 50, 50
		r.Total = 50
		r.Label = scoreLabel(50)
		r.DataLimited = true
		return r
	}
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	r.DataLimited = len(bars) < 20

	r.Trend = round2(trendScore(price, closes))
	r.Momentum = round2(momentumScore(closes))
	r.Position = round2(positionScore(price, bars))
	r.Volume = round2(volumeScore(bars))
	r.Risk = round2(riskScore(bars))

	total := wTrend*r.Trend + wMomentum*r.Momentum + wPosition*r.Position + wVolume*r.Volume + wRisk*r.Risk
	r.Total = round2(clamp0100(total))
	r.Label = scoreLabel(r.Total)
	return r
}

// trendScore 均线多空排列打分：5 个条件各 20 分；缺失的 MA 记中性 10 分。
func trendScore(price float64, closes []float64) float64 {
	ma5, ok5 := movingAverage(closes, 5)
	ma10, ok10 := movingAverage(closes, 10)
	ma20, ok20 := movingAverage(closes, 20)
	score := 0.0
	cond := func(avail, up bool) {
		if !avail {
			score += 10
		} else if up {
			score += 20
		}
	}
	cond(ok5, price > ma5)
	cond(ok10, price > ma10)
	cond(ok20, price > ma20)
	cond(ok5 && ok10, ma5 > ma10)
	cond(ok10 && ok20, ma10 > ma20)
	return clamp0100(score)
}

// momentumScore 近 5/20 日涨跌幅映射（50 为中性）与 RSI14 凹形分的加权合成。
// RSI 凹形逻辑（T1）：55~70 是健康强势区给满分，≥70 过热显著降分（追高保护）、
// ≤30 超卖动量弱——「越大越好」的线性映射会奖励过热，凹形才符合右侧交易纪律。
// 样本不足 rsiMinBars 时退回纯涨幅口径（不臆测）。
func momentumScore(closes []float64) float64 {
	m5 := clamp0100(50 + changeOverN(closes, 5)*3)
	m20 := clamp0100(50 + changeOverN(closes, 20)*2)
	chg := 0.5*m5 + 0.5*m20
	if len(closes) < rsiMinBars {
		return chg
	}
	rsi := rsiSeries(closes, 14)
	last := rsi[len(rsi)-1]
	if math.IsNaN(last) {
		return chg
	}
	return 0.6*chg + 0.4*rsiMomentumScore(last)
}

// rsiMomentumScore RSI → 动量分的凹形映射：
// ≤30 超卖 30 分；30~55 线性升至满分；55~70 满分平台；70~85 陡降至 40；>85 续降、下限 20。
func rsiMomentumScore(rsi float64) float64 {
	switch {
	case rsi <= 30:
		return 30
	case rsi < 55:
		return 30 + (rsi-30)/25*70
	case rsi <= 70:
		return 100
	case rsi <= 85:
		return 100 - (rsi-70)*4
	default:
		return math.Max(20, 40-(rsi-85)*2)
	}
}

// positionScore 现价在区间高低中的位置（越高越强，0-100）。
func positionScore(price float64, bars []datasource.Bar) float64 {
	hi, lo := bars[0].High, bars[0].Low
	for _, b := range bars {
		if b.High > hi {
			hi = b.High
		}
		if b.Low < lo && b.Low > 0 {
			lo = b.Low
		}
	}
	if hi <= lo {
		return 50
	}
	return clamp0100((price - lo) / (hi - lo) * 100)
}

// volumeScore 近 5 日均量相对近 20 日均量（放量偏强）。无成交量数据时中性 50。
func volumeScore(bars []datasource.Bar) float64 {
	avg := func(n int) float64 {
		if len(bars) < 1 {
			return 0
		}
		if n > len(bars) {
			n = len(bars)
		}
		var sum float64
		for _, b := range bars[len(bars)-n:] {
			sum += float64(b.Volume)
		}
		return sum / float64(n)
	}
	a5, a20 := avg(5), avg(20)
	if a20 <= 0 {
		return 50
	}
	return clamp0100(50 + (a5/a20-1)*50)
}

// riskScore 回撤反向分与 ATR/价百分比反向分的加权合成（T1 加入 ATR 维度：
// 回撤度量「已跌多少」，ATR 度量「日常波动多大」——高 ATR 意味着止损空间被迫放大）。
// ATR% ≤1% 满分，每 +1% 扣 25 分，≥5% 为 0；样本不足时退回纯回撤口径。
func riskScore(bars []datasource.Bar) float64 {
	n := 20
	if n > len(bars) {
		n = len(bars)
	}
	window := bars[len(bars)-n:]
	peak := window[0].High
	worst := 0.0 // 最负的 (low-peak)/peak
	for _, b := range window {
		if b.High > peak {
			peak = b.High
		}
		if peak > 0 {
			if dd := (b.Low - peak) / peak; dd < worst {
				worst = dd
			}
		}
	}
	ddPct := -worst * 100 // 正数
	ddScore := clamp0100(100 - ddPct*3)
	last := bars[len(bars)-1].Close
	if len(bars) < atrMinBars || last <= 0 {
		return ddScore
	}
	atr := atrSeries(bars, 14)
	atrPct := atr[len(atr)-1] / last * 100
	atrScore := clamp0100(100 - (atrPct-1)*25)
	return 0.6*ddScore + 0.4*atrScore
}

// ScoreView 评分 + 标的信息。
type ScoreView struct {
	Symbol string  `json:"symbol"`
	Market string  `json:"market"`
	Name   string  `json:"name"`
	Price  float64 `json:"price"`
	Date   string  `json:"trade_date"`
	ScoreResult
}

// Score 计算某只个股当日评分并落库快照。
func (s *ScoreService) Score(ctx context.Context, market, symbol string) (*ScoreView, error) {
	symbol, market, err := normalizeSymbolMarket(symbol, market)
	if err != nil {
		return nil, err
	}
	q, err := s.market.GetQuote(ctx, market, symbol)
	if err != nil {
		if errors.Is(err, datasource.ErrSymbolInvalid) {
			return nil, errors.New("无法识别的股票代码")
		}
		return nil, errors.New("行情数据暂不可用")
	}
	bars, _ := s.market.GetDailyBars(ctx, market, symbol, scoreBarLimit)
	res := computeScore(q.Price, bars)

	view := &ScoreView{
		Symbol: symbol, Market: market, Name: q.Name, Price: round2(q.Price),
		Date: q.DataTime.In(time.Local).Format("2006-01-02"), ScoreResult: res,
	}
	s.persist(view)
	return view, nil
}

func (s *ScoreService) persist(v *ScoreView) {
	if common.DB == nil || v.Date == "" {
		return
	}
	row := model.StockScore{
		Symbol: v.Symbol, Market: v.Market, TradeDate: v.Date,
		Total: v.Total, Trend: v.Trend, Momentum: v.Momentum, Position: v.Position,
		Volume: v.Volume, Risk: v.Risk, Label: v.Label, Price: v.Price, BarCount: v.BarCount,
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"total", "trend", "momentum", "position", "volume", "risk", "label", "price", "bar_count", "updated_at",
		}),
	}).Create(&row).Error; err != nil {
		common.SysWarn("评分快照落库失败 %s/%s: %v", v.Market, v.Symbol, err)
	}
}
