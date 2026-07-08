package service

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// M1 第二部分测试：因子宽表口径对拍 / NaN 语义 / 涨停判定 / DSL 校验与求值 /
// 人话化 / 扫描端到端（3 只样本手工验算）/ 自定义策略隔离 / 全规模性能冒烟。

// ---------- 测试辅助 ----------

// genTrendBars 合成升序日线：base 起步、每日 drift% 漂移 + 正弦扰动，量能随机波动。
// 日期从 2025-01-01 起逐日 +1（宽表只关心字典序）。
func genTrendBars(n int, base, driftPct float64) []datasource.Bar {
	bars := make([]datasource.Bar, n)
	price := base
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		chg := driftPct + 0.8*math.Sin(float64(i)/3.7)
		price *= 1 + chg/100
		if price < 0.5 {
			price = 0.5
		}
		open := price * (1 - 0.004)
		high := price * (1 + 0.01)
		low := open * (1 - 0.01)
		vol := int64(20000 + 8000*math.Sin(float64(i)/2.3) + float64(i%7)*1000)
		bars[i] = datasource.Bar{
			TradeDate: day.AddDate(0, 0, i).Format("2006-01-02"),
			Open:      round2(open), High: round2(high), Low: round2(low), Close: round2(price),
			Volume: vol, Amount: float64(vol) * price * 100, TurnoverRate: 2.5,
		}
	}
	return bars
}

// resetFactorTable 清空宽表包级缓存（测试隔离）。
func resetFactorTable() {
	factorTableMu.Lock()
	factorTableCur = nil
	factorTableMu.Unlock()
	factorFreshMu.Lock()
	factorFreshVal = ""
	factorFreshMu.Unlock()
}

// wideVal 按因子 key 取 computeWideRow 结果里的值。
func wideVal(vals []float64, key string) float64 { return vals[factorIndex[key]] }

// ---------- 宽表因子口径对拍 ----------

// TestComputeWideRowParity 宽表因子与 computeCandFactors/computeIndicatorSnapshot
// 对拍：两处共用底层序列函数，数值必须一致（candFactors 的 0=缺席语义按非零对拍）。
func TestComputeWideRowParity(t *testing.T) {
	bars := genTrendBars(120, 10, 0.3)
	price := bars[len(bars)-1].Close
	vals := computeWideRow("600001", wideStockMeta{Name: "对拍股"}, bars)
	cf := computeCandFactors(price, bars)
	if cf == nil {
		t.Fatal("candFactors 为 nil")
	}

	numPairs := []struct {
		key  string
		want float64
	}{
		{"ma5", cf.MA5}, {"ma10", cf.MA10}, {"ma20", cf.MA20}, {"ma60", cf.MA60},
		{"chg_5d", cf.Chg5d}, {"chg_20d", cf.Chg20d},
		{"vol_boost", cf.VolBoost}, {"vol_5v20", cf.Vol5v20},
		{"volatility_20", cf.Volatility20}, {"drawdown_20", cf.Drawdown20},
		{"bias_20", cf.Bias20}, {"pos_60", cf.Pos60},
		{"rsi_14", cf.RSI14},
		{"macd_dif", cf.MACDDif}, {"macd_dea", cf.MACDDea}, {"macd_hist", cf.MACDHist},
		{"boll_up", cf.BollUp}, {"boll_mid", cf.BollMid}, {"boll_low", cf.BollLow}, {"boll_pos", cf.BollPos},
		{"atr_14", cf.ATR14}, {"atr_pct", cf.ATRPct},
	}
	for _, p := range numPairs {
		if p.want == 0 {
			continue // candFactors 零值=缺席，不对拍
		}
		got := wideVal(vals, p.key)
		if math.IsNaN(got) || math.Abs(got-p.want) > 1e-9 {
			t.Errorf("因子 %s 口径漂移: 宽表 %v vs candFactors %v", p.key, got, p.want)
		}
	}
	boolPairs := []struct {
		key  string
		want bool
	}{
		{"bull_align", cf.BullAlign}, {"above_ma20", cf.AboveMA20},
		{"high_20d", cf.High20d}, {"macd_gold", cf.MACDGold}, {"macd_cross_up", cf.MACDXUp},
	}
	for _, p := range boolPairs {
		got := wideVal(vals, p.key)
		want := 0.0
		if p.want {
			want = 1
		}
		if got != want {
			t.Errorf("布尔因子 %s 口径漂移: 宽表 %v vs candFactors %v", p.key, got, p.want)
		}
	}
	// 筹码对拍（120 根 ≥chipMinBars，带换手必可算）。
	chip, err := computeChipDistribution(bars, 0)
	if err != nil {
		t.Fatalf("筹码计算失败: %v", err)
	}
	if got := wideVal(vals, "chip_profit"); math.Abs(got-chip.Profit) > 1e-9 {
		t.Errorf("chip_profit 漂移: %v vs %v", got, chip.Profit)
	}
	if got := wideVal(vals, "bar_count"); got != 120 {
		t.Errorf("bar_count = %v, want 120", got)
	}
}

