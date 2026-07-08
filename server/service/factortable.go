package service

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// M1 第二部分：全市场因子宽表（列式内存缓存）。
//
// 数据流：daily_bars（第一部分 clist 增量 + 250 日历史初始化维护）→ 流式按股分组
// → 并行计算单股因子行 → 转列式 map[factor][]float64。全市场约 5500 只 × 250 根，
// 构建目标 <5s（DB 读为大头，因子计算并行后 <2s）。内存占用 ~60 列 × 5500 行 ≈ 3MB。
//
// 口径纪律（防回归）：
//   - 指标一律复用 indicator.go 底层序列函数（rsiSeries/macdSeries/bollSeries/atrSeries）
//     与最小样本常量，窗口因子照 recfactor.go 的尾窗口径——单测与 computeCandFactors
//     对拍保证两处不漂移；
//   - 缺失=NaN（movingAverage 不满窗、指标样本不足、筹码拒算），条件求值对 NaN 恒 false，
//     不会把「数据不足」误判成命中；candFactors 的 0=缺席语义在此升级为显式 NaN；
//   - 涨停判定按板块阈值 9.8/19.8/ST 4.8（limitUpPctFor − 0.2 容差，收盘涨幅口径）；
//   - 宽表数据是「最近收盘日」口径（16:10 增量后为当日），盘中扫描見到的是昨收因子，
//     属设计而非 bug——选股是盘后研究工具，前端展示数据日期。
//
// 新鲜度：表级 TradeDate = market_sync_states MAX(last_bar_date)（增量同步逐日推进；
// 5500 行小表，避免对 130 万行 daily_bars 做 MAX 全扫）。个股末根 < TradeDate 的
// 行（停牌/数据滞后）标 stale，扫描引擎默认排除防误导。

// ---------- 因子注册表 ----------

// factorKind 因子值的展示格式（人话化与前端格式化共用）。
type factorKind string

const (
	fkPrice factorKind = "price" // 价格：2 位小数
	fkPct   factorKind = "pct"   // 百分比：带 %
	fkRatio factorKind = "ratio" // 倍数/无量纲：2 位小数
	fkInt   factorKind = "int"   // 整数计数
	fkBool  factorKind = "bool"  // 布尔（列内 1/0/NaN）
)

// factorDef 单个因子的元数据：DSL 校验、命中原因人话化、前端因子选择器三处共用。
type factorDef struct {
	Key   string     `json:"key"`
	Name  string     `json:"name"`  // 中文名（人话化用，如「量比(5日)」）
	Group string     `json:"group"` // 分组：行情/均线/形态/量能/指标/涨停/筹码/其他
	Kind  factorKind `json:"kind"`
	Desc  string     `json:"desc"` // 一句白话说明
}

