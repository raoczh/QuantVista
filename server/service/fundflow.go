package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 个股资金流历史（M3a）：push2his fflow/daykline 按需拉取 + 缓存（照 F2 财务缓存先例）。
// 触发点：个股详情资金流图 / 推荐流水线主力资金因子（预算内补拉），无全市场普查。
//
// 盘中口径纪律：**今日行情稳定前（16:00 前）不落今日行**——fflow 的当日 main_net 盘中
// 持续变化，若把半截值落库且新鲜判定认为「已有今日数据」，该行将永久停留在盘中值
//（与 marketwide 除权检测排除今天的根同一道理）。消费方（连续净流入天数）用截至
// 上一交易日的序列本就成立（T-1 信号），详情页缺今日一根盘后自动补上。

const (
	fflowBarLimit    = 250       // 缓存窗口（与日线/因子窗口对齐，不拉全历史占库）
	fflowTryCooldown = time.Hour // 同一标的拉取尝试冷却（成功失败都记）
	fflowRecBudget   = 16        // 单次推荐生成允许回上游补拉的标的数
	fflowStableHour  = 16        // 当日数据视为终态的小时（收盘后）
	flowStreakBonus  = 3         // 连续净流入天数加分门槛（≥3 天）
	flowVolumeWeight = 0.4       // 量能维中主力资金分的权重（0.6 原量能 + 0.4 资金）
)

// fflowSyncTry 包级共享的拉取冷却表（MoodService/ScoreService/推荐域多实例共用，
// 照 F2 finSyncTry 先例——实例字段会让冷却互相看不见）。
var (
	fflowTryMu    sync.Mutex
	fflowTry      = map[string]time.Time{}
	fflowAttempts = map[string]*fflowAttempt{}
)

type fflowAttempt struct {
	done chan struct{}
	err  error // done 关闭后只读。
}

func fflowTryAllowed(key string) bool {
	fflowTryMu.Lock()
	defer fflowTryMu.Unlock()
	if t, ok := fflowTry[key]; ok && time.Since(t) < fflowTryCooldown {
		return false
	}
	fflowTry[key] = time.Now()
	fflowAttempts[key] = &fflowAttempt{done: make(chan struct{})}
	return true
}

// stockFundFlowProbe 是推荐轮在补拉前一次性读取并冻结的本地资金流状态。
// Rows 可供详情页回退展示；只有 Fresh=true 才允许进入评分与 LLM 因子。
type stockFundFlowProbe struct {
	Market        string
	Symbol        string
	Rows          []model.FundFlowDaily
	Fresh         bool
	RefreshNeeded bool
	Err           error
}

func inspectStockFundFlow(market, symbol string, now time.Time) stockFundFlowProbe {
	return inspectStockFundFlowDB(common.DB, market, symbol, now)
}

func inspectStockFundFlowDB(db *gorm.DB, market, symbol string, now time.Time) stockFundFlowProbe {
	p := stockFundFlowProbe{Market: market, Symbol: symbol}
	if market != "cn" {
		return p
	}
	if db == nil {
		p.Err = errors.New("数据库不可用")
		return p
	}
	today := now.Format("2006-01-02")
	freshSince := prevOpenTradeDateDB(db, today)
	completeThrough := freshSince
	readThrough := now.AddDate(0, 0, -1).Format("2006-01-02")
	if now.Hour() >= fflowStableHour && isTradingDayTodayDB(db, now) {
		completeThrough = today
		readThrough = today
	}
	// 日历可能只补入了旧日线日期；它可用于时效比较，不能把更近的已终态事实从
	// 读取窗口中裁掉。排除今日盘中/未来数据的上界按当前自然日确定。
	if err := db.Where("symbol = ? AND market = ? AND trade_date <= ?", symbol, market, readThrough).
		Order("trade_date DESC").Limit(fflowBarLimit).Find(&p.Rows).Error; err != nil {
		p.Rows, p.Err = nil, fmt.Errorf("资金流读取失败: %w", err)
		return p
	}
	for left, right := 0, len(p.Rows)-1; left < right; left, right = left+1, right-1 {
		p.Rows[left], p.Rows[right] = p.Rows[right], p.Rows[left]
	}
	p.Fresh = len(p.Rows) > 0 && p.Rows[len(p.Rows)-1].TradeDate >= freshSince
	// T-1 仍可用于既有评分口径，但盘后详情必须尝试补上已终态的当日数据。
	p.RefreshNeeded = len(p.Rows) == 0 || p.Rows[len(p.Rows)-1].TradeDate < completeThrough
	return p
}