// TestComputeWideRowNaN 样本不足的因子必须是 NaN（不能冒充 0 参与条件命中）。
func TestComputeWideRowNaN(t *testing.T) {
	bars := genTrendBars(10, 10, 0.5) // 10 根：MA20/RSI/BOLL/MACD/chg_20d 全不足
	vals := computeWideRow("600001", wideStockMeta{}, bars)
	for _, key := range []string{"ma20", "ma60", "ma120", "ma250", "rsi_14", "macd_dif",
		"boll_mid", "atr_14", "chg_20d", "chg_60d", "high_20d", "bull_align",
		"above_ma20", "chip_profit", "vol_5v20"} {
		if !math.IsNaN(wideVal(vals, key)) {
			t.Errorf("因子 %s 样本不足应为 NaN，got %v", key, wideVal(vals, key))
		}
	}
	// 基础行情与 5 日窗仍有值。
	for _, key := range []string{"close", "chg_pct", "chg_5d", "vol_boost", "bar_count", "is_st"} {
		if math.IsNaN(wideVal(vals, key)) {
			t.Errorf("因子 %s 不应缺失", key)
		}
	}
}

// TestComputeWideRowLimitUp 涨停判定按板块阈值：主板 9.8 / 创业板 19.8 / ST 4.8。
func TestComputeWideRowLimitUp(t *testing.T) {
	mk := func(closes ...float64) []datasource.Bar {
		bars := make([]datasource.Bar, len(closes))
		day := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
		for i, c := range closes {
			bars[i] = datasource.Bar{TradeDate: day.AddDate(0, 0, i).Format("2006-01-02"),
				Open: c, High: c, Low: c, Close: c, Volume: 1000, Amount: c * 100000}
		}
		return bars
	}
	cases := []struct {
		name      string
		symbol    string
		stockName string
		closes    []float64
		today     float64 // 期望 limit_up_today（1/0）
		yest      float64
		count5    float64
	}{
		{"主板10%涨停", "600001", "甲股", []float64{10, 11}, 1, math.NaN(), 1},
		{"主板9%不算", "600001", "甲股", []float64{10, 10.9}, 0, math.NaN(), 0},
		{"昨日涨停今日回调", "600001", "甲股", []float64{10, 11, 10.8}, 0, 1, 1},
		{"创业板15%不算", "300001", "乙股", []float64{10, 11.5}, 0, math.NaN(), 0},
		{"创业板20%涨停", "300001", "乙股", []float64{10, 12}, 1, math.NaN(), 1},
		{"ST股5%涨停", "600002", "ST丙", []float64{10, 10.5}, 1, math.NaN(), 1},
		{"连板计数", "600001", "甲股", []float64{10, 11, 12.1, 13.31}, 1, 1, 3},
	}
	for _, c := range cases {
		vals := computeWideRow(c.symbol, wideStockMeta{Name: c.stockName, ST: isSTName(c.stockName)}, mk(c.closes...))
		if got := wideVal(vals, "limit_up_today"); got != c.today && !(math.IsNaN(got) && math.IsNaN(c.today)) {
			t.Errorf("%s: limit_up_today = %v, want %v", c.name, got, c.today)
		}
		if !math.IsNaN(c.yest) {
			if got := wideVal(vals, "limit_up_yest"); got != c.yest {
				t.Errorf("%s: limit_up_yest = %v, want %v", c.name, got, c.yest)
			}
		}
		if got := wideVal(vals, "limit_ups_5d"); got != c.count5 {
			t.Errorf("%s: limit_ups_5d = %v, want %v", c.name, got, c.count5)
		}
	}
}

// ---------- DSL 校验与求值 ----------

