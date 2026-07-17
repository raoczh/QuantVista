package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// ---------- S0-2 统一执行模拟器：标签结算手工验算 ----------

// labelBarsFixture 信号日 + 若干持有日（价格手工设计，便于精确验算）。
func labelBarsFixture() []datasource.Bar {
	return []datasource.Bar{
		{TradeDate: "2026-06-01", Open: 9.9, High: 10.1, Low: 9.8, Close: 10.0},   // 信号日
		{TradeDate: "2026-06-02", Open: 10.0, High: 10.9, Low: 9.8, Close: 10.2},  // 买入日（T+1 当日不判障碍）
		{TradeDate: "2026-06-03", Open: 10.3, High: 10.8, Low: 10.1, Close: 10.6}, // 持有第 2 日
		{TradeDate: "2026-06-04", Open: 10.7, High: 11.2, Low: 10.4, Close: 11.0}, // 持有第 3 日
		{TradeDate: "2026-06-05", Open: 11.0, High: 11.5, Low: 10.9, Close: 11.3},
	}
}

// TestSimulateLabelHold_Fixed 固定持有期：毛/净收益与 MFE/MAE 手工验算。
// entry=10.00×2000 股：buyAmount=20000、佣金 5（万2.5 最低5）、cost=20005；
// horizon=3 出场 bars[3] close=11.0：sellAmount=22000、佣金 5.5+印花税 11（万5），
// net=(21983.5-20005)/20005=9.89%；gross=10%；MFE=(11.2-10)/10=12%、MAE=(9.8-10)/10=-2%。
func TestSimulateLabelHold_Fixed(t *testing.T) {
	out := simulateLabelHold(labelBarsFixture(), 0, "600000", "某某股份", 3, 20000, 0, 0, "", "")
	if out.Status != btTraded {
		t.Fatalf("应成交，得到 %s", out.Status)
	}
	if out.BuyDate != "2026-06-02" || out.BuyPrice != 10.0 {
		t.Fatalf("入场应 2026-06-02@10.0: %+v", out)
	}
	if out.SellDate != "2026-06-04" || out.SellPrice != 11.0 {
		t.Fatalf("出场应 2026-06-04@11.0: %+v", out)
	}
	if out.GrossPct != 10.0 {
		t.Fatalf("毛收益应 10%%，得到 %v", out.GrossPct)
	}
	if out.NetPct != 9.89 {
		t.Fatalf("净收益应 9.89%%，得到 %v", out.NetPct)
	}
	if out.MfePct != 12.0 || out.MaePct != -2.0 {
		t.Fatalf("MFE/MAE 应 12/-2，得到 %v/%v", out.MfePct, out.MaePct)
	}
}

// TestSimulateLabelHold_Barriers 障碍语义：T+1 买入日不判（买入日 high=10.9>止盈价
// 也不触发）；持有第 2 日触止盈按障碍价成交；同日双触保守取止损。
func TestSimulateLabelHold_Barriers(t *testing.T) {
	bars := labelBarsFixture()
	// 止盈 10.7：买入日 high=10.9 不判（T+1），06-03 high=10.8≥10.7 触发。
	out := simulateLabelHold(bars, 0, "600000", "某某股份", 3, 20000, 10.7, 0, "", "")
	if !out.HitTakeProfit || out.SellDate != "2026-06-03" || out.SellPrice != 10.7 {
		t.Fatalf("应 06-03 触止盈@10.7: %+v", out)
	}
	if out.GrossPct != 7.0 {
		t.Fatalf("止盈毛收益应 7%%，得到 %v", out.GrossPct)
	}
	// MFE 只统计到出场日，且出场日只计入确定经历的价位（开盘 10.3 与障碍价 10.7，
	// 触发后的当日 high=10.8 不再计入）：max(买入日 10.9) → 9%。
	if out.MfePct != 9.0 {
		t.Fatalf("MFE 应 9%%，得到 %v", out.MfePct)
	}

	// 同日双触（06-03 low=10.1、high=10.8；止损 10.15、止盈 10.75）→ 保守取止损。
	out = simulateLabelHold(bars, 0, "600000", "某某股份", 3, 20000, 10.75, 10.15, "", "")
	if !out.HitStopLoss || out.HitTakeProfit || out.SellPrice != 10.15 {
		t.Fatalf("同日双触应取止损@10.15: %+v", out)
	}
}

// TestSimulateLabelHold_Skips 一字板买不进 / 拨款不足一手 / 数据未覆盖 pending。
func TestSimulateLabelHold_Skips(t *testing.T) {
	bars := labelBarsFixture()
	// 次日开盘涨幅 ≥ 涨停阈值−0.5 判一字板：开盘 11.0（+10%）。
	limitUp := append([]datasource.Bar{}, bars...)
	limitUp[1] = datasource.Bar{TradeDate: "2026-06-02", Open: 11.0, High: 11.0, Low: 11.0, Close: 11.0}
	if out := simulateLabelHold(limitUp, 0, "600000", "某某股份", 3, 20000, 0, 0, "", ""); out.Status != btSkipLimitUp {
		t.Fatalf("一字板应 skip_limit_up，得到 %s", out.Status)
	}
	// 拨款 900 元买不起一手（10 元×100 股）。
	if out := simulateLabelHold(bars, 0, "600000", "某某股份", 3, 900, 0, 0, "", ""); out.Status != btSkipCash {
		t.Fatalf("不足一手应 skip_cash，得到 %s", out.Status)
	}
	// horizon=60 数据未覆盖 → pending。
	if out := simulateLabelHold(bars, 0, "600000", "某某股份", 60, 20000, 0, 0, "", ""); out.Status != btPending {
		t.Fatalf("数据未覆盖应 pending，得到 %s", out.Status)
	}
	// 信号根=末根（当晚结算的新标签）：数据未到是 pending 不是 skip_suspend。
	if out := simulateLabelHold(bars, len(bars)-1, "600000", "某某股份", 3, 20000, 0, 0, "", ""); out.Status != btPending {
		t.Fatalf("次日数据未到应 pending，得到 %s", out.Status)
	}
}

