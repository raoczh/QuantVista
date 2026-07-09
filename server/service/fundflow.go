package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

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
	fflowBarLimit     = 250              // 缓存窗口（与日线/因子窗口对齐，不拉全历史占库）
	fflowTryCooldown  = time.Hour        // 同一标的拉取尝试冷却（成功失败都记）
	fflowRecBudget    = 16               // 单次推荐生成允许回上游补拉的标的数
	fflowStableHour   = 16               // 当日数据视为终态的小时（收盘后）
	flowStreakBonus   = 3                // 连续净流入天数加分门槛（≥3 天）
	flowVolumeWeight  = 0.4              // 量能维中主力资金分的权重（0.6 原量能 + 0.4 资金）
)

// fflowSyncTry 包级共享的拉取冷却表（MoodService/ScoreService/推荐域多实例共用，
// 照 F2 finSyncTry 先例——实例字段会让冷却互相看不见）。
var (
	fflowTryMu sync.Mutex
	fflowTry   = map[string]time.Time{}
)

func fflowTryAllowed(key string) bool {
	fflowTryMu.Lock()
	defer fflowTryMu.Unlock()
	if t, ok := fflowTry[key]; ok && time.Since(t) < fflowTryCooldown {
		return false
	}
	fflowTry[key] = time.Now()
	return true
}

// ensureStockFundFlow 读取某股资金流序列（升序，≤fflowBarLimit 根）；库存不新鲜且
// 预算允许时回上游补拉。budget 为 nil 表示不限预算（详情页单股场景）。
// 返回序列与「数据是否新鲜」（末行 ≥ 上一开市日）。失败返回库存（stale 也比没有强）。
func ensureStockFundFlow(ctx context.Context, em *datasource.EastMoneyAdapter, market, symbol string, budget *int) ([]model.FundFlowDaily, bool) {
	if common.DB == nil || market != "cn" {
		return nil, false
	}
	read := func() []model.FundFlowDaily {
		var rows []model.FundFlowDaily
		common.DB.Where("symbol = ? AND market = ?", symbol, market).
			Order("trade_date ASC").Limit(fflowBarLimit).Find(&rows)
		return rows
	}
	now := time.Now()
	freshSince := prevOpenTradeDate(now.Format("2006-01-02"))
	rows := read()
	if len(rows) > 0 && rows[len(rows)-1].TradeDate >= freshSince {
		return rows, true
	}
	if budget != nil {
		if *budget <= 0 {
			return rows, false
		}
		*budget--
	}
	if !fflowTryAllowed(market + ":" + symbol) {
		return rows, false
	}
	fctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	bars, err := em.GetStockFundFlow(fctx, market, symbol, fflowBarLimit)
	if err != nil {
		if !errors.Is(err, datasource.ErrNoData) {
			common.SysDebug("资金流历史拉取失败 %s: %v", symbol, err)
		}
		return rows, false
	}
	persistFundFlow(market, symbol, bars, now)
	rows = read()
	fresh := len(rows) > 0 && rows[len(rows)-1].TradeDate >= freshSince
	return rows, fresh
}

// persistFundFlow 资金流序列 upsert。16:00 前丢弃「今天」的行（盘中半截值防残留）。
func persistFundFlow(market, symbol string, bars []datasource.StockFundFlowBar, now time.Time) {
	today := now.Format("2006-01-02")
	allowToday := now.Hour() >= fflowStableHour
	recs := make([]model.FundFlowDaily, 0, len(bars))
	for _, b := range bars {
		if b.TradeDate == "" || (b.TradeDate == today && !allowToday) {
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
		return
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"main_net", "super_net", "large_net", "medium_net", "small_net",
			"main_pct", "close", "change_pct", "updated_at",
		}),
	}).CreateInBatches(recs, 200).Error; err != nil {
		common.SysWarn("资金流历史落库失败 %s: %v", symbol, err)
	}
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
//（+5% 强吸筹 →90、-5% 强流出 →10），50 为中性。样本不足 3 日不给分（ok=false）。
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
	flows, fresh := ensureStockFundFlow(ctx, s.em, market, symbol, nil)
	if len(flows) == 0 {
		return view, nil
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
