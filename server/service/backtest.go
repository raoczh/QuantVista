package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// M2 回测时光机：历史某日按条件树选股 → 次日开盘买入 → 持有 N 交易日收盘卖出，
// 统计收益/胜率/均值/中位 vs 上证基准。纯本地计算不碰 LLM。
//
// 无未来泄露的关键：信号日因子对「截至该日的日线切片」复跑 computeWideRowOpts——
// 与因子宽表/选股页同一套函数（口径纪律），TestBacktestNoFutureLeak 锁定切片求值
// 不受未来数据影响。
//
// A 股真实约束五件套（一条不能少，全部往「不利于策略」方向保守处理）：
//  1. 信号次日开盘成交（杜绝当日收盘价买入的未来函数）；
//  2. 次日开盘涨幅 ≥ 涨停阈值−0.5 判一字板买不进，跳过（开过板的机会保守放弃）；
//  3. 卖出日一字跌停（high==low 且收盘跌幅达阈值）卖不出，顺延下一交易日重试，
//     顺延到数据末尾仍卖不出按末根收盘强平并标注 forced；
//  4. 整百股取整，拨款不够一手放弃（skip_cash）；
//  5. 费率与模拟盘 tradeFee 同源（佣金万 2.5 最低 5 元 + 卖出印花税万 5）。
//
// 复权自洽校验（M1 遗留待办）：逐股检查相邻收盘涨跌是否超越涨停幅度 ×1.5
//（前复权序列不应出现的断层，多为除权未重锚或坏数据），可疑股整只剔除并透明计数。
//
// 数据边界（诚实披露，不掩饰）：daily_bars 每股约 250 根，信号日越早、可用历史窗
// 越短，ma250/pos_250 类长窗因子在早期信号日会缺失（NaN 不命中）；结果 notes 声明。

const (
	btDefaultLookback  = 60  // 默认信号窗口回看交易日数
	btMaxLookback      = 180
	btMinLookback      = 10
	btDefaultSignals   = 8 // 默认采样信号日数
	btMaxSignals       = 16
	btDefaultPerCap    = 20000 // 每标的默认拨款（元）
	btMinPerCap        = 5000
	btMaxPerCap        = 1000000
	btDefaultTopPerDay = 200 // 每信号日按成交额截前 N 只（照 DEVELOPMENT_PLAN M2）
	btMaxHoldDays      = 60
	btTradeSamples     = 5 // 每持有期输出 best/worst 样本条数
	// btAdjustSanityHeadSkip 复权自洽检查跳过序列头部根数：注册制新股上市前 5 日
	// 无涨跌幅限制，头部大跳变是正常现象而非除权断层。
	btAdjustSanityHeadSkip = 5
)

// backtestInflight 全局互斥：回测是数秒级重活（流式读 130 万行 + 全市场逐日因子），
// 同时只允许一个在跑（与 SyncTrackedDailyBars 的 atomic.Bool 同款纪律）。
var backtestInflight atomic.Bool

// BacktestService 回测：条件树策略时光机 + 历史推荐批次回验。
type BacktestService struct {
	market *MarketService
	// benchFn 基准日线获取（可注入假实现供测试；默认走 market.GetBenchmarkBars）。
	benchFn func(ctx context.Context) []datasource.Bar
}

func NewBacktestService(market *MarketService) *BacktestService {
	return &BacktestService{market: market}
}

// ---------- 请求/结果 ----------

// BacktestRequest 条件树回测入参（策略三选一，同 ScanRequest 语义）。
type BacktestRequest struct {
	StrategyKey string    `json:"strategy_key"`
	StrategyID  int64     `json:"strategy_id"`
	Tree        *CondNode `json:"tree"`

	LookbackDays int     `json:"lookback_days"` // 信号窗口回看交易日数（默认 60）
	SignalCount  int     `json:"signal_count"`  // 窗口内等距采样的信号日数（默认 8）
	HoldDays     []int   `json:"hold_days"`     // 持有期（交易日），默认 [5,10,20]，最多 3 个
	PerStockCap  float64 `json:"per_stock_cap"` // 每标的拨款（元，默认 2 万）
	TopPerDay    int     `json:"top_per_day"`   // 每信号日按成交额截前 N（默认 200）
	IncludeST    bool    `json:"include_st"`
}