// TestSimulateLabelHold_MarketAxis 市场轴口径：推荐日次日停牌判 skip_suspend（不得用
// 复牌远期价格假装成交）；个股中途停牌不拉长实际持有跨度。
func TestSimulateLabelHold_MarketAxis(t *testing.T) {
	// 次日（06-02）停牌：个股信号根后第一根是 06-03，与市场次日 06-02 不符。
	gap := []datasource.Bar{
		{TradeDate: "2026-06-01", Open: 9.9, High: 10.1, Low: 9.8, Close: 10.0},
		{TradeDate: "2026-06-03", Open: 10.3, High: 10.8, Low: 10.1, Close: 10.6},
		{TradeDate: "2026-06-04", Open: 10.7, High: 11.2, Low: 10.4, Close: 11.0},
	}
	if out := simulateLabelHold(gap, 0, "600000", "某某股份", 3, 20000, 0, 0, "2026-06-02", "2026-06-04"); out.Status != btSkipSuspend {
		t.Fatalf("推荐日次日停牌应 skip_suspend，得到 %s", out.Status)
	}
	// 中途停牌（06-03 缺）：horizon=3 市场到期日=06-04，按市场轴当日出场——
	// 旧的按个股第 N 根 K 线推进会顺延到 06-05（把持有期拉长）。
	mid := []datasource.Bar{
		{TradeDate: "2026-06-01", Open: 9.9, High: 10.1, Low: 9.8, Close: 10.0},
		{TradeDate: "2026-06-02", Open: 10.0, High: 10.9, Low: 9.8, Close: 10.2},
		{TradeDate: "2026-06-04", Open: 10.7, High: 11.2, Low: 10.4, Close: 11.0},
		{TradeDate: "2026-06-05", Open: 11.0, High: 11.5, Low: 10.9, Close: 11.3},
	}
	out := simulateLabelHold(mid, 0, "600000", "某某股份", 3, 20000, 0, 0, "2026-06-02", "2026-06-04")
	if out.Status != btTraded || out.SellDate != "2026-06-04" || out.SellPrice != 11.0 {
		t.Fatalf("中途停牌应仍在市场到期日 06-04 出场: %+v", out)
	}
	// 到期日（06-04）个股停牌：顺延复牌首根（06-05）收盘卖出并记 Deferred。
	expSusp := []datasource.Bar{
		{TradeDate: "2026-06-01", Open: 9.9, High: 10.1, Low: 9.8, Close: 10.0},
		{TradeDate: "2026-06-02", Open: 10.0, High: 10.9, Low: 9.8, Close: 10.2},
		{TradeDate: "2026-06-03", Open: 10.3, High: 10.8, Low: 10.1, Close: 10.6},
		{TradeDate: "2026-06-05", Open: 11.0, High: 11.5, Low: 10.9, Close: 11.3},
	}
	out = simulateLabelHold(expSusp, 0, "600000", "某某股份", 3, 20000, 0, 0, "2026-06-02", "2026-06-04")
	if out.Status != btTraded || out.SellDate != "2026-06-05" || out.Deferred != 1 {
		t.Fatalf("到期日停牌应顺延复牌首根且 Deferred=1: %+v", out)
	}
}

// TestSimulateLabelHold_ExcursionToExit 障碍出场日的 MFE/MAE 只计入确定经历的价位
//（开盘价与障碍成交价）——触发后的同日行情未经历，不得夸大。
func TestSimulateLabelHold_ExcursionToExit(t *testing.T) {
	bars := []datasource.Bar{
		{TradeDate: "2026-06-01", Open: 9.9, High: 10.1, Low: 9.8, Close: 10.0},  // 信号日
		{TradeDate: "2026-06-02", Open: 10.0, High: 10.2, Low: 9.95, Close: 10.1}, // 买入日
		// 出场日：低开触止损 9.5，随后全天暴力拉升 high=12（出场后的行情）。
		{TradeDate: "2026-06-03", Open: 9.8, High: 12.0, Low: 9.4, Close: 11.8},
		{TradeDate: "2026-06-04", Open: 11.8, High: 12.5, Low: 11.5, Close: 12.2},
	}
	out := simulateLabelHold(bars, 0, "600000", "某某股份", 3, 20000, 0, 9.5, "", "")
	if !out.HitStopLoss || out.SellPrice != 9.5 {
		t.Fatalf("应触止损@9.5: %+v", out)
	}
	// MFE：买入日 high=10.2（+2%）、出场日只计 open=9.8 与 exit=9.5 → MFE=2%。
	// 旧口径会把出场日 high=12（+20%）计入，夸大成 20%。
	if out.MfePct != 2.0 {
		t.Fatalf("MFE 应 2%%（出场日拉升不计入），得到 %v", out.MfePct)
	}
	// MAE：min(买入日 9.95−0.5%，出场日 open 9.8=−2%、exit 9.5=−5%) → −5%；
	// 出场日 low=9.4（−6%）在触发时点之后与否不可知，保守不计。
	if out.MaePct != -5.0 {
		t.Fatalf("MAE 应 -5%%，得到 %v", out.MaePct)
	}
}

// ---------- S1-1 regime 三档判定 ----------

