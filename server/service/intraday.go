package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

// M3b 盘中因子：腾讯 5 分钟线盘后全市场同步 + 五件盘中因子聚合落库。
//   - 数据源单一（腾讯 mkline 免鉴权），失败即诚实缺失该股该日，无备用源；
//   - 上游可回溯约 18 个交易日（实测 800 根），错过 job 的日子按游标补跑
//     （上限 intradayBackfillMaxDays），更早历史不追；
//   - 只落物化因子不存原始 bar（见 model/intraday.go 头注释的取舍声明）；
//   - 诚实门槛：盘中策略回测需累计 ≥60 交易日样本才开放，当前仅作推荐
//     T-1 加分信号，每轮同步日志打印累计天数。
//
// 错峰：数据 15:00 收盘即终态（cutoff 15:10 保证启动补跑及时），常态定时
// 17:05——避开 16:10 全市场日线 / 16:35 涨停池+人气 / 18:45 龙虎榜 / 19:05 财报
// 四个既有 job 的进程内高峰；腾讯域与东财互不影响。

const (
	optIntradayDay = "intraday_day" // 已成功同步的交易日游标

	intradayCutoffMin = 15*60 + 10 // 15:10 后当日 5 分钟线视为终态可采
	intradayJobHour   = 17         // 常态定时 17:05
	intradayJobMin    = 5

	// intradayWorkers/intradayThrottle 同步并发与单 worker 节流：
	// 5150 只 ÷ 8 worker × (80ms + 网络 RTT) ≈ 2~4 分钟/轮。
	intradayWorkers  = 8
	intradayThrottle = 80 * time.Millisecond

	// intradayMinBars 当日完整性门槛：<46 根（完整 48）说明当日数据残缺
	//（盘中拉取/上游缺根/临时停牌半天），整行不落——宁缺勿错。
	intradayMinBars = 46

	// intradayBackfillMaxDays 游标补跑回看的交易日上限（上游深度实测 18 日，留余量）。
	intradayBackfillMaxDays = 10

	// intradayAbortStreak 连续源类失败达此值判腾讯限流/故障，本轮中止
	//（照 wideInitAbortStreak 先例：限流期逐只硬扫只会加速打上游）。
	intradayAbortStreak = 20

	// intradayBacktestMinDays 盘中策略回测开放门槛（交易日数，诚实原则）。
	intradayBacktestMinDays = 60
)

// intradaySyncRunning 同步防抖（后台 job 与启动补跑共用）。
var intradaySyncRunning atomic.Bool

// IntradayService 盘中因子同步与查询。fetchMin5 可注入（单测假源）。
type IntradayService struct {
	fetchMin5 func(ctx context.Context, market, symbol string, count int) ([]datasource.Min5Bar, error)
}

func NewIntradayService() *IntradayService {
	ta := datasource.NewTencentAdapter()
	return &IntradayService{fetchMin5: ta.GetMin5Bars}
}

// ---------- 纯函数（单测锚点） ----------

// min5Date / min5Clock 从 bar 时间戳 YYYYMMDDHHmm 切日期（2026-07-09）与时分（HHmm）。
func min5Date(t string) string {
	if len(t) != 12 {
		return ""
	}
	return t[:4] + "-" + t[4:6] + "-" + t[6:8]
}

func min5Clock(t string) string {
	if len(t) != 12 {
		return ""
	}
	return t[8:]
}

// splitMin5ByDate 按交易日分组并组内按时间升序（上游本就升序，排序兜底防假源乱序）。
func splitMin5ByDate(bars []datasource.Min5Bar) map[string][]datasource.Min5Bar {
	out := map[string][]datasource.Min5Bar{}
	for _, b := range bars {
		d := min5Date(b.Time)
		if d == "" {
			continue
		}
		out[d] = append(out[d], b)
	}
	for d := range out {
		sort.Slice(out[d], func(i, j int) bool { return out[d][i].Time < out[d][j].Time })
	}
	return out
}

// min5Typical 单根典型价（上游无成交额，VWAP 以 量×典型价 估算；
// (O+H+L+C)/4 与 chip.go 筹码峰同一口径）。
func min5Typical(b datasource.Min5Bar) float64 {
	return (b.Open + b.High + b.Low + b.Close) / 4
}

