package service

import (
	"math"
	"testing"

	"quantvista/datasource"
)

// C12 K 线形态因子测试（第六十二批）。
//
// 每个形态一组：**手工构造的正例 + 至少一个反例**（反例都是「差一点点就命中」的边界，
// 不是随便一根不相关的 K 线——只有边界反例才能锁住判定式没有被悄悄放宽）。
// 另有两条全局纪律测试：
//   - 样本不足/坏根 → NaN（不可判定），**连 is_false 也不命中**（未知≠否）；
//   - 全部形态因子在注册表里都是布尔型且带判定式说明（防 AI 望文生义）。

// bar 便捷构造（日期只影响展示，形态判定不看日期）。
func kbar(open, high, low, closeP float64) datasource.Bar {
	return datasource.Bar{TradeDate: "2026-07-01", Open: open, High: high, Low: low, Close: closeP,
		Volume: 10000, Amount: 1e6}
}

// wantPattern 断言某形态的判定结果。
func wantPattern(t *testing.T, name string, got patternResult, hit, known bool) {
	t.Helper()
	if got.known != known {
		t.Fatalf("%s: known 应为 %v，got %v", name, known, got.known)
	}
	if known && got.hit != hit {
		t.Fatalf("%s: hit 应为 %v，got %v", name, hit, got.hit)
	}
}

// TestPatternDojiAndHammer 单根形态：十字星与锤子线。
func TestPatternDojiAndHammer(t *testing.T) {
	// 十字星正例：开 10.00 收 10.01（实体 0.01），振幅 10.30−9.70=0.60，
	// 0.01 ≤ 0.60×10% = 0.06 → 命中。
	wantPattern(t, "doji 正例", patDoji([]datasource.Bar{kbar(10.00, 10.30, 9.70, 10.01)}), true, true)
	// 反例①：实体 0.10 > 0.06 → 不命中（差一点点，锁住 10% 阈值没被放宽）。
	wantPattern(t, "doji 反例-实体偏大", patDoji([]datasource.Bar{kbar(10.00, 10.30, 9.70, 10.10)}), false, true)
	// 反例②：一字板（振幅 0）——四价相同，没有上下影线，**明确不是十字星**
	// （可判定的 false，不是缺失；否则「十字星选股」每天会选出一堆一字涨停）。
	wantPattern(t, "doji 反例-一字板", patDoji([]datasource.Bar{kbar(11.00, 11.00, 11.00, 11.00)}), false, true)

	// 锤子线正例：开 10.20 收 10.30（实体 0.10），下影 = 10.20−9.90 = 0.30 ≥ 2×0.10，
	// 上影 = 10.35−10.30 = 0.05 ≤ 0.10 → 命中。
	wantPattern(t, "hammer 正例", patHammer([]datasource.Bar{kbar(10.20, 10.35, 9.90, 10.30)}), true, true)
	// 反例①：下影 0.15 < 2×0.10 → 不命中。
	wantPattern(t, "hammer 反例-下影不够长", patHammer([]datasource.Bar{kbar(10.20, 10.35, 10.05, 10.30)}), false, true)
	// 反例②：射击之星（长上影、短下影）不得被当成锤子线。
	wantPattern(t, "hammer 反例-长上影", patHammer([]datasource.Bar{kbar(10.20, 10.80, 10.15, 10.30)}), false, true)
	// 反例③：实体为零（十字）走 doji 因子，锤子线不认。
	wantPattern(t, "hammer 反例-零实体", patHammer([]datasource.Bar{kbar(10.20, 10.25, 9.90, 10.20)}), false, true)
}