func regimeBars(up bool, n int) []datasource.Bar {
	bars := make([]datasource.Bar, n)
	price := 3000.0
	for i := 0; i < n; i++ {
		if up {
			price += 5
		} else {
			price -= 5
		}
		bars[i] = datasource.Bar{TradeDate: "d", Close: price}
	}
	return bars
}

// TestComputeRegime 表驱动：强市 offense / 弱市 defense / 数据缺失 neutral。
func TestComputeRegime(t *testing.T) {
	p := defaultRegimeParams()

	// 全部缺失 → neutral + 数据不足声明。
	r := computeRegime(nil, nil, 0, false, p)
	if r.Regime != RegimeNeutral || len(r.Signals) != 1 {
		t.Fatalf("数据缺失应 neutral: %+v", r)
	}

	// 强市：MA20/MA60 上方(+2) + 涨家 60%(+1) + 涨停/跌停 60/10(+1) + 主力净流入(+1) = 5 → offense。
	bull := computeRegime(regimeBars(true, 80),
		&datasource.Breadth{Advances: 3000, Declines: 2000, LimitUp: 60, LimitDown: 10}, 50, true, p)
	if bull.Regime != RegimeOffense || bull.Score != 5 {
		t.Fatalf("强市应 offense(5)，得到 %s(%d) %v", bull.Regime, bull.Score, bull.Signals)
	}

	// 弱市：均线下方(-2) + 涨家 30%(-1) + 跌停多(-1) + 主力净流出 500 亿(-1) = -5 → defense。
	bear := computeRegime(regimeBars(false, 80),
		&datasource.Breadth{Advances: 1500, Declines: 3500, LimitUp: 5, LimitDown: 80}, -500, true, p)
	if bear.Regime != RegimeDefense || bear.Score != -5 {
		t.Fatalf("弱市应 defense(-5)，得到 %s(%d) %v", bear.Regime, bear.Score, bear.Signals)
	}

	// 中性：均线上方(+2) + 涨家 48%(0) + 涨停/跌停 20/15(0) + 小幅净流出(0) = 2 → neutral。
	mid := computeRegime(regimeBars(true, 80),
		&datasource.Breadth{Advances: 2400, Declines: 2600, LimitUp: 20, LimitDown: 15}, -50, true, p)
	if mid.Regime != RegimeNeutral || mid.Score != 2 {
		t.Fatalf("中性应 neutral(2)，得到 %s(%d)", mid.Regime, mid.Score)
	}
}

// TestApplyRegimeGate 影子模式不改写 action、enforce 打开才改写；非 defense 零门控。
func TestApplyRegimeGate(t *testing.T) {
	picks := []recPick{
		{Symbol: "600000", Action: model.RecActionBuy},
		{Symbol: "600001", Action: model.RecActionWatch},
	}
	// 影子（默认）：记录 would_be 但 action 不动。
	gates := applyRegimeGate(picks, RegimeDefense, false)
	if len(gates) != 1 || gates[0].Symbol != "600000" || gates[0].WouldBeAction != model.RecActionWatch {
		t.Fatalf("影子门控应只记 buy 条目: %+v", gates)
	}
	if picks[0].Action != model.RecActionBuy {
		t.Fatalf("影子模式不得改写 action，得到 %s", picks[0].Action)
	}
	// enforce：改写 buy→watch 并追加风险。
	gates = applyRegimeGate(picks, RegimeDefense, true)
	if picks[0].Action != model.RecActionWatch || len(picks[0].Risks) == 0 {
		t.Fatalf("enforce 应改写 buy→watch 并注明: %+v", picks[0])
	}
	if len(gates) != 1 {
		t.Fatalf("enforce 门控数应 1: %+v", gates)
	}
	// 非 defense 零门控。
	if g := applyRegimeGate(picks, RegimeNeutral, true); g != nil {
		t.Fatalf("neutral 不应产生门控: %+v", g)
	}
}

// TestRegimeEnforceDefaultOff feature flag 必须默认关闭（影子模式转正凭数据评审）。
func TestRegimeEnforceDefaultOff(t *testing.T) {
	if recRegimeEnforce {
		t.Fatalf("recRegimeEnforce 必须默认 false（影子模式）")
	}
}

// ---------- S1-2 仓位公式（单位与整批归一化） ----------

// TestComputePositionPcts 手工验算：
//
//	日波动 2% → 年化 31.75% → 上限 15 档；base=0.18/0.3175×100=56.7→15；neutral×0.6=9。
//	日波动 1% → 年化 15.87% → 上限 20 档；base=113.4→20；×0.6=12。
//	日波动 1% + 批内相关 0.85 → 12×0.70=8.4。
func TestComputePositionPcts(t *testing.T) {
	p := defaultSizingParams()
	outs := computePositionPcts([]sizingInput{
		{Symbol: "A", Vol20Daily: 2},
		{Symbol: "B", Vol20Daily: 1},
		{Symbol: "C", Vol20Daily: 1, MaxCorr: 0.85},
		{Symbol: "D", Vol20Daily: 0}, // 样本不足
	}, RegimeNeutral, p)
	if outs[0].PositionPct != 9 {
		t.Fatalf("A 应 9%%，得到 %v（%s）", outs[0].PositionPct, outs[0].Why)
	}
	if outs[1].PositionPct != 12 {
		t.Fatalf("B 应 12%%，得到 %v", outs[1].PositionPct)
	}
	if outs[2].PositionPct != 8.4 {
		t.Fatalf("C 应 8.4%%（×0.70），得到 %v", outs[2].PositionPct)
	}
	if outs[3].PositionPct != 0 || outs[3].Why == "" {
		t.Fatalf("D 波动缺失应缺席并说明: %+v", outs[3])
	}
}