// computeIntradayFactors 由某日 5 分钟序列（升序）聚合五件盘中因子。
// ok=false 的情形：根数不足（<intradayMinBars）、锚点根缺失（0935/1030/1430/1500，
// 按时间戳查找而非下标——缺根时下标会错位）、全天或半场零成交（数据可疑）。
func computeIntradayFactors(bars []datasource.Min5Bar) (model.IntradayFactorDaily, bool) {
	var out model.IntradayFactorDaily
	if len(bars) < intradayMinBars {
		return out, false
	}
	byClock := make(map[string]datasource.Min5Bar, len(bars))
	for _, b := range bars {
		byClock[min5Clock(b.Time)] = b
	}
	openBar, ok1 := byClock["0935"]  // 首根（含集合竞价）
	amEnd, ok2 := byClock["1030"]    // 早盘 1 小时末根
	tailBase, ok3 := byClock["1430"] // 尾盘 30 分钟基准
	closeBar, ok4 := byClock["1500"] // 末根（含收盘竞价）
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return out, false
	}
	if openBar.Open <= 0 || tailBase.Close <= 0 {
		return out, false
	}

	var volAll, volTail int64
	var amtAll, amtAM, amtPM float64
	var volAM, volPM int64
	for _, b := range bars {
		clock := min5Clock(b.Time)
		amt := float64(b.Volume) * min5Typical(b)
		volAll += b.Volume
		amtAll += amt
		if clock > "1430" {
			volTail += b.Volume
		}
		if clock <= "1130" {
			volAM += b.Volume
			amtAM += amt
		} else {
			volPM += b.Volume
			amtPM += amt
		}
	}
	if volAll <= 0 || volAM <= 0 || volPM <= 0 {
		return out, false // 全天/半场零成交：极端异常行情或脏数据，不硬算
	}
	vwap := amtAll / float64(volAll)
	if vwap <= 0 {
		return out, false
	}

	out.Tail30Chg = round2((closeBar.Close - tailBase.Close) / tailBase.Close * 100)
	out.Tail30VolPct = round2(float64(volTail) / float64(volAll) * 100)
	out.MorningChg = round2((amEnd.Close - openBar.Open) / openBar.Open * 100)
	out.CloseVsVwap = round2((closeBar.Close - vwap) / vwap * 100)
	out.PmVwapUp = amtPM/float64(volPM) > amtAM/float64(volAM)
	out.Vwap = round2(vwap)
	out.BarCount = len(bars)
	return out, true
}

// min5CountForDays 覆盖 n 个交易日所需请求根数（48 根/日 + 上日尾部余量）。
func min5CountForDays(n int) int {
	if n < 1 {
		n = 1
	}
	c := 48*n + 12
	if c > 800 {
		c = 800
	}
	return c
}

// ---------- 盘后同步 ----------

