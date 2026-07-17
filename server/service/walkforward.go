package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// S3-5 walk-forward 评估基线（RECOMMENDATION_ACCURACY_PLAN §5 S3-5 + S3-3 评估口径）：
// 训练/验证/测试滚动切分，手工评分（五维 computeScore + strategyAdjust 策略加分）
// 作为 baseline 跑通整套评估——为 S4 模型化排序备好可复用的切分与评估框架。
//
// 口径纪律：
//   - 切分：Purge ≥ 该段最大持有期（训练段末信号的标签收益窗口不得与验证/测试段
//     重叠）、Embargo 再加 5 交易日；测试段只用于最终验收；折右对齐（最新数据必被
//     测试）。手工评分是固定规则无参数可拟合——训练/验证段对 baseline 只是占位，
//     框架完整性为 S4 模型换入准备（届时训练段拟合、验证段调参、测试段验收）；
//   - 窗口自适应：目标 24~36 月训练窗（480/90/60），当前全市场地基每股仅约 250 根
//     日线远不可得——按比例缩窗到实际可得数据并在报告声明（快照/日线积累后自动
//     趋近目标窗），连最小窗都放不下时 0 折是合法输出；
//   - 评分复刻（A 类第一版，§5 S3-3b 分层）：五维分（尾 90 根）+ 策略加分 + 低位
//     高换手扣分 + 系统级换手排除（>30% 硬拦、20~30% 高位死亡换手），与 scorePool
//     同款；C 类数据（估值/情绪/龙虎榜/人气/资金流/盘中/财务）无法可靠回填——相关
//     加分历史缺席，长线策略退化为纯技术面，报告如实声明；用户个性化筛选（股价/
//     市值/追高保护等偏好项）不进 baseline——评估对象是评分排序质量非个人筛选；
//   - 收益：simulateHold 统一执行净收益（次日开盘成交、扣费、五件套保守语义），
//     与 recommendation_labels 的 next_open 口径可比；alpha = 净收益 − 上证基准
//     区间收益（买入日 close → 卖出日 close，与 BatchBacktest/追踪同口径）；
//   - 评估指标（S3-3 落地）：Precision_net@K 与 Precision_alpha@K 分开报告、
//     净收益中位数与严重亏损率（net<-5%）并列；短线（5/10 日）/长线（20 日）分开，
//     长线 60 日窗在 250 根历史下连一折都切不出，等数据积累后加入（诚实缺席）；
//   - 月度走查（§5 S3 验收项）：最近 12 个月每月首个交易日 × 每策略评分 Top10
//     组合的成熟表现与成员明细，持有期未走完的近月如实标 pending。
//
// 纯测量零门控零 LLM 调用：不改写任何推荐行为，p11/s9 等版本号全部不变。
// 计算量：每信号日一次全市场 as-of 因子重算（含筹码，忠实复刻线上评分），
// 默认参数约 25~55 个横截面、数十秒级；全局互斥 + 进程内缓存（factoric 同款）。

const (
	// 目标窗口（§5 S3-5：训练 24~36 月/验证 3~6 月/测试 3~6 月，每 20 交易日滚动）。
	wfTargetTrain = 480
	wfTargetVal   = 90
	wfTargetTest  = 60
	wfStepDays    = 20
	wfEmbargoDays = 5
	// 缩窗下限：再小的段连一次像样的评估都撑不起，不如如实报 0 折。
	wfMinTrain = 60
	wfMinVal   = 15
	wfMinTest  = 15

	wfMaxFolds   = 3 // 最多保留最新 N 折（每信号日一次全市场重算，控制耗时）
	wfSegSignals = 4 // 每折每段（val/test）最多采样信号日数

	wfTopK          = 10  // 评分 Top-K 组合
	wfPerCap        = 2e4 // 模拟每标的拨款（与回测/召回评估同量级，够一手即可）
	wfMinAmount     = 3e7 // 流动性门槛：近 20 日成交额中位数 ≥3000 万（对齐召回机会集）
	wfSevereLoss    = -5.0
	wfMonthlyMonths = 12
)

// wfShortHolds / wfLongHolds 各段评估持有期。长线 60 日：purge=60 时 250 根历史
// 连最小窗都放不下（60+2×65+15+15>250），等日线/快照积累后再加入，报告声明。
var (
	wfShortHolds = []int{5, 10}
	wfLongHolds  = []int{20}
)

// ---------- 切分纯函数 ----------

// wfSpec 一套切分的窗口参数（单位：交易日）。
type wfSpec struct {
	Train   int `json:"train"`
	Val     int `json:"val"`
	Test    int `json:"test"`
	Step    int `json:"step"`
	Purge   int `json:"purge"`
	Embargo int `json:"embargo"`
}

