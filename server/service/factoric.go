package service

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// S3-4 因子 IC 验证（RECOMMENDATION_ACCURACY_PLAN §5 S3-4）：Go 实现 Spearman 秩相关，
// 对历史横截面因子 × 未来 5/10/20 日收益算 RankIC 时间序列，输出各因子 IC 均值/
// ICIR/胜率，管理后台只读页展示排行。
//
// 口径纪律：
//   - 历史横截面按 S3-3b **A 类因子**重建——可从历史日线可靠重建的技术/量价因子，
//     复用 M2 回测的 computeWideRowOpts as-of 切片能力（信号日因子=对截至该日的
//     日线切片复跑，无未来泄露）；C 类（资金/情绪/盘中）只前向积累不回填，不入本报表；
//   - 因子白名单只收横截面可比的无量纲因子（百分比/比率/计数）——close/ma5/boll_up
//     等价格量纲因子跨股排名无意义，macd_dif/dea/hist 随股价缩放同理排除；
//     chip_* 属 A 类但重算 150 价格档 × 每横截面全市场代价过大，暂不入（后续按需加）；
//   - 未来收益 = 因子日收盘 → h 个交易日后收盘（不扣费）——IC 测量因子区分度，
//     非可执行收益；净收益口径的验证在 walk-forward（S3-5）做；
//   - 权重与加分项的调整**只以样本外结果为准**，本报表只出数不做删改判定，
//     不设「必须删几个因子」的验收指标（§5 S3-4 明文）；
//   - 与推荐宇宙一致：排除 ST 与复权断层股（adjustSuspect）。
//
// 计算量：默认 ≤24 个横截面 × 全市场 ≈ 12 万次因子行重算（跳筹码），数秒级；
// 全局互斥防并发重跑，结果进程内缓存供只读页复用。

const (
	icHorizonMax = 20 // 最长收益窗口（右边界收缩用）
	icDateStep   = 8  // 横截面采样间隔（交易日）
	icMaxDates   = 24 // 最多横截面数
	icMinCross   = 50 // 单横截面最小有效样本量（低于则该日该因子不计 IC）
)

// icHorizons 收益窗口（交易日）。
var icHorizons = []int{5, 10, 20}

// icFactorKeys A 类无量纲因子白名单（IC 排行的固定口径；调整需连注释一起改）。
var icFactorKeys = []string{
	"chg_pct", "amount_yi", "turnover_rate",
	"bias_20", "bias_250", "ma_spread_pct",
	"chg_5d", "chg_20d", "chg_60d",
	"pos_60", "pos_250",
	"drawdown_20", "volatility_20",
	"vol_boost", "vol_5v20",
	"rsi_14", "boll_pos", "atr_pct",
	"limit_ups_5d",
}

// ---------- Spearman 秩相关 ----------

// rankAvg 平均秩（并列取平均，1 起）。
func rankAvg(xs []float64) []float64 {
	n := len(xs)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
	ranks := make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		avg := float64(i+j+2) / 2 // 秩 1 起：位置 i..j 的平均秩
		for k := i; k <= j; k++ {
			ranks[idx[k]] = avg
		}
		i = j + 1
	}
	return ranks
}

