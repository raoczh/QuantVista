package service

import (
	"quantvista/datasource"
)

// C12 K 线形态因子（第六十二批）。
//
// **纪律（改代码前先读）**：
//   - 形态是**描述性因子，不是买卖信号**——只进选股条件树与 AI 因子字典供用户/模型
//     自行组合与解读，**绝不进推荐评分权重**（score.go / recfactor.go 一律不引用本文件）。
//     理由：单根/两三根 K 线的形态在 A 股的样本外区分度极不稳定，把它加进评分等于
//     用未经验证的先验去改推荐排序；要用先做 IC/回测验证再说。
//   - **判定式写进 factorDefs 的 desc**：形态名在坊间有多种口径（「锤子线」有的要求
//     前置下跌趋势、有的不要），desc 不写死判定式，AI 与用户会各按各的理解使用，
//     选出来的票对不上名字。
//   - **未知 ≠ 否**：样本不足、日线有坏根（OHLC 非正、high<low）一律返回 known=false
//     → 因子列 NaN。条件树对 NaN 求值恒 false，连 `is_false` 也不命中——
//     「这只票没数据」不会被 `is_false` 当成「这只票确实不是该形态」选出来。
//   - 全部判定只吃**本地日线**（daily_bars，东财前复权口径），零上游请求。
//     前复权序列在除权后整段重刷，历史形态会随之变化，属既有口径边界。

// patternResult 单个形态的判定结果。known=false 表示**不可判定**（样本不足/坏根），
// 与 hit=false（可判定且不成立）严格区分。
type patternResult struct {
	hit   bool
	known bool
}

func patHit(b bool) patternResult  { return patternResult{hit: b, known: true} }
func patUnknown() patternResult    { return patternResult{} }
func patKnownFalse() patternResult { return patternResult{known: true} }

// patternFactorKeys C12 新增的形态因子键（注册表校验与测试共用的唯一清单）。
var patternFactorKeys = []string{
	"macd_golden_cross", "ma_bull_align",
	"morning_star", "evening_star", "bullish_engulfing", "dark_cloud",
	"hammer", "doji", "three_white_soldiers", "consecutive_up",
}

// barValid 单根日线是否可用于形态判定（OHLC 四价均为正且 high≥low≥0）。
// 停牌补的空行、上游坏根会让形态判定得出荒谬结论，一律视为不可判定。
func barValid(b datasource.Bar) bool {
	if b.Open <= 0 || b.High <= 0 || b.Low <= 0 || b.Close <= 0 {
		return false
	}
	if b.High < b.Low {
		return false
	}
	// 收/开必须落在当日高低区间内，否则该根数据自相矛盾。
	return b.High >= b.Open && b.High >= b.Close && b.Low <= b.Open && b.Low <= b.Close
}

// lastValidBars 取末尾 n 根并校验全部有效；不足或有坏根返回 (nil,false)。
func lastValidBars(bars []datasource.Bar, n int) ([]datasource.Bar, bool) {
	if len(bars) < n {
		return nil, false
	}
	win := bars[len(bars)-n:]
	for _, b := range win {
		if !barValid(b) {
			return nil, false
		}
	}
	return win, true
}

// K 线几何量（单位=元）。
func barBody(b datasource.Bar) float64 {
	if b.Close >= b.Open {
		return b.Close - b.Open
	}
	return b.Open - b.Close
}
func barRange(b datasource.Bar) float64 { return b.High - b.Low }
func barUpperShadow(b datasource.Bar) float64 {
	top := b.Open
	if b.Close > top {
		top = b.Close
	}
	return b.High - top
}
func barLowerShadow(b datasource.Bar) float64 {
	bottom := b.Open
	if b.Close < bottom {
		bottom = b.Close
	}
	return bottom - b.Low
}
func isBull(b datasource.Bar) bool { return b.Close > b.Open }
func isBear(b datasource.Bar) bool { return b.Close < b.Open }

// bodyMid 实体中点（开收均值）。
func bodyMid(b datasource.Bar) float64 { return (b.Open + b.Close) / 2 }

// dojiBodyRatio 十字星的实体/振幅上限。
const dojiBodyRatio = 0.1

// starBodyRatio 星线（早晨/黄昏之星的第二根）实体相对第一根实体的上限。
const starBodyRatio = 0.5

// consecutiveUpDays 「连续上涨」认定的交易日数（收盘口径）。
const consecutiveUpDays = 3

