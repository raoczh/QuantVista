package service

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// ---------- 切分纯函数：小样本手工验算 ----------

// TestWFSplitFolds n=50、spec{train20/val5/test5/step5/purge2/embargo1}：gap=3、
// need=20+5+5+2×3=36、nFolds=(50-36)/5+1=3。右对齐：最后一折 TestHi=49=n-1，
// 往左每折退 5。手工展开：
//
//	k=0: Train[4,23]  Val[27,31] Test[35,39]
//	k=1: Train[9,28]  Val[32,36] Test[40,44]
//	k=2: Train[14,33] Val[37,41] Test[45,49]
//
// 段间间隔恰为 gap=3（如 TrainHi=23 → ValLo=27，空 24/25/26 三个信号位）。
func TestWFSplitFolds(t *testing.T) {
	sp := wfSpec{Train: 20, Val: 5, Test: 5, Step: 5, Purge: 2, Embargo: 1}
	folds := wfSplitFolds(50, sp)
	want := []wfFold{
		{4, 23, 27, 31, 35, 39},
		{9, 28, 32, 36, 40, 44},
		{14, 33, 37, 41, 45, 49},
	}
	if len(folds) != len(want) {
		t.Fatalf("应 %d 折，得到 %d", len(want), len(folds))
	}
	for i, f := range folds {
		if f != want[i] {
			t.Fatalf("折 %d 应 %+v，得到 %+v", i, want[i], f)
		}
	}
	// 段间 purge+embargo 间隔与最新数据右贴边。
	for _, f := range folds {
		if f.ValLo-f.TrainHi-1 != sp.gap() || f.TestLo-f.ValHi-1 != sp.gap() {
			t.Fatalf("段间隔应为 gap=%d：%+v", sp.gap(), f)
		}
	}
	if folds[len(folds)-1].TestHi != 49 {
		t.Fatalf("最后一折测试段应右贴 n-1=49，得到 %d", folds[len(folds)-1].TestHi)
	}

	// 恰好一折：n=need=36 → Train[0,19] Val[23,27] Test[31,35]。
	one := wfSplitFolds(36, sp)
	if len(one) != 1 || (one[0] != wfFold{0, 19, 23, 27, 31, 35}) {
		t.Fatalf("n=36 应恰一折 {0,19,23,27,31,35}，得到 %+v", one)
	}
	// 放不下：n=35 → 0 折。
	if got := wfSplitFolds(35, sp); got != nil {
		t.Fatalf("n=35 应 0 折，得到 %+v", got)
	}
	// 折数截断 wfMaxFolds=3：n=60 数学上可铺 5 折，只保留最新 3 折且仍右贴边。
	capped := wfSplitFolds(60, sp)
	if len(capped) != wfMaxFolds {
		t.Fatalf("应截断为 %d 折，得到 %d", wfMaxFolds, len(capped))
	}
	if capped[len(capped)-1].TestHi != 59 || capped[0].TrainLo != 14 {
		t.Fatalf("截断后应保留最新 3 折（首折 TrainLo=14、末折 TestHi=59），得到 %+v", capped)
	}
}

// TestWFAdaptSpec 自适应缩窗手工验算（budget=n−2×gap，比例=480:90:60）。
func TestWFAdaptSpec(t *testing.T) {
	// 数据充足：n=700 ≥ need=660 → 目标窗不缩。
	sp, adapted, ok := wfAdaptSpec(700, 10)
	if !ok || adapted || sp.Train != wfTargetTrain || sp.Val != wfTargetVal || sp.Test != wfTargetTest {
		t.Fatalf("n=700 应用目标窗，得到 %+v adapted=%v ok=%v", sp, adapted, ok)
	}
	// 比例缩放：n=239、purge=10 → budget=209、ratio=209/630 →
	// train=int(159.24)=159、val=int(29.86)=29、test=int(19.90)=19（均高于下限）。
	sp, adapted, ok = wfAdaptSpec(239, 10)
	if !ok || !adapted || sp.Train != 159 || sp.Val != 29 || sp.Test != 19 {
		t.Fatalf("n=239 应缩为 {159,29,19}，得到 %+v adapted=%v ok=%v", sp, adapted, ok)
	}
	if sp.need() > 239 {
		t.Fatalf("缩窗后 need=%d 不应超过 n=239", sp.need())
	}
	// 下限兜底 + 超预算从训练段扣：n=124、purge=10 → budget=94、
	// train=71/val=13→15/test=8→15、sum=101 超 7 → train=64。
	sp, adapted, ok = wfAdaptSpec(124, 10)
	if !ok || !adapted || sp.Train != 64 || sp.Val != 15 || sp.Test != 15 {
		t.Fatalf("n=124 应缩为 {64,15,15}，得到 %+v ok=%v", sp, ok)
	}
	// 扣完训练段跌破下限：n=115、purge=10 → train=64→55 <60 → 0 折。
	if _, _, ok = wfAdaptSpec(115, 10); ok {
		t.Fatal("n=115 应 ok=false（0 折）")
	}
	// 预算连下限总和都不够：n=100、purge=20 → budget=50 <90 → 0 折。
	if _, _, ok = wfAdaptSpec(100, 20); ok {
		t.Fatal("n=100/purge=20 应 ok=false")
	}
}