// spearman Spearman 秩相关：对秩做 Pearson（并列平均秩，含并列时的通用口径）。
// 样本 <3 或任一侧零方差返回 NaN。
func spearman(xs, ys []float64) float64 {
	n := len(xs)
	if n < 3 || n != len(ys) {
		return math.NaN()
	}
	rx, ry := rankAvg(xs), rankAvg(ys)
	var sx, sy float64
	for i := 0; i < n; i++ {
		sx += rx[i]
		sy += ry[i]
	}
	mx, my := sx/float64(n), sy/float64(n)
	var cov, vx, vy float64
	for i := 0; i < n; i++ {
		dx, dy := rx[i]-mx, ry[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx <= 0 || vy <= 0 {
		return math.NaN()
	}
	return cov / math.Sqrt(vx*vy)
}

// icAggregate IC 时间序列的汇总统计：均值 / ICIR（均值/标准差，样本标准差）/
// 正 IC 占比。空序列返回零值 ok=false。
func icAggregate(ics []float64) (mean, icir, winRate float64, ok bool) {
	n := len(ics)
	if n == 0 {
		return 0, 0, 0, false
	}
	var sum float64
	wins := 0
	for _, v := range ics {
		sum += v
		if v > 0 {
			wins++
		}
	}
	mean = sum / float64(n)
	winRate = float64(wins) / float64(n) * 100
	if n >= 2 {
		var ss float64
		for _, v := range ics {
			ss += (v - mean) * (v - mean)
		}
		if sd := math.Sqrt(ss / float64(n-1)); sd > 0 {
			icir = mean / sd
		}
	}
	return mean, icir, winRate, true
}

// ---------- 报表结构 ----------

// ICHorizonAgg 单因子单收益窗口的 IC 汇总。
type ICHorizonAgg struct {
	MeanIC     float64 `json:"mean_ic"`      // RankIC 均值
	ICIR       float64 `json:"icir"`         // 均值/标准差
	WinRatePct float64 `json:"win_rate_pct"` // IC>0 的横截面占比
	Days       int     `json:"days"`         // 参与统计的横截面数
}

// FactorICStat 单因子排行行。
type FactorICStat struct {
	Key      string                  `json:"key"`
	Name     string                  `json:"name"`
	Group    string                  `json:"group"`
	Horizons map[string]ICHorizonAgg `json:"horizons"` // "5"/"10"/"20"
}

// FactorICReport IC 验证报表（管理后台只读页）。
type FactorICReport struct {
	TradeDate    string         `json:"trade_date"`    // 数据末日
	Dates        []string       `json:"dates"`         // 采样的横截面日期（升序）
	Universe     int            `json:"universe"`      // 参与股票数（剔 ST/断层后）
	StSkipped    int            `json:"st_skipped"`
	Suspects     int            `json:"adjust_suspect"`
	MinCross     int            `json:"min_cross"`
	Stats        []FactorICStat `json:"stats"` // 按 |10日 IC 均值| 降序
	Notes        []string       `json:"notes"`
	ElapsedMs    int64          `json:"elapsed_ms"`
	GeneratedAt  time.Time      `json:"generated_at"`
}

// ---------- 计算 ----------

var (
	icInflight  atomic.Bool
	icReportMu  sync.RWMutex
	icReportCur *FactorICReport
)

// CachedFactorICReport 上次计算结果（可能 nil）。
func CachedFactorICReport() *FactorICReport {
	icReportMu.RLock()
	defer icReportMu.RUnlock()
	return icReportCur
}

// icObservation 单股在单横截面的观测（factor 白名单值 + 各窗口未来收益，NaN=缺失）。
type icObservation struct {
	dateIdx int
	fvals   []float64
	rets    []float64
}

// RunFactorIC 全量重算 IC 报表（数秒级，全局互斥）。market 供基准日轴（不可得时
// 回退交易日历）；结果写进程内缓存。
func RunFactorIC(ctx context.Context, market *MarketService) (*FactorICReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !icInflight.CompareAndSwap(false, true) {
		return nil, errors.New("IC 计算进行中，请稍后再试")
	}
	defer icInflight.Store(false)
	start := time.Now()

	freshDate, err := wideFreshDate()
	if err != nil {
		return nil, err
	}
	bt := NewBacktestService(market)
	axis, _, _ := bt.marketAxis(ctx, freshDate)
	// 右边界收缩最长收益窗口 +1，从尾部按步长回采横截面日期。
	eligible := len(axis) - (icHorizonMax + 1)
	if eligible < 1 {
		return nil, errors.New("交易日轴数据不足，无法计算 IC")
	}
	var dates []string
	for i := eligible - 1; i >= 0 && len(dates) < icMaxDates; i -= icDateStep {
		dates = append(dates, axis[i])
	}
	sort.Strings(dates)
	dateIdxOf := make(map[string]int, len(dates))
	for i, d := range dates {
		dateIdxOf[d] = i
	}

	// 宇宙元数据（同 buildFactorTable）。
	var states []model.MarketSyncState
	if err := common.DB.Select("symbol", "name").Where("market = ?", "cn").Find(&states).Error; err != nil {
		return nil, err
	}
	metaBy := make(map[string]wideStockMeta, len(states))
	for _, st := range states {
		metaBy[st.Symbol] = wideStockMeta{Name: st.Name, ST: isSTName(st.Name)}
	}
	icKeyIdx := make([]int, len(icFactorKeys))
	for i, k := range icFactorKeys {
		icKeyIdx[i] = factorIndex[k]
	}

	// 流式读 daily_bars（与 buildFactorTable 同款读法）→ 并行按股重算横截面观测。
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
	obsCh := make(chan icObservation, 128)
	var universe, stSkipped, suspects atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < wideFactorWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				meta := metaBy[j.symbol]
				if meta.ST {
					stSkipped.Add(1)
					continue
				}
				if adjustSuspect(j.bars, j.symbol, meta.Name) {
					suspects.Add(1)
					continue
				}
				universe.Add(1)
				n := len(j.bars)
				for i, b := range j.bars {
					di, ok := dateIdxOf[b.TradeDate]
					if !ok || b.Close <= 0 {
						continue
					}
					// as-of 切片重算因子行（跳筹码，见白名单注释）。
					vals := computeWideRowOpts(j.symbol, meta, j.bars[:i+1], false)
					fv := make([]float64, len(icKeyIdx))
					for k, vi := range icKeyIdx {
						fv[k] = vals[vi]
					}
					rets := make([]float64, len(icHorizons))
					for hi, h := range icHorizons {
						rets[hi] = math.NaN()
						if i+h < n && j.bars[i+h].Close > 0 {
							rets[hi] = (j.bars[i+h].Close/b.Close - 1) * 100
						}
					}
					obsCh <- icObservation{dateIdx: di, fvals: fv, rets: rets}
				}
			}
		}()
	}

	// 收集：按 (横截面日, 因子) 组织对齐样本。
	type crossSection struct {
		fvals [][]float64 // [factor][obs]
		rets  [][]float64 // [horizon][obs]
	}
	sections := make([]*crossSection, len(dates))
	for i := range sections {
		cs := &crossSection{fvals: make([][]float64, len(icFactorKeys)), rets: make([][]float64, len(icHorizons))}
		sections[i] = cs
	}
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for o := range obsCh {
			cs := sections[o.dateIdx]
			for k, v := range o.fvals {
				cs.fvals[k] = append(cs.fvals[k], v)
			}
			for hi, v := range o.rets {
				cs.rets[hi] = append(cs.rets[hi], v)
			}
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
	close(obsCh)
	<-collectDone
	if scanErr != nil {
		return nil, scanErr
	}

	// 逐因子逐窗口：各横截面 Spearman → IC 序列 → 汇总。
	rep := &FactorICReport{
		TradeDate: freshDate, Dates: dates, MinCross: icMinCross,
		Universe: int(universe.Load()), StSkipped: int(stSkipped.Load()), Suspects: int(suspects.Load()),
		GeneratedAt: time.Now(),
	}
	for fi, key := range icFactorKeys {
		def, _ := factorByKey(key)
		st := FactorICStat{Key: key, Name: def.Name, Group: def.Group, Horizons: map[string]ICHorizonAgg{}}
		for hi, h := range icHorizons {
			var ics []float64
			for _, cs := range sections {
				var xs, ys []float64
				for oi := range cs.fvals[fi] {
					x, y := cs.fvals[fi][oi], cs.rets[hi][oi]
					if math.IsNaN(x) || math.IsNaN(y) {
						continue
					}
					xs = append(xs, x)
					ys = append(ys, y)
				}
				if len(xs) < icMinCross {
					continue
				}
				if ic := spearman(xs, ys); !math.IsNaN(ic) {
					ics = append(ics, ic)
				}
			}
			if mean, icir, win, ok := icAggregate(ics); ok {
				st.Horizons[intKey(h)] = ICHorizonAgg{
					MeanIC: round4(mean), ICIR: round2(icir), WinRatePct: round2(win), Days: len(ics),
				}
			}
		}
		if len(st.Horizons) > 0 {
			rep.Stats = append(rep.Stats, st)
		}
	}
	// 按 |10日 IC 均值| 降序（缺 10 日的排后）。
	rank := func(s FactorICStat) float64 {
		if a, ok := s.Horizons["10"]; ok {
			return math.Abs(a.MeanIC)
		}
		return -1
	}
	sort.SliceStable(rep.Stats, func(a, b int) bool { return rank(rep.Stats[a]) > rank(rep.Stats[b]) })
	rep.ElapsedMs = time.Since(start).Milliseconds()
	rep.Notes = append(rep.Notes,
		"口径：A 类因子按历史日线 as-of 切片重建（无未来泄露）；收益=因子日收盘→N 交易日后收盘（不扣费，测区分度非可执行收益）",
		"宇宙：剔除 ST 与复权断层股，与推荐候选宇宙一致；单横截面有效样本 <"+strconv.Itoa(icMinCross)+" 不计当日 IC",
		"防过拟合纪律：权重与加分项调整只以样本外结果为准；本报表只出数，不设「必须删几个因子」的指标（规划 §5 S3-4）",
		"历史窗口有限（每股约 250 根日线）：越早的横截面长窗因子（年线/250日位置）缺失越多，属数据现实非 bug",
	)

	icReportMu.Lock()
	icReportCur = rep
	icReportMu.Unlock()
	common.SysLog("因子 IC 计算完成: %d 个横截面，宇宙 %d 只，%d 因子 × %d 窗口，耗时 %dms",
		len(dates), rep.Universe, len(icFactorKeys), len(icHorizons), rep.ElapsedMs)
	return rep, nil
}

func intKey(h int) string { return strconv.Itoa(h) }