// fetchStockFundFlowReserved 执行一次已由推荐预热规划器占用冷却槽的真实请求。
// 请求失败仍返回补拉前库存及其原有时效，并且不会在本轮继续尝试其他标的。
func fetchStockFundFlowReserved(ctx context.Context, em *datasource.EastMoneyAdapter, p stockFundFlowProbe, now time.Time) (out stockFundFlowProbe) {
	out = p
	key := p.Market + ":" + p.Symbol
	fflowTryMu.Lock()
	attempt := fflowAttempts[key]
	fflowTryMu.Unlock()
	defer func() {
		fflowTryMu.Lock()
		defer fflowTryMu.Unlock()
		if attempt != nil {
			attempt.err = out.Err
			close(attempt.done)
		}
		if ctx.Err() != nil && fflowAttempts[key] == attempt {
			delete(fflowTry, key)
		}
	}()
	fctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	bars, err := em.GetStockFundFlow(fctx, p.Market, p.Symbol, fflowBarLimit)
	if err != nil {
		if !errors.Is(err, datasource.ErrNoData) {
			common.SysDebug("资金流历史拉取失败 %s: %v", p.Symbol, err)
			out.Err = fmt.Errorf("资金流拉取失败: %w", err)
		}
		return out
	}
	if err := persistFundFlowDB(common.DB.WithContext(fctx), p.Market, p.Symbol, bars, now); err != nil {
		out.Err = err
		return out
	}
	return inspectStockFundFlowDB(common.DB.WithContext(fctx), p.Market, p.Symbol, now)
}

// ensureStockFundFlow 读取某股资金流序列（升序，≤fflowBarLimit 根）；库存不新鲜且
// 预算允许时回上游补拉。budget 为 nil 表示不限预算（详情页单股场景）。
// 返回序列与「数据是否新鲜」（末行 ≥ 上一开市日）。失败返回库存（stale 也比没有强）。
func ensureStockFundFlow(ctx context.Context, em *datasource.EastMoneyAdapter, market, symbol string, budget *int) ([]model.FundFlowDaily, bool) {
	return ensureStockFundFlowAt(ctx, em, market, symbol, budget, time.Now())
}

func ensureStockFundFlowAt(ctx context.Context, em *datasource.EastMoneyAdapter, market, symbol string, budget *int, now time.Time) ([]model.FundFlowDaily, bool) {
	probe := ensureStockFundFlowProbe(ctx, em, market, symbol, budget, now)
	return probe.Rows, probe.Fresh
}

func ensureStockFundFlowProbe(ctx context.Context, em *datasource.EastMoneyAdapter, market, symbol string, budget *int, now time.Time) stockFundFlowProbe {
	if err := ctx.Err(); err != nil {
		return stockFundFlowProbe{Market: market, Symbol: symbol, Err: err}
	}
	db := common.DB
	if db != nil {
		db = db.WithContext(ctx)
	}
	probe := inspectStockFundFlowDB(db, market, symbol, now)
	if probe.Err != nil || !probe.RefreshNeeded {
		return probe
	}
	if budget != nil && *budget <= 0 {
		return probe
	}
	key := market + ":" + symbol
	if !fflowTryAllowed(key) {
		fflowTryMu.Lock()
		attempt := fflowAttempts[key]
		fflowTryMu.Unlock()
		if attempt == nil {
			probe.Err = errors.New("资金流刷新处于冷却期，请稍后重试")
			return probe
		}
		select {
		case <-ctx.Done():
			probe.Err = ctx.Err()
			return probe
		case <-attempt.done:
		}
		probe = inspectStockFundFlowDB(db, market, symbol, now)
		if probe.Err == nil {
			probe.Err = attempt.err
		}
		return probe
	}
	// 预算表示实际发出的上游请求数。冷却命中没有 I/O，不得白白占掉名额并让
	// 后续标的因遍历顺序失去补拉机会。
	if budget != nil {
		*budget--
	}
	return fetchStockFundFlowReserved(ctx, em, probe, now)
}