// factorDefs 宽表全部因子（有序；列构建与 DSL 校验的唯一权威清单）。
var factorDefs = []factorDef{
	// 行情（末根收盘日）
	{"close", "收盘价", "行情", fkPrice, "最近交易日收盘价"},
	{"open", "开盘价", "行情", fkPrice, "最近交易日开盘价"},
	{"high", "最高价", "行情", fkPrice, "最近交易日最高价"},
	{"low", "最低价", "行情", fkPrice, "最近交易日最低价"},
	{"chg_pct", "当日涨跌幅", "行情", fkPct, "最近交易日涨跌幅"},
	{"amount_yi", "成交额(亿)", "行情", fkRatio, "最近交易日成交额，单位亿元"},
	{"turnover_rate", "换手率", "行情", fkPct, "最近交易日换手率"},
	// 均线
	{"ma5", "5日均线", "均线", fkPrice, "5 日收盘均价"},
	{"ma10", "10日均线", "均线", fkPrice, "10 日收盘均价"},
	{"ma20", "20日均线", "均线", fkPrice, "20 日收盘均价"},
	{"ma60", "60日均线", "均线", fkPrice, "60 日收盘均价"},
	{"ma120", "120日均线", "均线", fkPrice, "120 日收盘均价（半年线）"},
	{"ma250", "250日均线", "均线", fkPrice, "250 日收盘均价（年线）"},
	{"above_ma20", "站上MA20", "均线", fkBool, "收盘价不低于 20 日均线"},
	{"above_ma60", "站上MA60", "均线", fkBool, "收盘价不低于 60 日均线"},
	{"above_ma120", "站上MA120", "均线", fkBool, "收盘价不低于 120 日均线"},
	{"bull_align", "均线多头排列", "均线", fkBool, "MA5>MA10>MA20"},
	{"bias_20", "MA20乖离率", "均线", fkPct, "(收盘/MA20−1)×100"},
	{"bias_250", "年线乖离率", "均线", fkPct, "(收盘/MA250−1)×100"},
	{"ma_spread_pct", "均线粘合度", "均线", fkPct, "MA5/10/20 的 (最大−最小)/最小×100，越小越粘合"},
	// 形态（涨跌/位置/新高）
	{"chg_5d", "近5日涨跌", "形态", fkPct, "近 5 个交易日累计涨跌幅"},
	{"chg_20d", "近20日涨跌", "形态", fkPct, "近 20 个交易日累计涨跌幅"},
	{"chg_60d", "近60日涨跌", "形态", fkPct, "近 60 个交易日累计涨跌幅"},
	{"pos_60", "60日区间位置", "形态", fkPct, "收盘价在近 60 日高低区间的位置（0=最低 100=最高）"},
	{"pos_250", "年内区间位置", "形态", fkPct, "收盘价在近 250 日高低区间的位置"},
	{"high_20d", "创20日新高", "形态", fkBool, "收盘创近 20 日收盘新高"},
	{"high_60d", "创60日新高", "形态", fkBool, "收盘创近 60 日收盘新高"},
	{"high_250d", "创年内新高", "形态", fkBool, "收盘创近 250 日收盘新高"},
	{"drawdown_20", "近20日最大回撤", "形态", fkPct, "近 20 日自高点最大回撤（正数）"},
	{"volatility_20", "波动率(20日)", "形态", fkPct, "近 20 日日收益率标准差"},
	// 量能
	{"vol_boost", "量比(5日)", "量能", fkRatio, "今日成交量 / 前 5 日均量"},
	{"vol_5v20", "量能趋势", "量能", fkRatio, "5 日均量 / 20 日均量"},
	// 指标（T1 口径）
	{"rsi_14", "RSI(14)", "指标", fkRatio, "Wilder 平滑 RSI"},
	{"macd_dif", "MACD DIF", "指标", fkRatio, "12/26 EMA 差"},
	{"macd_dea", "MACD DEA", "指标", fkRatio, "DIF 的 9 日 EMA"},
	{"macd_hist", "MACD柱", "指标", fkRatio, "2×(DIF−DEA)，A 股口径"},
	{"macd_gold", "MACD多头", "指标", fkBool, "DIF>DEA"},
	{"macd_cross_up", "MACD金叉(3日内)", "指标", fkBool, "近 3 日内 DIF 上穿 DEA"},
	{"boll_up", "布林上轨", "指标", fkPrice, "MA20+2σ"},
	{"boll_mid", "布林中轨", "指标", fkPrice, "MA20"},
	{"boll_low", "布林下轨", "指标", fkPrice, "MA20−2σ"},
	{"boll_pos", "布林带内位置", "指标", fkPct, "收盘在带内位置（<0 破下轨、>100 破上轨）"},
	{"atr_14", "ATR(14)", "指标", fkRatio, "Wilder 平滑真实波幅"},
	{"atr_pct", "ATR占比", "指标", fkPct, "ATR/收盘价×100（日均波动幅度）"},
	// 涨停（按板块阈值 9.8/19.8/ST 4.8，收盘涨幅口径）
	{"limit_up_today", "今日涨停", "涨停", fkBool, "最近交易日收盘涨停"},
	{"limit_up_yest", "昨日涨停", "涨停", fkBool, "上一交易日收盘涨停"},
	{"limit_ups_5d", "近5日涨停次数", "涨停", fkInt, "近 5 个交易日收盘涨停次数"},
	// 筹码（chip.go 三角衰减模型；<120 根拒算=缺失）
	{"chip_profit", "获利盘比例", "筹码", fkPct, "收盘价下方筹码占比"},
	{"chip_avg_cost", "筹码平均成本", "筹码", fkPrice, "全体持仓筹码的加权平均成本"},
	{"chip_bars", "筹码窗口根数", "筹码", fkInt, "参与筹码计算的日线根数（<210 精度受限）"},
	// 其他
	{"is_st", "ST/风险警示", "其他", fkBool, "名称含 ST 或退市警示"},
	{"bar_count", "日线根数", "其他", fkInt, "本股参与计算的日线根数"},
}

