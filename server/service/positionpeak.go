package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// D15 持仓期最高价（移动止盈的地基）。
//
// 用户的原话是「赚过 30% 没走，现在倒亏」——要能提醒这件事，必须知道**我持有期间**
// 这只票到过多高。买入日之前的高点不算，那不是我赚到过的利润。
//
// **口径全文见 model.Position 的 PeakPrice 注释**（加仓重置 / 减仓不重置 / 折算同步 /
// 平仓冻结），此处只放实现与更新时机：
//
//   - **初始化**：建仓时 PeakPrice=买入价、PeakFrom=买入日；存量持仓由列表读取时的
//     惰性初始化补齐（backfillPositionPeaks），并尽量用本地日线回填历史最高。
//   - **盘后更新**：每交易日 16:25（资产快照 16:20 之后）用本地 daily_bars 当日那根的
//     High 抬升峰值——零上游成本，且与 16:10 落库的全市场日线同源。
//   - **盘中判定**：alert 评估时用 max(落库峰值, 当日 fresh 行情的 High) 参与判断但
//     **不落库**——盘中峰值随时在变，落库交给盘后那一次，避免高频写与并发竞争。
//
// 前复权口径的诚实声明：回填走 daily_bars（东财前复权，除权后历史整段重刷），
// 与账面实际成交价可能有出入。偏差方向是**偏低**（除权后前复权历史价整体下移），
// 即偏保守、少触发——但仍必须标 PeakBackfilled 并在 UI 说明，不能假装是精确账面价。

const (
	// peakUpdateHour/Min 盘后峰值更新时点 16:25（错峰：16:10 全市场日线、16:20 资产快照、
	// 16:35 涨停池）。必须晚于日线同步——本任务直接读 daily_bars，日线没落库就无事可做。
	peakUpdateHour = 16
	peakUpdateMin  = 25

	// peakBackfillMaxDays 惰性初始化时回看本地日线的最大自然日数。
	// 超过这个跨度的老仓不再回溯（daily_bars 单只标的常态只有 250 根，
	// 再往前查也查不到；且越久远的前复权价与账面价偏差越大）。
	peakBackfillMaxDays = 400
)

// adjustPriceForCorpAction 价格侧除权除息折算（纯函数）。
//
// 峰值使用市场价格的标准除权公式，保证除权日前后的价格回撤口径连续。账本侧现金
// 分红单独计入已实现收益、不冲减剩余成本，因此这里与持仓成本的处理并不相同：
//
//	新价 = (原价 − 每10股派息/10) / (1 + (送股+转增)/10)
//
// 派息大于股价的极端情形钉在 0。
func adjustPriceForCorpAction(price, bonus, transfer, dividend float64) float64 {
	if price <= 0 {
		return 0
	}
	factor := 1 + (bonus+transfer)/10
	if factor <= 0 {
		return price
	}
	out := round4((price - dividend/10) / factor)
	if out < 0 {
		out = 0
	}
	return out
}

// peakDrawdownPct 自峰值的回撤百分比（纯函数，正数=已回撤）。
// 峰值非正（未初始化）或价格非正时返回 0——**不可判定不等于没回撤**，
// 调用方必须先检查 peak>0 再消费本值（evaluatePositionAlert 已如此）。
func peakDrawdownPct(peak, price float64) float64 {
	if peak <= 0 || price <= 0 {
		return 0
	}
	return round2((peak - price) / peak * 100)
}

// peakInitFor 一笔持仓的峰值初值（纯函数）。买入日缺失时用 today 作为统计起始日——
// 起始日只影响「从哪天起算」的展示与回填窗口，不影响回撤算式。
func peakInitFor(buyPrice float64, buyDate, today string) (float64, string) {
	from := buyDate
	if from == "" {
		from = today
	}
	return round4(buyPrice), from
}

