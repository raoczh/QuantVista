package service

import (
	"fmt"
	"strings"
	"time"

	"quantvista/datasource"
)

// 收盘趋势与盘中市场宽度分别注明时点。缺失只使对应证据缺席，市场状态继续影子观察。
func buildRecommendationMarketFacts(ov *Overview, bars []datasource.Bar, now time.Time, quoteDate, completedDate, state string) (*recMarketContext, RegimeResult) {
	mc := &recMarketContext{ObservedAt: now.Format(time.RFC3339), ExpectedQuoteDate: quoteDate}
	var breadth *datasource.Breadth
	mainNetYi, hasFlow := 0.0, false
	if ov != nil {
		for _, ix := range ov.Indices {
			at := ix.DataTime.In(time.Local)
			if !finiteRecNumber(ix.ChangePct) || ix.Price <= 0 || at.After(now.Add(time.Minute)) || quoteFreshness(at, now, state, quoteDate).Status != freshStatusFresh {
				continue
			}
			mc.Indices = append(mc.Indices, map[string]any{"name": ix.Name, "change_pct": round2(ix.ChangePct), "quote_as_of": at.Format("2006-01-02 15:04")})
			if len(mc.Indices) == 3 {
				break
			}
		}
		if b := ov.Breadth; b != nil && b.TradeDate == quoteDate && b.Advances >= 0 && b.Declines >= 0 && b.Advances+b.Declines > 0 && b.LimitUp >= 0 && b.LimitDown >= 0 {
			breadth = b
			mc.Breadth = map[string]any{"advances": b.Advances, "declines": b.Declines, "limit_up": b.LimitUp, "limit_down": b.LimitDown, "trade_date": b.TradeDate, "captured_at": b.DataTime.Format(time.RFC3339)}
		}
		if f := ov.FundFlow; f != nil && f.TradeDate == quoteDate && finiteRecNumber(f.MainNet) {
			mainNetYi = round2(f.MainNet / 1e8)
			hasFlow = true
			mc.MainNetYi = &mainNetYi
			mc.FlowDate = f.TradeDate
		}
	}
	if len(mc.Indices) == 0 {
		mc.Missing = append(mc.Missing, "缺少时点有效的主要指数报价")
	}
	if breadth == nil {
		mc.Missing = append(mc.Missing, "市场涨跌家数缺失或业务日期不符")
	}
	if !hasFlow {
		mc.Missing = append(mc.Missing, "市场资金流缺失或业务日期不符")
	}
	completed, note := completedBenchmarkBars(bars, now, completedDate)
	if note != "" {
		mc.Missing = append(mc.Missing, note)
		completed = nil
	}
	if len(completed) > 0 {
		mc.BenchAsOf = completed[len(completed)-1].TradeDate
		closes := make([]float64, len(completed))
		for i, b := range completed {
			closes[i] = b.Close
		}
		last := closes[len(closes)-1]
		var parts []string
		for _, window := range []int{60, 200} {
			if ma, ok := movingAverage(closes, window); ok {
				position := "上方"
				if last < ma {
					position = "下方"
				}
				parts = append(parts, fmt.Sprintf("上证收于MA%d%s", window, position))
			} else {
				mc.Missing = append(mc.Missing, fmt.Sprintf("基准MA%d样本不足", window))
			}
		}
		mc.BenchTrend = strings.Join(parts, "，")
	}
	regime := computeRegime(completed, breadth, mainNetYi, hasFlow, defaultRegimeParams())
	regime.BenchmarkAsOf, regime.BreadthAsOf, regime.FundFlowAsOf = mc.BenchAsOf, "", mc.FlowDate
	if breadth != nil {
		regime.BreadthAsOf = breadth.TradeDate
	}
	regime.Missing = append([]string(nil), mc.Missing...)
	return mc, regime
}