// TestWFSampleN 等距采样：[0,19] 取 4 → 0/6/13/19；段短于 m 全取；m=1 取段末。
func TestWFSampleN(t *testing.T) {
	if got := wfSampleN(0, 19, 4); fmt.Sprint(got) != "[0 6 13 19]" {
		t.Fatalf("[0,19]×4 应 [0 6 13 19]，得到 %v", got)
	}
	if got := wfSampleN(10, 13, 8); fmt.Sprint(got) != "[10 11 12 13]" {
		t.Fatalf("段短于 m 应全取，得到 %v", got)
	}
	if got := wfSampleN(5, 5, 3); fmt.Sprint(got) != "[5]" {
		t.Fatalf("单点段应 [5]，得到 %v", got)
	}
	if got := wfSampleN(7, 3, 2); got != nil {
		t.Fatalf("空段应 nil，得到 %v", got)
	}
}

// TestWFMonthlyIdxs 每月首个交易日；轴起点所在月被窗口截断，丢弃。
func TestWFMonthlyIdxs(t *testing.T) {
	axis := []string{"2026-01-15", "2026-01-16", "2026-02-02", "2026-02-03", "2026-03-02"}
	if got := wfMonthlyIdxs(axis, 4, 12); fmt.Sprint(got) != "[2 4]" {
		t.Fatalf("应丢弃截断首月得 [2 4]，得到 %v", got)
	}
	if got := wfMonthlyIdxs(axis, 4, 1); fmt.Sprint(got) != "[4]" {
		t.Fatalf("months=1 应只留最近月 [4]，得到 %v", got)
	}
	if got := wfMonthlyIdxs(axis, 1, 12); len(got) != 0 {
		t.Fatalf("只剩截断首月时应为空，得到 %v", got)
	}
}

// ---------- 评分复刻：与 scorePool 口径逐项对拍 ----------

// wfParityBars 确定性波动序列（正弦+缓涨，量随波动），控制换手率可注入。
func wfParityBars(n int, turnover float64) []datasource.Bar {
	base := time.Date(2025, 6, 2, 0, 0, 0, 0, time.Local)
	bars := make([]datasource.Bar, 0, n)
	for i := 0; i < n; i++ {
		price := 10 + math.Sin(float64(i)/5)*1.2 + float64(i)*0.01
		open := price * 0.998
		bars = append(bars, datasource.Bar{
			TradeDate: base.AddDate(0, 0, i).Format("2006-01-02"),
			Open:      round2(open), High: round2(price * 1.012), Low: round2(open * 0.99),
			Close: round2(price), Volume: 900_000 + int64(i%7)*50_000,
			Amount: price * 5_000_000, TurnoverRate: turnover,
		})
	}
	return bars
}