// TestComputePositionPctsNormalize defense 预算 30%：三只各 base=20×0.3=6，Σ=18 不缩；
// 五只低波动票 Σ 超预算按比例缩，缩后 Σ≈预算。
func TestComputePositionPctsNormalize(t *testing.T) {
	p := defaultSizingParams()
	ins := make([]sizingInput, 8)
	for i := range ins {
		ins[i] = sizingInput{Symbol: "S", Vol20Daily: 0.6} // 年化 9.5% → 上限 25；base=0.18/0.0952×100=189→25
	}
	outs := computePositionPcts(ins, RegimeOffense, p) // offense 系数 1.0：每只 25，Σ=200 > 预算 100
	var sum float64
	for _, o := range outs {
		sum += o.PositionPct
	}
	if sum < 99.5 || sum > 100.5 {
		t.Fatalf("整批归一化后 Σ 应≈100，得到 %v", sum)
	}
	if outs[0].PositionPct != outs[7].PositionPct {
		t.Fatalf("同参数条目缩放后应相等: %v vs %v", outs[0].PositionPct, outs[7].PositionPct)
	}
}

// TestPairwiseCorr 完全同向序列相关≈1；反向≈-1；样本不足返回 0。
func TestPairwiseCorr(t *testing.T) {
	a := make([]float64, 61)
	b := make([]float64, 61)
	c := make([]float64, 61)
	pa, pb, pc := 10.0, 20.0, 30.0
	for i := 0; i < 61; i++ {
		d := 0.01
		if i%3 == 0 {
			d = -0.02
		}
		pa *= 1 + d
		pb *= 1 + d // 与 a 完全同向
		pc *= 1 - d // 与 a 完全反向
		a[i], b[i], c[i] = pa, pb, pc
	}
	if corr := pairwiseCorr(a, b); corr < 0.999 {
		t.Fatalf("同向序列相关应≈1，得到 %v", corr)
	}
	if corr := pairwiseCorr(a, c); corr > -0.99 {
		t.Fatalf("反向序列相关应≈-1，得到 %v", corr)
	}
	if corr := pairwiseCorr(a[:10], b[:10]); corr != 0 {
		t.Fatalf("样本不足应返回 0，得到 %v", corr)
	}
}

// TestPairwiseCorrAligned 停牌错位场景：两条完全同向的序列，其中一条中途停牌 5 天——
// 位置对齐会把不同交易日的收益配对（相关性失真），日期交集对齐仍应 ≈1。
func TestPairwiseCorrAligned(t *testing.T) {
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	var datesA, datesB []string
	var a, b []float64
	pa, pb := 10.0, 20.0
	for i := 0; i < 70; i++ {
		d := 0.01
		if i%3 == 0 {
			d = -0.02
		}
		pa *= 1 + d
		pb *= 1 + d // 与 a 完全同向
		td := day.AddDate(0, 0, i).Format("2006-01-02")
		datesA = append(datesA, td)
		a = append(a, pa)
		if i >= 20 && i < 25 {
			continue // B 股停牌 5 天：B 序列缺这 5 根
		}
		datesB = append(datesB, td)
		b = append(b, pb)
	}
	if corr := pairwiseCorrAligned(datesA, a, datesB, b); corr < 0.99 {
		t.Fatalf("日期对齐后同向序列相关应≈1，得到 %v", corr)
	}
	// 对照：位置对齐（尾部截齐）把错位的交易日配对，相关性明显失真。
	if corr := pairwiseCorr(a, b); corr > 0.99 {
		t.Fatalf("位置对齐在停牌错位下不应仍≈1（否则本用例失去意义），得到 %v", corr)
	}
	// 交集不足 21 个交易日返回 0 不判。
	if corr := pairwiseCorrAligned(datesA[:10], a[:10], datesB[:10], b[:10]); corr != 0 {
		t.Fatalf("交集样本不足应返回 0，得到 %v", corr)
	}
	// 长度不一致的防御路径。
	if corr := pairwiseCorrAligned(datesA, a[:5], datesB, b); corr != 0 {
		t.Fatalf("dates 与 closes 长度不符应返回 0，得到 %v", corr)
	}
}

// ---------- S0-5 事件 + 标签端到端（DB） ----------

func cleanLabelTables(t *testing.T) {
	t.Helper()
	// trading_calendars 一并清空：标签结算的市场轴（marketAxis 回退日历）若吃到其他
	// 测试残留的日历，会让本文件的固定日期 fixture 走市场轴口径而非旧格子口径。
	for _, tbl := range []string{"recommendation_labels", "recommendation_candidate_events",
		"recommendation_batches", "recommendations", "daily_bars", "market_sync_states", "positions",
		"trading_calendars"} {
		common.DB.Exec("DELETE FROM " + tbl)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"recommendation_labels", "recommendation_candidate_events",
			"recommendation_batches", "recommendations", "daily_bars", "market_sync_states", "positions",
			"trading_calendars"} {
			common.DB.Exec("DELETE FROM " + tbl)
		}
	})
}

// seedLabelBars 给标的落一段日线（从 startDate 起 n 个自然日，每日 +1% 平滑上涨）。
func seedLabelBars(t *testing.T, symbol, startDate string, n int, base float64) {
	t.Helper()
	d, _ := time.Parse("2006-01-02", startDate)
	price := base
	for i := 0; i < n; i++ {
		td := d.AddDate(0, 0, i).Format("2006-01-02")
		open := price
		price = price * 1.01
		bar := model.DailyBar{
			Symbol: symbol, Market: "cn", TradeDate: td,
			Open: open, High: price * 1.005, Low: open * 0.995, Close: price,
			Volume: 100000, Amount: price * 1e7, Source: "eastmoney",
		}
		if err := common.DB.Create(&bar).Error; err != nil {
			t.Fatalf("seed bar 失败: %v", err)
		}
	}
}