// BacktestTrade 单笔模拟交易（best/worst 样本展示）。
type BacktestTrade struct {
	Symbol     string  `json:"symbol"`
	Name       string  `json:"name"`
	SignalDate string  `json:"signal_date"`
	BuyDate    string  `json:"buy_date"`
	SellDate   string  `json:"sell_date"`
	BuyPrice   float64 `json:"buy_price"`
	SellPrice  float64 `json:"sell_price"`
	ReturnPct  float64 `json:"return_pct"`
	AlphaPct   *float64 `json:"alpha_pct,omitempty"` // nil=基准数据缺失
	Deferred   int     `json:"deferred,omitempty"`   // 跌停顺延次数
	Forced     bool    `json:"forced,omitempty"`     // 顺延到末根强平
}

// BacktestHoldStat 单个持有期的统计。
type BacktestHoldStat struct {
	HoldDays        int     `json:"hold_days"`
	Trades          int     `json:"trades"` // 实际成交样本数
	WinRate         float64 `json:"win_rate"`
	AvgReturnPct    float64 `json:"avg_return_pct"`
	MedianReturnPct float64 `json:"median_return_pct"`
	BestPct         float64 `json:"best_pct"`
	WorstPct        float64 `json:"worst_pct"`
	AvgAlphaPct     float64 `json:"avg_alpha_pct"`
	BenchAvgPct     float64 `json:"bench_avg_pct"`
	AlphaSample     int     `json:"alpha_sample"` // 基准有效的样本数
	SkipLimitUp     int     `json:"skip_limit_up"`
	SkipCash        int     `json:"skip_cash"`
	SkipSuspend     int     `json:"skip_suspend"`
	Deferred        int     `json:"deferred"` // 跌停顺延总次数
	Forced          int     `json:"forced"`
	Pending         int     `json:"pending"` // 个股停牌等导致持有期未走完

	BestTrades  []BacktestTrade `json:"best_trades"`
	WorstTrades []BacktestTrade `json:"worst_trades"`
}

// BacktestDayStat 单个信号日的聚合行。
type BacktestDayStat struct {
	Date        string             `json:"date"`
	Matched     int                `json:"matched"` // 条件命中数（截断前）
	Taken       int                `json:"taken"`   // 按成交额截断后进入模拟的数量
	AvgReturns  map[string]float64 `json:"avg_returns"` // hold(字符串) → 平均收益 %
	TradedByHold map[string]int    `json:"traded_by_hold"`
}

// BacktestResult 条件树回测结果。
type BacktestResult struct {
	Strategy      string             `json:"strategy"`
	Conditions    []string           `json:"conditions"`
	TradeDate     string             `json:"trade_date"` // 数据末日
	SignalDates   []string           `json:"signal_dates"`
	Universe      int                `json:"universe"`       // 参与逐股处理的标的数
	AdjustSuspect int                `json:"adjust_suspect"` // 复权自洽校验剔除的股票数
	StSkipped     int                `json:"st_skipped"`
	Stats         []BacktestHoldStat `json:"stats"`
	Days          []BacktestDayStat  `json:"days"`
	Notes         []string           `json:"notes"`
	ElapsedMs     int64              `json:"elapsed_ms"`
}

// ---------- 单笔持有期模拟 ----------
// 五件套执行语义已抽至 execution_sim.go（S0-2 统一执行模拟器）：holdOutcome、
// simulateHold、isOneWordLimitDown、adjustSuspect、cnDailyBarsAsc——tracking 标签
// 结算与回测共用同一套函数，严禁在此另写一份「回测版」执行逻辑。

// ---------- 条件树的单行求值复用 ----------

// newRowTable 单行迷你宽表：worker 循环内就地覆写第 0 行，复用 evalCondRow 的
// 求值语义（NaN 恒不命中等），保证回测与选股页零口径分叉。
func newRowTable() *FactorTable {
	t := &FactorTable{
		Symbols: []string{""}, Names: []string{""}, LastDates: []string{""},
		cols: make(map[string][]float64, len(factorDefs)),
	}
	for _, d := range factorDefs {
		t.cols[d.Key] = make([]float64, 1)
	}
	return t
}

func (t *FactorTable) fillRow0(vals []float64) {
	for j, d := range factorDefs {
		t.cols[d.Key][0] = vals[j]
	}
}

// treeUsesChipFactor 条件树是否引用筹码因子（决定切片求值能否跳过筹码复算提速）。
func treeUsesChipFactor(n *CondNode) bool {
	if n == nil {
		return false
	}
	if strings.HasPrefix(n.Factor, "chip_") || strings.HasPrefix(n.Ref, "chip_") {
		return true
	}
	for i := range n.All {
		if treeUsesChipFactor(&n.All[i]) {
			return true
		}
	}
	for i := range n.Any {
		if treeUsesChipFactor(&n.Any[i]) {
			return true
		}
	}
	return false
}