// SyncIntradayFactors 全市场同步 dates（升序交易日列表）的盘中因子：
// 宇宙=market_sync_states cn 全体，每股一次请求覆盖全部目标日，8 worker 并发。
// 返回落库总行数。连续源类失败达阈值中止（返回已完成部分与错误）。
func (s *IntradayService) SyncIntradayFactors(ctx context.Context, dates []string) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	if len(dates) == 0 {
		return 0, nil
	}
	if !intradaySyncRunning.CompareAndSwap(false, true) {
		return 0, ErrSyncInProgress
	}
	defer intradaySyncRunning.Store(false)

	var symbols []string
	if err := common.DB.Model(&model.MarketSyncState{}).
		Where("market = ?", "cn").Order("symbol").Pluck("symbol", &symbols).Error; err != nil {
		return 0, err
	}
	if len(symbols) == 0 {
		return 0, errors.New("宇宙字典为空（全市场日线尚未初始化）")
	}

	dateSet := make(map[string]bool, len(dates))
	for _, d := range dates {
		dateSet[d] = true
	}
	count := min5CountForDays(len(dates))

	var (
		mu      sync.Mutex
		rows    []model.IntradayFactorDaily
		srcFail atomic.Int64 // 连续源类失败（ErrNoData/ErrSymbolInvalid 不算）
		aborted atomic.Bool
		wg      sync.WaitGroup
	)
	jobs := make(chan string)
	worker := func() {
		defer wg.Done()
		for sym := range jobs {
			if ctx.Err() != nil || aborted.Load() {
				continue // 排空通道
			}
			fctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			bars, err := s.fetchMin5(fctx, "cn", sym, count)
			cancel()
			switch {
			case err == nil:
				srcFail.Store(0)
				byDate := splitMin5ByDate(bars)
				mu.Lock()
				for d, dayBars := range byDate {
					if !dateSet[d] {
						continue
					}
					if f, ok := computeIntradayFactors(dayBars); ok {
						f.Symbol, f.Market, f.TradeDate = sym, "cn", d
						rows = append(rows, f)
					}
				}
				mu.Unlock()
			case errors.Is(err, datasource.ErrNoData), errors.Is(err, datasource.ErrSymbolInvalid):
				// 标的问题（退市/长停/代码格式）：静默跳过，不计源失败。
				srcFail.Store(0)
			case ctx.Err() != nil:
				// 整体取消/超时，非源故障。
			default:
				if srcFail.Add(1) >= intradayAbortStreak {
					aborted.Store(true)
				}
			}
			time.Sleep(intradayThrottle)
		}
	}
	wg.Add(intradayWorkers)
	for i := 0; i < intradayWorkers; i++ {
		go worker()
	}
	for _, sym := range symbols {
		if ctx.Err() != nil || aborted.Load() {
			break
		}
		jobs <- sym
	}
	close(jobs)
	wg.Wait()

	if aborted.Load() {
		return 0, fmt.Errorf("腾讯 5 分钟线连续 %d 只失败，疑似限流，本轮中止（未落库）", intradayAbortStreak)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// 先删目标日再插（事务）：盘中/重复触发的重跑以最终拉取为准，幂等。
	if err := common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("market = ? AND trade_date IN ?", "cn", dates).
			Delete(&model.IntradayFactorDaily{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.CreateInBatches(rows, 500).Error
	}); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// intradayPendingDates 游标（不含）到 target（含）之间的开市日升序列表，
// 上限 intradayBackfillMaxDays（超出的更早日子诚实放弃——上游深度有限）。
// cursor 为空（首轮部署）只做 target 一日。
func intradayPendingDates(cursor, target string) []string {
	if target == "" {
		return nil
	}
	if cursor == "" || cursor >= target {
		if cursor == target {
			return nil
		}
		return []string{target}
	}
	var dates []string
	if common.DB != nil {
		if err := common.DB.Model(&model.TradingCalendar{}).
			Where("market = ? AND is_open = ? AND trade_date > ? AND trade_date <= ?", "cn", true, cursor, target).
			Order("trade_date ASC").Pluck("trade_date", &dates).Error; err != nil {
			dates = nil
		}
	}
	if len(dates) == 0 {
		dates = []string{target} // 无日历回退：至少补目标日
	}
	if len(dates) > intradayBackfillMaxDays {
		dates = dates[len(dates)-intradayBackfillMaxDays:]
	}
	return dates
}

// IntradayAccumulatedDays 已积累的盘中因子交易日数（盘中策略回测 60 日门槛的进度）。
func IntradayAccumulatedDays() int64 {
	if common.DB == nil {
		return 0
	}
	var n int64
	common.DB.Model(&model.IntradayFactorDaily{}).Where("market = ?", "cn").
		Distinct("trade_date").Count(&n)
	return n
}

// StartIntradayJobs 盘后 5 分钟线同步：启动 4 分钟后按游标补跑（与 mood 的 3 分钟
// 错开），此后每日 17:05。
func StartIntradayJobs() *IntradayService {
	svc := NewIntradayService()
	run := func() {
		target := moodTargetDate(time.Now(), intradayCutoffMin)
		cursor := optionValue(optIntradayDay)
		if cursor == target {
			return
		}
		dates := intradayPendingDates(cursor, target)
		if len(dates) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		start := time.Now()
		n, err := svc.SyncIntradayFactors(ctx, dates)
		if err != nil {
			if errors.Is(err, ErrSyncInProgress) {
				return
			}
			common.SysWarn("盘中因子同步失败 %s: %v", strings.Join(dates, ","), err)
			return
		}
		_ = model.UpsertOption(optIntradayDay, target)
		common.SysLog("盘中因子同步完成 %s：%d 行，耗时 %v；累计 %d 个交易日（盘中策略回测需满 %d 日才开放）",
			strings.Join(dates, ","), n, time.Since(start).Round(time.Second), IntradayAccumulatedDays(), intradayBacktestMinDays)
	}
	go func() {
		if common.DB == nil {
			return
		}
		time.Sleep(4 * time.Minute)
		run()
		for {
			time.Sleep(time.Until(nextDailyAt(time.Now(), intradayJobHour, intradayJobMin)))
			run()
		}
	}()
	return svc
}

// ---------- 消费查询 ----------

// intradaySignalsFor 批量查询候选的最近一日盘中因子（T-1 口径：盘中生成推荐时
// 今日 job 未跑，按 MAX(trade_date) 查询最近已同步交易日，与 lhbSignalsFor 同款）。
// P1 水位：库内最新日期落后应有交易日超过容忍值时按无信号处理（signalDateUsable），
// 旧盘中形态不冒充近期信号。
func intradaySignalsFor(symbols []string) map[string]model.IntradayFactorDaily {
	out := map[string]model.IntradayFactorDaily{}
	if common.DB == nil || len(symbols) == 0 {
		return out
	}
	var latest string
	common.DB.Model(&model.IntradayFactorDaily{}).Where("market = ?", "cn").
		Select("MAX(trade_date)").Scan(&latest)
	if !signalDateUsable(latest) {
		return out
	}
	var rows []model.IntradayFactorDaily
	common.DB.Where("market = ? AND trade_date = ? AND symbol IN ?", "cn", latest, symbols).Find(&rows)
	for _, r := range rows {
		out[r.Symbol] = r
	}
	return out
}