// TestPatternEngulfingAndDarkCloud 两根形态：看涨吞没与乌云盖顶。
func TestPatternEngulfingAndDarkCloud(t *testing.T) {
	// 看涨吞没正例：前阴（开 10.50 收 10.00），今阳（开 9.95 收 10.60）——
	// 9.95 ≤ 10.00 且 10.60 ≥ 10.50 → 实体完全吞没。
	wantPattern(t, "engulfing 正例", patBullishEngulfing([]datasource.Bar{
		kbar(10.50, 10.55, 9.95, 10.00),
		kbar(9.95, 10.65, 9.90, 10.60),
	}), true, true)
	// 反例①：今阳但收盘 10.40 < 前开 10.50，只吞了一半 → 不命中。
	wantPattern(t, "engulfing 反例-未吞完", patBullishEngulfing([]datasource.Bar{
		kbar(10.50, 10.55, 9.95, 10.00),
		kbar(9.95, 10.45, 9.90, 10.40),
	}), false, true)
	// 反例②：前一根是阳线（不是阴线）→ 形态前提不成立。
	wantPattern(t, "engulfing 反例-前阳", patBullishEngulfing([]datasource.Bar{
		kbar(10.00, 10.55, 9.95, 10.50),
		kbar(9.95, 10.65, 9.90, 10.60),
	}), false, true)

	// 乌云盖顶正例：前阳（开 10.00 收 11.00，中点 10.50），今高开 11.20 收 10.40（<10.50 且 >10.00）。
	wantPattern(t, "darkcloud 正例", patDarkCloud([]datasource.Bar{
		kbar(10.00, 11.05, 9.95, 11.00),
		kbar(11.20, 11.25, 10.35, 10.40),
	}), true, true)
	// 反例①：今收 10.60 > 中点 10.50 → 只是普通回落，不算乌云盖顶。
	wantPattern(t, "darkcloud 反例-未破中点", patDarkCloud([]datasource.Bar{
		kbar(10.00, 11.05, 9.95, 11.00),
		kbar(11.20, 11.25, 10.55, 10.60),
	}), false, true)
	// 反例②：今收 9.90 < 前开 10.00 → 那是**看跌吞没**（更强的形态），
	// 两者互斥才能让「乌云盖顶」名副其实。
	wantPattern(t, "darkcloud 反例-已成吞没", patDarkCloud([]datasource.Bar{
		kbar(10.00, 11.05, 9.95, 11.00),
		kbar(11.20, 11.25, 9.85, 9.90),
	}), false, true)
	// 反例③：今低开（开 10.90 < 前收 11.00）→ 不满足高开前提。
	wantPattern(t, "darkcloud 反例-未高开", patDarkCloud([]datasource.Bar{
		kbar(10.00, 11.05, 9.95, 11.00),
		kbar(10.90, 10.95, 10.35, 10.40),
	}), false, true)
}

// TestPatternStars 三根形态：早晨之星与黄昏之星。
func TestPatternStars(t *testing.T) {
	// 早晨之星正例：①阴线 12.00→10.00（实体 2.00，中点 11.00）；
	// ②小实体星线 9.90→9.95（实体 0.05 ≤ 1.00，实体顶 9.95 ≤ ①收 10.00）；
	// ③阳线收 11.20 > 11.00 → 命中。
	wantPattern(t, "morning_star 正例", patMorningStar([]datasource.Bar{
		kbar(12.00, 12.10, 9.90, 10.00),
		kbar(9.90, 10.00, 9.80, 9.95),
		kbar(10.00, 11.30, 9.95, 11.20),
	}), true, true)
	// 反例①：第三根只收到 10.80 < 中点 11.00 → 收复不足，不命中。
	wantPattern(t, "morning_star 反例-收复不足", patMorningStar([]datasource.Bar{
		kbar(12.00, 12.10, 9.90, 10.00),
		kbar(9.90, 10.00, 9.80, 9.95),
		kbar(10.00, 10.90, 9.95, 10.80),
	}), false, true)
	// 反例②：第二根实体过大（10.00→11.00，实体 1.00 > 2.00×50% 的边界外，
	// 且实体顶 11.00 高于 ①收盘）→ 那是普通反弹不是星线。
	wantPattern(t, "morning_star 反例-星线实体过大", patMorningStar([]datasource.Bar{
		kbar(12.00, 12.10, 9.90, 10.00),
		kbar(10.00, 11.10, 9.90, 11.00),
		kbar(11.00, 11.60, 10.95, 11.50),
	}), false, true)

	// 黄昏之星正例（与早晨之星严格对称）：①阳线 10.00→12.00（中点 11.00）；
	// ②小实体星线 12.10→12.05（实体 0.05，实体底 12.05 ≥ ①收 12.00）；
	// ③阴线收 10.80 < 11.00。
	wantPattern(t, "evening_star 正例", patEveningStar([]datasource.Bar{
		kbar(10.00, 12.10, 9.95, 12.00),
		kbar(12.10, 12.20, 12.00, 12.05),
		kbar(12.00, 12.05, 10.70, 10.80),
	}), true, true)
	// 反例：第三根只跌到 11.20 > 中点 11.00 → 不命中。
	wantPattern(t, "evening_star 反例-跌幅不足", patEveningStar([]datasource.Bar{
		kbar(10.00, 12.10, 9.95, 12.00),
		kbar(12.10, 12.20, 12.00, 12.05),
		kbar(12.00, 12.05, 11.15, 11.20),
	}), false, true)
}