// ---------- 条件树回测主流程 ----------

// btCandidate 单股在单个信号日的命中与模拟结果（截断/聚合在收集后进行）。
type btCandidate struct {
	Symbol     string
	Name       string
	SignalDate string
	AmountYi   float64
	Holds      map[int]holdOutcome
}

// Run 执行条件树回测。全局互斥（进行中返回错误），数秒级重活。
func (s *BacktestService) Run(ctx context.Context, userID int64, req BacktestRequest) (*BacktestResult, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !backtestInflight.CompareAndSwap(false, true) {
		return nil, errors.New("已有回测在进行中，请稍后再试")
	}
	defer backtestInflight.Store(false)
	start := time.Now()

	// 策略树（复用选股服务的解析与校验）。
	scr := ScreenerService{}
	tree, name, err := scr.resolveTree(userID, ScanRequest{
		StrategyKey: req.StrategyKey, StrategyID: req.StrategyID, Tree: req.Tree,
	})
	if err != nil {
		return nil, err
	}
	if _, err := validateCondTree(tree, 1); err != nil {
		return nil, err
	}

	// 参数钳制。
	lookback := clampInt(req.LookbackDays, btMinLookback, btMaxLookback, btDefaultLookback)
	signalCount := clampInt(req.SignalCount, 1, btMaxSignals, btDefaultSignals)
	perCap := req.PerStockCap
	if perCap <= 0 {
		perCap = btDefaultPerCap
	}
	perCap = math.Min(math.Max(perCap, btMinPerCap), btMaxPerCap)
	topPerDay := clampInt(req.TopPerDay, 10, btDefaultTopPerDay, btDefaultTopPerDay)
	holds, err := normalizeHoldDays(req.HoldDays)
	if err != nil {
		return nil, err
	}
	maxHold := holds[len(holds)-1]

	// 数据末日 + 市场日轴 + 基准收盘。
	freshDate, err := wideFreshDate()
	if err != nil {
		return nil, err
	}
	axis, benchClose, benchNote := s.marketAxis(ctx, freshDate)
	if len(axis) < maxHold+3 {
		return nil, errors.New("交易日轴数据不足（基准指数与交易日历均不可用或过短），无法回测")
	}
	marketLast := axis[len(axis)-1] // 市场轴末日：区分个股退市/长停与真未成熟
	signalDates, err := sampleSignalDates(axis, lookback, signalCount, maxHold)
	if err != nil {
		return nil, err
	}
	axisIndex := make(map[string]int, len(axis))
	for i, d := range axis {
		axisIndex[d] = i
	}

	// 宇宙元数据（同 buildFactorTable：states 小表一次读全）。
	var states []model.MarketSyncState
	if err := common.DB.Select("symbol", "name").Where("market = ?", "cn").Find(&states).Error; err != nil {
		return nil, err
	}
	metaBy := make(map[string]wideStockMeta, len(states))
	for _, st := range states {
		metaBy[st.Symbol] = wideStockMeta{Name: st.Name, ST: isSTName(st.Name)}
	}

	// #8① ST as-of（防幸存者偏差）：优先宇宙快照（S0-3）按信号日判定——后来才变 ST 的
	// 股票不得在其健康期历史信号日被剔除；快照未覆盖的信号日回退当前名称并 Notes 声明。
	// 与 walkforward.go 同款（每信号日一次快照查询，非逐股，共 len(signalDates) 次）。
	stByDate := universeSTByDates(signalDates)
	stFallback := len(stByDate) < len(signalDates)

	withChip := treeUsesChipFactor(tree)

	// 流式读 daily_bars（与 buildFactorTable 同款：ORDER BY symbol, trade_date 恰合
	// 唯一索引序免 filesort，别改列序）→ 逐股 worker 处理，内存 O(单股)。
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
	jobs := make(chan job, 16)
	candCh := make(chan btCandidate, 64)
	var universe, suspects, stSkipped atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < wideFactorWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rowTable := newRowTable()
			for j := range jobs {
				meta := metaBy[j.symbol]
				universe.Add(1)
				// ST as-of：快照命中按快照，缺失回退当前名称。
				stAt := func(d string) bool {
					if set, ok := stByDate[d]; ok {
						return set[j.symbol]
					}
					return meta.ST
				}
				// 整只剔除只在「所有信号日 as-of 均为 ST」时（后来才变 ST 的股票其健康期
				// 信号日不受影响，仅在下方按信号日逐日排除）。
				if !req.IncludeST {
					allST := true
					for _, d := range signalDates {
						if !stAt(d) {
							allST = false
							break
						}
					}
					if allST {
						stSkipped.Add(1)
						continue
					}
				}
				// 复权自洽校验：断层股整只剔除（结果透明计数）。
				if adjustSuspect(j.bars, j.symbol, meta.Name) {
					suspects.Add(1)
					continue
				}
				// 该股日期 → 下标索引（信号日定位）。
				dateIdx := make(map[string]int, len(j.bars))
				for i, b := range j.bars {
					dateIdx[b.TradeDate] = i
				}
				for _, d := range signalDates {
					if !req.IncludeST && stAt(d) {
						continue // 该信号日 as-of 为 ST：仅排除当日观测
					}
					i, ok := dateIdx[d]
					if !ok {
						continue // 信号日停牌：无信号
					}
					sub := j.bars[:i+1]
					vals := computeWideRowOpts(j.symbol, meta, sub, withChip)
					rowTable.fillRow0(vals)
					if !evalCondRow(rowTable, tree, 0) {
						continue
					}
					nextDate := ""
					aiIdx, onAxis := axisIndex[d]
					if onAxis && aiIdx+1 < len(axis) {
						nextDate = axis[aiIdx+1]
					}
					cand := btCandidate{
						Symbol: j.symbol, Name: meta.Name, SignalDate: d,
						AmountYi: round2(j.bars[i].Amount / 1e8),
						Holds:    make(map[int]holdOutcome, len(holds)),
					}
					for _, h := range holds {
						sellDate := ""
						if onAxis && aiIdx+h+1 < len(axis) {
							sellDate = axis[aiIdx+h+1] // 卖出根=买入根(aiIdx+1)+h；个股中途停牌不拉长持有跨度
						}
						cand.Holds[h] = simulateHold(j.bars, i, j.symbol, meta.Name, h, perCap, nextDate, sellDate, marketLast)
					}
					candCh <- cand
				}
			}
		}()
	}
	var cands []btCandidate
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for c := range candCh {
			cands = append(cands, c)
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
	close(jobs)
	wg.Wait()
	close(candCh)
	<-collectDone
	if scanErr != nil {
		return nil, scanErr
	}

	res := s.aggregate(tree, name, cands, holds, signalDates, topPerDay, benchClose)
	res.TradeDate = freshDate
	res.Universe = int(universe.Load())
	res.AdjustSuspect = int(suspects.Load())
	res.StSkipped = int(stSkipped.Load())
	res.ElapsedMs = time.Since(start).Milliseconds()
	res.Notes = append(res.Notes,
		"回测基于前复权日线与东财口径成交额；费率=佣金万2.5(最低5元)+卖出印花税万5，与模拟盘一致",
		"信号次日开盘成交；开盘涨幅≥涨停阈值-0.5 视为一字板放弃；一字跌停顺延卖出；整百股取整",
		fmt.Sprintf("每信号日按成交额取前 %d 只命中进入模拟；每标的拨款 %.0f 元", topPerDay, perCap),
		"历史窗口有限（每股约 250 根）：越早的信号日可用历史越短，年线类长窗因子可能缺失导致命中偏少",
	)
	if res.AdjustSuspect > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("复权自洽校验剔除 %d 只存在收盘断层的标的（疑似除权未重锚、ST 状态变更或坏数据）", res.AdjustSuspect))
	}
	if !req.IncludeST {
		res.Notes = append(res.Notes, "ST 判定按宇宙快照 as-of 各信号日——后来才变 ST 的股票在其健康期历史信号日不被剔除（防幸存者偏差）")
		if stFallback {
			res.Notes = append(res.Notes, "部分信号日无宇宙快照（S0-3 部署前），ST 回退当前名称判定，存在轻微幸存者偏差")
		}
	}
	if benchNote != "" {
		res.Notes = append(res.Notes, benchNote)
	}
	common.SysLog("回测完成: %s，信号日 %d 个，宇宙 %d 只，命中 %d 条，耗时 %dms",
		name, len(signalDates), res.Universe, len(cands), res.ElapsedMs)
	return res, nil
}