// factorIndex key → factorDefs 下标；factorByKey 快速查 def。
var (
	factorIndex = func() map[string]int {
		m := make(map[string]int, len(factorDefs))
		for i, d := range factorDefs {
			m[d.Key] = i
		}
		return m
	}()
)

func factorByKey(key string) (factorDef, bool) {
	i, ok := factorIndex[key]
	if !ok {
		return factorDef{}, false
	}
	return factorDefs[i], true
}

// ---------- 宽表结构 ----------

// FactorTable 列式因子宽表：Symbols 为行索引，cols 按因子 key 存列（NaN=缺失）。
// 构建完成后只读，多请求并发扫描无锁。
type FactorTable struct {
	TradeDate string // 全市场最新交易日（表数据的"新鲜"基准）
	BuiltAt   time.Time
	BuildMs   int64
	ScanMs    int64 // DB 流式读耗时（含在 BuildMs 内，供性能观察）
	Symbols   []string
	Names     []string
	LastDates []string // 各股末根交易日（= TradeDate 为 fresh）
	cols      map[string][]float64
}

// Len 行数（宇宙内标的数）。
func (t *FactorTable) Len() int { return len(t.Symbols) }

// Col 取因子列；未知因子返回 nil。
func (t *FactorTable) Col(key string) []float64 { return t.cols[key] }

// Fresh 第 i 行是否新鲜（末根=表交易日；停牌/数据滞后股为 false）。
func (t *FactorTable) Fresh(i int) bool { return t.LastDates[i] == t.TradeDate }

// ---------- 单股因子行计算 ----------

// wideStockMeta 计算单股因子行所需的宇宙元数据。
type wideStockMeta struct {
	Name string
	ST   bool
}