// TestRecordBatchFactsAndAdvance 端到端：事件与标签创建 → 统一模拟结算 → 影子标签同样成熟。
func TestRecordBatchFactsAndAdvance(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)

	signalDate := "2026-06-01"
	created, _ := time.Parse("2006-01-02 15:04", "2026-06-01 15:30")
	batch := &model.RecommendationBatch{
		UserID: 1, Type: model.RecTypeShortTerm, Market: "cn", Strategy: "momentum",
		Status: model.RecStatusSuccess, Regime: RegimeDefense, CreatedAt: created,
	}
	if err := common.DB.Create(batch).Error; err != nil {
		t.Fatalf("建批次失败: %v", err)
	}
	detail, _ := json.Marshal(recPick{Symbol: "600100", Action: model.RecActionBuy})
	rec := model.Recommendation{
		BatchID: batch.ID, UserID: 1, Symbol: "600100", Market: "cn", Name: "甲",
		Action: model.RecActionBuy, RefPrice: 10, DetailJSON: string(detail),
	}
	if err := common.DB.Create(&rec).Error; err != nil {
		t.Fatalf("建条目失败: %v", err)
	}

	pool := []candidate{
		{Symbol: "600100", Market: "cn", Name: "甲", Price: 10, Score: 80, SentToLLM: true,
			Sources: []string{"gainer"}, Factors: &candFactors{Chg5d: 6}, TurnoverRate: 8,
			lastBarDate: signalDate, lastBarClose: 10},
		{Symbol: "600200", Market: "cn", Name: "乙", Price: 20, Score: 70, SentToLLM: true,
			Sources: []string{"active"}},
		{Symbol: "600300", Market: "cn", Name: "丙", Price: 30, Score: 0,
			Sources: []string{"turnover"}, Excluded: "换手率 35.00% 超过 30%（极端换手，无论位置高低大概率异常）"},
	}
	picks := []recPick{{Symbol: "600100", Action: model.RecActionBuy}}
	gates := []gateNote{{Symbol: "600100", GateType: model.GateRegimeShadow, WouldBeAction: model.RecActionWatch, Reason: "防守档影子"}}
	recordBatchFacts(batch, pool, []model.Recommendation{rec}, picks, []recReject{{Symbol: "600200", Reason: "量价背离"}}, gates, map[string]string{"600100": "银行"})

	// 事件断言：3 只候选各一行，stage 与门控正确。
	var events []model.RecommendationCandidateEvent
	common.DB.Order("symbol").Find(&events)
	if len(events) != 3 {
		t.Fatalf("应 3 条事件，得到 %d", len(events))
	}
	if events[0].CandidateStage != model.CandStagePicked || events[0].GateType != model.GateRegimeShadow ||
		events[0].WouldBeAction != model.RecActionWatch || events[0].PostGateAction != model.RecActionBuy {
		t.Fatalf("picked 事件（含影子门控）不符: %+v", events[0])
	}
	if events[1].CandidateStage != model.CandStageLLMList || events[1].RejectionReason != "量价背离" {
		t.Fatalf("llm_list 落选事件不符: %+v", events[1])
	}
	if events[2].CandidateStage != model.CandStageFiltered {
		t.Fatalf("filtered 事件不符: %+v", events[2])
	}

	// 标签断言：3 候选 × 5 持有期 = 15 行 pending；picked 挂 rec_id、其余挂事件。
	var labels []model.RecommendationLabel
	common.DB.Find(&labels)
	if len(labels) != 15 {
		t.Fatalf("应 15 条标签，得到 %d", len(labels))
	}
	for _, l := range labels {
		if l.MaturityStatus != model.LabelPending || l.EntryMode != model.EntryModeNextOpen {
			t.Fatalf("初始标签应 pending/next_open: %+v", l)
		}
		if l.Symbol == "600100" {
			if l.RecommendationID != rec.ID || l.Regime != RegimeDefense || l.Industry != "银行" ||
				l.EntryChg5dPct != 6 || l.EntryScore != 80 {
				t.Fatalf("picked 标签归因维度不符: %+v", l)
			}
		} else if l.RecommendationID != 0 || l.CandidateEventID == 0 {
			t.Fatalf("影子标签应挂事件行: %+v", l)
		}
	}

	// 结算：600100/600200 落 12 根日线（1/5/10 成熟，20/60 pending）；600300 无日线。
	seedLabelBars(t, "600100", signalDate, 12, 10)
	seedLabelBars(t, "600200", signalDate, 12, 20)
	if _, err := AdvanceRecommendationLabels(context.Background(), nil); err != nil {
		t.Fatalf("推进失败: %v", err)
	}

	var matured []model.RecommendationLabel
	common.DB.Where("maturity_status = ?", model.LabelMatured).Find(&matured)
	// 两只有日线的标的 × horizon 1/5/10 = 6 条成熟（20/60 数据不足仍 pending）。
	if len(matured) != 6 {
		t.Fatalf("应 6 条成熟标签，得到 %d", len(matured))
	}
	for _, l := range matured {
		if l.EntryDate == "" || l.EntryPrice <= 0 || l.ExitDate == "" || l.NetReturnPct == 0 {
			t.Fatalf("成熟标签应有完整入出场: %+v", l)
		}
		if l.HasBench {
			t.Fatalf("无基准时 HasBench 应 false: %+v", l)
		}
		if l.GrossReturnPct <= 0 {
			t.Fatalf("平滑上涨序列毛收益应为正: %+v", l)
		}
	}
	var pending int64
	common.DB.Model(&model.RecommendationLabel{}).Where("maturity_status = ?", model.LabelPending).Count(&pending)
	if pending != 9 { // 有日线两只的 20/60（4 条）+ 无日线一只的 5 条（signal 90 日内不判 no_data）
		t.Fatalf("pending 应 9 条，得到 %d", pending)
	}
}