// TestWFScoreStockParity wfScoreStock 必须与 scorePool 的组装口径逐策略一致：
// 五维分吃尾 90 根、candFactors 吃尾 210 根、筹码本地复算、strategyAdjust、
// 低位高换手分级扣分（20~25 → -5 / 25~30 → -8）。手工侧独立复刻组装再对拍，
// 锁死窗口与顺序不漂移。
func TestWFScoreStockParity(t *testing.T) {
	defs := wfStrategyList()
	if len(defs) != len(shortStrategies)+len(longStrategies) {
		t.Fatalf("策略清单应含全部短线+长线策略，得到 %d", len(defs))
	}
	for _, turnover := range []float64{3, 22, 27} {
		bars := wfParityBars(250, turnover)
		got := wfScoreStock("600001", "对拍股", bars, defs)
		if got == nil {
			t.Fatalf("turnover=%.0f 不应被排除", turnover)
		}
		// 手工侧：scorePool 同款组装。
		barsFact := bars[len(bars)-chipBarLimit:]
		barsScore := barsFact[len(barsFact)-factorBarLimit:]
		price := bars[len(bars)-1].Close
		sc := computeScore(price, barsScore)
		f := computeCandFactors(price, barsFact)
		if chip, err := computeChipDistribution(barsFact, 0); err == nil {
			f.ChipProfit, f.ChipAvgCost, f.ChipBars = chip.Profit, chip.AvgCost, chip.BarCount
		}
		c := candidate{Symbol: "600001", Name: "对拍股", Price: price,
			Amount: bars[len(bars)-1].Amount, TurnoverRate: turnover}
		for si, def := range defs {
			delta, _ := strategyAdjust(def.recType, def.key, c, f)
			if turnover > deadTurnoverPct {
				if turnover > 25 {
					delta -= 8
				} else {
					delta -= 5
				}
			}
			want := round2(clamp0100(sc.Total + delta))
			if math.Abs(got[si]-want) > 1e-9 {
				t.Fatalf("turnover=%.0f 策略 %s/%s：应 %v，得到 %v", turnover, def.recType, def.key, want, got[si])
			}
		}
	}
	// 系统级排除：>30% 极端换手直接出局。
	if got := wfScoreStock("600001", "对拍股", wfParityBars(250, 35), defs); got != nil {
		t.Fatal("换手 35% 应被极端换手硬拦")
	}
	// 流动性门槛：近 20 日成交额中位数 <3000 万出局。
	thin := wfParityBars(250, 3)
	for i := range thin {
		thin[i].Amount = 1e6
	}
	if got := wfScoreStock("600001", "对拍股", thin, defs); got != nil {
		t.Fatal("成交额中位数 100 万应被流动性门槛排除")
	}
	// 高位死亡换手：尾部拉至 60 日高位 + 换手 22% → applyTurnoverPosFilter 排除。
	high := wfParityBars(250, 22)
	for i := len(high) - 3; i < len(high); i++ {
		high[i].Close = 30
		high[i].High = 30.5
		high[i].Open = 29.8
		high[i].Low = 29.5
	}
	if got := wfScoreStock("600001", "对拍股", high, defs); got != nil {
		t.Fatal("60 日高位 + 换手 22% 应被死亡换手排除")
	}
}

// ---------- 端到端（假日线，165 交易日 × 19 只股） ----------

// seedWFFixture 构造小宇宙：14 只常价股 + 1 只单调上涨（每日 +1%，评分应最高）
// + 1 只单调下跌（评分应最低，Top10 之外）+ 1 只 ST + 1 只低流动性 + 1 只复权断层。
// 165 个交易日下短线（purge=10）与长线（purge=20）各能切出 1 折（下限兜底路径）。
// 基准不可得 → marketAxis 回退交易日历、alpha 如实缺席。
func seedWFFixture(t *testing.T) []string {
	t.Helper()
	common.DB.Exec("DELETE FROM daily_bars")
	common.DB.Exec("DELETE FROM trading_calendars")
	common.DB.Exec("DELETE FROM market_sync_states")
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.Local)
	var dates []string
	for d := 0; d < 165; d++ {
		dates = append(dates, base.AddDate(0, 0, d).Format("2006-01-02"))
	}
	for _, d := range dates {
		common.DB.Exec("INSERT INTO trading_calendars (market, trade_date, is_open) VALUES ('cn', ?, 1)", d)
	}
	type stock struct {
		symbol string
		name   string
		daily  float64 // 日涨幅
		price  float64
		amount float64
	}
	stocks := []stock{
		{"600100", "上涨股", 0.01, 10, 6e7},
		{"600101", "下跌股", -0.01, 20, 6e7},
		{"600102", "ST测试", 0, 8, 5e7},
		{"600103", "低流动", 0, 9, 1e6},
		{"600104", "断层股", 0, 12, 5e7},
	}
	for k := 0; k < 14; k++ {
		stocks = append(stocks, stock{fmt.Sprintf("600%03d", 200+k), fmt.Sprintf("常价%02d", k), 0, 5 + float64(k)*0.5, 5e7})
	}
	var bars []model.DailyBar
	var states []model.MarketSyncState
	for _, s := range stocks {
		price := s.price
		for d := 0; d < 165; d++ {
			open := price
			price = price * (1 + s.daily)
			if s.symbol == "600104" && d == 80 {
				price *= 1.5 // 复权断层：+50% 远超涨停幅×1.5 容差
			}
			bars = append(bars, model.DailyBar{
				Symbol: s.symbol, Market: "cn", TradeDate: dates[d],
				Open: open, High: price * 1.001, Low: open * 0.999, Close: price,
				Volume: 1_000_000, Amount: s.amount, TurnoverRate: 3, Source: "eastmoney",
			})
		}
		states = append(states, model.MarketSyncState{
			Symbol: s.symbol, Market: "cn", Name: s.name, LastBarDate: dates[len(dates)-1],
		})
	}
	if err := common.DB.CreateInBatches(bars, 500).Error; err != nil {
		t.Fatalf("seed bars 失败: %v", err)
	}
	if err := common.DB.CreateInBatches(states, 100).Error; err != nil {
		t.Fatalf("seed states 失败: %v", err)
	}
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM daily_bars")
		common.DB.Exec("DELETE FROM trading_calendars")
		common.DB.Exec("DELETE FROM market_sync_states")
	})
	return dates
}