func (sp wfSpec) gap() int  { return sp.Purge + sp.Embargo }
func (sp wfSpec) need() int { return sp.Train + sp.Val + sp.Test + 2*sp.gap() }

// wfFold 一折的轴下标（闭区间；下标基于信号位轴 0..n-1）。
type wfFold struct {
	TrainLo, TrainHi int
	ValLo, ValHi     int
	TestLo, TestHi   int
}

// wfSplitFolds 右对齐滚动切分：n 为可用信号位数（日轴长已收缩 maxHold+1）。
// 最后一折测试段右边界恰贴 n-1（最新数据必被测试），往左按 Step 滚动铺折，
// 放不下即止；折序返回旧→新。
func wfSplitFolds(n int, sp wfSpec) []wfFold {
	if sp.Train <= 0 || sp.Val <= 0 || sp.Test <= 0 || sp.Step <= 0 || n < sp.need() {
		return nil
	}
	nFolds := (n-sp.need())/sp.Step + 1
	if nFolds > wfMaxFolds {
		nFolds = wfMaxFolds
	}
	folds := make([]wfFold, 0, nFolds)
	for k := 0; k < nFolds; k++ {
		lo := (n - sp.need()) - (nFolds-1-k)*sp.Step
		f := wfFold{TrainLo: lo}
		f.TrainHi = f.TrainLo + sp.Train - 1
		f.ValLo = f.TrainHi + sp.gap() + 1
		f.ValHi = f.ValLo + sp.Val - 1
		f.TestLo = f.ValHi + sp.gap() + 1
		f.TestHi = f.TestLo + sp.Test - 1
		folds = append(folds, f)
	}
	return folds
}

// wfAdaptSpec 窗口自适应：数据够目标窗直接用；不足时训练:验证:测试按 480:90:60
// 比例缩到实际可得（下限兜底，超预算部分从训练段扣——评估段优先保住），连最小窗
// 都放不下返回 ok=false（0 折）。adapted=true 表示报告须声明「窗口按实际数据缩放」。
func wfAdaptSpec(n, purge int) (sp wfSpec, adapted, ok bool) {
	sp = wfSpec{Train: wfTargetTrain, Val: wfTargetVal, Test: wfTargetTest,
		Step: wfStepDays, Purge: purge, Embargo: wfEmbargoDays}
	if n >= sp.need() {
		return sp, false, true
	}
	budget := n - 2*sp.gap()
	if budget < wfMinTrain+wfMinVal+wfMinTest {
		return sp, true, false
	}
	ratio := float64(budget) / float64(wfTargetTrain+wfTargetVal+wfTargetTest)
	shrink := func(target, min int) int {
		v := int(float64(target) * ratio)
		if v < min {
			v = min
		}
		return v
	}
	sp.Train = shrink(wfTargetTrain, wfMinTrain)
	sp.Val = shrink(wfTargetVal, wfMinVal)
	sp.Test = shrink(wfTargetTest, wfMinTest)
	if over := sp.Train + sp.Val + sp.Test - budget; over > 0 {
		sp.Train -= over
		if sp.Train < wfMinTrain {
			return sp, true, false
		}
	}
	return sp, true, true
}