// TestAdvanceLabelsNextDaySuspend 端到端：推荐日次日个股停牌——市场轴（回退交易
// 日历）给出真实次日，标签必须判 skip_suspend，不得用复牌后的远期 K 线假装成交。
func TestAdvanceLabelsNextDaySuspend(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)

	signalDate := "2026-06-01"
	// 市场日历：06-01 ~ 06-12 每日开市（cleanLabelTables 已清空日历，本用例自建轴）。
	d, _ := time.Parse("2006-01-02", signalDate)
	for i := 0; i < 12; i++ {
		common.DB.Exec("INSERT INTO trading_calendars (market, trade_date, is_open) VALUES ('cn', ?, 1)",
			d.AddDate(0, 0, i).Format("2006-01-02"))
	}
	// 个股：信号日有 bar，次日（06-02）停牌缺失，06-03 起复牌连续。
	seedLabelBars(t, "600100", signalDate, 1, 10)
	seedLabelBars(t, "600100", "2026-06-03", 10, 10)

	created, _ := time.Parse("2006-01-02 15:04", "2026-06-01 15:30")
	batch := &model.RecommendationBatch{UserID: 1, Type: model.RecTypeShortTerm, Market: "cn",
		Status: model.RecStatusSuccess, CreatedAt: created}
	common.DB.Create(batch)
	rec := model.Recommendation{BatchID: batch.ID, UserID: 1, Symbol: "600100", Market: "cn",
		Action: model.RecActionBuy, RefPrice: 10}
	common.DB.Create(&rec)
	label := model.RecommendationLabel{
		RecommendationID: rec.ID, HorizonDays: 5, EntryMode: model.EntryModeNextOpen,
		BatchID: batch.ID, UserID: 1, Symbol: "600100", Market: "cn",
		Type: model.RecTypeShortTerm, Action: model.RecActionBuy,
		SignalDate: signalDate, MaturityStatus: model.LabelPending, LabelVersion: labelVersion,
	}
	common.DB.Create(&label)

	if _, err := AdvanceRecommendationLabels(context.Background(), nil); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	var got model.RecommendationLabel
	common.DB.First(&got, label.ID)
	if got.MaturityStatus != model.LabelSkipped || got.SkipReason != btSkipSuspend {
		t.Fatalf("次日停牌应 skipped/skip_suspend（不得用复牌远期价格成交）: %+v", got)
	}
}
// TestBackfillActualLabels 持仓血缘存在时补建 actual_position 行并按实际买入价结算。
func TestBackfillActualLabels(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)

	signalDate := "2026-06-01"
	created, _ := time.Parse("2006-01-02 15:04", "2026-06-01 15:30")
	batch := &model.RecommendationBatch{UserID: 1, Type: model.RecTypeShortTerm, Market: "cn",
		Status: model.RecStatusSuccess, CreatedAt: created}
	common.DB.Create(batch)
	rec := model.Recommendation{BatchID: batch.ID, UserID: 1, Symbol: "600100", Market: "cn",
		Action: model.RecActionBuy, RefPrice: 10}
	common.DB.Create(&rec)
	pool := []candidate{{Symbol: "600100", Market: "cn", Name: "甲", Price: 10, SentToLLM: true}}
	recordBatchFacts(batch, pool, []model.Recommendation{rec},
		[]recPick{{Symbol: "600100", Action: model.RecActionBuy}}, nil, nil, nil)

	// 用户以 10.30 实际建仓（次日追高买入）。
	pos := model.Position{UserID: 1, Symbol: "600100", Market: "cn", Status: "holding",
		BuyPrice: 10.30, BuyDate: "2026-06-02", Quantity: 200, RecommendationID: rec.ID}
	if err := common.DB.Create(&pos).Error; err != nil {
		t.Fatalf("建持仓失败: %v", err)
	}
	seedLabelBars(t, "600100", signalDate, 12, 10)
	if _, err := AdvanceRecommendationLabels(context.Background(), nil); err != nil {
		t.Fatalf("推进失败: %v", err)
	}

	var actuals []model.RecommendationLabel
	common.DB.Where("entry_mode = ?", model.EntryModeActual).Order("horizon_days").Find(&actuals)
	if len(actuals) != len(model.LabelHorizons) {
		t.Fatalf("actual 行应 %d 条，得到 %d", len(model.LabelHorizons), len(actuals))
	}
	if actuals[0].PositionID != pos.ID || actuals[0].ActualBuyPrice != 10.30 {
		t.Fatalf("actual 行应带血缘与实际买入价: %+v", actuals[0])
	}
	// horizon=5 应成熟且入场价=实际买入价（与统一模拟 next_open 并列不混算）。
	var h5 model.RecommendationLabel
	common.DB.Where("entry_mode = ? AND horizon_days = 5", model.EntryModeActual).First(&h5)
	if h5.MaturityStatus != model.LabelMatured || h5.EntryPrice != 10.30 {
		t.Fatalf("actual h5 应按实际价结算: %+v", h5)
	}
	// 幂等：再跑一轮不重复建行。
	if _, err := AdvanceRecommendationLabels(context.Background(), nil); err != nil {
		t.Fatalf("二次推进失败: %v", err)
	}
	var cnt int64
	common.DB.Model(&model.RecommendationLabel{}).Where("entry_mode = ?", model.EntryModeActual).Count(&cnt)
	if int(cnt) != len(model.LabelHorizons) {
		t.Fatalf("actual 行应幂等不重复，得到 %d", cnt)
	}
}

