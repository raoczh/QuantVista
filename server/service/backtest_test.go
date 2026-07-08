package service

import (
	"context"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// M2 回测测试：A 股约束五件套（一字板跳过/跌停顺延/整百股/费率对齐）、
// 复权自洽校验、无未来泄露对拍、信号日采样、端到端全流程、推荐批次回验。

// flatBars 造恒定价格的升序日线（open=high=low=close=price），日期 2025-01-01 起逐自然日。
func flatBars(n int, price float64) []datasource.Bar {
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]datasource.Bar, n)
	for i := 0; i < n; i++ {
		bars[i] = datasource.Bar{
			TradeDate: day.AddDate(0, 0, i).Format("2006-01-02"),
			Open:      price, High: price, Low: price, Close: price,
			Volume: 10000, Amount: price * 1000000, TurnoverRate: 2,
		}
	}
	return bars
}

// ---------- simulateHold 五件套 ----------

// TestSimulateHoldNormalWithFee 正常成交：收益必须与 tradeFee 手算严格对齐。
// 信号日收盘 10，次日开盘 10.10 买入（涨 1% 非一字板），第 5 日收盘 11.00 卖出。
// qty=floor(20000/1010)×100=1900；买 19190+5=19195；卖 20900−5.23−10.45=20884.32；
// ret=(20884.32−19195)/19195=8.80%。
func TestSimulateHoldNormalWithFee(t *testing.T) {
	bars := flatBars(10, 10)
	bars[1].Open = 10.10
	bars[5].Close = 11.00
	o := simulateHold(bars, 0, "600001", "普通股", 5, 20000, bars[1].TradeDate)
	if o.Status != btTraded {
		t.Fatalf("期望成交，得到 %s", o.Status)
	}
	if o.BuyPrice != 10.10 || o.SellPrice != 11.00 {
		t.Fatalf("买卖价错误: buy=%v sell=%v", o.BuyPrice, o.SellPrice)
	}
	if o.BuyDate != bars[1].TradeDate || o.SellDate != bars[5].TradeDate {
		t.Fatalf("买卖日错误: %s → %s", o.BuyDate, o.SellDate)
	}
	if math.Abs(o.ReturnPct-8.8) > 0.01 {
		t.Fatalf("收益与费率手算不符: got %v want 8.80", o.ReturnPct)
	}
}

// TestSimulateHoldLimitUpSkip 一字板跳过：次日开盘涨幅 ≥ 涨停阈值−0.5 买不进。
func TestSimulateHoldLimitUpSkip(t *testing.T) {
	cases := []struct {
		symbol  string
		name    string
		openPct float64
		want    string
	}{
		{"600001", "主板股", 9.6, btSkipLimitUp}, // 主板阈值 10−0.5=9.5
		{"600001", "主板股", 9.3, btTraded},
		{"300001", "创业板股", 9.6, btTraded}, // 创业板阈值 20−0.5=19.5
		{"300001", "创业板股", 19.6, btSkipLimitUp},
		{"600001", "ST国改", 4.6, btSkipLimitUp}, // ST 阈值 5−0.5=4.5
	}
	for _, c := range cases {
		bars := flatBars(10, 10)
		bars[1].Open = round2(10 * (1 + c.openPct/100))
		o := simulateHold(bars, 0, c.symbol, c.name, 5, 20000, bars[1].TradeDate)
		if o.Status != c.want {
			t.Errorf("%s %s 开盘+%v%%: 期望 %s 得到 %s", c.symbol, c.name, c.openPct, c.want, o.Status)
		}
	}
}

