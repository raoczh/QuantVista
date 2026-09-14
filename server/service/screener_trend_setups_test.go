package service

import (
	"math"
	"strings"
	"testing"

	"quantvista/datasource"
	"quantvista/model"
)

func TestDMIUsesCompleteWilderSeedsAndDirection(t *testing.T) {
	// period=2 的手算样本：前两个 TR=2、+DM=1；随后一次 -DM=1。
	// 首个 DX=100、次个 DX=0，所以首个 ADX=50；下一 DX=200/3。
	bars := []datasource.Bar{
		{High: 10, Low: 8, Close: 9}, {High: 11, Low: 9, Close: 10},
		{High: 12, Low: 10, Close: 11}, {High: 11, Low: 9, Close: 10},
		{High: 13, Low: 9, Close: 12}, {High: 14, Low: 8, Close: 10},
	}
	p, m, adx := directionalMovementSeries(bars, 2)
	if !math.IsNaN(p[1]) || !math.IsNaN(adx[2]) || p[2] != 50 || m[2] != 0 || adx[3] != 50 {
		t.Fatalf("完整种子或首个有效下标错误：%v %v %v", p, m, adx)
	}
	if math.Abs(adx[4]-175.0/3) > 1e-9 || math.Abs(adx[5]-62.5) > 1e-9 {
		t.Fatalf("Wilder递推或相同方向扩张的处理错误：%v", adx)
	}
	_, _, prefix := directionalMovementSeries(bars[:4], 2)
	if prefix[3] != adx[3] {
		t.Fatal("以后发生的价格不能改变当时的ADX")
	}
	flat := genTrendBars(150, 20, 0)
	for i := range flat {
		setupTestCandle(flat, i, 20, 20, 20, 20, 10000000)
	}
	if f := extendedSetupFactors(flat); f["adx_14"] != 0 || f["dmi_bull_cross"] != 0 || f["nr7_break"] != 0 {
		t.Fatalf("平盘不应出现强趋势或假收敛突破：%v", f)
	}
	falling := genTrendBars(150, 25, -.03)
	for i := range falling {
		c := 25 - float64(i)*.03
		setupTestCandle(falling, i, c, c+.2, c-.2, c, 10000000)
	}
	f := extendedSetupFactors(falling)
	if f["adx_14"] < 90 || f["dmi_pdi14"] >= f["dmi_mdi14"] || f["dmi_bull_cross"] != 0 {
		t.Fatalf("高ADX的下跌趋势不能当成多头：%v", f)
	}
}

func nr7TestBars() []datasource.Bar {
	bars := genTrendBars(100, 20, 0)
	for i := range bars {
		setupTestCandle(bars, i, 20, 20.6, 19.4, 20, 10000000)
	}
	setupTestCandle(bars, 98, 20, 20.1, 19.9, 20, 5000000)
	setupTestCandle(bars, 99, 20.05, 20.6, 20, 20.5, 16000000)
	return bars
}

func TestNR7NeedsPriorContractionAndCloseConfirmation(t *testing.T) {
	bars := nr7TestBars()
	b, _ := builtinScreenByKey("nr7-breakout")
	strat := builtinScreenStrategyTemplate(model.RecTypeShortTerm, b)
	hit := evaluateStrategyHit(&strat, "600100", wideStockMeta{}, bars)
	if hit == nil || !hit.Full {
		t.Fatalf("完整NR7条件应贯穿推荐选股求值：%+v", hit)
	}
	verified := false
	for _, v := range candidateLabeledValues(candidate{StrategyHit: hit}) {
		if v.Path == "strategy_hit.values.vol_boost" && v.Value == hit.Values["vol_boost"] {
			verified = true
		}
	}
	if !verified {
		t.Fatal("发送给AI的策略原始指标必须同步进入数值核验域")
	}
	for _, tc := range []struct {
		name string
		edit func([]datasource.Bar)
	}{
		{"盘中突破但收盘未确认", func(v []datasource.Bar) { v[99].Close = 20.1 }},
		{"窄幅有并列不算严格收敛", func(v []datasource.Bar) { v[95].High, v[95].Low = 20.1, 19.9 }},
		{"一字线", func(v []datasource.Bar) { v[98].High, v[98].Low = 20, 20 }},
		{"停牌低量假收敛", func(v []datasource.Bar) { v[98].Volume = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := append([]datasource.Bar{}, bars...)
			tc.edit(v)
			if extendedSetupFactors(v)["nr7_break"] != 0 {
				t.Fatal("不完整的突破证据不能命中")
			}
		})
	}
	// NR7允许突破昨日小区间，不能被统一研究价误改成必须创20日新高。
	setupTestCandle(bars, 90, 20.7, 21, 20, 20.8, 10000000)
	last := bars[len(bars)-1]
	c := candidate{Symbol: "600100", Market: "cn", Price: last.Close, QuoteAsOf: last.TradeDate + " 15:10"}
	p := buildResearchPricePlan(c, bars, researchPriceContextFor(model.RecTypeShortTerm, &strat))
	if p.Status == "unavailable" || strings.Contains(strings.Join(p.SetupReasons, " "), "所选突破尚无") {
		t.Fatalf("NR7应使用昨日窄幅高点而非20日收盘高点：%+v", p)
	}
}