// TestPatternSoldiersAndConsecutive 红三兵与三连涨。
func TestPatternSoldiersAndConsecutive(t *testing.T) {
	// 红三兵正例：三根阳线，收盘 10.50 < 11.00 < 11.50，
	// 每根开盘落在前一根实体内（10.30∈[10.00,10.50]、10.80∈[10.30,11.00]）。
	wantPattern(t, "soldiers 正例", patThreeWhiteSoldiers([]datasource.Bar{
		kbar(10.00, 10.55, 9.95, 10.50),
		kbar(10.30, 11.05, 10.25, 11.00),
		kbar(10.80, 11.55, 10.75, 11.50),
	}), true, true)
	// 反例①：第三根跳空高开（开 11.20 > 前收 11.00）→ 加速赶顶不是稳步推进。
	wantPattern(t, "soldiers 反例-跳空高开", patThreeWhiteSoldiers([]datasource.Bar{
		kbar(10.00, 10.55, 9.95, 10.50),
		kbar(10.30, 11.05, 10.25, 11.00),
		kbar(11.20, 11.65, 11.15, 11.60),
	}), false, true)
	// 反例②：中间一根是阴线。
	wantPattern(t, "soldiers 反例-含阴线", patThreeWhiteSoldiers([]datasource.Bar{
		kbar(10.00, 10.55, 9.95, 10.50),
		kbar(10.45, 10.65, 10.30, 10.40),
		kbar(10.35, 11.05, 10.30, 11.00),
	}), false, true)

	// 三连涨正例（收盘口径）：10.00 → 10.10 → 10.20 → 10.30，需要 4 根。
	wantPattern(t, "consecutive 正例", patConsecutiveUp([]datasource.Bar{
		kbar(9.95, 10.05, 9.90, 10.00),
		kbar(10.00, 10.15, 9.98, 10.10),
		kbar(10.10, 10.25, 10.05, 10.20),
		kbar(10.20, 10.35, 10.15, 10.30),
	}), true, true)
	// 反例①：中间一天收平（10.10 → 10.10，不算涨）。
	wantPattern(t, "consecutive 反例-收平", patConsecutiveUp([]datasource.Bar{
		kbar(9.95, 10.05, 9.90, 10.00),
		kbar(10.00, 10.15, 9.98, 10.10),
		kbar(10.10, 10.25, 10.05, 10.10),
		kbar(10.10, 10.35, 10.05, 10.30),
	}), false, true)
	// 反例②：只有 3 根 → 判不了「3 连涨」（缺一天基准），不可判定而非「没涨」。
	wantPattern(t, "consecutive 反例-样本不足", patConsecutiveUp([]datasource.Bar{
		kbar(10.00, 10.15, 9.98, 10.10),
		kbar(10.10, 10.25, 10.05, 10.20),
		kbar(10.20, 10.35, 10.15, 10.30),
	}), false, false)
	// 低开高走的阴线只要收盘更高也算涨（收盘口径，不看阴阳）。
	wantPattern(t, "consecutive 正例-阴线收高", patConsecutiveUp([]datasource.Bar{
		kbar(9.95, 10.05, 9.90, 10.00),
		kbar(10.30, 10.35, 10.05, 10.10),
		kbar(10.40, 10.45, 10.15, 10.20),
		kbar(10.50, 10.55, 10.25, 10.30),
	}), true, true)
}

