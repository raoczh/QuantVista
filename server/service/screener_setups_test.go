package service

import (
	"math"
	"testing"

	"quantvista/datasource"
	"quantvista/model"
)

func setupTestCandle(bars []datasource.Bar, i int, open, high, low, close float64, volume int64) {
	bars[i].Open, bars[i].High, bars[i].Low, bars[i].Close, bars[i].Volume = open, high, low, close, volume
	bars[i].Amount = close * float64(volume)
}

func TestCommonSetupsRequireSequentialEvidence(t *testing.T) {
	bars := genTrendBars(100, 10, 0)
	for i := range bars {
		setupTestCandle(bars, i, 10, 10.2, 9.8, 10, 1000000)
	}
	for _, key := range []string{"ma20_cross_ma60", "donchian55_break", "boll_squeeze_break", "breakout_retest", "kdj_low_cross", "rsi_rising", "boll_lower_reclaim"} {
		if commonSetupFactors(bars)[key] != 0 {
			t.Fatalf("横盘不应凭空出现%s", key)
		}
	}
	t.Run("均线金叉需保持", func(t *testing.T) {
		v := append([]datasource.Bar{}, bars...)
		setupTestCandle(v, 97, 10, 10.7, 9.9, 10.6, 1000000)
		setupTestCandle(v, 98, 10.5, 10.7, 10.3, 10.5, 1000000)
		setupTestCandle(v, 99, 10.4, 10.7, 10.3, 10.6, 1000000)
		if commonSetupFactors(v)["ma20_cross_ma60"] != 1 {
			t.Fatal("缺少真实均线金叉")
		}
		setupTestCandle(v, 99, 9, 9.1, 7.9, 8, 1000000)
		if commonSetupFactors(v)["ma20_cross_ma60"] != 0 {
			t.Fatal("已再次死叉不能继续命中")
		}
	})
	t.Run("通道突破不含当日", func(t *testing.T) {
		v := append([]datasource.Bar{}, bars...)
		setupTestCandle(v, 99, 10, 10.5, 9.9, 10.3, 1500000)
		if commonSetupFactors(v)["donchian55_break"] != 1 {
			t.Fatal("收盘已经超过此前高点")
		}
		setupTestCandle(v, 99, 10, 10.5, 9.9, 10.2, 1500000)
		if commonSetupFactors(v)["donchian55_break"] != 0 {
			t.Fatal("触及前高不等于突破")
		}
	})
	t.Run("先收口后突破", func(t *testing.T) {
		v := append([]datasource.Bar{}, bars...)
		for i := 0; i < 80; i++ {
			c := 9.5 + float64(i%2)
			setupTestCandle(v, i, c, c+.1, c-.1, c, 1000000)
		}
		setupTestCandle(v, 99, 10, 11.1, 9.95, 11, 2000000)
		if commonSetupFactors(v)["boll_squeeze_break"] != 1 {
			t.Fatal("收口突破应被识别")
		}
		setupTestCandle(v, 99, 10, 10.2, 9.8, 10, 2000000)
		if commonSetupFactors(v)["boll_squeeze_break"] != 0 {
			t.Fatal("仅收口不能当突破")
		}
	})
	t.Run("突破必须在回踩前", func(t *testing.T) {
		v := append([]datasource.Bar{}, bars...)
		setupTestCandle(v, 94, 10, 10.9, 9.99, 10.8, 2000000)
		setupTestCandle(v, 95, 10.8, 11, 10.65, 10.9, 1200000)
		setupTestCandle(v, 96, 10.8, 10.85, 10.35, 10.6, 900000)
		setupTestCandle(v, 97, 10.6, 10.65, 10.15, 10.4, 800000)
		setupTestCandle(v, 98, 10.4, 10.5, 10.05, 10.3, 700000)
		setupTestCandle(v, 99, 10.22, 10.45, 10.12, 10.4, 800000)
		if commonSetupFactors(v)["breakout_retest"] != 1 {
			t.Fatal("缺少放量突破后的平台回踩确认")
		}
		setupTestCandle(v, 96, 10.2, 10.3, 9.85, 9.9, 900000)
		if commonSetupFactors(v)["breakout_retest"] != 0 {
			t.Fatal("中间失守的平台不能冒充健康回踩")
		}
	})
	t.Run("KDJ低位金叉", func(t *testing.T) {
		v := genTrendBars(60, 20, -.1)
		for i := range v {
			c := 20 - float64(i)*.1
			setupTestCandle(v, i, c+.02, c+.1, c-.1, c, 1000000)
		}
		if commonSetupFactors(v)["kdj_low_cross"] != 0 {
			t.Fatal("持续下跌未金叉")
		}
		c := v[58].Close
		setupTestCandle(v, 59, c+.01, c+.5, c-.02, c+.4, 1000000)
		if commonSetupFactors(v)["kdj_low_cross"] != 1 {
			t.Fatal("低位上穿应识别")
		}
	})
	for key, need := range map[string]int{"ma20_cross_ma60": 63, "donchian55_break": 56, "boll_squeeze_break": 80, "breakout_retest": 31, "kdj_low_cross": 30, "rsi_prior5_min": 20} {
		if _, ok := commonSetupFactors(bars[:need-1])[key]; ok {
			t.Fatalf("%s 窗口不足应未知", key)
		}
	}
}