// computePatternFactors 由升序日线算全部形态因子（唯一入口，computeWideRowOpts 调用）。
// 返回 map[因子key]判定；未收录的键由调用方保持 NaN。
func computePatternFactors(bars []datasource.Bar) map[string]patternResult {
	out := make(map[string]patternResult, len(patternFactorKeys))
	out["macd_golden_cross"] = patMacdGoldenCross(bars)
	out["ma_bull_align"] = patMaBullAlign(bars)
	out["morning_star"] = patMorningStar(bars)
	out["evening_star"] = patEveningStar(bars)
	out["bullish_engulfing"] = patBullishEngulfing(bars)
	out["dark_cloud"] = patDarkCloud(bars)
	out["hammer"] = patHammer(bars)
	out["doji"] = patDoji(bars)
	out["three_white_soldiers"] = patThreeWhiteSoldiers(bars)
	out["consecutive_up"] = patConsecutiveUp(bars)
	return out
}

// patMacdGoldenCross **当日**金叉：前一根 DIF≤DEA、最新根 DIF>DEA。
// 与既有 macd_cross_up（近 3 日内出现过金叉）刻意不同——「今天刚金叉」与
// 「这几天金叉过」是两个不同的决策时点，两个因子都保留，desc 里写清区别。
func patMacdGoldenCross(bars []datasource.Bar) patternResult {
	n := len(bars)
	// macdSeries 的前 macdMinBars-1 根不可信（EMA 预热），且需要相邻两根比较。
	if n < macdMinBars+1 {
		return patUnknown()
	}
	closes := make([]float64, n)
	for i, b := range bars {
		if b.Close <= 0 {
			return patUnknown() // 坏根会污染整条 EMA
		}
		closes[i] = b.Close
	}
	dif, dea, _ := macdSeries(closes)
	return patHit(dif[n-2] <= dea[n-2] && dif[n-1] > dea[n-1])
}

// patMaBullAlign 完全多头排列：MA5>MA10>MA20>MA60 且收盘站上 MA5。
// 与既有 bull_align（仅 MA5>MA10>MA20）的关系：本因子严格更强，命中必然蕴含 bull_align。
// MA 一律 round2 后比较，与 computeWideRowOpts 的均线列口径逐位一致（避免同一天
// 「宽表里 MA5==MA10 而本因子按未舍入值判 >」这种自相矛盾）。
func patMaBullAlign(bars []datasource.Bar) patternResult {
	n := len(bars)
	if n < 60 {
		return patUnknown()
	}
	closes := make([]float64, n)
	for i, b := range bars {
		if b.Close <= 0 {
			return patUnknown()
		}
		closes[i] = b.Close
	}
	ma := func(w int) (float64, bool) {
		v, ok := movingAverage(closes, w)
		return round2(v), ok
	}
	ma5, ok5 := ma(5)
	ma10, ok10 := ma(10)
	ma20, ok20 := ma(20)
	ma60, ok60 := ma(60)
	if !ok5 || !ok10 || !ok20 || !ok60 {
		return patUnknown()
	}
	price := round2(closes[n-1])
	return patHit(ma5 > ma10 && ma10 > ma20 && ma20 > ma60 && price >= ma5)
}

// patMorningStar 早晨之星（底部三根反转形态）：
//
//	① 阴线（实体 > 0）
//	② 小实体星线：实体 ≤ ①实体的 50%，且实体最高点 ≤ ①的收盘价（位置在下方）
//	③ 阳线，收盘 > ①实体中点（收复第一根一半以上）
//
// **不强制要求跳空缺口**：A 股 T+1 且有涨跌停，真缺口远少于美股，
// 强制缺口会让这个形态在 A 股几乎不出现。该放宽写在 desc 里。
func patMorningStar(bars []datasource.Bar) patternResult {
	win, ok := lastValidBars(bars, 3)
	if !ok {
		return patUnknown()
	}
	b1, b2, b3 := win[0], win[1], win[2]
	body1 := barBody(b1)
	if !isBear(b1) || body1 <= 0 {
		return patKnownFalse()
	}
	if barBody(b2) > body1*starBodyRatio {
		return patKnownFalse()
	}
	starTop := b2.Open
	if b2.Close > starTop {
		starTop = b2.Close
	}
	if starTop > b1.Close {
		return patKnownFalse() // 星线实体没有落在第一根阴线的下沿之下
	}
	return patHit(isBull(b3) && b3.Close > bodyMid(b1))
}

// patEveningStar 黄昏之星（顶部三根反转形态），与早晨之星严格对称。
func patEveningStar(bars []datasource.Bar) patternResult {
	win, ok := lastValidBars(bars, 3)
	if !ok {
		return patUnknown()
	}
	b1, b2, b3 := win[0], win[1], win[2]
	body1 := barBody(b1)
	if !isBull(b1) || body1 <= 0 {
		return patKnownFalse()
	}
	if barBody(b2) > body1*starBodyRatio {
		return patKnownFalse()
	}
	starBottom := b2.Open
	if b2.Close < starBottom {
		starBottom = b2.Close
	}
	if starBottom < b1.Close {
		return patKnownFalse()
	}
	return patHit(isBear(b3) && b3.Close < bodyMid(b1))
}