func TestRunWalkForwardEndToEnd(t *testing.T) {
	setupTestDB(t)
	seedWFFixture(t)
	// 60s 新鲜日期缓存会残留其他测试的值，清掉（IC 端到端同款）。
	factorFreshMu.Lock()
	factorFreshVal = ""
	factorFreshMu.Unlock()

	rep, err := RunWalkForward(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunWalkForward 失败: %v", err)
	}
	// 宇宙口径：19 只 − ST 1 − 断层 1 = 17（低流动是观测级过滤，不进股级剔除计数）。
	if rep.Universe != 17 || rep.StSkipped != 1 || rep.Suspects != 1 {
		t.Fatalf("宇宙应 17/ST 1/断层 1，得到 %d/%d/%d", rep.Universe, rep.StSkipped, rep.Suspects)
	}
	if len(rep.Sections) != 2 {
		t.Fatalf("应短线+长线两段，得到 %d", len(rep.Sections))
	}
	short, long := rep.Sections[0], rep.Sections[1]
	if short.RecType != model.RecTypeShortTerm || long.RecType != model.RecTypeLongTerm {
		t.Fatalf("段类型应 short_term/long_term，得到 %s/%s", short.RecType, long.RecType)
	}
	// 165 轴：短线 eligible=154、长线 eligible=144，均走「下限兜底+训练段扣减」缩窗路径，各 1 折。
	for _, sec := range []WFSectionReport{short, long} {
		if !sec.Adapted || len(sec.Folds) != 1 {
			t.Fatalf("%s 应缩窗且恰 1 折，得到 adapted=%v folds=%d（note=%s）", sec.RecType, sec.Adapted, len(sec.Folds), sec.SpecNote)
		}
		if sec.Spec.Val < wfMinVal || sec.Spec.Test < wfMinTest || sec.Spec.Train < wfMinTrain {
			t.Fatalf("%s 缩窗不得跌破下限：%+v", sec.RecType, sec.Spec)
		}
	}
	// 行数：1 折 × 2 段(val/test) × 策略数 × 持有期数；单折无 Fold=0 合并行。
	if want := 2 * len(short.Strategies) * len(short.Holds); len(short.Rows) != want {
		t.Fatalf("短线行数应 %d，得到 %d", want, len(short.Rows))
	}
	for _, r := range short.Rows {
		if r.Fold == 0 {
			t.Fatal("单折不应有 Fold=0 合并行")
		}
	}
	// 指标不变式 + alpha 如实缺席（基准不可得）。
	for _, sec := range rep.Sections {
		for _, r := range sec.Rows {
			if r.Trades+r.Skipped+r.Forced+r.Pending != r.Picked {
				t.Fatalf("名额守恒破裂：%+v", r)
			}
			if r.Picked > r.Signals*rep.TopK {
				t.Fatalf("Picked 超 Signals×K：%+v", r)
			}
			if r.AlphaSample != 0 {
				t.Fatalf("基准缺失时 alpha 样本应为 0，得到 %+v", r)
			}
		}
	}
	// 月度走查：上涨股评分最高应居短线 momentum 各月 Top-K 首位，且持有 10 日净收益为正；
	// 低流动/ST/断层股不得出现在任何组合成员里。
	momMonths := 0
	for _, m := range short.Monthly {
		if m.Strategy != "momentum" {
			continue
		}
		momMonths++
		if m.Hold != 10 {
			t.Fatalf("短线月度走查代表持有期应 10，得到 %d", m.Hold)
		}
		if len(m.Items) == 0 || m.Items[0].Symbol != "600100" {
			t.Fatalf("%s 月 momentum Top-K 首位应为上涨股，得到 %+v", m.Month, m.Items)
		}
		if m.Items[0].NetPct == nil || *m.Items[0].NetPct <= 0 {
			t.Fatalf("上涨股持有 10 日净收益应为正，得到 %+v", m.Items[0])
		}
		if len(m.Items) > rep.TopK {
			t.Fatalf("组合成员不得超过 TopK：%d", len(m.Items))
		}
		for _, it := range m.Items {
			if it.Symbol == "600103" || it.Symbol == "600102" || it.Symbol == "600104" {
				t.Fatalf("低流动/ST/断层股不得入组合：%s", it.Symbol)
			}
			if it.Symbol == "600101" {
				t.Fatalf("下跌股评分最低（17 只有效宇宙第 16+ 名）不应进 Top10")
			}
		}
	}
	if momMonths == 0 {
		t.Fatal("月度走查应至少覆盖一个月")
	}
	if len(long.Monthly) == 0 {
		t.Fatal("长线月度走查不应为空（折与月度走查相互独立）")
	}
	if CachedWalkForwardReport() != rep {
		t.Fatal("结果应写入进程内缓存")
	}
}