func TestValidateCondTree(t *testing.T) {
	bad := []struct {
		name string
		node CondNode
	}{
		{"未知因子", leafV("no_such", ">", 1)},
		{"未知操作符", CondNode{Factor: "rsi_14", Op: "!=", Value: fptr(1)}},
		{"缺比较值", CondNode{Factor: "rsi_14", Op: ">"}},
		{"between缺上界", CondNode{Factor: "rsi_14", Op: "between", Value: fptr(30)}},
		{"布尔因子用数值op", leafV("bull_align", ">", 0)},
		{"数值因子用is_true", leafTrue("rsi_14")},
		{"ref是布尔因子", leafRef("close", ">", "bull_align")},
		{"value与ref并存", CondNode{Factor: "close", Op: ">", Ref: "ma20", Value: fptr(1)}},
		{"组叶混用", CondNode{All: []CondNode{leafTrue("bull_align")}, Factor: "rsi_14", Op: ">", Value: fptr(1)}},
		{"all与any并存", CondNode{All: []CondNode{leafTrue("bull_align")}, Any: []CondNode{leafTrue("is_st")}}},
		{"空树", CondNode{}},
	}
	for _, c := range bad {
		if _, err := validateCondTree(&c.node, 1); err == nil {
			t.Errorf("%s: 应校验失败", c.name)
		}
	}
	// 合法树 + between 宽容交换。
	n := allOf(leafBetween("rsi_14", 45, 30), leafRef("close", ">", "ma20"), leafTrue("bull_align"))
	cnt, err := validateCondTree(&n, 1)
	if err != nil || cnt != 3 {
		t.Fatalf("合法树校验失败: cnt=%d err=%v", cnt, err)
	}
	if *n.All[0].Value != 30 || *n.All[0].Value2 != 45 {
		t.Fatal("between 上下界未宽容交换")
	}
	// 全部内置策略必须通过校验（防手写树笔误）。
	for _, b := range builtinScreens {
		tree := b.Tree
		if _, err := validateCondTree(&tree, 1); err != nil {
			t.Errorf("内置策略 %s 校验失败: %v", b.Key, err)
		}
	}
}

// miniTable 手工小宽表：2 行（600001 命中形态 / 600002 反例含 NaN）。
func miniTable() *FactorTable {
	nan := math.NaN()
	return &FactorTable{
		TradeDate: "2026-07-08",
		Symbols:   []string{"600001", "600002"},
		Names:     []string{"甲股", "乙股"},
		LastDates: []string{"2026-07-08", "2026-07-08"},
		cols: map[string][]float64{
			"close":      {21.35, 8.0},
			"ma20":       {20.8, 8.5},
			"rsi_14":     {38.2, nan},
			"vol_boost":  {2.13, 1.0},
			"bull_align": {1, 0},
			"is_st":      {0, 0},
			"amount_yi":  {5.6, 1.2},
			"chg_pct":    {2.1, -1.0},
			"turnover_rate": {3.3, 1.1},
			"pos_60":     {42, 80},
		},
	}
}

func TestEvalCondAndExplain(t *testing.T) {
	tb := miniTable()
	tree := allOf(
		leafBetween("rsi_14", 30, 45),
		leafRef("close", ">", "ma20"),
		leafBetween("vol_boost", 1.5, 5),
		leafTrue("bull_align"),
	)
	if _, err := validateCondTree(&tree, 1); err != nil {
		t.Fatal(err)
	}
	if !evalCondRow(tb, &tree, 0) {
		t.Fatal("行 0 应命中")
	}
	if evalCondRow(tb, &tree, 1) {
		t.Fatal("行 1 不应命中（rsi NaN + close<ma20）")
	}
	var reasons []string
	explainRow(tb, &tree, 0, &reasons)
	if len(reasons) != 4 {
		t.Fatalf("命中原因应 4 条, got %d: %v", len(reasons), reasons)
	}
	wants := []string{
		"✓ RSI(14) 介于 30~45（当前 38.2）",
		"✓ 收盘价 > 20日均线（21.35 > 20.8）",
		"✓ 量比(5日) 介于 1.5~5（当前 2.13）",
		"✓ 均线多头排列",
	}
	for i, w := range wants {
		if reasons[i] != w {
			t.Errorf("原因[%d] = %q, want %q", i, reasons[i], w)
		}
	}

	// any 分支：只列命中的那支。
	anyTree := CondNode{Any: []CondNode{
		leafV("rsi_14", ">", 90),  // 不命中
		leafV("pos_60", "<", 50), // 命中
	}}
	if _, err := validateCondTree(&anyTree, 1); err != nil {
		t.Fatal(err)
	}
	if !evalCondRow(tb, &anyTree, 0) {
		t.Fatal("any 应命中")
	}
	reasons = nil
	explainRow(tb, &anyTree, 0, &reasons)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "60日区间位置") {
		t.Fatalf("any 应只列命中分支: %v", reasons)
	}

	// NaN 因子的 is_false 也不命中（未知≠否）。
	nanBool := &FactorTable{TradeDate: "d", Symbols: []string{"s"}, Names: []string{""},
		LastDates: []string{"d"}, cols: map[string][]float64{"macd_gold": {math.NaN()}}}
	f := leafFalse("macd_gold")
	if evalCondRow(nanBool, &f, 0) {
		t.Fatal("NaN 布尔因子 is_false 不应命中")
	}
}