// wfSampleN 段 [lo,hi] 内等距采样 ≤m 个下标（含两端附近；与回测 sampleSignalDates
// 同思路）。m<=1 取段末（最新）。
func wfSampleN(lo, hi, m int) []int {
	n := hi - lo + 1
	if n <= 0 || m <= 0 {
		return nil
	}
	if m > n {
		m = n
	}
	if m == 1 {
		return []int{hi}
	}
	step := float64(n-1) / float64(m-1)
	seen := map[int]bool{}
	out := make([]int, 0, m)
	for k := 0; k < m; k++ {
		i := lo + int(math.Round(float64(k)*step))
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	return out
}

// wfMonthlyIdxs 最近 months 个月每月首个交易日的轴下标（≤maxIdx）。轴起点所在的
// 月份几乎必然被历史窗口截断（「首个交易日」实为窗口起点非真月首），丢弃不计。
func wfMonthlyIdxs(axis []string, maxIdx, months int) []int {
	if maxIdx >= len(axis) {
		maxIdx = len(axis) - 1
	}
	firstOf := map[string]int{}
	var order []string
	for i := 0; i <= maxIdx; i++ {
		if len(axis[i]) < 7 {
			continue
		}
		m := axis[i][:7]
		if _, ok := firstOf[m]; !ok {
			firstOf[m] = i
			order = append(order, m)
		}
	}
	if len(order) > 0 && firstOf[order[0]] == 0 {
		order = order[1:]
	}
	if len(order) > months {
		order = order[len(order)-months:]
	}
	out := make([]int, 0, len(order))
	for _, m := range order {
		out = append(out, firstOf[m])
	}
	return out
}

// ---------- 评分复刻 ----------

// wfStrategyDef 参与评估的（类型, 策略）对——从 shortStrategies/longStrategies
// 单一权威构建，禁手抄清单。
type wfStrategyDef struct {
	recType string
	key     string
	name    string
}

func wfStrategyList() []wfStrategyDef {
	out := make([]wfStrategyDef, 0, len(shortStrategies)+len(longStrategies))
	for _, s := range shortStrategies {
		out = append(out, wfStrategyDef{model.RecTypeShortTerm, s.Key, s.Name})
	}
	for _, s := range longStrategies {
		out = append(out, wfStrategyDef{model.RecTypeLongTerm, s.Key, s.Name})
	}
	return out
}

// wfScoreStock 对截至信号日的日线切片复刻线上量化评分，一次算出全部策略总分
// （与 wfStrategyList 对齐）。返回 nil 表示该股该日不参与：现价缺失、流动性门槛
// 不达标、或命中系统级换手排除（>30% 极端换手 / 20~30% 且 60 日高位死亡换手）。
//
// 与 scorePool 的口径对齐点：五维分吃尾 factorBarLimit=90 根（positionScore 全窗
// 口径防漂移）、candFactors 吃尾 chipBarLimit=210 根、筹码本地复算、低位高换手
// 分级扣分（20~25% -5 / 25~30% -8）。差异（as-of 现实）：现价=信号日收盘（线上是
// 实时行情）；C 类字段（估值/情绪/榜单/资金流/盘中/财务）历史缺席，相关加分不触发。
func wfScoreStock(symbol, name string, sub []datasource.Bar, defs []wfStrategyDef) []float64 {
	n := len(sub)
	if n == 0 {
		return nil
	}
	last := sub[n-1]
	price := last.Close
	if price <= 0 {
		return nil
	}
	// 流动性门槛：近 20 根成交额中位数（对齐召回评估机会集口径）。
	amtWin := sub
	if len(amtWin) > 20 {
		amtWin = amtWin[len(amtWin)-20:]
	}
	amts := make([]float64, 0, len(amtWin))
	for _, b := range amtWin {
		if b.Amount > 0 {
			amts = append(amts, b.Amount)
		}
	}
	if len(amts) == 0 {
		return nil
	}
	sort.Float64s(amts)
	if median(amts) < wfMinAmount {
		return nil
	}

	barsFact := sub
	if len(barsFact) > chipBarLimit {
		barsFact = barsFact[len(barsFact)-chipBarLimit:]
	}
	barsScore := barsFact
	if len(barsScore) > factorBarLimit {
		barsScore = barsScore[len(barsScore)-factorBarLimit:]
	}
	sc := computeScore(price, barsScore)
	f := computeCandFactors(price, barsFact)
	if f == nil {
		return nil
	}
	if chip, err := computeChipDistribution(barsFact, 0); err == nil {
		f.ChipProfit = chip.Profit
		f.ChipAvgCost = chip.AvgCost
		f.ChipBars = chip.BarCount
	}

	c := candidate{Symbol: symbol, Name: name, Price: price,
		Amount: last.Amount, TurnoverRate: last.TurnoverRate}
	// 系统级换手排除（非用户偏好，属评分宇宙口径）：>30% 无条件硬拦；
	// 20~30% 且 60 日区间高位=死亡换手排除（applyTurnoverPosFilter 同款）。
	if c.TurnoverRate > deadTurnoverHardPct {
		return nil
	}
	if reason := applyTurnoverPosFilter(c, f); reason != "" {
		return nil
	}

	scores := make([]float64, len(defs))
	for si, def := range defs {
		delta, _ := strategyAdjust(def.recType, def.key, c, f)
		if c.TurnoverRate > deadTurnoverPct {
			// 低位高换手分级扣分（scorePool 同款；能走到这里必是低位保留档）。
			if c.TurnoverRate > 25 {
				delta -= 8
			} else {
				delta -= 5
			}
		}
		scores[si] = round2(clamp0100(sc.Total + delta))
	}
	return scores
}

// ---------- 报表结构 ----------

// WFMetric 一组样本（若干信号日 × Top-K 名额）的评估指标（S3-3 口径落地）。
type WFMetric struct {
	Signals int `json:"signals"` // 参与的信号日数
	Picked  int `json:"picked"`  // Top-K 名额总数（signals×K 上限）
	Trades  int `json:"trades"`  // 实际成交样本数
	Skipped int `json:"skipped"` // 防御计数：Top-K 已限定可成交机会集，正常恒 0
	Pending int `json:"pending"` // 持有期未走完（月度走查近月的常态）

	PrecisionNetPct float64 `json:"precision_net_pct"` // net>0 占比（Precision_net@K）
	MedianNetPct    float64 `json:"median_net_pct"`    // 净收益中位数
	AvgNetPct       float64 `json:"avg_net_pct"`
	SevereLossPct   float64 `json:"severe_loss_pct"` // net<-5% 占比（严重亏损率）

	AlphaSample       int     `json:"alpha_sample"`
	PrecisionAlphaPct float64 `json:"precision_alpha_pct"` // alpha>0 占比（Precision_alpha@K）
	MedianAlphaPct    float64 `json:"median_alpha_pct"`
}

// WFSegRow 一折一段一策略一持有期的指标行；Fold=0 表示全部折的该段合并。
type WFSegRow struct {
	Fold         int    `json:"fold"`    // 1 起；0=全折合并
	Segment      string `json:"segment"` // val / test
	Strategy     string `json:"strategy"`
	StrategyName string `json:"strategy_name"`
	Hold         int    `json:"hold"`
	WFMetric
}

// WFFoldView 折的日期范围（供报表展示与复现）。
type WFFoldView struct {
	Fold        int       `json:"fold"`
	TrainRange  [2]string `json:"train_range"`
	ValRange    [2]string `json:"val_range"`
	TestRange   [2]string `json:"test_range"`
	ValSignals  int       `json:"val_signals"`
	TestSignals int       `json:"test_signals"`
}

// WFMonthlyItem 月度走查组合成员明细。
type WFMonthlyItem struct {
	Symbol   string   `json:"symbol"`
	Name     string   `json:"name"`
	Score    float64  `json:"score"`
	Status   string   `json:"status"` // traded / skip_* / pending
	NetPct   *float64 `json:"net_pct,omitempty"`
	AlphaPct *float64 `json:"alpha_pct,omitempty"`
}

// WFMonthlyRow 月度走查一行：某月首个交易日 × 某策略的评分 Top-K 组合。
type WFMonthlyRow struct {
	Month        string `json:"month"`
	SignalDate   string `json:"signal_date"`
	Strategy     string `json:"strategy"`
	StrategyName string `json:"strategy_name"`
	Hold         int    `json:"hold"`
	WFMetric
	Items []WFMonthlyItem `json:"items"`
}

// WFSectionReport 短线/长线各一段的评估结果。
type WFSectionReport struct {
	RecType    string         `json:"rec_type"` // short_term / long_term
	Holds      []int          `json:"holds"`
	Strategies []string       `json:"strategies"`
	Spec       wfSpec         `json:"spec"`        // 实际采用的窗口
	TargetSpec wfSpec         `json:"target_spec"` // 目标窗口（§5 S3-5）
	Adapted    bool           `json:"adapted"`     // true=窗口按实际数据缩放
	SpecNote   string         `json:"spec_note"`
	Folds      []WFFoldView   `json:"folds"`
	Rows       []WFSegRow     `json:"rows"`
	Monthly    []WFMonthlyRow `json:"monthly"`
}

// WalkForwardReport walk-forward 评估基线报表。
type WalkForwardReport struct {
	TradeDate   string            `json:"trade_date"`
	TopK        int               `json:"top_k"`
	Universe    int               `json:"universe"`
	StSkipped   int               `json:"st_skipped"`
	Suspects    int               `json:"adjust_suspect"`
	Sections    []WFSectionReport `json:"sections"`
	Notes       []string          `json:"notes"`
	ElapsedMs   int64             `json:"elapsed_ms"`
	GeneratedAt time.Time         `json:"generated_at"`
}

// ---------- 评估引擎 ----------

var (
	wfInflight  atomic.Bool
	wfReportMu  sync.RWMutex
	wfReportCur *WalkForwardReport
)

// CachedWalkForwardReport 上次计算结果（可能 nil）。
func CachedWalkForwardReport() *WalkForwardReport {
	wfReportMu.RLock()
	defer wfReportMu.RUnlock()
	return wfReportCur
}

// wfHoldRes 单观测单持有期的执行结局（紧凑暂存，聚合时引用）。
type wfHoldRes struct {
	status   string
	netPct   float64
	buyDate  string
	sellDate string
}

// wfObs 单股单信号日的观测：全部策略总分 + 全部持有期执行结局。
type wfObs struct {
	sigIdx int
	symbol string
	name   string
	scores []float64
	holds  []wfHoldRes
}

// wfSection 内部：一段（短线/长线）的切分与信号日规划。
type wfSection struct {
	recType  string
	holds    []int
	maxHold  int
	eligible int
	spec     wfSpec
	adapted  bool
	specOK   bool
	folds    []wfFold
	valIdxs  [][]int // 每折 val 段采样下标
	testIdxs [][]int // 每折 test 段采样下标
}

func buildWFSection(recType string, holds []int, axisLen int) wfSection {
	sec := wfSection{recType: recType, holds: holds, maxHold: holds[len(holds)-1]}
	// 信号日 i 的到期卖出根为 i+maxHold（须 ≤ 轴末）→ 可用信号位 axisLen−maxHold
	//（恰好能走完最长持有期的最后一个信号日也参与）。
	sec.eligible = axisLen - sec.maxHold
	if sec.eligible < 1 {
		sec.eligible = 0
	}
	sec.spec, sec.adapted, sec.specOK = wfAdaptSpec(sec.eligible, sec.maxHold)
	if !sec.specOK {
		return sec
	}
	sec.folds = wfSplitFolds(sec.eligible, sec.spec)
	for _, f := range sec.folds {
		sec.valIdxs = append(sec.valIdxs, wfSampleN(f.ValLo, f.ValHi, wfSegSignals))
		sec.testIdxs = append(sec.testIdxs, wfSampleN(f.TestLo, f.TestHi, wfSegSignals))
	}
	return sec
}

// RunWalkForward 全量重算 walk-forward 评估（数十秒级重活，全局互斥）；
// 结果写进程内缓存。market 供基准日轴与 alpha（不可得时 alpha 如实缺席）。
func RunWalkForward(ctx context.Context, market *MarketService) (*WalkForwardReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !wfInflight.CompareAndSwap(false, true) {
		return nil, errors.New("walk-forward 评估进行中，请稍后再试")
	}
	defer wfInflight.Store(false)
	start := time.Now()

	freshDate, err := wideFreshDate()
	if err != nil {
		return nil, err
	}
	bt := NewBacktestService(market)
	axis, benchClose, benchNote := bt.marketAxis(ctx, freshDate)
	if len(axis) < wfMinTrain {
		return nil, errors.New("交易日轴数据不足，无法评估")
	}

	// 两段切分（短线/长线各自 purge）+ 月度走查信号日（每月首个交易日，两段共用）。
	sections := []wfSection{
		buildWFSection(model.RecTypeShortTerm, wfShortHolds, len(axis)),
		buildWFSection(model.RecTypeLongTerm, wfLongHolds, len(axis)),
	}
	monthlyIdxs := wfMonthlyIdxs(axis, len(axis)-2, wfMonthlyMonths)

	// 信号日并集（升序）。
	sigSet := map[int]bool{}
	for _, sec := range sections {
		for _, idxs := range append(append([][]int{}, sec.valIdxs...), sec.testIdxs...) {
			for _, i := range idxs {
				sigSet[i] = true
			}
		}
	}
	for _, i := range monthlyIdxs {
		sigSet[i] = true
	}
	sigIdxs := make([]int, 0, len(sigSet))
	for i := range sigSet {
		sigIdxs = append(sigIdxs, i)
	}
	sort.Ints(sigIdxs)
	if len(sigIdxs) == 0 {
		return nil, errors.New("历史数据不足以规划任何信号日（连月度走查也不可得）")
	}
	sigDates := make([]string, len(sigIdxs))
	for k, i := range sigIdxs {
		sigDates[k] = axis[i]
	}
	common.SysLog("walk-forward 评估开始: %d 个横截面（每横截面一次全市场 as-of 因子重算）", len(sigIdxs))

	// 宇宙元数据（states 小表一次读全，与回测/IC 同款）。
	var states []model.MarketSyncState
	if err := common.DB.Select("symbol", "name").Where("market = ?", "cn").Find(&states).Error; err != nil {
		return nil, err
	}
	metaBy := make(map[string]wideStockMeta, len(states))
	for _, st := range states {
		metaBy[st.Symbol] = wideStockMeta{Name: st.Name, ST: isSTName(st.Name)}
	}
	// ST as-of（防前视/幸存者偏差）：优先宇宙快照（S0-3）按信号日判定——后来才变
	// ST 的股票不得被提前从历史样本剔除；快照未覆盖的日期回退当前名称并在 Notes 声明。
	stByDate := universeSTByDates(sigDates)
	stFallback := len(stByDate) < len(sigDates)
	defs := wfStrategyList()
	allHolds := wfAllHolds()

	// 流式读全市场日线 → worker 池按股评分+结算（重计算照 backtest.go 模式）。
	type job struct {
		symbol string
		bars   []datasource.Bar
	}
	jobs := make(chan job, 16)
	obsCh := make(chan *wfObs, 128)
	var universe, stSkipped, suspects atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < wideFactorWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				meta := metaBy[j.symbol]
				if isCNFund(j.symbol) {
					continue // 全市场地基理论上无基金，自选来源残留防御
				}
				stAt := func(d string) bool {
					if set, ok := stByDate[d]; ok {
						return set[j.symbol]
					}
					return meta.ST // 快照未覆盖：回退当前名称
				}
				allST := true
				for _, d := range sigDates {
					if !stAt(d) {
						allST = false
						break
					}
				}
				if allST {
					stSkipped.Add(1)
					continue
				}
				if adjustSuspect(j.bars, j.symbol, meta.Name) {
					suspects.Add(1)
					continue
				}
				universe.Add(1)
				dateIdx := make(map[string]int, len(j.bars))
				for i, b := range j.bars {
					dateIdx[b.TradeDate] = i
				}
				for k, d := range sigDates {
					if stAt(d) {
						continue // 该信号日为 ST（as-of）：仅排除当日观测
					}
					i, ok := dateIdx[d]
					if !ok {
						continue // 信号日停牌：无信号
					}
					scores := wfScoreStock(j.symbol, meta.Name, j.bars[:i+1], defs)
					if scores == nil {
						continue
					}
					sigIdx := sigIdxs[k]
					nextDate := ""
					if sigIdx+1 < len(axis) {
						nextDate = axis[sigIdx+1]
					}
					holds := make([]wfHoldRes, len(allHolds))
					for hi, hd := range allHolds {
						sellDate := labelFarFuture // 到期日在轴外（未来）：必 pending
						if sigIdx+hd < len(axis) {
							sellDate = axis[sigIdx+hd] // 市场轴到期日：停牌不拉长持有跨度
						}
						o := simulateHold(j.bars, i, j.symbol, meta.Name, hd, wfPerCap, nextDate, sellDate)
						holds[hi] = wfHoldRes{status: o.Status, netPct: o.ReturnPct,
							buyDate: o.BuyDate, sellDate: o.SellDate}
					}
					obsCh <- &wfObs{sigIdx: sigIdx, symbol: j.symbol, name: meta.Name,
						scores: scores, holds: holds}
				}
			}
		}()
	}

	// 收集：按信号日分桶。
	byDate := map[int][]*wfObs{}
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for o := range obsCh {
			byDate[o.sigIdx] = append(byDate[o.sigIdx], o)
		}
	}()

	streamErr := streamCNDailyBars(ctx, func(symbol string, bars []datasource.Bar) {
		jobs <- job{symbol: symbol, bars: bars}
	})
	close(jobs)
	wg.Wait()
	close(obsCh)
	<-collectDone
	if streamErr != nil {
		return nil, streamErr
	}

	// 每（信号日, 策略）预排 Top-K（score 降序、并列按代码升序稳定可复现）。
	// 机会集限定当日可成交（§184 口径，与召回评估一致）：入场判定 skip_*（一字板
	// 买不进/次日停牌/不足一手）的观测不参与 Top-K——否则「推了一堆买不进的涨停票、
	// 只成交 1 只且盈利」会显示 100% Precision。skip 状态由入场段决定与持有期无关，
	// 取任一持有期判定即可；pending（持有期未走完）保留。
	holdIdxOf := map[int]int{}
	for hi, h := range allHolds {
		holdIdxOf[h] = hi
	}
	topOf := make(map[int][][]*wfObs, len(byDate)) // sigIdx → [策略下标] → TopK 观测
	for sigIdx, list := range byDate {
		tradable := make([]*wfObs, 0, len(list))
		for _, o := range list {
			switch o.holds[0].status {
			case btTraded, btPending:
				tradable = append(tradable, o)
			}
		}
		tops := make([][]*wfObs, len(defs))
		for si := range defs {
			sorted := append([]*wfObs(nil), tradable...)
			sort.Slice(sorted, func(a, b int) bool {
				if sorted[a].scores[si] != sorted[b].scores[si] {
					return sorted[a].scores[si] > sorted[b].scores[si]
				}
				return sorted[a].symbol < sorted[b].symbol
			})
			if len(sorted) > wfTopK {
				sorted = sorted[:wfTopK]
			}
			tops[si] = sorted
		}
		topOf[sigIdx] = tops
	}

	benchRet := func(buyDate, sellDate string) *float64 {
		b0, ok0 := benchClose[buyDate]
		b1, ok1 := benchClose[sellDate]
		if !ok0 || !ok1 || b0 <= 0 {
			return nil
		}
		v := round2((b1 - b0) / b0 * 100)
		return &v
	}

	// aggregate 一组（信号日 × 策略 × 持有期）名额 → 指标。
	aggregate := func(sigList []int, si, hold int) WFMetric {
		hi := holdIdxOf[hold]
		m := WFMetric{Signals: 0}
		var nets, alphas []float64
		severe := 0
		for _, sigIdx := range sigList {
			tops, ok := topOf[sigIdx]
			if !ok {
				continue
			}
			m.Signals++
			for _, o := range tops[si] {
				m.Picked++
				r := o.holds[hi]
				switch r.status {
				case btTraded:
					m.Trades++
					nets = append(nets, r.netPct)
					if r.netPct < wfSevereLoss {
						severe++
					}
					if a := benchRet(r.buyDate, r.sellDate); a != nil {
						alphas = append(alphas, round2(r.netPct-*a))
					}
				case btPending:
					m.Pending++
				default:
					m.Skipped++
				}
			}
		}
		if m.Trades > 0 {
			wins := 0
			var sum float64
			for _, v := range nets {
				sum += v
				if v > 0 {
					wins++
				}
			}
			sort.Float64s(nets)
			m.PrecisionNetPct = round2(float64(wins) / float64(m.Trades) * 100)
			m.MedianNetPct = round2(median(nets))
			m.AvgNetPct = round2(sum / float64(m.Trades))
			m.SevereLossPct = round2(float64(severe) / float64(m.Trades) * 100)
		}
		if m.AlphaSample = len(alphas); m.AlphaSample > 0 {
			wins := 0
			for _, v := range alphas {
				if v > 0 {
					wins++
				}
			}
			sort.Float64s(alphas)
			m.PrecisionAlphaPct = round2(float64(wins) / float64(m.AlphaSample) * 100)
			m.MedianAlphaPct = round2(median(alphas))
		}
		return m
	}

	// 组装分段报表。
	rep := &WalkForwardReport{TradeDate: freshDate, TopK: wfTopK, GeneratedAt: time.Now()}
	targetSpec := wfSpec{Train: wfTargetTrain, Val: wfTargetVal, Test: wfTargetTest,
		Step: wfStepDays, Embargo: wfEmbargoDays}
	for _, sec := range sections {
		view := WFSectionReport{RecType: sec.recType, Holds: sec.holds,
			Spec: sec.spec, TargetSpec: targetSpec, Adapted: sec.adapted}
		view.TargetSpec.Purge = sec.maxHold
		secDefIdx := make([]int, 0, len(defs))
		for si, def := range defs {
			if def.recType == sec.recType {
				secDefIdx = append(secDefIdx, si)
				view.Strategies = append(view.Strategies, def.key)
			}
		}
		switch {
		case !sec.specOK:
			view.SpecNote = fmt.Sprintf("历史数据不足（可用信号位 %d 个，最小窗需 %d 个），本段 0 折——等日线/快照积累后自动可用",
				sec.eligible, wfMinTrain+wfMinVal+wfMinTest+2*(sec.maxHold+wfEmbargoDays))
		case sec.adapted:
			view.SpecNote = fmt.Sprintf("窗口已按实际数据缩放（目标 24~36 月训练窗需 %d 个信号位，当前可用 %d 个）；测试段只用于最终验收",
				view.TargetSpec.need(), sec.eligible)
		default:
			view.SpecNote = "已达目标窗口（24~36 月训练窗）"
		}
		if sec.recType == model.RecTypeLongTerm {
			view.SpecNote += "；60 日持有期在当前历史下连一折都切不出，等数据积累后加入"
		}
		for fi, f := range sec.folds {
			view.Folds = append(view.Folds, WFFoldView{
				Fold:       fi + 1,
				TrainRange: [2]string{axis[f.TrainLo], axis[f.TrainHi]},
				ValRange:   [2]string{axis[f.ValLo], axis[f.ValHi]},
				TestRange:  [2]string{axis[f.TestLo], axis[f.TestHi]},
				ValSignals: len(sec.valIdxs[fi]), TestSignals: len(sec.testIdxs[fi]),
			})
		}
		// 指标行：各折 val/test + 全折 test 合并（Fold=0）。滚动窗口的测试段可能重叠，
		// 合并前按信号日去重——同一信号日重复计入会虚高 Signals/Trades 并让重复样本
		// 加权汇总指标。
		for _, si := range secDefIdx {
			def := defs[si]
			for _, hold := range sec.holds {
				var allTest []int
				seenTest := map[int]bool{}
				for fi := range sec.folds {
					view.Rows = append(view.Rows, WFSegRow{Fold: fi + 1, Segment: "val",
						Strategy: def.key, StrategyName: def.name, Hold: hold,
						WFMetric: aggregate(sec.valIdxs[fi], si, hold)})
					view.Rows = append(view.Rows, WFSegRow{Fold: fi + 1, Segment: "test",
						Strategy: def.key, StrategyName: def.name, Hold: hold,
						WFMetric: aggregate(sec.testIdxs[fi], si, hold)})
					for _, idx := range sec.testIdxs[fi] {
						if !seenTest[idx] {
							seenTest[idx] = true
							allTest = append(allTest, idx)
						}
					}
				}
				if len(sec.folds) > 1 {
					view.Rows = append(view.Rows, WFSegRow{Fold: 0, Segment: "test",
						Strategy: def.key, StrategyName: def.name, Hold: hold,
						WFMetric: aggregate(allTest, si, hold)})
				}
			}
		}
		// 月度走查：每月首个交易日 × 该段策略 × 代表持有期（短线 10 / 长线 20）。
		monthlyHold := sec.holds[len(sec.holds)-1]
		hi := holdIdxOf[monthlyHold]
		for _, si := range secDefIdx {
			def := defs[si]
			for _, sigIdx := range monthlyIdxs {
				tops, ok := topOf[sigIdx]
				if !ok {
					continue
				}
				row := WFMonthlyRow{Month: axis[sigIdx][:7], SignalDate: axis[sigIdx],
					Strategy: def.key, StrategyName: def.name, Hold: monthlyHold,
					WFMetric: aggregate([]int{sigIdx}, si, monthlyHold)}
				for _, o := range tops[si] {
					item := WFMonthlyItem{Symbol: o.symbol, Name: o.name,
						Score: o.scores[si], Status: o.holds[hi].status}
					if r := o.holds[hi]; r.status == btTraded {
						net := r.netPct
						item.NetPct = &net
						if a := benchRet(r.buyDate, r.sellDate); a != nil {
							alpha := round2(r.netPct - *a)
							item.AlphaPct = &alpha
						}
					}
					row.Items = append(row.Items, item)
				}
				view.Monthly = append(view.Monthly, row)
			}
		}
		rep.Sections = append(rep.Sections, view)
	}

	rep.Universe = int(universe.Load())
	rep.StSkipped = int(stSkipped.Load())
	rep.Suspects = int(suspects.Load())
	rep.ElapsedMs = time.Since(start).Milliseconds()
	rep.Notes = append(rep.Notes,
		"baseline=手工评分（五维分+策略加分+换手扣分），A 类因子按历史日线 as-of 切片复刻（无未来泄露）；手工评分无参数可拟合——训练/验证段为 S4 模型化排序占位，对 baseline 而言全部段均是样本外",
		"收益=统一执行模拟净收益（次日开盘成交、扣费、持有期按市场交易日推进）；alpha=净收益−上证基准区间收益（close→close）",
		"Top-K 组合从当日可成交机会集中选出（一字板买不进/次日停牌/不足一手先剔除，§184 口径与召回评估一致）——Precision 分母不被不可成交标的虚高",
		"Precision_net@K 与 Precision_alpha@K 分开报告；净收益中位数与严重亏损率（net<-5%）并列——不许靠少量大涨样本拉均值（S3-3 口径）",
		"C 类数据（估值/情绪/龙虎榜/人气/资金流/盘中/财务）无法可靠回填，相关策略加分历史缺席——长线策略在本报表退化为纯技术面，与线上实际评分存在已声明的口径差",
		"评分宇宙：剔除 ST/复权断层/成交额中位数 <3000 万/极端与高位死亡换手，与推荐候选宇宙方向一致；用户个性化筛选（股价/市值/追高保护偏好）不进 baseline",
		"Purge=各段最大持有期、Embargo="+fmt.Sprint(wfEmbargoDays)+" 交易日；折右对齐保证最新数据被测试；重叠测试段合并前按信号日去重；单折样本极少时指标波动大，凭多折与样本量判读",
	)
	if stFallback {
		rep.Notes = append(rep.Notes, "部分信号日早于宇宙快照（S0-3）积累起点，ST 判定回退当前名称——后来才变 ST 的股票会被提前剔除（轻微幸存者偏差），随快照积累自动消失")
	}
	if benchNote != "" {
		rep.Notes = append(rep.Notes, benchNote)
	}

	wfReportMu.Lock()
	wfReportCur = rep
	wfReportMu.Unlock()
	common.SysLog("walk-forward 评估完成: %d 个横截面，宇宙 %d 只（剔 ST %d/断层 %d），耗时 %dms",
		len(sigIdxs), rep.Universe, rep.StSkipped, rep.Suspects, rep.ElapsedMs)
	return rep, nil
}