// computeWideRow 由升序日线算一行因子（按 factorDefs 序）。bars 需 ≥1 根。
// 布尔列 1/0，条件性缺失（样本不足）为 NaN。
func computeWideRow(symbol string, meta wideStockMeta, bars []datasource.Bar) []float64 {
	nan := math.NaN()
	vals := make([]float64, len(factorDefs))
	for i := range vals {
		vals[i] = nan
	}
	set := func(key string, v float64) { vals[factorIndex[key]] = v }
	setBool := func(key string, b bool) {
		if b {
			set(key, 1)
		} else {
			set(key, 0)
		}
	}

	n := len(bars)
	if n == 0 {
		return vals
	}
	// 与因子窗口对齐：极老库存可能超 250 根，截尾（所有窗口 ≤250）。
	if n > wideBarLimit {
		bars = bars[n-wideBarLimit:]
		n = wideBarLimit
	}
	closes := make([]float64, n)
	vols := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
		vols[i] = float64(b.Volume)
	}
	last := bars[n-1]
	price := last.Close
	if price <= 0 {
		return vals
	}

	// 行情
	set("close", round2(price))
	set("open", round2(last.Open))
	set("high", round2(last.High))
	set("low", round2(last.Low))
	set("amount_yi", round2(last.Amount/1e8))
	if last.TurnoverRate > 0 {
		set("turnover_rate", round2(last.TurnoverRate))
	}
	if n >= 2 && closes[n-2] > 0 {
		set("chg_pct", round2((price/closes[n-2]-1)*100))
	}

	// 均线族（movingAverage 不满窗返回 !ok → 保持 NaN）
	mas := map[string]float64{}
	for _, w := range []struct {
		key string
		n   int
	}{{"ma5", 5}, {"ma10", 10}, {"ma20", 20}, {"ma60", 60}, {"ma120", 120}, {"ma250", 250}} {
		if v, ok := movingAverage(closes, w.n); ok {
			r := round2(v)
			set(w.key, r)
			mas[w.key] = r
		}
	}
	if v, ok := mas["ma20"]; ok {
		setBool("above_ma20", price >= v)
		if v > 0 {
			set("bias_20", round2((price/v-1)*100))
		}
	}
	if v, ok := mas["ma60"]; ok {
		setBool("above_ma60", price >= v)
	}
	if v, ok := mas["ma120"]; ok {
		setBool("above_ma120", price >= v)
	}
	if v, ok := mas["ma250"]; ok && v > 0 {
		set("bias_250", round2((price/v-1)*100))
	}
	if ma5, ok5 := mas["ma5"]; ok5 {
		if ma10, ok10 := mas["ma10"]; ok10 {
			if ma20, ok20 := mas["ma20"]; ok20 {
				setBool("bull_align", ma5 > ma10 && ma10 > ma20)
				mn, mx := ma5, ma5
				for _, v := range []float64{ma10, ma20} {
					if v < mn {
						mn = v
					}
					if v > mx {
						mx = v
					}
				}
				if mn > 0 {
					set("ma_spread_pct", round2((mx-mn)/mn*100))
				}
			}
		}
	}

	// 涨跌/位置/新高（changeOverN 样本不足返回 0——这里改用显式满窗判定防 0 歧义）
	for _, w := range []struct {
		key string
		n   int
	}{{"chg_5d", 5}, {"chg_20d", 20}, {"chg_60d", 60}} {
		if n > w.n && closes[n-1-w.n] > 0 {
			set(w.key, changeOverN(closes, w.n))
		}
	}
	rangePos := func(win []datasource.Bar) (float64, bool) {
		hi, lo := win[0].High, win[0].Low
		for _, b := range win {
			if b.High > hi {
				hi = b.High
			}
			if b.Low < lo && b.Low > 0 {
				lo = b.Low
			}
		}
		if hi > lo {
			return round2((price - lo) / (hi - lo) * 100), true
		}
		return 50, true
	}
	win60 := bars
	if len(win60) > 60 {
		win60 = win60[len(win60)-60:]
	}
	if v, ok := rangePos(win60); ok {
		set("pos_60", v)
	}
	if v, ok := rangePos(bars); ok {
		set("pos_250", v) // bars 已截尾 ≤250
	}
	// 创 N 日新高：收盘 ≥ 前 N 根收盘最大值（recfactor High20d 同口径，收盘假突破更少）
	newHigh := func(w int) (bool, bool) {
		if n < w+1 {
			return false, false
		}
		maxPrev := closes[n-1-w]
		for _, c := range closes[n-1-w : n-1] {
			if c > maxPrev {
				maxPrev = c
			}
		}
		return closes[n-1] >= maxPrev, true
	}
	if v, ok := newHigh(20); ok {
		setBool("high_20d", v)
	}
	if v, ok := newHigh(60); ok {
		setBool("high_60d", v)
	}
	if n >= 30 { // 年内新高允许不满 250 根（上市即满足语义），但样本太少无意义
		maxPrev := closes[0]
		for _, c := range closes[:n-1] {
			if c > maxPrev {
				maxPrev = c
			}
		}
		setBool("high_250d", closes[n-1] >= maxPrev)
	}

	// 近 20 日波动率/回撤（recfactor 同口径）
	w20 := bars
	if len(w20) > 20 {
		w20 = w20[len(w20)-20:]
	}
	if len(w20) >= 2 {
		var rets []float64
		for i := 1; i < len(w20); i++ {
			if w20[i-1].Close > 0 {
				rets = append(rets, (w20[i].Close-w20[i-1].Close)/w20[i-1].Close*100)
			}
		}
		set("volatility_20", round2(stddev(rets)))
		peak := w20[0].High
		worst := 0.0
		for _, b := range w20 {
			if b.High > peak {
				peak = b.High
			}
			if peak > 0 {
				if dd := (b.Low - peak) / peak; dd < worst {
					worst = dd
				}
			}
		}
		set("drawdown_20", round2(-worst*100))
	}

	// 量能（recfactor 同口径：今日量/前 5 日均量，剔当日）
	if n >= 6 {
		var prev5 float64
		for _, v := range vols[n-6 : n-1] {
			prev5 += v
		}
		prev5 /= 5
		if prev5 > 0 {
			set("vol_boost", round2(vols[n-1]/prev5))
		}
	}
	if n >= 20 {
		avgVol := func(w int) float64 {
			var s float64
			for _, v := range vols[n-w:] {
				s += v
			}
			return s / float64(w)
		}
		if a20 := avgVol(20); a20 > 0 {
			set("vol_5v20", round2(avgVol(5)/a20))
		}
	}

	// T1 指标：直接复用 indicator.go 底层序列函数与最小样本常量（口径唯一权威）。
	if n >= rsiMinBars {
		rsi := rsiSeries(closes, 14)
		if !math.IsNaN(rsi[n-1]) {
			set("rsi_14", round2(rsi[n-1]))
		}
	}
	if n >= atrMinBars {
		atr := atrSeries(bars, 14)
		set("atr_14", round2(atr[n-1])) // round2 与 computeIndicatorSnapshot 对齐（对拍口径）
		set("atr_pct", round2(atr[n-1]/price*100))
	}
	if n >= bollMinBars {
		up, mid, low := bollSeries(closes, 20, 2)
		set("boll_up", round2(up[n-1]))
		set("boll_mid", round2(mid[n-1]))
		set("boll_low", round2(low[n-1]))
		if band := up[n-1] - low[n-1]; band > 0 {
			set("boll_pos", round2((price-low[n-1])/band*100))
		}
	}
	if n >= macdMinBars {
		dif, dea, hist := macdSeries(closes)
		set("macd_dif", round3(dif[n-1]))
		set("macd_dea", round3(dea[n-1]))
		set("macd_hist", round3(hist[n-1]))
		setBool("macd_gold", dif[n-1] > dea[n-1])
		crossUp := false
		for j := n - 3; j < n; j++ {
			if j >= 1 && dif[j-1] <= dea[j-1] && dif[j] > dea[j] {
				crossUp = true
				break
			}
		}
		setBool("macd_cross_up", crossUp)
	}

	// 涨停（板块阈值 = limitUpPctFor − 0.2：主板 9.8 / 创业科创 19.8 / ST 4.8）
	limitThreshold := limitUpPctFor(symbol, meta.Name) - 0.2
	isLimitUp := func(i int) (bool, bool) {
		if i < 1 || closes[i-1] <= 0 {
			return false, false
		}
		return (closes[i]/closes[i-1]-1)*100 >= limitThreshold, true
	}
	if v, ok := isLimitUp(n - 1); ok {
		setBool("limit_up_today", v)
	}
	if v, ok := isLimitUp(n - 2); ok {
		setBool("limit_up_yest", v)
	}
	if n >= 2 {
		cnt, known := 0, false
		for i := n - 5; i < n; i++ {
			if v, ok := isLimitUp(i); ok {
				known = true
				if v {
					cnt++
				}
			}
		}
		if known {
			set("limit_ups_5d", float64(cnt))
		}
	}

	// 筹码（<120 根或换手不可得时 err → 保持 NaN）
	if chip, err := computeChipDistribution(bars, 0); err == nil {
		set("chip_profit", chip.Profit)
		set("chip_avg_cost", chip.AvgCost)
		set("chip_bars", float64(chip.BarCount))
	}

	setBool("is_st", meta.ST)
	set("bar_count", float64(n))
	return vals
}