// ---------- 扫描端到端（DB；3 只样本手工验算） ----------

// seedWideStock 造一只股：日线落库 + states 行（last_bar_date=末根）。
func seedWideStock(t *testing.T, symbol, name string, bars []datasource.Bar) {
	t.Helper()
	rows := make([]model.DailyBar, 0, len(bars))
	for _, b := range bars {
		rows = append(rows, model.DailyBar{Symbol: symbol, Market: "cn", TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
			Volume: b.Volume, Amount: b.Amount, TurnoverRate: b.TurnoverRate, Source: "eastmoney"})
	}
	if err := common.DB.CreateInBatches(rows, 200).Error; err != nil {
		t.Fatalf("落日线失败: %v", err)
	}
	st := model.MarketSyncState{Symbol: symbol, Market: "cn", Name: name,
		InitStatus: "done", BarsCount: len(bars), LastBarDate: bars[len(bars)-1].TradeDate}
	if err := common.DB.Create(&st).Error; err != nil {
		t.Fatalf("落 state 失败: %v", err)
	}
}

func TestScreenerScanEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	common.DB.Where("1 = 1").Delete(&model.ScreenerStrategy{})
	resetFactorTable()
	defer resetFactorTable()

	// 3 只样本（手工验算）：
	//   600100 温和上升趋势（每日 +0.4% 漂移）→ 线性上升序列 MA5>MA10>MA20 且价在
	//          MA60 上方（bull_align + above_ma60 恒成立），末日涨幅温和 → 命中多头排列
	//   600200 单边下跌（每日 -0.5%）→ 均线空头，不命中
	//   600300 ST 上升趋势 → 形态命中但默认被 ST 过滤
	up := genTrendBars(80, 10, 0.4)
	seedWideStock(t, "600100", "甲股", up)
	seedWideStock(t, "600200", "乙股", genTrendBars(80, 20, -0.5))
	seedWideStock(t, "600300", "ST丙股", genTrendBars(80, 5, 0.4))

	svc := NewScreenerService()
	res, err := svc.Scan(context.Background(), 1, ScanRequest{StrategyKey: "bull-align-trend"})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if res.Universe != 3 || res.StSkipped != 1 || res.Scanned != 2 {
		t.Fatalf("全景计数异常: universe=%d st=%d scanned=%d", res.Universe, res.StSkipped, res.Scanned)
	}
	if res.Matched != 1 || len(res.Items) != 1 || res.Items[0].Symbol != "600100" {
		t.Fatalf("命中应恰为 600100: matched=%d items=%v", res.Matched, res.Items)
	}
	hit := res.Items[0]
	if hit.Name != "甲股" || hit.Price <= 0 || len(hit.Reasons) == 0 {
		t.Fatalf("命中行不完整: %+v", hit)
	}
	// 手工验算锚点：均线多头 + 站上 MA60 两条人话原因必在。
	joined := strings.Join(hit.Reasons, "|")
	if !strings.Contains(joined, "✓ 均线多头排列") || !strings.Contains(joined, "✓ 站上MA60") {
		t.Fatalf("命中原因缺关键条目: %v", hit.Reasons)
	}
	// 宽表值与直接计算对拍（第三重验算）。
	vals := computeWideRow("600100", wideStockMeta{Name: "甲股"}, up)
	if hit.Price != wideVal(vals, "close") {
		t.Fatalf("命中价 %v ≠ 直算收盘 %v", hit.Price, wideVal(vals, "close"))
	}

	// include_st：ST 股形态同样命中。
	res2, err := svc.Scan(context.Background(), 1, ScanRequest{StrategyKey: "bull-align-trend", IncludeST: true})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Matched != 2 {
		t.Fatalf("含 ST 应命中 2 只, got %d", res2.Matched)
	}

	// 临时树扫描 + 人话化端到端。
	tree := allOf(leafRef("close", ">", "ma20"), leafV("chg_60d", ">", 5))
	res3, err := svc.Scan(context.Background(), 1, ScanRequest{Tree: &tree})
	if err != nil {
		t.Fatal(err)
	}
	if res3.Matched != 1 || res3.Items[0].Symbol != "600100" {
		t.Fatalf("临时树命中异常: %+v", res3.Items)
	}
}