// TestSimulateHoldDeferSell 跌停顺延：卖出日一字跌停卖不出顺延重试；
// 顺延到数据末尾仍一字跌停按末根收盘强平并标 forced。
func TestSimulateHoldDeferSell(t *testing.T) {
	// bars[2] 为卖出目标日（hold=2），设为一字跌停（10 → 9.0，high==low）。
	bars := flatBars(6, 10)
	bars[2].Open, bars[2].High, bars[2].Low, bars[2].Close = 9, 9, 9, 9
	bars[3].Close = 8.8 // 顺延日正常波动可卖
	bars[3].High, bars[3].Low = 8.9, 8.7
	o := simulateHold(bars, 0, "600001", "普通股", 2, 20000, bars[1].TradeDate)
	if o.Status != btTraded || o.Deferred != 1 || o.Forced {
		t.Fatalf("期望顺延 1 次成交: %+v", o)
	}
	if o.SellDate != bars[3].TradeDate || o.SellPrice != 8.8 {
		t.Fatalf("顺延后应在 bars[3] 收盘卖出: %+v", o)
	}

	// 从卖出日到末尾全为一字跌停 → 末根强平。
	bars2 := flatBars(5, 10)
	p := 10.0
	for i := 2; i < 5; i++ { // 每日一字跌停 10%
		p = round2(p * 0.9)
		bars2[i].Open, bars2[i].High, bars2[i].Low, bars2[i].Close = p, p, p, p
	}
	o2 := simulateHold(bars2, 0, "600001", "普通股", 2, 20000, bars2[1].TradeDate)
	// j=2/3/4 三根均一字跌停各顺延一次，越界后按末根收盘强平。
	if o2.Status != btTraded || !o2.Forced || o2.Deferred != 3 {
		t.Fatalf("期望顺延 3 次后末根强平: %+v", o2)
	}
	if o2.SellDate != bars2[4].TradeDate {
		t.Fatalf("强平应在末根: %+v", o2)
	}
}

// TestSimulateHoldLotAndCash 整百股取整与拨款不足。
func TestSimulateHoldLotAndCash(t *testing.T) {
	// 股价 199：20000/(199×100)=1.005 → 100 股。
	bars := flatBars(10, 199)
	o := simulateHold(bars, 0, "600001", "普通股", 5, 20000, bars[1].TradeDate)
	if o.Status != btTraded {
		t.Fatalf("199 元股 2 万拨款应能买一手: %+v", o)
	}
	// 股价 201：20000/(201×100)<1 → 一手都买不起。
	bars2 := flatBars(10, 201)
	o2 := simulateHold(bars2, 0, "600001", "普通股", 5, 20000, bars2[1].TradeDate)
	if o2.Status != btSkipCash {
		t.Fatalf("201 元股 2 万拨款应 skip_cash: %+v", o2)
	}
}

// TestSimulateHoldSuspendAndPending 停牌跳过与持有期未走完。
func TestSimulateHoldSuspendAndPending(t *testing.T) {
	// 次日停牌：个股缺市场次日 bar（nextDate 与 bars[i+1] 不符）。
	bars := flatBars(10, 10)
	o := simulateHold(bars, 0, "600001", "普通股", 5, 20000, "2024-12-31")
	if o.Status != btSkipSuspend {
		t.Fatalf("次日日期不符应 skip_suspend: %+v", o)
	}
	// 信号日就是末根 → 无次日数据。
	o2 := simulateHold(bars, len(bars)-1, "600001", "普通股", 5, 20000, "")
	if o2.Status != btSkipSuspend {
		t.Fatalf("末根信号应 skip_suspend: %+v", o2)
	}
	// 买入后数据不足持有期 → pending。
	o3 := simulateHold(bars, 7, "600001", "普通股", 5, 20000, bars[8].TradeDate)
	if o3.Status != btPending {
		t.Fatalf("持有期未走完应 pending: %+v", o3)
	}
}

// ---------- 复权自洽校验 ----------