// TestPatternMacdAndMaAlign MACD 当日金叉与完全多头排列。
func TestPatternMacdAndMaAlign(t *testing.T) {
	// 构造：先长期下跌（DIF 在 DEA 之下），再急速拉升制造金叉。
	closes := make([]float64, 0, 80)
	p := 20.0
	for i := 0; i < 60; i++ { // 缓慢下跌
		p *= 0.995
		closes = append(closes, p)
	}
	for i := 0; i < 8; i++ { // 急拉
		p *= 1.05
		closes = append(closes, p)
	}
	bars := make([]datasource.Bar, len(closes))
	for i, c := range closes {
		bars[i] = kbar(c*0.995, c*1.01, c*0.99, c)
	}
	// 逐根扫出真正的当日金叉那一天，再断言该切片命中、其后一天不命中
	// （「当日」语义的关键：金叉的次日不再算金叉）。
	crossAt := -1
	dif, dea, _ := macdSeries(closes)
	for i := macdMinBars; i < len(closes); i++ {
		if dif[i-1] <= dea[i-1] && dif[i] > dea[i] {
			crossAt = i
			break
		}
	}
	if crossAt < 0 || crossAt+1 >= len(bars) {
		t.Fatal("构造数据未产生可用的金叉样本")
	}
	wantPattern(t, "macd 金叉当日", patMacdGoldenCross(bars[:crossAt+1]), true, true)
	wantPattern(t, "macd 金叉次日", patMacdGoldenCross(bars[:crossAt+2]), false, true)
	// 样本不足：EMA 未预热，不可判定。
	wantPattern(t, "macd 样本不足", patMacdGoldenCross(bars[:macdMinBars]), false, false)

	// 完全多头排列：稳定上涨 80 根 → MA5>MA10>MA20>MA60 且价格站上 MA5。
	up := genTrendBars(80, 10, 0.25)
	wantPattern(t, "ma_bull_align 正例", patMaBullAlign(up), true, true)
	// **蕴含关系**：完全多头排列命中时，宽表里较弱的 bull_align 必须也命中，
	// 否则两个因子会给出自相矛盾的画像。
	vals := computeWideRow("600001", wideStockMeta{Name: "多头股"}, up)
	if wideVal(vals, "ma_bull_align") == 1 && wideVal(vals, "bull_align") != 1 {
		t.Fatal("ma_bull_align 命中时 bull_align 必须同时命中（前者严格更强）")
	}
	// 反例：稳定下跌 → 空头排列。
	down := genTrendBars(80, 20, -0.25)
	wantPattern(t, "ma_bull_align 反例-下跌", patMaBullAlign(down), false, true)
	// 样本不足（<60 根）→ 不可判定。
	wantPattern(t, "ma_bull_align 样本不足", patMaBullAlign(up[:50]), false, false)
}