// TestScreenerStaleExcluded 停牌股（末根落后）默认排除、include_stale 放开。
func TestScreenerStaleExcluded(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	resetFactorTable()
	defer resetFactorTable()

	fresh := genTrendBars(80, 10, 0.4)
	seedWideStock(t, "600100", "甲股", fresh)
	stale := genTrendBars(70, 8, 0.4) // 末根日期早于甲股
	seedWideStock(t, "600400", "停牌股", stale)

	// 用相位无关的必命中树（close>0），聚焦验证 stale 排除逻辑本身。
	tree := leafV("close", ">", 0)
	svc := NewScreenerService()
	res, err := svc.Scan(context.Background(), 1, ScanRequest{Tree: &tree})
	if err != nil {
		t.Fatal(err)
	}
	if res.StaleSkipped != 1 || res.Matched != 1 || res.Items[0].Symbol != "600100" {
		t.Fatalf("stale 应排除: stale=%d matched=%d", res.StaleSkipped, res.Matched)
	}
	res2, err := svc.Scan(context.Background(), 1, ScanRequest{Tree: &tree, IncludeStale: true})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Matched != 2 {
		t.Fatalf("include_stale 应命中 2, got %d", res2.Matched)
	}
}

// TestEnsureFactorTableFreshness 增量推进 last_bar_date 后旧表判过期自动重建。
func TestEnsureFactorTableFreshness(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	resetFactorTable()
	defer resetFactorTable()

	bars := genTrendBars(30, 10, 0.3)
	seedWideStock(t, "600100", "甲股", bars)
	t1, err := ensureFactorTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lastDate := bars[len(bars)-1].TradeDate
	if t1.TradeDate != lastDate {
		t.Fatalf("表日期 %s ≠ 数据末根 %s", t1.TradeDate, lastDate)
	}
	// 缓存命中：同日期再取是同一张表。
	t2, _ := ensureFactorTable(context.Background())
	if t2 != t1 {
		t.Fatal("新鲜表应直接复用缓存")
	}
	// 推进一日（模拟次日增量）：新 bar + states last_bar_date 前进 → 重建。
	next := "2026-12-31"
	common.DB.Create(&model.DailyBar{Symbol: "600100", Market: "cn", TradeDate: next,
		Open: 12, High: 12.5, Low: 11.9, Close: 12.2, Volume: 30000, Amount: 3.6e7, Source: "eastmoney"})
	common.DB.Model(&model.MarketSyncState{}).Where("symbol = ?", "600100").
		Update("last_bar_date", next)
	resetFreshCacheOnly()
	t3, err := ensureFactorTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if t3 == t1 || t3.TradeDate != next {
		t.Fatalf("过期表未重建: %s", t3.TradeDate)
	}
}

// resetFreshCacheOnly 只清新鲜日期缓存（保留宽表，模拟 60s TTL 到期）。
func resetFreshCacheOnly() {
	factorFreshMu.Lock()
	factorFreshVal = ""
	factorFreshMu.Unlock()
}

// ---------- 自定义策略 ----------