// marketAxis 市场交易日轴 + 基准收盘 map。优先上证基准日线（指数不停牌，天然完整
// 日历 + 基准价一石二鸟）；基准不可得时回退 trading_calendar（此时无 alpha）。
func (s *BacktestService) marketAxis(ctx context.Context, freshDate string) (axis []string, benchClose map[string]float64, note string) {
	bench := s.fetchBench(ctx)
	benchClose = make(map[string]float64, len(bench))
	for _, b := range bench {
		if b.TradeDate <= freshDate && b.Close > 0 {
			axis = append(axis, b.TradeDate)
			benchClose[b.TradeDate] = b.Close
		}
	}
	sort.Strings(axis)
	if len(axis) > 0 {
		return axis, benchClose, ""
	}
	// 回退：交易日历（alpha 缺失如实声明）。
	var dates []string
	common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date <= ?", "cn", true, freshDate).
		Order("trade_date").Pluck("trade_date", &dates)
	if len(dates) > wideBarLimit {
		dates = dates[len(dates)-wideBarLimit:]
	}
	return dates, benchClose, "基准指数数据不可得，本次回测无基准对比（alpha 缺失）"
}

func (s *BacktestService) fetchBench(ctx context.Context) []datasource.Bar {
	if s.benchFn != nil {
		return s.benchFn(ctx)
	}
	if s.market == nil {
		return nil
	}
	_, bars, err := s.market.GetBenchmarkBars(ctx, "cn", benchBarLimit)
	if err != nil {
		common.SysWarn("回测拉取基准指数失败: %v", err)
		return nil
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].TradeDate < bars[j].TradeDate })
	return bars
}