// TestLabelRebaseAdjust S0-4 防前复权重锚：标签结算时 RefClose 与序列同日收盘偏差
// 超容差 → 止盈/止损按复权因子调整（原价位在重锚后的序列上永远触发/永不触发）。
func TestLabelRebaseAdjust(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)

	// 序列相对生成时点整体重锚为一半（如 10 送 10）：生成时收盘 20，重锚后 10。
	signalDate := "2026-06-01"
	seedLabelBars(t, "600100", signalDate, 12, 10) // 重锚后的序列 base=10
	created, _ := time.Parse("2006-01-02 15:04", "2026-06-01 15:30")
	batch := &model.RecommendationBatch{UserID: 1, Type: model.RecTypeShortTerm, Market: "cn",
		Status: model.RecStatusSuccess, CreatedAt: created}
	common.DB.Create(batch)
	// 生成时价位（旧基准）：止盈 21.4（=重锚后 10.7）。信号日收盘（旧基准）=20.2。
	detail, _ := json.Marshal(recPick{Symbol: "600100", Action: model.RecActionBuy, TakeProfit: 21.4, StopLoss: 19.0})
	rec := model.Recommendation{BatchID: batch.ID, UserID: 1, Symbol: "600100", Market: "cn",
		Action: model.RecActionBuy, RefPrice: 20.2, DetailJSON: string(detail)}
	common.DB.Create(&rec)

	var sigClose float64
	common.DB.Model(&model.DailyBar{}).Select("close").
		Where("symbol = ? AND trade_date = ?", "600100", signalDate).Scan(&sigClose)
	label := model.RecommendationLabel{
		RecommendationID: rec.ID, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
		BatchID: batch.ID, UserID: 1, Symbol: "600100", Market: "cn",
		Type: model.RecTypeShortTerm, Action: model.RecActionBuy,
		SignalDate: signalDate, MaturityStatus: model.LabelPending,
		RefDate: signalDate, RefClose: sigClose * 2, // 生成时的旧基准收盘=当前序列同日收盘×2
		LabelVersion: labelVersion,
	}
	common.DB.Create(&label)

	if _, err := AdvanceRecommendationLabels(context.Background(), nil); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	var got model.RecommendationLabel
	common.DB.First(&got, label.ID)
	if got.MaturityStatus != model.LabelMatured {
		t.Fatalf("应成熟: %+v", got)
	}
	// 调整后止盈=21.4/2=10.7：平滑 +1% 序列在持有期内触达（不调整则 21.4 永不触发）。
	if !got.HitTakeProfit || got.ExitPrice != 10.7 {
		t.Fatalf("重锚调整后应触止盈@10.7: %+v", got)
	}
}

// ---------- S0-4 Performance 新口径 ----------