// ---------- 构建 ----------

// wideFactorWorkers 并行计算的 worker 数（因子计算是 CPU 密集，DB 读单流）。
func wideFactorWorkers() int {
	w := runtime.NumCPU()
	if w > 8 {
		w = 8
	}
	if w < 2 {
		w = 2
	}
	return w
}

// factorRowResult worker 输出的单股行。
type factorRowResult struct {
	symbol   string
	name     string
	lastDate string
	vals     []float64
}

// buildFactorTable 全量构建宽表：流式读 daily_bars（ORDER BY symbol 分组连续）→
// 并行算行 → 按 symbol 排序 → 转列。
func buildFactorTable(ctx context.Context) (*FactorTable, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	start := time.Now()
	tradeDate, err := wideFreshDate()
	if err != nil {
		return nil, err
	}

	// 宇宙元数据：name/ST（5500 行小表一次读全）。
	var states []model.MarketSyncState
	if err := common.DB.Select("symbol", "name").Where("market = ?", "cn").Find(&states).Error; err != nil {
		return nil, err
	}
	metaBy := make(map[string]wideStockMeta, len(states))
	for _, st := range states {
		metaBy[st.Symbol] = wideStockMeta{Name: st.Name, ST: isSTName(st.Name)}
	}

	// 流式读 + 并行计算。
	rows, err := common.DB.Model(&model.DailyBar{}).
		Select("symbol", "trade_date", "open", "high", "low", "close", "volume", "amount", "turnover_rate").
		Where("market = ?", "cn").
		Order("symbol, trade_date").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type job struct {
		symbol string
		bars   []datasource.Bar
	}
	jobs := make(chan job, 32)
	results := make(chan factorRowResult, 64)
	var wg sync.WaitGroup
	for w := 0; w < wideFactorWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				meta := metaBy[j.symbol] // 缺失时零值：name 空、非 ST（daily_bars 有而宇宙无的旧标的）
				results <- factorRowResult{
					symbol:   j.symbol,
					name:     meta.Name,
					lastDate: j.bars[len(j.bars)-1].TradeDate,
					vals:     computeWideRow(j.symbol, meta, j.bars),
				}
			}
		}()
	}
	collected := make([]factorRowResult, 0, len(states)+64)
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for r := range results {
			collected = append(collected, r)
		}
	}()

	var scanErr error
	var cur []datasource.Bar
	curSymbol := ""
	flush := func() {
		if curSymbol != "" && len(cur) > 0 {
			jobs <- job{symbol: curSymbol, bars: cur}
		}
		cur = nil
	}
	scanStart := time.Now()
	for rows.Next() {
		if ctx.Err() != nil {
			scanErr = ctx.Err()
			break
		}
		var sym, td string
		var open, high, low, closeP, amount, turnover float64
		var volume int64
		if err := rows.Scan(&sym, &td, &open, &high, &low, &closeP, &volume, &amount, &turnover); err != nil {
			scanErr = err
			break
		}
		if sym != curSymbol {
			flush()
			curSymbol = sym
			cur = make([]datasource.Bar, 0, wideBarLimit)
		}
		cur = append(cur, datasource.Bar{
			TradeDate: td, Open: open, High: high, Low: low, Close: closeP,
			Volume: volume, Amount: amount, TurnoverRate: turnover,
		})
	}
	if scanErr == nil {
		scanErr = rows.Err()
	}
	flush()
	scanMs := time.Since(scanStart).Milliseconds()
	close(jobs)
	wg.Wait()
	close(results)
	<-collectDone
	if scanErr != nil {
		return nil, scanErr
	}

	sort.Slice(collected, func(i, j int) bool { return collected[i].symbol < collected[j].symbol })
	t := &FactorTable{
		TradeDate: tradeDate,
		BuiltAt:   time.Now(),
		ScanMs:    scanMs,
		Symbols:   make([]string, len(collected)),
		Names:     make([]string, len(collected)),
		LastDates: make([]string, len(collected)),
		cols:      make(map[string][]float64, len(factorDefs)),
	}
	for _, d := range factorDefs {
		t.cols[d.Key] = make([]float64, len(collected))
	}
	for i, r := range collected {
		t.Symbols[i] = r.symbol
		t.Names[i] = r.name
		t.LastDates[i] = r.lastDate
		for j, d := range factorDefs {
			t.cols[d.Key][i] = r.vals[j]
		}
	}
	t.BuildMs = time.Since(start).Milliseconds()
	return t, nil
}