// wfAllHolds 全部段持有期的并集（升序去重）。
func wfAllHolds() []int {
	seen := map[int]bool{}
	var out []int
	for _, h := range append(append([]int{}, wfShortHolds...), wfLongHolds...) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Ints(out)
	return out
}

// universeSTByDates 各评估日的 ST 集合（宇宙快照 as-of：取 ≤ 该日的最近快照日）。
// 返回 date → ST symbol 集合；快照未覆盖的日期不出现在结果里——调用方回退当前
// 名称判定并声明偏差（后来才变 ST 的股票会被提前剔除）。walk-forward 与因子 IC
// 的历史评估共用。
func universeSTByDates(dates []string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if common.DB == nil {
		return out
	}
	for _, d := range dates {
		var snapDay string
		common.DB.Model(&model.StockUniverseDaily{}).
			Where("trade_date <= ?", d).Select("MAX(trade_date)").Scan(&snapDay)
		if snapDay == "" {
			continue
		}
		var syms []string
		common.DB.Model(&model.StockUniverseDaily{}).
			Where("trade_date = ? AND is_st = ?", snapDay, true).Pluck("symbol", &syms)
		set := make(map[string]bool, len(syms))
		for _, s := range syms {
			set[s] = true
		}
		out[d] = set
	}
	return out
}
