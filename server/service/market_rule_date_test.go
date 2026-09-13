package service

import (
	"math"
	"testing"

	"quantvista/datasource"
)

func TestAStockRiskWarningLimitChangesOnEffectiveDate(t *testing.T) {
	for _, symbol := range []string{"600100", "000100"} {
		for _, tc := range []struct {
			date string
			want float64
		}{
			{"2026-07-03", 5}, {"2026-07-05", 5}, {"2026-07-06", 10}, {"2026-09-11 15:00", 10}, {"", 5}, {"2026-99-99", 5},
		} {
			if got := limitUpPctForDate(symbol, "*ST测试", tc.date); got != tc.want {
				t.Fatalf("%s %s: want %v got %v", symbol, tc.date, tc.want, got)
			}
		}
		c := candidate{Symbol: symbol, Name: "ST测试", Price: 10.5, ChangePct: 5, QuoteAsOf: "2026-07-03 15:00"}
		if !isAtLimitUp(c) {
			t.Fatal("旧交易日 5% 应仍是封板近似")
		}
		c.QuoteAsOf = "2026-07-06 15:00"
		if isAtLimitUp(c) {
			t.Fatal("新交易日 5% 不能再误判封板")
		}
		c.LimitUp = 10.5
		if !isAtLimitUp(c) {
			t.Fatal("实际涨停报价仍优先于名称/日期近似")
		}
	}
	for _, tc := range []struct {
		symbol string
		want   float64
	}{{"300100", 20}, {"688100", 20}, {"920100", 30}} {
		for _, date := range []string{"2026-07-03", "2026-07-06"} {
			if got := limitUpPctForDate(tc.symbol, "ST测试", date); got != tc.want {
				t.Fatalf("其他板块被主板规则覆盖：%s %v", tc.symbol, got)
			}
		}
	}
}

func TestAStockFactorsCountLimitsByEachTradingDate(t *testing.T) {
	bars := genTrendBars(65, 10, 0)
	for i := range bars {
		setupTestCandle(bars, i, 10, 10.1, 9.9, 10, 1000000)
	}
	for i, date := range []string{"2026-07-02", "2026-07-03", "2026-07-06"} {
		bars[len(bars)-3+i].TradeDate = date
	}
	setupTestCandle(bars, 63, 10, 10.5, 10, 10.5, 1000000)
	setupTestCandle(bars, 64, 10.5, 11.025, 10.5, 11.025, 1000000)
	row := computeWideRowOpts("000100", wideStockMeta{Name: "历史简称", ST: true}, bars, false)
	if row[factorIndex["limit_up_yest"]] != 1 || row[factorIndex["limit_up_today"]] != 0 || row[factorIndex["limit_ups_5d"]] != 1 || math.Abs(row[factorIndex["day_limit_ratio"]]-.5) > 1e-8 {
		t.Fatal("同一窗口跨政策日：旧日 5% 算涨停，新日 5% 仅达到幅度一半；ST 身份优先保留")
	}
}

func TestAStockSimulationUsesEntryAndExitDatesSeparately(t *testing.T) {
	bar := func(date string, open, high, low, close float64) datasource.Bar {
		return datasource.Bar{TradeDate: date, Open: open, High: high, Low: low, Close: close, Volume: 100000, Source: "eastmoney"}
	}
	for _, symbol := range []string{"000100", "600100"} {
		old := []datasource.Bar{bar("2026-07-02", 10, 10, 10, 10), bar("2026-07-03", 10.5, 10.6, 10.4, 10.5)}
		if _, _, _, skip := simEntry(old, 0, symbol, "ST测试", 10000, "2026-07-03"); skip != btSkipLimitUp {
			t.Fatalf("旧日开盘涨停应不可买：%s", skip)
		}
		old[0].TradeDate, old[1].TradeDate = "2026-07-03", "2026-07-06"
		if _, _, _, skip := simEntry(old, 0, symbol, "ST测试", 10000, "2026-07-06"); skip != "" {
			t.Fatalf("新日开盘 5%% 不再是涨停：%s", skip)
		}
		// 买入日在新规之前，卖出日在新规之后：不能把买入日的 5% 冻结成整段规则。
		bars := []datasource.Bar{bar("2026-07-02", 10, 10, 10, 10), bar("2026-07-03", 10, 10.1, 9.9, 10), bar("2026-07-06", 9.5, 9.5, 9.5, 9.5), bar("2026-07-07", 9.6, 9.7, 9.5, 9.6)}
		out := simulateHold(bars, 0, symbol, "ST测试", 1, 10000, "2026-07-03", "2026-07-06", "2026-07-07")
		if out.SellDate != "2026-07-06" || out.Deferred != 0 {
			t.Fatalf("新规后跌 5%% 不能误判一字跌停顺延：%+v", out)
		}
		label := simulateLabelHold(bars, 0, symbol, "ST测试", 2, 10000, 12, 9.8, "2026-07-03", "2026-07-07", "2026-07-07")
		if label.SellDate != "2026-07-06" || label.SellPrice != 9.5 || !label.HitStopLoss || label.Deferred != 0 {
			t.Fatalf("计划标签也须逐日检查并承担开盘缺口：%+v", label)
		}
	}
}