func TestAdjustSuspect(t *testing.T) {
	// 正常序列不触发。
	bars := genTrendBars(60, 10, 0.3)
	if adjustSuspect(bars, "600001", "普通股") {
		t.Fatal("正常序列误报断层")
	}
	// 中段 -35% 断层（主板容差 10×1.5=15）触发。
	broken := genTrendBars(60, 10, 0.3)
	for i := 30; i < 60; i++ {
		broken[i].Close = round2(broken[i].Close * 0.65)
	}
	if !adjustSuspect(broken, "600001", "普通股") {
		t.Fatal("中段断层未检出")
	}
	// 头部（前 5 根）大跳变不触发（注册制新股上市初期无涨跌幅限制）。
	head := flatBars(30, 10)
	head[3].Close = 15
	if adjustSuspect(head, "600001", "普通股") {
		t.Fatal("头部跳变不应触发（新股窗口）")
	}
	// ST 股容差 5×1.5=7.5：6% 不触发、9% 触发。
	st := flatBars(30, 10)
	st[20].Close = 10.6
	if adjustSuspect(st, "600001", "ST某股") {
		t.Fatal("ST 6% 波动误报")
	}
	st[20].Close = 10.9
	if !adjustSuspect(st, "600001", "ST某股") {
		t.Fatal("ST 9% 断层未检出")
	}
}

// ---------- 无未来泄露 + skipChip 对拍 ----------