// sampleSignalDates 在日轴内采样信号日：右边界收缩 maxHold+1（信号日 i 的卖出根
// = 买入根(i+1)+maxHold = i+maxHold+1，须 ≤ 轴末，恰好能走完最长持有期的最后一个
// 信号日也参与），回看 lookback 个交易日为窗口，窗口内等距取 signalCount 个（含首尾附近）。
func sampleSignalDates(axis []string, lookback, signalCount, maxHold int) ([]string, error) {
	eligible := len(axis) - maxHold - 1
	if eligible < 1 {
		return nil, errors.New("历史数据不足以覆盖最长持有期")
	}
	lo := eligible - lookback
	if lo < 0 {
		lo = 0
	}
	window := axis[lo:eligible]
	if len(window) == 0 {
		return nil, errors.New("信号窗口为空")
	}
	if signalCount > len(window) {
		signalCount = len(window)
	}
	out := make([]string, 0, signalCount)
	if signalCount == 1 {
		return append(out, window[len(window)-1]), nil
	}
	step := float64(len(window)-1) / float64(signalCount-1)
	seen := map[string]bool{}
	for k := 0; k < signalCount; k++ {
		d := window[int(math.Round(float64(k)*step))]
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// aggregate 聚合候选：每信号日按成交额截 topPerDay，再按持有期出统计与逐日行。
func (s *BacktestService) aggregate(tree *CondNode, name string, cands []btCandidate, holds []int, signalDates []string, topPerDay int, benchClose map[string]float64) *BacktestResult {
	res := &BacktestResult{
		Strategy:    name,
		Conditions:  describeCondTree(tree),
		SignalDates: signalDates,
		Stats:       make([]BacktestHoldStat, 0, len(holds)),
		Days:        make([]BacktestDayStat, 0, len(signalDates)),
	}
	byDay := map[string][]btCandidate{}
	for _, c := range cands {
		byDay[c.SignalDate] = append(byDay[c.SignalDate], c)
	}
	taken := make([]btCandidate, 0, len(cands))
	dayMatched := map[string]int{}
	dayTaken := map[string]int{}
	for _, d := range signalDates {
		list := byDay[d]
		dayMatched[d] = len(list)
		sort.Slice(list, func(a, b int) bool { return list[a].AmountYi > list[b].AmountYi })
		if len(list) > topPerDay {
			list = list[:topPerDay]
		}
		dayTaken[d] = len(list)
		taken = append(taken, list...)
	}

	// 基准区间收益（买入日 close → 卖出日 close，与推荐追踪 benchRange 同 close 口径）。
	benchRet := func(buyDate, sellDate string) *float64 {
		b0, ok0 := benchClose[buyDate]
		b1, ok1 := benchClose[sellDate]
		if !ok0 || !ok1 || b0 <= 0 {
			return nil
		}
		v := round2((b1 - b0) / b0 * 100)
		return &v
	}

	type daySums struct {
		sum    map[int]float64
		traded map[int]int
	}
	dayAgg := map[string]*daySums{}
	for _, d := range signalDates {
		dayAgg[d] = &daySums{sum: map[int]float64{}, traded: map[int]int{}}
	}

	for _, h := range holds {
		st := BacktestHoldStat{HoldDays: h}
		var rets []float64
		var trades []BacktestTrade
		var sumRet, sumAlpha, sumBench float64
		for _, c := range taken {
			o := c.Holds[h]
			switch o.Status {
			case btSkipLimitUp:
				st.SkipLimitUp++
				continue
			case btSkipCash:
				st.SkipCash++
				continue
			case btSkipSuspend:
				st.SkipSuspend++
				continue
			case btPending:
				st.Pending++
				continue
			}
			st.Deferred += o.Deferred
			if o.Forced {
				// 末根强平收益偏保守（真实中卖不出），单列计数、不进 Trades/胜率/均值
				// 主统计（否则少量强平坏结局会污染排序质量评估）。
				st.Forced++
				continue
			}
			st.Trades++
			rets = append(rets, o.ReturnPct)
			sumRet += o.ReturnPct
			tr := BacktestTrade{
				Symbol: c.Symbol, Name: c.Name, SignalDate: c.SignalDate,
				BuyDate: o.BuyDate, SellDate: o.SellDate,
				BuyPrice: o.BuyPrice, SellPrice: o.SellPrice,
				ReturnPct: o.ReturnPct, Deferred: o.Deferred, Forced: o.Forced,
			}
			if br := benchRet(o.BuyDate, o.SellDate); br != nil {
				alpha := round2(o.ReturnPct - *br)
				tr.AlphaPct = &alpha
				st.AlphaSample++
				sumAlpha += alpha
				sumBench += *br
			}
			trades = append(trades, tr)
			da := dayAgg[c.SignalDate]
			da.sum[h] += o.ReturnPct
			da.traded[h]++
		}
		if st.Trades > 0 {
			wins := 0
			for _, r := range rets {
				if r > 0 {
					wins++
				}
			}
			st.WinRate = round2(float64(wins) / float64(st.Trades) * 100)
			st.AvgReturnPct = round2(sumRet / float64(st.Trades))
			sorted := append([]float64(nil), rets...)
			sort.Float64s(sorted)
			st.MedianReturnPct = round2(median(sorted))
			st.BestPct = round2(sorted[len(sorted)-1])
			st.WorstPct = round2(sorted[0])
		}
		if st.AlphaSample > 0 {
			st.AvgAlphaPct = round2(sumAlpha / float64(st.AlphaSample))
			st.BenchAvgPct = round2(sumBench / float64(st.AlphaSample))
		}
		sort.Slice(trades, func(a, b int) bool { return trades[a].ReturnPct > trades[b].ReturnPct })
		n := len(trades)
		top := btTradeSamples
		if top > n {
			top = n
		}
		st.BestTrades = append([]BacktestTrade(nil), trades[:top]...)
		worst := append([]BacktestTrade(nil), trades[n-top:]...)
		// 最差样本按收益升序展示。
		sort.Slice(worst, func(a, b int) bool { return worst[a].ReturnPct < worst[b].ReturnPct })
		st.WorstTrades = worst
		res.Stats = append(res.Stats, st)
	}

	for _, d := range signalDates {
		row := BacktestDayStat{
			Date: d, Matched: dayMatched[d], Taken: dayTaken[d],
			AvgReturns: map[string]float64{}, TradedByHold: map[string]int{},
		}
		da := dayAgg[d]
		for _, h := range holds {
			key := fmt.Sprintf("%d", h)
			row.TradedByHold[key] = da.traded[h]
			if da.traded[h] > 0 {
				row.AvgReturns[key] = round2(da.sum[h] / float64(da.traded[h]))
			}
		}
		res.Days = append(res.Days, row)
	}
	return res
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// normalizeHoldDays 校验持有期：默认 [5,10,20]，去重升序，1~60 日，最多 3 个。
func normalizeHoldDays(hs []int) ([]int, error) {
	if len(hs) == 0 {
		return []int{5, 10, 20}, nil
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(hs))
	for _, h := range hs {
		if h < 1 || h > btMaxHoldDays {
			return nil, fmt.Errorf("持有期需在 1~%d 交易日之间", btMaxHoldDays)
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if len(out) > 3 {
		return nil, errors.New("持有期最多 3 个")
	}
	sort.Ints(out)
	return out, nil
}

// ---------- 历史推荐批次回测（alpha 分布） ----------

// BatchBacktestRequest batch_id>0 回验单个批次；=0 回验近 90 天全部成功批次。
type BatchBacktestRequest struct {
	BatchID     int64   `json:"batch_id"`
	PerStockCap float64 `json:"per_stock_cap"`
}

// BatchPickRow 单条推荐在各持有期的回验结果。
type BatchPickRow struct {
	BatchID    int64  `json:"batch_id"`
	BatchTitle string `json:"batch_title"`
	Type       string `json:"type"`
	SignalDate string `json:"signal_date"` // 推荐日（或其前最近交易日）
	Symbol     string `json:"symbol"`
	Name       string `json:"name"`
	Action     string `json:"action"`
	// hold(字符串) → 结果；status 非 traded 时 return/alpha 无意义。
	Holds map[string]BatchHoldCell `json:"holds"`
}

// BatchHoldCell 单持有期格子。
type BatchHoldCell struct {
	Status    string   `json:"status"`
	ReturnPct float64  `json:"return_pct"`
	AlphaPct  *float64 `json:"alpha_pct,omitempty"`
}

// AlphaBucket alpha 分布直方图桶。
type AlphaBucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// BatchHoldStat 推荐回验的单持有期统计 + alpha 直方图。
type BatchHoldStat struct {
	HoldDays     int           `json:"hold_days"`
	Trades       int           `json:"trades"`
	WinRate      float64       `json:"win_rate"`
	AvgReturnPct float64       `json:"avg_return_pct"`
	AvgAlphaPct  float64       `json:"avg_alpha_pct"`
	AlphaSample  int           `json:"alpha_sample"`
	Pending      int           `json:"pending"`
	Skipped      int           `json:"skipped"` // 一字板/停牌/资金不足合计
	NoData       int           `json:"no_data"`
	AlphaHist    []AlphaBucket `json:"alpha_hist"`
}

// BatchBacktestResult 推荐批次回验结果。
type BatchBacktestResult struct {
	Batches int             `json:"batches"`
	Picks   int             `json:"picks"`
	Stats   []BatchHoldStat `json:"stats"`
	Rows    []BatchPickRow  `json:"rows"` // 最多 200 行（新批次在前）
	Notes   []string        `json:"notes"`
}

const batchBacktestMaxRows = 200

var alphaBucketEdges = []float64{-10, -5, 0, 5, 10}

func alphaBucketLabel(i int) string {
	switch i {
	case 0:
		return "<-10%"
	case len(alphaBucketEdges):
		return ">+10%"
	default:
		lo, hi := alphaBucketEdges[i-1], alphaBucketEdges[i]
		return fmt.Sprintf("%+.0f%%~%+.0f%%", lo, hi)
	}
}

func alphaBucketIndex(a float64) int {
	for i, e := range alphaBucketEdges {
		if a < e {
			return i
		}
	}
	return len(alphaBucketEdges)
}

// BatchBacktest 把历史推荐批次的 picks 当「策略」跑同一套持有期引擎，输出 alpha 分布。
// 与前向追踪（tracking.go）互补：追踪看「到今天为止」，回验看「固定持有期的既成事实」。
func (s *BacktestService) BatchBacktest(ctx context.Context, userID int64, req BatchBacktestRequest) (*BatchBacktestResult, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	perCap := req.PerStockCap
	if perCap <= 0 {
		perCap = btDefaultPerCap
	}
	perCap = math.Min(math.Max(perCap, btMinPerCap), btMaxPerCap)

	var batches []model.RecommendationBatch
	q := common.DB.Where("user_id = ? AND status = ?", userID, model.RecStatusSuccess)
	if req.BatchID > 0 {
		q = q.Where("id = ?", req.BatchID)
	} else {
		q = q.Where("created_at >= ?", time.Now().AddDate(0, 0, -trackWindowDays))
	}
	if err := q.Order("created_at DESC").Find(&batches).Error; err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return nil, errors.New("没有可回验的推荐批次")
	}

	holds := []int{5, 10, 20}
	// 市场日轴 + 基准收盘（基准优先、回退日历）：轴供「推荐日次日停牌判 skip_suspend」
	// 与「到期日按市场交易日定位」（个股中途停牌不拉长持有跨度），与标签结算同口径。
	axis, benchClose, _ := s.marketAxis(ctx, time.Now().Format("2006-01-02"))
	marketLast := ""
	if len(axis) > 0 {
		marketLast = axis[len(axis)-1] // 市场轴末日：区分个股退市/长停与真未成熟
	}

	res := &BatchBacktestResult{Batches: len(batches)}
	type acc struct {
		trades, pending, skipped, noData, alphaSample int
		wins                                          int
		sumRet, sumAlpha                              float64
		hist                                          []int
	}
	accBy := map[int]*acc{}
	for _, h := range holds {
		accBy[h] = &acc{hist: make([]int, len(alphaBucketEdges)+1)}
	}

	for _, b := range batches {
		var recs []model.Recommendation
		if err := common.DB.Where("batch_id = ? AND user_id = ?", b.ID, b.UserID).
			Order("sort_order").Find(&recs).Error; err != nil {
			continue
		}
		recDate := b.CreatedAt.In(time.Local).Format("2006-01-02")
		for _, rec := range recs {
			res.Picks++
			row := BatchPickRow{
				BatchID: b.ID, BatchTitle: b.Title, Type: b.Type,
				Symbol: rec.Symbol, Name: rec.Name, Action: rec.Action,
				Holds: map[string]BatchHoldCell{},
			}
			bars := cnDailyBarsAsc(rec.Symbol)
			// 信号根：推荐日当日（盘中/收盘后生成都算当日信号）或其前最近一根。
			i := -1
			for k := len(bars) - 1; k >= 0; k-- {
				if bars[k].TradeDate <= recDate {
					i = k
					break
				}
			}
			if i < 0 {
				for _, h := range holds {
					row.Holds[fmt.Sprintf("%d", h)] = BatchHoldCell{Status: "no_data"}
					accBy[h].noData++
				}
			} else {
				row.SignalDate = bars[i].TradeDate
				for _, h := range holds {
					nextDate, sellDate := labelAxisDates(axis, recDate, h)
					o := simulateHold(bars, i, rec.Symbol, rec.Name, h, perCap, nextDate, sellDate, marketLast)
					cell := BatchHoldCell{Status: o.Status, ReturnPct: o.ReturnPct}
					a := accBy[h]
					switch o.Status {
					case btTraded:
						a.trades++
						a.sumRet += o.ReturnPct
						if o.ReturnPct > 0 {
							a.wins++
						}
						if b0, ok0 := benchClose[o.BuyDate]; ok0 && b0 > 0 {
							if b1, ok1 := benchClose[o.SellDate]; ok1 {
								alpha := round2(o.ReturnPct - round2((b1-b0)/b0*100))
								cell.AlphaPct = &alpha
								a.alphaSample++
								a.sumAlpha += alpha
								a.hist[alphaBucketIndex(alpha)]++
							}
						}
					case btPending:
						a.pending++
					default:
						a.skipped++
					}
					row.Holds[fmt.Sprintf("%d", h)] = cell
				}
			}
			if len(res.Rows) < batchBacktestMaxRows {
				res.Rows = append(res.Rows, row)
			}
		}
	}

	for _, h := range holds {
		a := accBy[h]
		st := BatchHoldStat{
			HoldDays: h, Trades: a.trades, Pending: a.pending,
			Skipped: a.skipped, NoData: a.noData, AlphaSample: a.alphaSample,
		}
		if a.trades > 0 {
			st.WinRate = round2(float64(a.wins) / float64(a.trades) * 100)
			st.AvgReturnPct = round2(a.sumRet / float64(a.trades))
		}
		if a.alphaSample > 0 {
			st.AvgAlphaPct = round2(a.sumAlpha / float64(a.alphaSample))
		}
		for i := 0; i <= len(alphaBucketEdges); i++ {
			st.AlphaHist = append(st.AlphaHist, AlphaBucket{Label: alphaBucketLabel(i), Count: a.hist[i]})
		}
		res.Stats = append(res.Stats, st)
	}
	res.Notes = append(res.Notes,
		"回验口径：推荐日（或其前最近交易日）次日开盘买入、持有 5/10/20 交易日收盘卖出，A 股约束与费率同条件树回测",
		"alpha=个股区间收益−上证同区间收益（买入日收盘→卖出日收盘口径）；基准缺失的样本不计入 alpha",
		"pending=持有期尚未走完；skipped=一字板买不进/停牌/拨款不足一手",
	)
	if len(benchClose) == 0 {
		res.Notes = append(res.Notes, "基准指数数据不可得，本次回验无 alpha")
	}
	return res, nil
}