func TestScreenerStrategyCRUD(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.ScreenerStrategy{})

	svc := NewScreenerService()
	tree := allOf(leafV("rsi_14", "<", 30), leafV("amount_yi", ">=", 1))
	saved, err := svc.SaveStrategy(1, SaveStrategyRequest{Name: "超卖捡漏", Desc: "d", Period: "swing", Risk: "high", Tree: &tree})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if saved.ID == 0 || len(saved.Conditions) != 2 {
		t.Fatalf("保存结果异常: %+v", saved)
	}
	// 坏树被拦。
	badTree := leafV("no_such", ">", 1)
	if _, err := svc.SaveStrategy(1, SaveStrategyRequest{Name: "坏", Tree: &badTree}); err == nil {
		t.Fatal("未知因子应被拦")
	}
	// 更新。
	tree2 := allOf(leafV("rsi_14", "<", 25))
	if _, err := svc.SaveStrategy(1, SaveStrategyRequest{ID: saved.ID, Name: "超卖捡漏2", Tree: &tree2}); err != nil {
		t.Fatal(err)
	}
	// 列表（user 1 有、user 2 无）。
	v1, _ := svc.Strategies(1)
	if len(v1.Custom) != 1 || v1.Custom[0].Name != "超卖捡漏2" {
		t.Fatalf("user1 自定义列表异常: %+v", v1.Custom)
	}
	if len(v1.Builtin) < 20 {
		t.Fatalf("内置策略应 ≥20 个, got %d", len(v1.Builtin))
	}
	if len(v1.Factors) != len(factorDefs) {
		t.Fatal("因子字典缺失")
	}
	v2, _ := svc.Strategies(2)
	if len(v2.Custom) != 0 {
		t.Fatal("user2 不应看到 user1 的策略")
	}
	// 跨用户改删被拦。
	if _, err := svc.SaveStrategy(2, SaveStrategyRequest{ID: saved.ID, Name: "偷改", Tree: &tree}); err == nil {
		t.Fatal("跨用户更新应失败")
	}
	if err := svc.DeleteStrategy(2, saved.ID); err == nil {
		t.Fatal("跨用户删除应失败")
	}
	if err := svc.DeleteStrategy(1, saved.ID); err != nil {
		t.Fatal(err)
	}
}

// ---------- 全规模性能冒烟 ----------

// TestFactorComputePerf 5150 只 × 250 根合成数据的因子计算（不含 DB 读）并行耗时。
// 目标 <5s；CI 慢机放宽断言到 20s，实际耗时打日志供人工核对。
func TestFactorComputePerf(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过")
	}
	const nStocks = 5150
	base := genTrendBars(250, 10, 0.1)
	jobs := make(chan int, 64)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < wideFactorWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				_ = computeWideRow("600001", wideStockMeta{Name: "股"}, base)
			}
		}()
	}
	for i := 0; i < nStocks; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("因子计算 %d 只 × 250 根（含筹码），%d worker 耗时 %v", nStocks, wideFactorWorkers(), elapsed)
	if elapsed > 20*time.Second {
		t.Fatalf("因子计算过慢: %v（目标 <5s，CI 容忍 20s）", elapsed)
	}
}

// TestStrategySignalHits 推荐池策略信号来源：宽表就绪时返回命中、无数据时静默空。
func TestStrategySignalHits(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	resetFactorTable()
	defer resetFactorTable()

	// 无日线：best-effort 空，不报错。
	if hits := strategySignalHits(context.Background(), model.RecTypeLongTerm, "leader", 10); hits != nil {
		t.Fatalf("无数据应返回 nil, got %d", len(hits))
	}
	// 有数据：leader → bull-align-trend 命中上升趋势股。
	seedWideStock(t, "600100", "甲股", genTrendBars(80, 10, 0.4))
	resetFreshCacheOnly()
	hits := strategySignalHits(context.Background(), model.RecTypeLongTerm, "leader", 10)
	if len(hits) != 1 || hits[0].Symbol != "600100" {
		t.Fatalf("策略信号命中异常: %+v", hits)
	}
	// 映射表覆盖全部推荐策略组合（笔误防护）。
	for _, c := range []struct{ rt, sk string }{
		{model.RecTypeShortTerm, "momentum"}, {model.RecTypeShortTerm, "pullback"}, {model.RecTypeShortTerm, "active"},
		{model.RecTypeLongTerm, "value"}, {model.RecTypeLongTerm, "growth"}, {model.RecTypeLongTerm, "leader"},
	} {
		key := recStrategySignalKey(c.rt, c.sk)
		if _, ok := builtinScreenByKey(key); !ok {
			t.Errorf("推荐策略 %s/%s 映射到不存在的选股策略 %q", c.rt, c.sk, key)
		}
	}
}