// fundFlowForScoring 收紧评分消费口径：ensureStockFundFlow 为详情页保留 stale
// 库存，但评分、策略因子与 LLM 候选只能使用已通过交易日新鲜度检查的序列。
func fundFlowForScoring(rows []model.FundFlowDaily, fresh bool) []model.FundFlowDaily {
	if !fresh {
		return nil
	}
	return rows
}

// persistFundFlow 资金流序列 upsert。16:00 前丢弃「今天」的行（盘中半截值防残留）。
func persistFundFlow(market, symbol string, bars []datasource.StockFundFlowBar, now time.Time) error {
	return persistFundFlowDB(common.DB, market, symbol, bars, now)
}

func persistFundFlowDB(db *gorm.DB, market, symbol string, bars []datasource.StockFundFlowBar, now time.Time) error {
	if db == nil {
		return errors.New("数据库不可用")
	}
	today := now.Format("2006-01-02")
	allowToday := now.Hour() >= fflowStableHour
	recs := make([]model.FundFlowDaily, 0, len(bars))
	for _, b := range bars {
		if b.TradeDate == "" || b.TradeDate > today || (b.TradeDate == today && !allowToday) {
			continue
		}
		recs = append(recs, model.FundFlowDaily{
			Symbol: symbol, Market: market, TradeDate: b.TradeDate,
			MainNet: b.MainNet, SuperNet: b.SuperNet, LargeNet: b.LargeNet,
			MediumNet: b.MediumNet, SmallNet: b.SmallNet,
			MainPct: round2(b.MainPct), Close: b.Close, ChangePct: round2(b.ChangePct),
		})
	}
	if len(recs) == 0 {
		return nil
	}
	if err := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"main_net", "super_net", "large_net", "medium_net", "small_net",
			"main_pct", "close", "change_pct", "updated_at",
		}),
	}).CreateInBatches(recs, 200).Error; err != nil {
		common.SysWarn("资金流历史落库失败 %s: %v", symbol, err)
		return fmt.Errorf("资金流保存失败: %w", err)
	}
	return nil
}

// ---------- 纯函数（单测锚点） ----------

// mainNetStreakDays 末端连续同号主力净额天数：正=连续净流入 N 天、负=连续净流出 N 天、
// 0=无数据或末日净额恰为 0。零值行终止计数（停牌日 main_net=0 不应跨越延续「连续」语义）。
func mainNetStreakDays(flows []model.FundFlowDaily) int {
	n := len(flows)
	if n == 0 {
		return 0
	}
	last := flows[n-1].MainNet
	if last == 0 {
		return 0
	}
	streak := 0
	for i := n - 1; i >= 0; i-- {
		v := flows[i].MainNet
		if v == 0 || (v > 0) != (last > 0) {
			break
		}
		streak++
	}
	if last < 0 {
		return -streak
	}
	return streak
}

// mainNetSum 末端 n 日主力净额合计（元）。
func mainNetSum(flows []model.FundFlowDaily, n int) float64 {
	if n > len(flows) {
		n = len(flows)
	}
	var sum float64
	for _, f := range flows[len(flows)-n:] {
		sum += f.MainNet
	}
	return sum
}

// flowVolumeScore 主力资金分（0-100）：近 5 日主力净占比均值的线性映射
// （+5% 强吸筹 →90、-5% 强流出 →10），50 为中性。样本不足 3 日不给分（ok=false）。
// 用净占比而非净额：跨市值可比（1 亿净流入对茅台是噪声、对小票是主升）。
func flowVolumeScore(flows []model.FundFlowDaily) (float64, bool) {
	n := len(flows)
	if n < 3 {
		return 0, false
	}
	w := 5
	if w > n {
		w = n
	}
	var sum float64
	for _, f := range flows[n-w:] {
		sum += f.MainPct
	}
	avg := sum / float64(w)
	return clamp0100(50 + avg*8), true
}