// TestPerformanceBuyMatured 买入胜率只统计 buy AND 成熟；watch 单独；active 不进分母；
// degraded 批次条目剔除单独计数。
func TestPerformanceBuyMatured(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM recommendation_statuses")
	common.DB.Exec("DELETE FROM recommendation_batches")
	svc := &TrackingService{}

	deg := &model.RecommendationBatch{UserID: 7, Type: model.RecTypeShortTerm, Status: model.RecStatusDegraded}
	common.DB.Create(deg)
	rows := []model.RecommendationStatus{
		// buy 成熟：止盈 +15（胜）、止损 -8（负）、过期 +1（胜）→ 买入胜率 2/3。
		{RecommendationID: 1, UserID: 7, Type: model.RecTypeShortTerm, Action: model.RecActionBuy, Outcome: model.RecOutcomeTakeProfit, ReturnPct: 15, AlphaPct: 10},
		{RecommendationID: 2, UserID: 7, Type: model.RecTypeShortTerm, Action: model.RecActionBuy, Outcome: model.RecOutcomeStopLoss, ReturnPct: -8, AlphaPct: -5},
		{RecommendationID: 3, UserID: 7, Type: model.RecTypeShortTerm, Action: model.RecActionBuy, Outcome: model.RecOutcomeExpired, ReturnPct: 1, AlphaPct: 0},
		// buy 未成熟（active）：不进买入胜率分母。
		{RecommendationID: 4, UserID: 7, Type: model.RecTypeShortTerm, Action: model.RecActionBuy, Outcome: model.RecOutcomeActive, ReturnPct: 50},
		// watch：单独统计（旧口径会把它混进胜率虚增）。
		{RecommendationID: 5, UserID: 7, Type: model.RecTypeShortTerm, Action: model.RecActionWatch, Outcome: model.RecOutcomeExpired, ReturnPct: 20},
		// 长线 tracking 未到复盘周期：不算成熟。
		{RecommendationID: 6, UserID: 7, Type: model.RecTypeLongTerm, Action: model.RecActionBuy, Outcome: model.RecOutcomeTracking, ElapsedTradeDays: 10, ReturnPct: 9},
		// 长线超复盘周期：算成熟。
		{RecommendationID: 7, UserID: 7, Type: model.RecTypeLongTerm, Action: model.RecActionBuy, Outcome: model.RecOutcomeTracking, ElapsedTradeDays: 70, ReturnPct: -3, AlphaPct: -2},
		// degraded 批次条目：整体剔除。
		{RecommendationID: 8, BatchID: deg.ID, UserID: 7, Type: model.RecTypeShortTerm, Action: model.RecActionBuy, Outcome: model.RecOutcomeTakeProfit, ReturnPct: 30},
	}
	for i := range rows {
		if err := common.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	p, err := svc.Performance(7, "")
	if err != nil {
		t.Fatalf("Performance 失败: %v", err)
	}
	if p.BuyMatured != 4 { // 短线 3 + 长线超周期 1
		t.Fatalf("买入成熟样本应 4，得到 %d", p.BuyMatured)
	}
	if p.BuyWinRate != 50 { // 4 中 2 胜（15/1 正、-8/-3 负）
		t.Fatalf("买入胜率应 50，得到 %v", p.BuyWinRate)
	}
	if p.BuyActive != 2 { // 短线 active 1 + 长线未到期 1
		t.Fatalf("未成熟买入应 2，得到 %d", p.BuyActive)
	}
	if p.WatchSample != 1 || p.WatchWinRate != 100 {
		t.Fatalf("watch 应单独统计（1 条全胜），得到 %d/%v", p.WatchSample, p.WatchWinRate)
	}
	if p.DegradedExcluded != 1 {
		t.Fatalf("degraded 条目应剔除计数 1，得到 %d", p.DegradedExcluded)
	}
	if p.Sample != 7 { // 全样本不含 degraded
		t.Fatalf("全样本应 7，得到 %d", p.Sample)
	}
}

// ---------- S0-6 归因报表 ----------

// TestRecAttribution 分组统计手工验算：胜率/中位/P10/严重亏损率按 net_return 口径。
func TestRecAttribution(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)

	mk := func(id int64, strat string, net float64, chg5d float64) model.RecommendationLabel {
		return model.RecommendationLabel{
			RecommendationID: id, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
			UserID: 9, Symbol: "600100", Market: "cn", Type: model.RecTypeShortTerm,
			Action: model.RecActionBuy, Strategy: strat, Regime: RegimeNeutral,
			EntryChg5dPct: chg5d, NetReturnPct: net, AlphaPct: net - 1, HasBench: true,
			MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
		}
	}
	rows := []model.RecommendationLabel{
		mk(1, "momentum", 8, 20),  // chg5d>15% 桶
		mk(2, "momentum", -6, 18), // 严重亏损（<-5）
		mk(3, "pullback", 2, -2),  // chg5d -5~0%
		mk(4, "pullback", -1, 1),  // chg5d 0~5%
	}
	// watch 条目：action 维度单独统计，其余维度（策略/来源等）不得混入稀释买入归因。
	watchRow := mk(6, "momentum", 50, 0)
	watchRow.Action = model.RecActionWatch
	rows = append(rows, watchRow)
	// 影子标签与 pending 不进报表。
	rows = append(rows, model.RecommendationLabel{
		CandidateEventID: 99, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
		UserID: 9, Symbol: "600200", Market: "cn", NetReturnPct: 100,
		MaturityStatus: model.LabelMatured,
	})
	rows = append(rows, mk(5, "momentum", 0, 0))
	rows[len(rows)-1].MaturityStatus = model.LabelPending
	for i := range rows {
		if err := common.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	rep, err := RecAttribution(9, model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("报表失败: %v", err)
	}
	if rep.Sample != 5 || rep.SampleBuy != 4 || rep.Pending != 1 {
		t.Fatalf("成熟样本应 5（buy 4）、pending 1，得到 %d/%d/%d", rep.Sample, rep.SampleBuy, rep.Pending)
	}
	var momentum, actionBuy, actionWatch *AttributionCell
	for i := range rep.Groups {
		g := &rep.Groups[i]
		switch {
		case g.Dim == "strategy" && g.Key == "momentum":
			momentum = g
		case g.Dim == "action" && g.Key == model.RecActionBuy:
			actionBuy = g
		case g.Dim == "action" && g.Key == model.RecActionWatch:
			actionWatch = g
		}
	}
	if momentum == nil {
		t.Fatalf("应有 momentum 分组")
	}
	// 策略维度只统计 buy：momentum 的 watch(+50) 不得混入（旧口径 Sample 会是 3、
	// 胜率被 watch 拉高）。
	if momentum.Sample != 2 || momentum.WinRate != 50 || momentum.AvgNetPct != 1 || momentum.MedianNetPct != 1 {
		t.Fatalf("momentum 统计不符（watch 不得混入）: %+v", momentum)
	}
	if momentum.SevereLossPct != 50 { // -6 一条
		t.Fatalf("严重亏损率应 50，得到 %v", momentum.SevereLossPct)
	}
	// action 维度：buy/watch 并列对照。
	if actionBuy == nil || actionBuy.Sample != 4 || actionWatch == nil || actionWatch.Sample != 1 {
		t.Fatalf("action 维度应 buy 4/watch 1: %+v %+v", actionBuy, actionWatch)
	}
	// 非法 horizon 拒绝。
	if _, err := RecAttribution(9, "", 7); err == nil {
		t.Fatalf("非法持有期应报错")
	}
}

// ---------- S0-4 自选成交额中位数补齐 ----------

func TestMedianAmountsFor(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM daily_bars")
	t.Cleanup(func() { common.DB.Exec("DELETE FROM daily_bars") })
	// 25 根：中位数只吃最近 20 根。最近 20 根 amount = 101..120（×1e6），中位 =110.5e6。
	for i := 1; i <= 25; i++ {
		common.DB.Create(&model.DailyBar{
			Symbol: "600500", Market: "cn",
			TradeDate: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02"),
			Close:     10, Amount: float64(95+i) * 1e6, Source: "eastmoney",
		})
	}
	out := medianAmountsFor("cn", []string{"600500", "600501"})
	if v := out["600500"]; v != 110.5e6 {
		t.Fatalf("中位数应 110.5e6，得到 %v", v)
	}
	if _, ok := out["600501"]; ok {
		t.Fatalf("无日线标的不应出现在结果中")
	}
}