func TestAStockFactorWindowsAndCatalog(t *testing.T) {
	bars := genTrendBars(21, 10, 0)
	for i := range bars {
		setupTestCandle(bars, i, 10, 10.2, 9.8, 10, 1000000)
	}
	setupTestCandle(bars, 0, 5, 5.1, 4.9, 5, 1000000)
	want := 22.36 // 20个收益：100% 与19个0%；样本标准差 sqrt(500)。
	row := computeWideRowOpts("600100", wideStockMeta{}, bars, false)
	if row[factorIndex["volatility_20"]] != want || computeCandFactors(10, bars).Volatility20 != want {
		t.Fatal("20日波动率必须含边界收益")
	}
	flat := genTrendBars(80, 10, 0)
	for i := range flat {
		setupTestCandle(flat, i, 10, 10.2, 9.8, 10, 1000000)
	}
	row = computeWideRowOpts("600100", wideStockMeta{}, flat, false)
	if row[factorIndex["high_20d"]] != 0 || !math.IsNaN(row[factorIndex["high_250d"]]) {
		t.Fatal("平价不是新高，短历史不能算年内新高")
	}
	if limitUpPctFor("300100", "ST回归") != 20 || limitUpPctFor("688100", "*ST回归") != 20 || limitUpPctFor("920100", "") != 30 {
		t.Fatal("板块涨停幅度不能被ST名称覆盖")
	}
	if cnMinimumBuyQuantity("688100") != 200 || affordableBoardLotQuantity("cn", "688100", 10, 1100, 100) != 0 {
		t.Fatal("科创板不能生成100股新买入计划")
	}
	for _, b := range builtinScreens {
		if _, err := validateCondTree(&b.Tree, 0); err != nil {
			t.Fatalf("%s: %v", b.Key, err)
		}
		short := builtinScreenStrategyTemplate(model.RecTypeShortTerm, b)
		long := builtinScreenStrategyTemplate(model.RecTypeLongTerm, b)
		if short.baseKey != long.baseKey || short.Intent != long.Intent {
			t.Fatalf("%s 不能因持有周期改变选股意图", b.Key)
		}
	}
	// RSI区间值相同，但没有此前超卖事实的标的不应命中“超卖回升”。
	b, _ := builtinScreenByKey("rsi-oversold-up")
	row[factorIndex["rsi_14"]], row[factorIndex["chg_pct"]], row[factorIndex["rsi_rising"]] = 35, 1, 1
	row[factorIndex["rsi_prior5_min"]] = 33
	if evalCondRow(singleRowFactorTable(row), &b.Tree, 0) {
		t.Fatal("没有超卖经历不能声称超卖回升")
	}
	row[factorIndex["rsi_prior5_min"]] = 25
	if !evalCondRow(singleRowFactorTable(row), &b.Tree, 0) {
		t.Fatal("超卖后回升的完整条件应该命中")
	}
}