// isSTName 名称含 ST（含 *ST）或退市警示。
func isSTName(name string) bool {
	return strings.Contains(strings.ToUpper(name), "ST") || strings.Contains(name, "退")
}

// wideFreshDate 全市场"最新交易日"基准：states MAX(last_bar_date)。
// states 为空（第一部分未部署）时回退 daily_bars MAX（行数也少，代价可接受）。
func wideFreshDate() (string, error) {
	var d sql.NullString
	if err := common.DB.Model(&model.MarketSyncState{}).
		Where("market = ? AND last_bar_date <> ''", "cn").
		Select("MAX(last_bar_date)").Scan(&d).Error; err != nil {
		return "", err
	}
	if d.Valid && d.String != "" {
		return d.String, nil
	}
	if err := common.DB.Model(&model.DailyBar{}).
		Where("market = ?", "cn").
		Select("MAX(trade_date)").Scan(&d).Error; err != nil {
		return "", err
	}
	if !d.Valid || d.String == "" {
		return "", errors.New("daily_bars 无 A 股日线数据（先在管理端启动全市场同步/初始化）")
	}
	return d.String, nil
}

// ---------- 缓存管理（全局单例） ----------

var (
	factorTableMu  sync.RWMutex // 保护 factorTableCur 指针
	factorTableCur *FactorTable
	factorBuildMu  sync.Mutex  // 构建互斥：懒加载调用方阻塞等待同一次构建
	factorBuilding atomic.Bool // 状态展示 + 异步触发防抖
	// factorFreshCache 新鲜日期的 60s 缓存（防每次扫描都查 states MAX）。
	factorFreshMu   sync.Mutex
	factorFreshVal  string
	factorFreshAt   time.Time
	factorFreshTTL  = time.Minute
)