func TestRSI2ReclaimRequiresRisingLongTrend(t *testing.T) {
	bars := genTrendBars(250, 20, .02)
	for i := range bars {
		c := 20 + float64(i)*.02
		setupTestCandle(bars, i, c, c+.2, c-.2, c, 10000000)
	}
	for i, c := range []float64{24.4, 24, 23.5, 23.65} {
		setupTestCandle(bars, 246+i, c, c+.1, c-.1, c, 10000000)
	}
	b, _ := builtinScreenByKey("rsi2-trend-reclaim")
	row := computeWideRowOpts("600100", wideStockMeta{}, bars, false)
	if !evalCondRow(singleRowFactorTable(row), &b.Tree, 0) {
		t.Fatalf("长期上升中的短期修复应命中：%v", commonSetupFactors(bars))
	}
	// RSI2相同的修复，长期均线下行时不属于本策略。
	row[factorIndex["ma200_rising"]] = 0
	if evalCondRow(singleRowFactorTable(row), &b.Tree, 0) {
		t.Fatal("长期下行不能只靠超卖修复通过")
	}
	bars[249].Low = bars[248].Low - .1
	if extendedSetupFactors(bars)["rsi2_reclaim"] != 0 {
		t.Fatal("低点还在下移时不应标为企稳")
	}
}

func TestLongTrendTemplateAndNewFactorsRequireFullWindows(t *testing.T) {
	bars := genTrendBars(250, 10, .03)
	for i := range bars {
		c := 10 + float64(i)*.03
		setupTestCandle(bars, i, c, c+.4, c-.4, c, 10000000)
	}
	b, _ := builtinScreenByKey("long-trend-template")
	if !evalCondRow(singleRowFactorTable(computeWideRowOpts("600100", wideStockMeta{}, bars, false)), &b.Tree, 0) {
		t.Fatal("完整长期趋势模板应命中")
	}
	for _, tc := range []struct {
		key  string
		bars int
	}{{"dmi_bull_cross", 149}, {"adx_14", 149}, {"ma200_rising", 219}, {"long_trend_template", 249}, {"nr7_break", 7}, {"rsi2_reclaim", 29}} {
		row := computeWideRowOpts("600100", wideStockMeta{}, bars[:tc.bars], false)
		if !math.IsNaN(row[factorIndex[tc.key]]) || evalCondRow(singleRowFactorTable(row), &CondNode{Factor: tc.key, Op: "is_false"}, 0) {
			t.Fatalf("%s未满窗必须是未知，而非false", tc.key)
		}
	}
	for _, key := range []string{"dmi-trend-confirm", "nr7-breakout", "rsi2-trend-reclaim", "long-trend-template"} {
		b, ok := builtinScreenByKey(key)
		if !ok {
			t.Fatalf("缺少内置策略%s", key)
		}
		short, long := builtinScreenStrategyTemplate(model.RecTypeShortTerm, b), builtinScreenStrategyTemplate(model.RecTypeLongTerm, b)
		if short.baseKey != long.baseKey || short.Intent != long.Intent || profileUsesFinance(long.baseKey) {
			t.Fatalf("%s被持有周期改成另一种选股意图", key)
		}
	}
}