// effectiveQuoteTradeDate 返回已通过 freshness 校验的行情实际观测交易日。
// DataTime 优先：盘前 ExpectedDate 仍是上一交易日，但 09:15 后可能已拿到今天的
// 集合竞价行情，此时提醒和起算日判断必须归到今天。休市日的 fresh 行情 DataTime
// 按 freshness 契约只能落在 ExpectedDate，因此仍会稳定去重到最近交易日。
func effectiveQuoteTradeDate(fq FreshQuoteResult, fallback string) string {
	if fq.Quote != nil && !fq.Quote.DataTime.IsZero() {
		return fq.Quote.DataTime.In(time.Local).Format("2006-01-02")
	}
	if fq.Fresh.ExpectedDate != "" {
		return fq.Fresh.ExpectedDate
	}
	return fallback
}

// peakFromLocalBarsDB 用本地日线回填 (from, before) 区间的最高价与所在交易日。
// 两端都严格排除：起算日的整日 OHLC 含建仓/加仓前波动，不能算作持仓期峰值；
// 评估交易日由 fresh quote 的盘中 High 单独处理，避免日线与盘中口径重复。
// 返回 (最高价, 交易日, 是否查到)。查询失败或无数据一律返回 false ——
// **绝不返回 0 让调用方误以为「历史最高是 0」**。
func peakFromLocalBarsDB(db *gorm.DB, market, symbol, from, before string) (float64, string, bool, error) {
	if db == nil || symbol == "" || from == "" || before == "" || from >= before {
		return 0, "", false, nil
	}
	var row struct {
		High      float64
		TradeDate string
	}
	// 取区间内 high 最大的那一根（并列取最早的一根，结果稳定可复现）。
	err := db.Model(&model.DailyBar{}).
		Select("high, trade_date").
		Where("symbol = ? AND market = ? AND trade_date > ? AND trade_date < ? AND high > 0",
			symbol, market, from, before).
		Order("high DESC, trade_date ASC").Limit(1).Scan(&row).Error
	if err != nil {
		return 0, "", false, err
	}
	if row.High <= 0 {
		return 0, "", false, nil
	}
	return round4(row.High), row.TradeDate, true, nil
}

func peakHistoryLower(from, before string) string {
	lower := from
	if upper, err := time.ParseInLocation("2006-01-02", before, time.Local); err == nil {
		if limit := upper.AddDate(0, 0, -peakBackfillMaxDays).Format("2006-01-02"); limit > lower {
			lower = limit
		}
	}
	return lower
}

// fillPositionPeakFromLocalBars 用起算日之后、before 之前的本地日线抬升内存中的峰值。
// 调用方负责把 p 保存到同一事务；返回是否发生变化。
func fillPositionPeakFromLocalBars(db *gorm.DB, p *model.Position, before string) (bool, error) {
	if p == nil || p.PeakFrom == "" || p.PeakPrice <= 0 {
		return false, nil
	}
	lower := peakHistoryLower(p.PeakFrom, before)
	hi, date, ok, err := peakFromLocalBarsDB(db, p.Market, p.Symbol, lower, before)
	if err != nil || !ok || hi <= p.PeakPrice {
		return false, err
	}
	p.PeakPrice, p.PeakDate, p.PeakBackfilled = hi, date, true
	return true, nil
}

// ensurePositionPeakTx 事务内补齐单笔持仓的峰值（调用方须已持有行锁）。
// 幂等：PeakFrom 非空即认为已初始化，直接返回。返回是否确有写入。
//
// 只对 holding 仓初始化——已平仓的持仓没有「还能回撤多少」可言。
func ensurePositionPeakTx(tx *gorm.DB, p *model.Position, today string) (bool, error) {
	if p.Status != model.PositionStatusHolding || p.PeakFrom != "" {
		return false, nil
	}
	price, from := peakInitFor(p.BuyPrice, p.BuyDate, today)
	if price <= 0 {
		return false, nil // 无买入价（异常数据）：不猜，留空等下次
	}
	p.PeakPrice, p.PeakDate, p.PeakFrom, p.PeakBackfilled = price, from, from, false
	if _, err := fillPositionPeakFromLocalBars(tx, p, today); err != nil {
		return false, err
	}
	return true, tx.Model(&model.Position{}).Where("id = ? AND user_id = ?", p.ID, p.UserID).
		Updates(map[string]any{
			"peak_price": p.PeakPrice, "peak_date": p.PeakDate,
			"peak_from": p.PeakFrom, "peak_backfilled": p.PeakBackfilled,
		}).Error
}