// CurrentFactorTable 当前宽表（可能 nil / 过期；扫描走 ensureFactorTable）。
func CurrentFactorTable() *FactorTable {
	factorTableMu.RLock()
	defer factorTableMu.RUnlock()
	return factorTableCur
}

// FactorTableBuilding 是否正在构建（状态端点展示）。
func FactorTableBuilding() bool { return factorBuilding.Load() }

// cachedFreshDate 带 60s 缓存的新鲜日期。
func cachedFreshDate() (string, error) {
	factorFreshMu.Lock()
	defer factorFreshMu.Unlock()
	if factorFreshVal != "" && time.Since(factorFreshAt) < factorFreshTTL {
		return factorFreshVal, nil
	}
	d, err := wideFreshDate()
	if err != nil {
		return "", err
	}
	factorFreshVal, factorFreshAt = d, time.Now()
	return d, nil
}

// ensureFactorTable 懒加载：有新鲜表直接返回；过期/缺失则同步构建（互斥+双检，
// 并发请求等待同一次构建完成）。首次构建 3~10s，个人自用可接受。
func ensureFactorTable(ctx context.Context) (*FactorTable, error) {
	fresh, err := cachedFreshDate()
	if err != nil {
		return nil, err
	}
	if t := CurrentFactorTable(); t != nil && t.TradeDate >= fresh {
		return t, nil
	}
	factorBuildMu.Lock()
	defer factorBuildMu.Unlock()
	if t := CurrentFactorTable(); t != nil && t.TradeDate >= fresh {
		return t, nil // 双检：等待期间别人已建好
	}
	factorBuilding.Store(true)
	defer factorBuilding.Store(false)
	// 构建用独立超时（不随单个请求取消——成果全局共享）。
	buildCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = ctx // 调用方取消不中断构建；等待本身受 factorBuildMu 阻塞语义约束
	t, err := buildFactorTable(buildCtx)
	if err != nil {
		return nil, err
	}
	factorTableMu.Lock()
	factorTableCur = t
	factorTableMu.Unlock()
	common.SysLog("因子宽表构建完成: %s，%d 只，%d 因子，耗时 %dms（DB 读 %dms）",
		t.TradeDate, t.Len(), len(factorDefs), t.BuildMs, t.ScanMs)
	return t, nil
}

// RebuildFactorTableAsync 异步重建（增量同步完成后/管理端手动触发挂这里）。
// TryLock 防抖：已有构建在跑则跳过（它会建出同样新鲜的表）。
func RebuildFactorTableAsync(reason string) {
	if common.DB == nil {
		return
	}
	go func() {
		if !factorBuildMu.TryLock() {
			return
		}
		defer factorBuildMu.Unlock()
		factorBuilding.Store(true)
		defer factorBuilding.Store(false)
		// 使缓存的新鲜日期失效（增量刚推进了 last_bar_date）。
		factorFreshMu.Lock()
		factorFreshVal = ""
		factorFreshMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		t, err := buildFactorTable(ctx)
		if err != nil {
			common.SysWarn("因子宽表重建失败（%s）: %v", reason, err)
			return
		}
		factorTableMu.Lock()
		factorTableCur = t
		factorTableMu.Unlock()
		common.SysLog("因子宽表重建完成（%s）: %s，%d 只，耗时 %dms（DB 读 %dms）",
			reason, t.TradeDate, t.Len(), t.BuildMs, t.ScanMs)
	}()
}

// FactorTableStatusView 宽表状态（选股页状态条 + 管理端）。
type FactorTableStatusView struct {
	Ready     bool      `json:"ready"`
	Building  bool      `json:"building"`
	TradeDate string    `json:"trade_date,omitempty"`
	BuiltAt   time.Time `json:"built_at,omitempty"`
	BuildMs   int64     `json:"build_ms,omitempty"`
	Universe  int       `json:"universe"`
	Factors   int       `json:"factors"`
}

// FactorTableStatus 当前宽表状态快照。
func FactorTableStatus() FactorTableStatusView {
	v := FactorTableStatusView{Building: FactorTableBuilding(), Factors: len(factorDefs)}
	if t := CurrentFactorTable(); t != nil {
		v.Ready = true
		v.TradeDate = t.TradeDate
		v.BuiltAt = t.BuiltAt
		v.BuildMs = t.BuildMs
		v.Universe = t.Len()
	}
	return v
}