// eqNaN 两浮点相等（NaN==NaN 视为相等）。
func eqNaN(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

// TestBacktestNoFutureLeak as-of 切片求值与「拷贝出的独立序列」求值完全一致——
// 证明信号日因子只依赖截至该日的数据，未来根不参与（无未来泄露的核心断言）。
func TestBacktestNoFutureLeak(t *testing.T) {
	full := genTrendBars(120, 10, 0.4)
	meta := wideStockMeta{Name: "对拍股"}
	sub := full[:80]
	valsSlice := computeWideRowOpts("600001", meta, sub, true)
	indep := append([]datasource.Bar(nil), full[:80]...)
	valsIndep := computeWideRowOpts("600001", meta, indep, true)
	for j, d := range factorDefs {
		if !eqNaN(valsSlice[j], valsIndep[j]) {
			t.Errorf("因子 %s 受切片上下文影响: %v vs %v", d.Key, valsSlice[j], valsIndep[j])
		}
	}
	// 切片=全长时与宽表全量口径一致。
	valsFull := computeWideRow("600001", meta, full)
	valsAt := computeWideRowOpts("600001", meta, full[:len(full)], true)
	for j, d := range factorDefs {
		if !eqNaN(valsFull[j], valsAt[j]) {
			t.Errorf("全长切片与全量不一致 %s: %v vs %v", d.Key, valsFull[j], valsAt[j])
		}
	}
}

// TestComputeWideRowOptsParity skipChip 模式：筹码列 NaN、其余列与全量逐列相等。
func TestComputeWideRowOptsParity(t *testing.T) {
	bars := genTrendBars(150, 12, 0.2)
	meta := wideStockMeta{Name: "对拍股"}
	withChip := computeWideRow("600001", meta, bars)
	noChip := computeWideRowOpts("600001", meta, bars, false)
	for j, d := range factorDefs {
		switch d.Key {
		case "chip_profit", "chip_avg_cost", "chip_bars":
			if !math.IsNaN(noChip[j]) {
				t.Errorf("skipChip 模式筹码列 %s 应为 NaN，得到 %v", d.Key, noChip[j])
			}
			if math.IsNaN(withChip[j]) {
				t.Errorf("全量模式筹码列 %s 不应为 NaN（150 根可算）", d.Key)
			}
		default:
			if !eqNaN(withChip[j], noChip[j]) {
				t.Errorf("非筹码列 %s 两模式不一致: %v vs %v", d.Key, withChip[j], noChip[j])
			}
		}
	}
}

func TestTreeUsesChipFactor(t *testing.T) {
	noChip := allOf(leafV("close", ">", 5), leafTrue("bull_align"))
	if treeUsesChipFactor(&noChip) {
		t.Fatal("无筹码因子误判")
	}
	withChip := allOf(leafV("chip_profit", "<", 10))
	if !treeUsesChipFactor(&withChip) {
		t.Fatal("chip_profit 叶子未识别")
	}
	refChip := CondNode{Any: []CondNode{leafRef("close", ">", "chip_avg_cost")}}
	if !treeUsesChipFactor(&refChip) {
		t.Fatal("ref 引用筹码因子未识别")
	}
}

// ---------- 信号日采样 ----------

func TestSampleSignalDates(t *testing.T) {
	axis := make([]string, 40)
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range axis {
		axis[i] = day.AddDate(0, 0, i).Format("2006-01-02")
	}
	got, err := sampleSignalDates(axis, 10, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	// eligible=34（尾部留 maxHold+1=6），窗口=axis[24:34]，等距 3 个=24/29/33。
	want := []string{axis[24], axis[29], axis[33]}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("采样错误: got %v want %v", got, want)
	}
	// 最晚信号日后必须有 maxHold+1 个轴日。
	if got[2] != axis[len(axis)-1-(5+1)] {
		t.Fatalf("右边界收缩错误: %v", got[2])
	}
	// 数据不足报错。
	if _, err := sampleSignalDates(axis[:5], 10, 3, 5); err == nil {
		t.Fatal("数据不足应报错")
	}
}

// ---------- 端到端 ----------

// seedBacktestDB 铺 3 只股（600001 恒 10 元命中、600002 恒 1 元不命中、600003 ST）
// 各 40 根 + states 宇宙字典。返回日期轴。内存库 cache=shared 测试间共享，
// 进场清 + t.Cleanup 退场清，防污染其他测试的「恰好 N 条」断言。
func seedBacktestDB(t *testing.T) []string {
	t.Helper()
	cleanBacktestTables := func() {
		common.DB.Where("1=1").Delete(&model.DailyBar{})
		common.DB.Where("1=1").Delete(&model.MarketSyncState{})
		common.DB.Where("1=1").Delete(&model.RecommendationBatch{})
		common.DB.Where("1=1").Delete(&model.Recommendation{})
	}
	cleanBacktestTables()
	t.Cleanup(cleanBacktestTables)

	stocks := []struct {
		symbol string
		name   string
		price  float64
	}{
		{"600001", "命中股", 10}, {"600002", "低价股", 1}, {"600003", "ST风险", 10},
	}
	var axis []string
	for _, s := range stocks {
		bars := flatBars(40, s.price)
		for _, b := range bars {
			row := model.DailyBar{
				Symbol: s.symbol, Market: "cn", TradeDate: b.TradeDate,
				Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
				Volume: b.Volume, Amount: b.Amount, TurnoverRate: b.TurnoverRate, Source: "eastmoney",
			}
			if err := common.DB.Create(&row).Error; err != nil {
				t.Fatalf("铺日线失败: %v", err)
			}
		}
		if axis == nil {
			for _, b := range bars {
				axis = append(axis, b.TradeDate)
			}
		}
		st := model.MarketSyncState{
			Symbol: s.symbol, Market: "cn", Name: s.name,
			InitStatus: "done", LastBarDate: bars[len(bars)-1].TradeDate, BarsCount: 40,
		}
		if err := common.DB.Create(&st).Error; err != nil {
			t.Fatalf("铺 states 失败: %v", err)
		}
	}
	return axis
}

// fakeBench 与个股同日期轴的恒定基准（收益恒 0 → alpha=个股收益）。
func fakeBench(axis []string) []datasource.Bar {
	out := make([]datasource.Bar, len(axis))
	for i, d := range axis {
		out[i] = datasource.Bar{TradeDate: d, Close: 3000}
	}
	return out
}

// TestBacktestRunEndToEnd 全流程：命中/排除/ST/统计/alpha/费率损耗全部手工验算。
func TestBacktestRunEndToEnd(t *testing.T) {
	setupTestDB(t)
	axis := seedBacktestDB(t)
	svc := &BacktestService{benchFn: func(ctx context.Context) []datasource.Bar { return fakeBench(axis) }}

	tree := allOf(leafV("close", ">", 5))
	res, err := svc.Run(context.Background(), 1, BacktestRequest{
		Tree: &tree, LookbackDays: 10, SignalCount: 2, HoldDays: []int{2}, PerStockCap: 20000,
	})
	if err != nil {
		t.Fatalf("回测失败: %v", err)
	}
	if res.Universe != 3 || res.StSkipped != 1 || res.AdjustSuspect != 0 {
		t.Fatalf("宇宙计数错误: %+v", res)
	}
	if len(res.SignalDates) != 2 || len(res.Days) != 2 {
		t.Fatalf("信号日数错误: %v", res.SignalDates)
	}
	if len(res.Stats) != 1 || res.Stats[0].HoldDays != 2 {
		t.Fatalf("持有期统计缺失: %+v", res.Stats)
	}
	st := res.Stats[0]
	// 600001 两个信号日各成交一笔（600002 不命中、600003 ST 排除）。
	if st.Trades != 2 {
		t.Fatalf("期望 2 笔成交，得到 %d（skip: %+v）", st.Trades, st)
	}
	// 恒定价格 10 元、2000 股：纯费用损耗 ret=-0.1%（买 20000+5；卖 20000−5−10）。
	if math.Abs(st.AvgReturnPct-(-0.1)) > 0.005 {
		t.Fatalf("费用损耗收益不符: %v want -0.10", st.AvgReturnPct)
	}
	if st.WinRate != 0 {
		t.Fatalf("恒定价格不应有胜率: %v", st.WinRate)
	}
	// 基准恒 3000 → alpha=收益本身。
	if st.AlphaSample != 2 || math.Abs(st.AvgAlphaPct-(-0.1)) > 0.005 {
		t.Fatalf("alpha 不符: sample=%d avg=%v", st.AlphaSample, st.AvgAlphaPct)
	}
	for _, day := range res.Days {
		if day.Matched != 1 || day.Taken != 1 {
			t.Fatalf("逐日计数错误: %+v", day)
		}
	}
	if len(st.BestTrades) != 2 || st.BestTrades[0].Symbol != "600001" {
		t.Fatalf("样本明细缺失: %+v", st.BestTrades)
	}
}

// TestBacktestRunInflightMutex 全局互斥：进行中第二个请求直接拒绝。
func TestBacktestRunInflightMutex(t *testing.T) {
	setupTestDB(t)
	backtestInflight.Store(true)
	defer backtestInflight.Store(false)
	svc := &BacktestService{}
	if _, err := svc.Run(context.Background(), 1, BacktestRequest{StrategyKey: "vol-break-20d"}); err == nil {
		t.Fatal("互斥期应拒绝")
	}
}

// TestBacktestAdjustSuspectExcluded 断层股被剔除且透明计数。
func TestBacktestAdjustSuspectExcluded(t *testing.T) {
	setupTestDB(t)
	axis := seedBacktestDB(t)
	// 把 600001 中段人为砸出 -40% 断层（模拟除权未重锚）。
	if err := common.DB.Model(&model.DailyBar{}).
		Where("symbol = ? AND trade_date >= ?", "600001", axis[20]).
		Update("close", 6.0).Error; err != nil {
		t.Fatal(err)
	}
	svc := &BacktestService{benchFn: func(ctx context.Context) []datasource.Bar { return fakeBench(axis) }}
	tree := allOf(leafV("close", ">", 0.5))
	res, err := svc.Run(context.Background(), 1, BacktestRequest{
		Tree: &tree, LookbackDays: 10, SignalCount: 2, HoldDays: []int{2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.AdjustSuspect != 1 {
		t.Fatalf("断层股应被剔除计数 1，得到 %d", res.AdjustSuspect)
	}
	// 命中只剩 600002（低价股 close=1 > 0.5）。
	if res.Stats[0].Trades != 2 {
		t.Fatalf("剩余 600002 两信号日应各成交一笔: %+v", res.Stats[0])
	}
	for _, tr := range res.Stats[0].BestTrades {
		if tr.Symbol == "600001" {
			t.Fatal("断层股不应出现在成交样本中")
		}
	}
}

// ---------- 推荐批次回验 ----------

func TestBatchBacktestAlpha(t *testing.T) {
	setupTestDB(t)
	axis := seedBacktestDB(t)
	// 批次日=第 10 根（2025-01-11 盘后生成语义）。
	recTime := time.Date(2025, 1, 11, 16, 0, 0, 0, time.Local)
	batch := model.RecommendationBatch{
		UserID: 1, Type: model.RecTypeShortTerm, Market: "cn", Strategy: "momentum",
		Title: "测试批次", Status: model.RecStatusSuccess, CreatedAt: recTime,
	}
	if err := common.DB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	picks := []model.Recommendation{
		{BatchID: batch.ID, UserID: 1, Symbol: "600001", Market: "cn", Name: "命中股", Action: "buy", RefPrice: 10},
		{BatchID: batch.ID, UserID: 1, Symbol: "999999", Market: "cn", Name: "无数据股", Action: "watch", RefPrice: 5},
	}
	for i := range picks {
		if err := common.DB.Create(&picks[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	svc := &BacktestService{benchFn: func(ctx context.Context) []datasource.Bar { return fakeBench(axis) }}
	res, err := svc.BatchBacktest(context.Background(), 1, BatchBacktestRequest{BatchID: batch.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 1 || res.Picks != 2 || len(res.Rows) != 2 {
		t.Fatalf("批次/条目计数错误: %+v", res)
	}
	if len(res.Stats) != 3 {
		t.Fatalf("应有 5/10/20 三个持有期: %d", len(res.Stats))
	}
	for _, st := range res.Stats {
		// 600001 数据充足成交；999999 无日线 no_data。
		if st.Trades != 1 || st.NoData != 1 {
			t.Fatalf("hold=%d 期望 1 成交 1 无数据: %+v", st.HoldDays, st)
		}
		// 恒定价格纯费用损耗 -0.1%，基准恒 0 → alpha=-0.1，落 -5%~0% 桶。
		if st.AlphaSample != 1 || math.Abs(st.AvgAlphaPct-(-0.1)) > 0.005 {
			t.Fatalf("hold=%d alpha 错误: %+v", st.HoldDays, st)
		}
		total := 0
		for _, b := range st.AlphaHist {
			total += b.Count
		}
		if total != 1 {
			t.Fatalf("直方图总数应 1: %+v", st.AlphaHist)
		}
		if st.AlphaHist[2].Count != 1 { // 桶序: <-10 / -10~-5 / -5~0 / 0~5 / 5~10 / >10
			t.Fatalf("alpha=-0.1 应落 -5~0 桶: %+v", st.AlphaHist)
		}
	}
	// 用户隔离：他人批次不可见。
	if _, err := svc.BatchBacktest(context.Background(), 2, BatchBacktestRequest{BatchID: batch.ID}); err == nil {
		t.Fatal("跨用户回验应报错")
	}
}

// TestAlphaBucketIndex 直方图分桶边界。
func TestAlphaBucketIndex(t *testing.T) {
	cases := []struct {
		a    float64
		want int
	}{{-15, 0}, {-10, 1}, {-7, 1}, {-5, 2}, {-0.1, 2}, {0, 3}, {4.9, 3}, {5, 4}, {9.9, 4}, {10, 5}, {20, 5}}
	for _, c := range cases {
		if got := alphaBucketIndex(c.a); got != c.want {
			t.Errorf("alpha=%v 应落桶 %d，得到 %d", c.a, c.want, got)
		}
	}
	if alphaBucketLabel(0) != "<-10%" || alphaBucketLabel(5) != ">+10%" {
		t.Errorf("端桶标签错误: %s / %s", alphaBucketLabel(0), alphaBucketLabel(5))
	}
}

// TestNormalizeHoldDays 持有期参数校验。
func TestNormalizeHoldDays(t *testing.T) {
	if hs, _ := normalizeHoldDays(nil); len(hs) != 3 || hs[0] != 5 || hs[2] != 20 {
		t.Fatalf("默认持有期错误: %v", hs)
	}
	if hs, _ := normalizeHoldDays([]int{20, 5, 5}); len(hs) != 2 || hs[0] != 5 {
		t.Fatalf("去重升序错误: %v", hs)
	}
	if _, err := normalizeHoldDays([]int{0}); err == nil {
		t.Fatal("非法持有期应报错")
	}
	if _, err := normalizeHoldDays([]int{1, 2, 3, 4}); err == nil {
		t.Fatal("超过 3 个应报错")
	}
}