// applyFlowScore 把主力资金分融合进五维评分的量能维（0.6 原量能 + 0.4 资金分），
// 按原权重重算综合分。资金流缺失时原样返回——computeScore 本身不动，
// 与 T1「样本不足退回纯旧口径」同一纪律，所有既有对拍/单测不受影响。
func applyFlowScore(r ScoreResult, flows []model.FundFlowDaily) ScoreResult {
	fs, ok := flowVolumeScore(flows)
	if !ok {
		return r
	}
	r.Volume = round2(clamp0100((1-flowVolumeWeight)*r.Volume + flowVolumeWeight*fs))
	total := wTrend*r.Trend + wMomentum*r.Momentum + wPosition*r.Position + wVolume*r.Volume + wRisk*r.Risk
	r.Total = round2(clamp0100(total))
	r.Label = scoreLabel(r.Total)
	return r
}

// StockFundFlowView 详情页资金流响应：逐日序列（近 days 根）+ 汇总。
type StockFundFlowView struct {
	Symbol string             `json:"symbol"`
	Market string             `json:"market"`
	Days   []StockFundFlowDay `json:"days"`
	// 汇总（亿元）：今日/5日/10日/20日主力净额与连续净流入天数。
	MainNet1dYi  float64 `json:"main_net_1d_yi"`
	MainNet5dYi  float64 `json:"main_net_5d_yi"`
	MainNet10dYi float64 `json:"main_net_10d_yi"`
	MainNet20dYi float64 `json:"main_net_20d_yi"`
	StreakDays   int     `json:"streak_days"` // 正=连续净流入天数，负=连续净流出
	Fresh        bool    `json:"fresh"`       // 末行是否 ≥ 上一开市日（false=缓存偏旧）
	LastDate     string  `json:"last_date,omitempty"`
	Note         string  `json:"note,omitempty"`
}

// StockFundFlowDay 单日行（金额单位亿元，前端直接可画）。
type StockFundFlowDay struct {
	Date      string  `json:"date"`
	MainNetYi float64 `json:"main_net_yi"`
	MainPct   float64 `json:"main_pct"`
	Close     float64 `json:"close"`
	ChangePct float64 `json:"change_pct"`
}

// StockFundFlow 详情页个股资金流（按需拉取+缓存；非 A 股/基金无此数据返回空 Days）。
func (s *MoodService) StockFundFlow(ctx context.Context, market, symbol string, days int) (*StockFundFlowView, error) {
	symbol, market, err := normalizeSymbolMarket(symbol, market)
	if err != nil {
		return nil, err
	}
	if days <= 0 || days > fflowBarLimit {
		days = 90
	}
	view := &StockFundFlowView{Symbol: symbol, Market: market, Days: []StockFundFlowDay{}}
	if market != "cn" || isCNFund(symbol) {
		return view, nil // ETF/非 A 股无个股资金流口径，自然为空
	}
	probe := ensureStockFundFlowProbe(ctx, s.em, market, symbol, nil, time.Now())
	flows, fresh := probe.Rows, probe.Fresh
	if len(flows) == 0 {
		if probe.Err != nil {
			return nil, probe.Err
		}
		return view, nil
	}
	if probe.Err != nil {
		view.Note = "刷新失败，以下展示最近已知数据：" + probe.Err.Error()
	} else if probe.RefreshNeeded {
		view.Note = "最新终态资金流尚未补齐，以下展示最近已知数据"
	}
	view.Fresh = fresh
	view.LastDate = flows[len(flows)-1].TradeDate
	view.MainNet1dYi = round2(mainNetSum(flows, 1) / 1e8)
	view.MainNet5dYi = round2(mainNetSum(flows, 5) / 1e8)
	view.MainNet10dYi = round2(mainNetSum(flows, 10) / 1e8)
	view.MainNet20dYi = round2(mainNetSum(flows, 20) / 1e8)
	view.StreakDays = mainNetStreakDays(flows)
	tail := flows
	if len(tail) > days {
		tail = tail[len(tail)-days:]
	}
	for _, f := range tail {
		view.Days = append(view.Days, StockFundFlowDay{
			Date: f.TradeDate, MainNetYi: round2(f.MainNet / 1e8),
			MainPct: f.MainPct, Close: f.Close, ChangePct: f.ChangePct,
		})
	}
	return view, nil
}
