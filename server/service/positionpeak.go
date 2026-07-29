package service

import (
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
// 与 computeCorpAdjust 的成本公式同构——把
//
//	costAfter = (cost×qty − 每10股派息×qty/10) / (qty×factor)
//
// 里的 qty 消掉即得本式，故峰值与成本在除权日按同一比例移动，回撤百分比不会因除权跳变：
//
//	新价 = (原价 − 每10股派息/10) / (1 + (送股+转增)/10)
//
// 派息大于股价的极端情形钉在 0（与 computeCorpAdjust 的成本钉零同款处理）。
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

// peakFromLocalBars 用本地日线回填 [from, today] 区间的最高价与所在交易日。
// 返回 (最高价, 交易日, 是否查到)。查询失败或无数据一律返回 false ——
// **绝不返回 0 让调用方误以为「历史最高是 0」**。
func peakFromLocalBars(market, symbol, from, today string) (float64, string, bool) {
	if common.DB == nil || symbol == "" || from == "" {
		return 0, "", false
	}
	var row struct {
		High      float64
		TradeDate string
	}
	// 取区间内 high 最大的那一根（并列取最早的一根，结果稳定可复现）。
	err := common.DB.Model(&model.DailyBar{}).
		Select("high, trade_date").
		Where("symbol = ? AND market = ? AND trade_date >= ? AND trade_date <= ? AND high > 0",
			symbol, market, from, today).
		Order("high DESC, trade_date ASC").Limit(1).Scan(&row).Error
	if err != nil || row.High <= 0 {
		return 0, "", false
	}
	return round4(row.High), row.TradeDate, true
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
	date := from
	backfilled := false
	// 回填窗口下界：建仓日与 peakBackfillMaxDays 取较晚者。
	lower := from
	if limit := time.Now().AddDate(0, 0, -peakBackfillMaxDays).Format("2006-01-02"); limit > lower {
		lower = limit
	}
	if hi, d, ok := peakFromLocalBars(p.Market, p.Symbol, lower, today); ok && hi > price {
		price, date, backfilled = hi, d, true
	}
	p.PeakPrice, p.PeakDate, p.PeakFrom, p.PeakBackfilled = price, date, from, backfilled
	return true, tx.Model(&model.Position{}).Where("id = ? AND user_id = ?", p.ID, p.UserID).
		Updates(map[string]any{
			"peak_price": p.PeakPrice, "peak_date": p.PeakDate,
			"peak_from": p.PeakFrom, "peak_backfilled": p.PeakBackfilled,
		}).Error
}

// backfillPositionPeaks 列表读取时的批量惰性初始化（同 backfillPositionLedgers 的形态）。
// 逐笔独立事务：单条失败不影响其它，也不阻断列表返回。
// 返回是否确有写入（调用方据此决定要不要重读）。
func backfillPositionPeaks(userID int64, positions []model.Position) bool {
	if common.DB == nil || len(positions) == 0 {
		return false
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	wrote := false
	for _, p := range positions {
		if p.Status != model.PositionStatusHolding || p.PeakFrom != "" {
			continue
		}
		id := p.ID
		err := common.DB.Transaction(func(tx *gorm.DB) error {
			var cur model.Position
			if err := lockedPosition(tx, userID, id, &cur); err != nil {
				return err
			}
			ok, err := ensurePositionPeakTx(tx, &cur, today)
			if err == nil && ok {
				wrote = true
			}
			return err
		})
		if err != nil {
			common.SysWarn("持仓 %d 初始化持仓期峰值失败: %v", id, err)
		}
	}
	return wrote
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
	if err := common.DB.Where("status = ? AND peak_from <> ?",
		model.PositionStatusHolding, "").Find(&positions).Error; err != nil {
		common.SysWarn("持仓峰值更新读取持仓失败: %v", err)
		return 0
	}
	if len(positions) == 0 {
		return 0
	}
	// 一次批量取当日全部相关日线（绝不逐仓查库）。
	seen := map[string]bool{}
	var syms []string
	for _, p := range positions {
		if !seen[p.Symbol] {
			seen[p.Symbol] = true
			syms = append(syms, p.Symbol)
		}
	}
	var bars []model.DailyBar
	if err := common.DB.Where("symbol IN ? AND trade_date = ?", syms, tradeDate).
		Find(&bars).Error; err != nil {
		common.SysWarn("持仓峰值更新读取日线失败: %v", err)
		return 0
	}
	barByKey := map[string]model.DailyBar{}
	for _, b := range bars {
		barByKey[QuoteKey(b.Market, b.Symbol)] = b
	}

	updated := 0
	for _, p := range positions {
		bar, ok := barByKey[QuoteKey(p.Market, p.Symbol)]
		if !ok {
			continue
		}
		// 事件早于峰值起算日的不算（当天建仓、当天盘后跑到更早的 bar 属异常，防御性判断）。
		if p.PeakFrom != "" && bar.TradeDate < p.PeakFrom {
			continue
		}
		next, nextDate, changed := bumpPeakWithBar(p.PeakPrice, p.PeakDate, bar.High, bar.TradeDate)
		if !changed {
			continue
		}
		// 条件更新：只在峰值仍是我们读到的那个值时才写，避免与用户加仓（重置峰值）
		// 并发时把刚重置的峰值又抬回旧高点。
		res := common.DB.Model(&model.Position{}).
			Where("id = ? AND status = ? AND peak_price = ?", p.ID, model.PositionStatusHolding, p.PeakPrice).
			Updates(map[string]any{"peak_price": next, "peak_date": nextDate})
		if res.Error != nil {
			common.SysWarn("持仓 %d 峰值更新失败: %v", p.ID, res.Error)
			continue
		}
		if res.RowsAffected > 0 {
			updated++
		}
	}
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

// peakViewFor 组装峰值视图。price<=0（无 fresh 行情）时 DrawdownPct 留空——
// fail-closed：拿不到当前有效价就不给回撤数字，不用旧价算。
func peakViewFor(p model.Position, price float64) *PeakView {
	if p.PeakFrom == "" || p.PeakPrice <= 0 {
		return nil
	}
	v := &PeakView{Price: round2(p.PeakPrice), Date: p.PeakDate, From: p.PeakFrom, Backfilled: p.PeakBackfilled}
	if price > 0 {
		v.DrawdownPct = peakDrawdownPct(p.PeakPrice, price)
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