// patBullishEngulfing 看涨吞没：前一根阴线，最新一根阳线且实体完全包住前一根实体。
// 用 ≤/≥（含等号）而非严格不等：A 股平开高走同样构成吞没，卡严格不等会漏掉大量真实案例。
func patBullishEngulfing(bars []datasource.Bar) patternResult {
	win, ok := lastValidBars(bars, 2)
	if !ok {
		return patUnknown()
	}
	prev, cur := win[0], win[1]
	if !isBear(prev) || barBody(prev) <= 0 {
		return patKnownFalse()
	}
	if !isBull(cur) || barBody(cur) <= 0 {
		return patKnownFalse()
	}
	// prev 阴线：上沿=Open、下沿=Close。cur 阳线实体须覆盖 [prev.Close, prev.Open]。
	return patHit(cur.Open <= prev.Close && cur.Close >= prev.Open)
}

// patDarkCloud 乌云盖顶：前一根阳线，最新一根高开收阴且收盘跌破前一根实体中点，
// 但**未跌破前一根开盘**——跌破就是「看跌吞没」，是另一个（更强的）形态，
// 两者互斥才能让「乌云盖顶」这个名字名副其实。
func patDarkCloud(bars []datasource.Bar) patternResult {
	win, ok := lastValidBars(bars, 2)
	if !ok {
		return patUnknown()
	}
	prev, cur := win[0], win[1]
	if !isBull(prev) || barBody(prev) <= 0 {
		return patKnownFalse()
	}
	if !isBear(cur) {
		return patKnownFalse()
	}
	return patHit(cur.Open > prev.Close && cur.Close < bodyMid(prev) && cur.Close > prev.Open)
}

// patHammer 锤子线：下影线 ≥ 实体 2 倍、上影线 ≤ 实体，实体非零。
//
// **只描述当日形态、不判位置**：同样的形状出现在低位叫「锤子线」（看涨），
// 出现在高位叫「上吊线」（看跌）。位置判定交给用户/模型用 pos_60、chg_20d 等因子组合，
// 本因子不替他们下结论——这正是「形态是描述性因子不是信号」的具体体现。
func patHammer(bars []datasource.Bar) patternResult {
	win, ok := lastValidBars(bars, 1)
	if !ok {
		return patUnknown()
	}
	b := win[0]
	body := barBody(b)
	if body <= 0 {
		return patKnownFalse() // 实体为零属十字星族，走 doji 因子
	}
	return patHit(barLowerShadow(b) >= 2*body && barUpperShadow(b) <= body)
}

// patDoji 十字星：实体 ≤ 当日振幅的 10%，且振幅 > 0。
// **一字板（振幅=0）明确不算**——它没有上下影线，把它算成十字星会让「十字星选股」
// 每天选出一堆涨跌停一字板。振幅为零是可判定的事实，故返回 false 而非 unknown。
func patDoji(bars []datasource.Bar) patternResult {
	win, ok := lastValidBars(bars, 1)
	if !ok {
		return patUnknown()
	}
	b := win[0]
	rng := barRange(b)
	if rng <= 0 {
		return patKnownFalse()
	}
	return patHit(barBody(b) <= rng*dojiBodyRatio)
}

// patThreeWhiteSoldiers 红三兵：连续三根阳线，收盘逐日走高，
// 且每根开盘价落在前一根实体之内（prevOpen ≤ open ≤ prevClose，不跳空高开）。
// 开盘落在前一根实体内是经典口径的核心——连续三根跳空高开的是加速赶顶，不是稳步推进。
func patThreeWhiteSoldiers(bars []datasource.Bar) patternResult {
	win, ok := lastValidBars(bars, 3)
	if !ok {
		return patUnknown()
	}
	for _, b := range win {
		if !isBull(b) || barBody(b) <= 0 {
			return patKnownFalse()
		}
	}
	for i := 1; i < 3; i++ {
		prev, cur := win[i-1], win[i]
		if cur.Close <= prev.Close {
			return patKnownFalse()
		}
		if cur.Open < prev.Open || cur.Open > prev.Close {
			return patKnownFalse()
		}
	}
	return patHit(true)
}

// patConsecutiveUp 三连涨：最近 consecutiveUpDays 个交易日收盘价逐日走高。
// **按收盘口径**（不看阴阳）：低开高走的阴线只要收盘比昨天高，对持有者就是赚的。
func patConsecutiveUp(bars []datasource.Bar) patternResult {
	need := consecutiveUpDays + 1
	n := len(bars)
	if n < need {
		return patUnknown()
	}
	win := bars[n-need:]
	for _, b := range win {
		if b.Close <= 0 {
			return patUnknown()
		}
	}
	for i := 1; i < need; i++ {
		if win[i].Close <= win[i-1].Close {
			return patKnownFalse()
		}
	}
	return patHit(true)
}
