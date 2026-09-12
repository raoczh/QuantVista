package service

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"quantvista/datasource"
)

const recommendationTimeVersion = "rt1"

type recReturnAnchor struct {
	TradeDate string  `json:"trade_date"`
	Close     float64 `json:"close"`
}

// recTimeFacts 分别冻结完整日线信号与报价时点；现价收益的分母也保存，便于核验。
// CurrentReturns 不改变历史日线的涨幅、形态、成交量和指标。
type recTimeFacts struct {
	Version        string                     `json:"version"`
	SignalDate     string                     `json:"signal_date"`
	SignalClose    float64                    `json:"signal_close"`
	QuoteAsOf      string                     `json:"quote_as_of"`
	ReturnAnchors  map[string]recReturnAnchor `json:"return_anchors,omitempty"`
	CurrentReturns map[string]float64         `json:"current_returns,omitempty"`
}

// recommendationCompletedBars 只使用在报价时点已经收盘的真实日线。
// 不合成盘中 K 线；不将半日成交量与历史完整日成交量比较。
func recommendationCompletedBars(bars []datasource.Bar, c candidate) ([]datasource.Bar, string) {
	if len(bars) == 0 {
		return nil, "日线数据获取失败，未参与量化评分"
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", c.QuoteAsOf, time.Local)
	if err != nil {
		return nil, "行情时点缺失或无效，无法核对日线与现价"
	}
	date := at.Format("2006-01-02")
	beforeClose := at.Hour()*60+at.Minute() < 15*60
	n := len(bars)
	for n > 0 && (bars[n-1].TradeDate > date || (beforeClose && bars[n-1].TradeDate == date)) {
		n--
	}
	if n == 0 {
		return nil, "缺少在报价时点已完成的日线，不能将盘中行情当成收盘信号"
	}
	completed := bars[:n]
	if err := validateAdjustedBars(c.Market, completed); err != nil {
		return nil, "日线价格口径或序列无效，未参与评分"
	}
	for i, b := range completed {
		if _, err := time.Parse("2006-01-02", b.TradeDate); err != nil || (i > 0 && b.TradeDate <= completed[i-1].TradeDate) {
			return nil, "日线日期缺失或顺序异常，无法确定信号窗口"
		}
		for _, v := range []float64{b.Open, b.High, b.Low, b.Close} {
			if v <= 0 || !finiteRecNumber(v) {
				return nil, "日线价格缺失或无效，无法计算信号"
			}
		}
		if b.High < math.Max(b.Open, b.Close) || b.Low > math.Min(b.Open, b.Close) || b.High < b.Low {
			return nil, "日线高低价与开收盘价不一致，无法计算信号"
		}
	}
	expected := date
	if beforeClose {
		expected = prevOpenTradeDate(date)
	}
	if last := completed[n-1].TradeDate; last < expected {
		return nil, fmt.Sprintf("日线信号仅截至 %s，报价时点应有 %s 的完整日线", last, expected)
	}
	return completed, ""
}

func buildRecTimeFacts(c candidate, bars []datasource.Bar) *recTimeFacts {
	if len(bars) == 0 {
		return nil
	}
	last := bars[len(bars)-1]
	out := &recTimeFacts{
		Version: recommendationTimeVersion, SignalDate: last.TradeDate, SignalClose: last.Close,
		QuoteAsOf: c.QuoteAsOf, ReturnAnchors: map[string]recReturnAnchor{}, CurrentReturns: map[string]float64{},
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", c.QuoteAsOf, time.Local)
	if err != nil {
		return out
	}
	quoteDate := at.Format("2006-01-02")
	if quoteDate < last.TradeDate {
		return out
	}
	for _, days := range []int{5, 20, 60} {
		idx := len(bars) - 1 - days
		// 今日尚无完整日线时，以现价为第 5/20/60 个收益终点，而非多算一天。
		if quoteDate > last.TradeDate {
			idx++
		}
		if idx < 0 {
			continue
		}
		anchor := bars[idx]
		if anchor.Close <= 0 || !finiteRecNumber(anchor.Close) || !finiteRecNumber(c.Price) || c.Price <= 0 {
			continue
		}
		key := strconv.Itoa(days)
		out.ReturnAnchors[key] = recReturnAnchor{TradeDate: anchor.TradeDate, Close: anchor.Close}
		out.CurrentReturns[key] = round2((c.Price/anchor.Close - 1) * 100)
	}
	return out
}

func finiteRecNumber(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func currentReturnAt(c candidate, price float64, days int) (float64, bool) {
	if c.Timing == nil || price <= 0 || !finiteRecNumber(price) {
		return 0, false
	}
	anchor, ok := c.Timing.ReturnAnchors[strconv.Itoa(days)]
	if !ok || anchor.Close <= 0 || !finiteRecNumber(anchor.Close) {
		return 0, false
	}
	return round2((price/anchor.Close - 1) * 100), true
}

type recFinalCheck struct {
	QuoteAsOf    string                `json:"quote_as_of"`
	Price        float64               `json:"price"`
	Passed       bool                  `json:"passed"`
	Reason       string                `json:"reason,omitempty"`
	Strategy     *strategyCurrentCheck `json:"strategy,omitempty"`
	EntryQuality *recEntryQuality      `json:"entry_quality,omitempty"`
}