// syncPositionPeaksBefore 批量补齐每笔 holding 在 (PeakFrom, before) 内遗漏的本地日线峰值。
// 一次读出相关日线，再按 position 的独立起算日归并；条件更新避免覆盖并发加仓/折算。
// 返回实际写入的 position id 集合，并同步更新 positions 切片中的对应值。
func syncPositionPeaksBefore(ctx context.Context, userID int64, positions []model.Position, before string) (map[int64]bool, error) {
	updated := map[int64]bool{}
	if common.DB == nil || len(positions) == 0 || before == "" {
		return updated, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	type target struct {
		index       int
		original    model.Position
		candidate   model.Position
		searchAfter string
	}
	targets := make([]target, 0, len(positions))
	byKey := map[string][]int{}
	seenSymbols := map[string]bool{}
	var symbols []string
	// 市场集合与 symbol 一起收集：daily_bars 的索引都以 (symbol, market) 或 (market, ...)
	// 起头，只给 symbol 的查询拿不到 trade_date 那一段，等于逐股扫全历史再在内存里筛日期。
	seenMarkets := map[string]bool{}
	var markets []string
	globalLower := before
	for i := range positions {
		p := positions[i]
		if p.Status != model.PositionStatusHolding || (userID > 0 && p.UserID != userID) {
			continue
		}
		candidate := p
		if candidate.PeakFrom == "" {
			price, from := peakInitFor(candidate.BuyPrice, candidate.BuyDate, before)
			if price <= 0 {
				continue
			}
			candidate.PeakPrice, candidate.PeakDate = price, from
			candidate.PeakFrom, candidate.PeakBackfilled = from, false
		}
		lower := peakHistoryLower(candidate.PeakFrom, before)
		targets = append(targets, target{index: i, original: p, candidate: candidate, searchAfter: lower})
		ti := len(targets) - 1
		key := QuoteKey(candidate.Market, candidate.Symbol)
		byKey[key] = append(byKey[key], ti)
		if !seenSymbols[candidate.Symbol] {
			seenSymbols[candidate.Symbol] = true
			symbols = append(symbols, candidate.Symbol)
		}
		if !seenMarkets[candidate.Market] {
			seenMarkets[candidate.Market] = true
			markets = append(markets, candidate.Market)
		}
		if lower < globalLower {
			globalLower = lower
		}
	}
	if len(targets) == 0 {
		return updated, nil
	}
	var bars []model.DailyBar
	if len(symbols) > 0 && globalLower < before {
		// market 一并入条件走索引；跨市场同名代码的行仍按 QuoteKey 归并，不会错配。
		if err := common.DB.WithContext(ctx).Where(
			"market IN ? AND symbol IN ? AND trade_date > ? AND trade_date < ? AND high > 0",
			markets, symbols, globalLower, before,
		).Order("trade_date ASC, id ASC").Find(&bars).Error; err != nil {
			return updated, err
		}
	}
	for _, bar := range bars {
		for _, ti := range byKey[QuoteKey(bar.Market, bar.Symbol)] {
			t := &targets[ti]
			if bar.TradeDate <= t.searchAfter || bar.TradeDate <= t.candidate.PeakFrom || bar.TradeDate >= before {
				continue
			}
			if bar.High > t.candidate.PeakPrice {
				t.candidate.PeakPrice = round4(bar.High)
				t.candidate.PeakDate = bar.TradeDate
				t.candidate.PeakBackfilled = true
			}
		}
	}
	for _, t := range targets {
		if t.candidate.PeakPrice == t.original.PeakPrice && t.candidate.PeakDate == t.original.PeakDate &&
			t.candidate.PeakFrom == t.original.PeakFrom && t.candidate.PeakBackfilled == t.original.PeakBackfilled {
			continue
		}
		res := common.DB.WithContext(ctx).Model(&model.Position{}).
			Where("id = ? AND user_id = ? AND status = ? AND peak_from = ? AND peak_price = ?",
				t.original.ID, t.original.UserID, model.PositionStatusHolding,
				t.original.PeakFrom, t.original.PeakPrice).
			Updates(map[string]any{
				"peak_price": t.candidate.PeakPrice, "peak_date": t.candidate.PeakDate,
				"peak_from": t.candidate.PeakFrom, "peak_backfilled": t.candidate.PeakBackfilled,
			})
		if res.Error != nil {
			return updated, res.Error
		}
		if res.RowsAffected > 0 {
			positions[t.index] = t.candidate
			updated[t.original.ID] = true
		}
	}
	return updated, nil
}

// backfillPositionPeaks 列表读取时的批量惰性初始化（同 backfillPositionLedgers 的形态）。
// 逐笔独立事务：单条失败不影响其它，也不阻断列表返回。
// 返回是否确有写入（调用方据此决定要不要重读）。
func backfillPositionPeaks(userID int64, positions []model.Position) bool {
	if common.DB == nil || len(positions) == 0 {
		return false
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	updated, err := syncPositionPeaksBefore(context.Background(), userID, positions, today)
	if err != nil {
		common.SysWarn("用户 %d 补齐持仓期峰值失败: %v", userID, err)
		return false
	}
	return len(updated) > 0
}

// resetPeakOnBuy 加仓后重置峰值（在 AddTrade 的同一事务内调用）。
//
// **这是本批最需要解释的一个决定**：加权成本已因加仓改变，加仓前的高点不再是
// 「按当前这本账赚到过的利润」。不重置的具体后果——10 元建仓、涨到 20（峰值 20）、
// 回落到 12 时加仓一倍（成本变 11），若沿用峰值 20，回撤 40% 会在加仓当场触发
// 「移动止盈」提醒，而用户刚刚做的恰恰是看好后的加仓。这类误报是系统性的
// （回调加仓是最常见的加仓时机），故选择重置：宁可漏报不误报。
//
// 减仓不调用本函数——剩余仓位的持有期是连续的，赚到过的高点依然算数。
func resetPeakOnBuy(p *model.Position, price float64, tradeDate, today string) {
	date := tradeDate
	if date == "" {
		date = today
	}
	p.PeakPrice = round4(price)
	p.PeakDate = date
	p.PeakFrom = date
	p.PeakBackfilled = false
}

// rebuildPositionPeakOnBuyTx 按修正后的首笔买入/历史加仓重建峰值，并补齐起算日之后、
// today 之前的本地日线；起算日自身整日 OHLC 明确排除。
func rebuildPositionPeakOnBuyTx(tx *gorm.DB, p *model.Position, price float64, tradeDate, today string) error {
	resetPeakOnBuy(p, price, tradeDate, today)
	_, err := fillPositionPeakFromLocalBars(tx, p, today)
	return err
}

// ---------- 盘后更新 ----------

// bumpPeakWithBar 用一根日线抬升峰值（纯函数）。返回 (新峰值, 新峰值日, 是否变化)。
// **只抬不降**：峰值是历史最高的定义，回落不改写它。
func bumpPeakWithBar(peak float64, peakDate string, high float64, barDate string) (float64, string, bool) {
	if high <= 0 || barDate == "" {
		return peak, peakDate, false
	}
	if high <= peak {
		return peak, peakDate, false
	}
	return round4(high), barDate, true
}

// RunPositionPeakUpdate 盘后按 tradeDate 那根日线抬升全部 holding 持仓的峰值。
// 返回实际更新的持仓数。
//
// 数据来源是本地 daily_bars（16:10 全市场日线同步的产物），**零上游请求**；
// 当日无 bar 的标的（停牌/未同步）本轮跳过——没有数据就是没有数据，不猜、不用昨天的。
func RunPositionPeakUpdate(tradeDate string) int {
	if common.DB == nil || tradeDate == "" {
		return 0
	}
	var positions []model.Position
	if err := common.DB.Where("status = ?", model.PositionStatusHolding).Find(&positions).Error; err != nil {
		common.SysWarn("持仓峰值更新读取持仓失败: %v", err)
		return 0
	}
	if len(positions) == 0 {
		return 0
	}
	// 先补 (PeakFrom, tradeDate) 的历史缺口，再处理 tradeDate 当日；服务停机漏掉的
	// 任意中间交易日因此会在下一次运行时恢复，而不是永久丢失峰值。
	updatedIDs, err := syncPositionPeaksBefore(context.Background(), 0, positions, tradeDate)
	if err != nil {
		common.SysWarn("持仓峰值更新补历史缺口失败: %v", err)
		return 0
	}
	// 一次批量取当日全部相关日线（绝不逐仓查库）。market 同样入条件：只给 symbol 的话
	// idx_bar_market_date 完全用不上，只能按 symbol 前缀逐股取全历史再在内存里筛当天。
	seen := map[string]bool{}
	var syms []string
	seenMkt := map[string]bool{}
	var mkts []string
	for _, p := range positions {
		if !seen[p.Symbol] {
			seen[p.Symbol] = true
			syms = append(syms, p.Symbol)
		}
		if !seenMkt[p.Market] {
			seenMkt[p.Market] = true
			mkts = append(mkts, p.Market)
		}
	}
	var bars []model.DailyBar
	if err := common.DB.Where("market IN ? AND symbol IN ? AND trade_date = ?", mkts, syms, tradeDate).
		Find(&bars).Error; err != nil {
		common.SysWarn("持仓峰值更新读取日线失败: %v", err)
		return 0
	}
	barByKey := map[string]model.DailyBar{}
	for _, b := range bars {
		barByKey[QuoteKey(b.Market, b.Symbol)] = b
	}

	for _, p := range positions {
		bar, ok := barByKey[QuoteKey(p.Market, p.Symbol)]
		if !ok {
			continue
		}
		// 起算日整日 OHLC 含建仓/加仓前波动，无法判断先后；当天峰值只允许
		// 盘中链路使用成交后的当前价，盘后日线必须从下一交易日起才参与。
		if p.PeakFrom != "" && bar.TradeDate <= p.PeakFrom {
			continue
		}
		next, nextDate, changed := bumpPeakWithBar(p.PeakPrice, p.PeakDate, bar.High, bar.TradeDate)
		if !changed {
			continue
		}
		// 条件更新：只在峰值仍是我们读到的那个值时才写，避免与用户加仓（重置峰值）
		// 并发时把刚重置的峰值又抬回旧高点。
		res := common.DB.Model(&model.Position{}).
			Where("id = ? AND status = ? AND peak_from = ? AND peak_price = ?",
				p.ID, model.PositionStatusHolding, p.PeakFrom, p.PeakPrice).
			Updates(map[string]any{"peak_price": next, "peak_date": nextDate})
		if res.Error != nil {
			common.SysWarn("持仓 %d 峰值更新失败: %v", p.ID, res.Error)
			continue
		}
		if res.RowsAffected > 0 {
			updatedIDs[p.ID] = true
		}
	}
	updated := len(updatedIDs)
	if updated > 0 {
		common.SysLog("持仓期最高价更新 %s：%d 笔", tradeDate, updated)
	}
	return updated
}

// StartPositionPeakJob 每交易日 16:25 更新持仓期最高价。
// 非交易日跳过（没有新 bar 可用；周末重跑也只是幂等地读同一根）。
func StartPositionPeakJob() {
	go func() {
		if common.DB == nil {
			return
		}
		time.Sleep(5 * time.Minute) // 启动错峰（在资产快照的 4 分钟之后）
		for {
			now := time.Now()
			if isTradingDayToday(now) && now.Hour()*60+now.Minute() >= peakUpdateHour*60+peakUpdateMin {
				RunPositionPeakUpdate(now.Format("2006-01-02"))
			}
			time.Sleep(time.Until(nextDailyAt(time.Now(), peakUpdateHour, peakUpdateMin)))
		}
	}()
}

// ---------- 展示 ----------

// PeakView 持仓峰值的 API 视图（挂在 PositionView 上）。
type PeakView struct {
	Price       float64 `json:"price"`          // 持仓期最高价（元/股）
	Date        string  `json:"date"`           // 该最高价所在交易日
	From        string  `json:"from"`           // 峰值统计起始日
	DrawdownPct float64 `json:"drawdown_pct"`   // 自峰值回撤 %（仅 fresh 行情时有值）
	Backfilled  bool    `json:"backfilled"`     // 由前复权日线回填（口径提示）
	Note        string  `json:"note,omitempty"` // 口径说明
}

// peakViewFor 组装峰值视图。盘中用 max(落库峰值, fresh High, 现价) 展示，避免
// 16:25 落库前出现负回撤或 AI 漏掉盘中新高；起算交易日整日 OHLC 不可判序，仍只用现价。
// price<=0（无 fresh 行情）时 DrawdownPct 留空，不用旧价算。
func peakViewFor(p model.Position, price, dayHigh float64, tradeDate string) *PeakView {
	if p.PeakFrom == "" || p.PeakPrice <= 0 {
		return nil
	}
	peak, peakDate := p.PeakPrice, p.PeakDate
	if p.PeakFrom > tradeDate && tradeDate != "" {
		price, dayHigh = 0, 0 // 行情代表的交易日早于本次持仓起算日
	} else {
		candidate := price
		if p.PeakFrom != tradeDate && dayHigh > candidate {
			candidate = dayHigh
		}
		if candidate > peak {
			peak = candidate
			peakDate = tradeDate
		}
	}
	v := &PeakView{Price: round2(peak), Date: peakDate, From: p.PeakFrom, Backfilled: p.PeakBackfilled}
	if price > 0 {
		v.DrawdownPct = peakDrawdownPct(peak, price)
	}
	if p.PeakBackfilled {
		v.Note = "最高价含本地日线（前复权口径）回填，与账面实际成交价可能有出入"
	}
	return v
}

// ---------- 供 corpadjust 复用 ----------

// errPeakNotInitialized 峰值尚未初始化（折算时无需处理峰值）。
var errPeakNotInitialized = errors.New("持仓期峰值尚未初始化")

// peakAfterCorpAdjust 除权除息折算后的峰值（含起始日不变的语义说明）。
// 峰值未初始化时返回 errPeakNotInitialized，调用方据此跳过峰值处理（不是错误）。
func peakAfterCorpAdjust(p model.Position, bonus, transfer, dividend float64) (float64, error) {
	if p.PeakFrom == "" || p.PeakPrice <= 0 {
		return 0, errPeakNotInitialized
	}
	return adjustPriceForCorpAction(p.PeakPrice, bonus, transfer, dividend), nil
}

// peakAdjustNote 折算的峰值说明（写进 adjust 流水 note 的补充段，便于事后核对）。
func peakAdjustNote(before, after float64) string {
	if before <= 0 {
		return ""
	}
	return fmt.Sprintf("；持仓期最高价同步折算 %.4g → %.4g", before, after)
}