// TestPatternUnknownOnBadBars 坏根与样本不足一律不可判定——
// **这是本组最重要的一条**：NaN 在条件树里连 `is_false` 都不命中，
// 「这只票没数据」不会被 `is_false` 当成「确实不是该形态」选出来。
func TestPatternUnknownOnBadBars(t *testing.T) {
	// 情形①：坏根落在窗口中间（停牌补的空行），**最新一根有效**。
	// 多根形态窗口被污染 → 不可判定；单根形态只看最新一根 → 仍可判定（这是对的，
	// 「昨天停牌」不影响「今天这根是不是十字星」）。
	midBad := []datasource.Bar{
		kbar(10.00, 10.50, 9.50, 10.20),
		{TradeDate: "2026-07-02"}, // 停牌补的空行：四价全 0
		kbar(10.10, 10.60, 9.60, 10.30),
	}
	for _, key := range []string{"morning_star", "evening_star", "bullish_engulfing",
		"dark_cloud", "three_white_soldiers"} {
		if got := computePatternFactors(midBad)[key]; got.known {
			t.Fatalf("%s 窗口内有坏根应不可判定（NaN），got %+v", key, got)
		}
	}
	for _, key := range []string{"hammer", "doji"} {
		if got := computePatternFactors(midBad)[key]; !got.known {
			t.Fatalf("%s 只看最新一根，最新根有效时应可判定，got %+v", key, got)
		}
	}

	// 情形②：最新一根就是坏根 → 单根形态也不可判定。
	lastBad := []datasource.Bar{
		kbar(10.00, 10.50, 9.50, 10.20),
		kbar(10.10, 10.60, 9.60, 10.30),
		{TradeDate: "2026-07-03"},
	}
	for _, key := range []string{"hammer", "doji"} {
		if got := computePatternFactors(lastBad)[key]; got.known {
			t.Fatalf("%s 最新根为坏根应不可判定，got %+v", key, got)
		}
	}

	// 空日线：全部不可判定。
	for key, got := range computePatternFactors(nil) {
		if got.known {
			t.Fatalf("%s 在空日线上应不可判定，got %+v", key, got)
		}
	}

	// 落到宽表列上：不可判定 → NaN；条件树对 NaN 恒 false（is_true 与 is_false 都不命中）。
	vals := computeWideRow("600001", wideStockMeta{Name: "坏根股"}, midBad)
	if !math.IsNaN(wideVal(vals, "morning_star")) {
		t.Fatalf("窗口含坏根时 morning_star 列应为 NaN，got %v", wideVal(vals, "morning_star"))
	}
	tbl := &FactorTable{
		TradeDate: "2026-07-03", Symbols: []string{"600001"}, Names: []string{"坏根股"},
		LastDates: []string{"2026-07-03"}, cols: map[string][]float64{},
	}
	for _, d := range factorDefs {
		tbl.cols[d.Key] = []float64{vals[factorIndex[d.Key]]}
	}
	for _, op := range []string{"is_true", "is_false"} {
		node := CondNode{Factor: "morning_star", Op: op}
		if evalCondRow(tbl, &node, 0) {
			t.Fatalf("NaN 因子的 %s 不得命中（未知≠否）", op)
		}
	}
}

// TestPatternFactorsRegistered 全部形态因子已注册、为布尔型、在「形态」组，
// 且 desc 写死了判定式（防 AI 与用户按各自理解使用同一个名字）。
func TestPatternFactorsRegistered(t *testing.T) {
	for _, key := range patternFactorKeys {
		def, ok := factorByKey(key)
		if !ok {
			t.Fatalf("形态因子 %s 未注册进 factorDefs", key)
		}
		if def.Kind != fkBool {
			t.Fatalf("形态因子 %s 应为布尔型，got %s", key, def.Kind)
		}
		if def.Group != "形态" {
			t.Fatalf("形态因子 %s 应在「形态」组，got %s", key, def.Group)
		}
		if len([]rune(def.Desc)) < 20 {
			t.Fatalf("形态因子 %s 的 desc 必须写清判定式（当前过短：%q）", key, def.Desc)
		}
	}
	// computePatternFactors 的键集合必须与清单一致（新增形态忘了注册会在此暴露）。
	got := computePatternFactors(nil)
	if len(got) != len(patternFactorKeys) {
		t.Fatalf("形态因子数量不一致：清单 %d，计算 %d", len(patternFactorKeys), len(got))
	}
	for _, key := range patternFactorKeys {
		if _, ok := got[key]; !ok {
			t.Fatalf("computePatternFactors 缺少 %s", key)
		}
	}
}

// TestPatternFactorsNotScored 形态因子与股息率**不得进推荐评分/量化因子**：
// 它们只是描述性因子，进评分等于用未经 IC/回测验证的先验改推荐排序。
// 用 IC 白名单做锚：形态是布尔（秩相关退化）、div_yield 历史不可 as-of 重建，
// 两者都不能进 icFactorKeys。
func TestPatternFactorsNotScored(t *testing.T) {
	banned := append(append([]string{}, patternFactorKeys...), "div_yield")
	for _, key := range banned {
		for _, ic := range icFactorKeys {
			if ic == key {
				t.Fatalf("%s 不应进 IC 白名单（布尔因子秩相关退化 / 股息率无历史 as-of）", key)
			}
		}
	}
}
