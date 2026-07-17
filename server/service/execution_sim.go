package service

import (
	"math"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// S0-2 统一执行模拟器：从 backtest.go 抽出的 A 股执行语义单一权威——
// tracking 标签结算（reclabel.go）、BatchBacktest、条件树回测、未来模型验证共用。
//
// A 股真实约束五件套（一条不能少，全部往「不利于策略」方向保守处理）：
//  1. 信号次日开盘成交（杜绝当日收盘价买入的未来函数）；
//  2. 次日开盘涨幅 ≥ 涨停阈值−0.5 判一字板买不进，跳过（开过板的机会保守放弃）；
//  3. 卖出日一字跌停（high==low 且收盘跌幅达阈值）卖不出，顺延下一交易日重试，
//     顺延到数据末尾仍卖不出按末根收盘强平并标注 forced；
//  4. 整百股取整，拨款不够一手放弃（skip_cash）；
//  5. 费率与模拟盘 tradeFee 同源（佣金万 2.5 最低 5 元 + 卖出印花税万 5）。
//
// 另含固定持有期 + 止盈/止损障碍的标签结算（simulateLabelHold）：障碍按当日
// high/low 判盘中触达、T+1（买入日不判障碍）、同日双触保守取止损、障碍价成交。

// holdOutcome 单标的单持有期的模拟结局。
type holdOutcome struct {
	Status    string // traded / skip_limit_up / skip_cash / skip_suspend / pending
	BuyDate   string
	SellDate  string
	BuyPrice  float64
	SellPrice float64
	ReturnPct float64
	Deferred  int
	Forced    bool
}

const (
	btTraded      = "traded"
	btSkipLimitUp = "skip_limit_up"
	btSkipCash    = "skip_cash"
	btSkipSuspend = "skip_suspend"
	btPending     = "pending"
)

// isOneWordLimitDown 一字跌停（无法卖出）：全天无波动（high==low）且收盘跌幅达到
// 板块跌停阈值−0.5。仅收盘跌停但盘中有波动的日子可以卖出（按收盘成交，偏保守）。
func isOneWordLimitDown(b, prev datasource.Bar, limitPct float64) bool {
	if prev.Close <= 0 || b.High <= 0 {
		return false
	}
	return b.High == b.Low && (b.Close/prev.Close-1)*100 <= -(limitPct - 0.5)
}

// simEntry 统一入场段：信号日下标 i，次日开盘买入。返回买入根下标、股数与总成本；
// 不可成交时 outcome.Status 非空（skip_*）。
// 信号根之后个股暂无数据返回 pending 而非 skip_suspend——纯函数无法区分「停牌中」
// 与「数据未同步到」（当晚结算的新标签必然落在这里），交给调用方等待：复牌/次日
// 数据落库后，bars[i+1] 与 nextDate 的比对自然给出 skip_suspend 或正常成交。
func simEntry(bars []datasource.Bar, i int, symbol, name string, perCap float64, nextDate string) (buyIdx int, qty, cost float64, skip string) {
	if i+1 >= len(bars) {
		return 0, 0, 0, btPending // 信号日后暂无数据（数据未到/停牌中，等下一轮）
	}
	sig, buy := bars[i], bars[i+1]
	if buy.Open <= 0 || sig.Close <= 0 {
		return 0, 0, 0, btSkipSuspend // 坏数据不假装成交
	}
	if nextDate != "" && buy.TradeDate != nextDate {
		return 0, 0, 0, btSkipSuspend // 次日停牌（个股缺市场次日的 bar）
	}
	limitPct := limitUpPctFor(symbol, name)
	// 五件套②：开盘涨幅 ≥ 涨停阈值−0.5 判一字板买不进。
	if (buy.Open/sig.Close-1)*100 >= limitPct-0.5 {
		return 0, 0, 0, btSkipLimitUp
	}
	// 五件套④：整百股取整，钱不够一手放弃。
	qty = math.Floor(perCap/(buy.Open*100)) * 100
	if qty < 100 {
		return 0, 0, 0, btSkipCash
	}
	buyAmount := buy.Open * qty
	buyFee, buyTax := tradeFee("cn", model.PaperSideBuy, symbol, buyAmount)
	return i + 1, qty, buyAmount + buyFee + buyTax, ""
}

// simulateHold 从信号日下标 i 起模拟一笔「次日开盘买入、持有 holdN 交易日收盘卖出」。
// 持有语义（A 股 T+1）：卖出根 = 买入根 + holdN，即买入根之后第 holdN 个交易日收盘
// 卖出（holdN=1 即买入次日卖出，绝不当日买卖）。
// bars 为该股升序日线全序列；nextDate 为市场日轴上的次一交易日（非空时校验个股
// 次日是否停牌——停牌期无法按计划买入，跳过而非用复牌远期价格假装成交）；
// sellDate 为市场日轴上的到期卖出日（信号日后第 holdN+1 个交易日）——非空时按市场
// 日期定位出场根，个股中途停牌不再拉长实际持有跨度（旧的按个股第 N 根 K 线推进会
// 让停牌股顺延到复牌后第 N 根，不同股票的持有天数不可比）；到期日个股停牌视同
// 卖不出，顺延到复牌首根收盘卖出。marketLastDate 为市场日轴末日（非空时区分「真未
// 成熟」与「个股已退市/长停」——个股末根 < sellDate 但市场轴已过 sellDate = 个股停更，
// 按末根收盘强平计成交，不静默判 pending 让坏结局的高分股虚高 Precision）。
// 空 sellDate 回退按个股 K 线数推进（无轴调用方）。
func simulateHold(bars []datasource.Bar, i int, symbol, name string, holdN int, perCap float64, nextDate, sellDate, marketLastDate string) holdOutcome {
	buyIdx, qty, cost, skip := simEntry(bars, i, symbol, name, perCap, nextDate)
	if skip != "" {
		return holdOutcome{Status: skip}
	}
	buy := bars[buyIdx]
	limitPct := limitUpPctFor(symbol, name)
	out := holdOutcome{Status: btTraded, BuyDate: buy.TradeDate, BuyPrice: buy.Open}

	// 卖出目标：卖出根 = 买入根 + holdN（买入根之后第 holdN 个交易日收盘卖出）。
	var j int
	if sellDate != "" {
		if bars[len(bars)-1].TradeDate < sellDate {
			// 个股末根未达市场到期日。区分个股退市/长停与真未成熟。
			if marketLastDate != "" && marketLastDate >= sellDate {
				j = len(bars) - 1 // 市场已过到期日、个股停更 → 退市/长停：末根收盘强平
				out.Forced = true
			} else {
				return holdOutcome{Status: btPending, BuyDate: buy.TradeDate, BuyPrice: buy.Open}
			}
		} else {
			j = buyIdx
			for j < len(bars) && bars[j].TradeDate < sellDate {
				j++
			}
			if j < len(bars) && bars[j].TradeDate != sellDate {
				out.Deferred++ // 到期日个股停牌：顺延到复牌首根（记一次顺延，不精确到天数）
			}
		}
	} else {
		j = i + holdN + 1
		if j >= len(bars) {
			return holdOutcome{Status: btPending, BuyDate: buy.TradeDate, BuyPrice: buy.Open}
		}
	}
	// 五件套③：一字跌停卖不出，顺延重试。
	for j < len(bars) && isOneWordLimitDown(bars[j], bars[j-1], limitPct) {
		out.Deferred++
		j++
	}
	if j >= len(bars) {
		j = len(bars) - 1 // 顺延到末尾仍一字跌停：按末根收盘强平（真实中卖不出，如实标注）
		out.Forced = true
	}
	sell := bars[j]
	if sell.Close <= 0 {
		// 出场根坏数据（停牌/未同步的 0 价）：不伪造成熟收益，返回 pending 等待（与
		// simEntry 买入侧 Open<=0 判 skip_suspend 对称，出场侧保守取 pending）。
		return holdOutcome{Status: btPending, BuyDate: buy.TradeDate, BuyPrice: buy.Open}
	}
	sellAmount := sell.Close * qty
	sellFee, sellTax := tradeFee("cn", model.PaperSideSell, symbol, sellAmount)
	proceeds := sellAmount - sellFee - sellTax
	out.SellDate = sell.TradeDate
	out.SellPrice = sell.Close
	out.ReturnPct = round2((proceeds - cost) / cost * 100)
	return out
}

// labelOutcome 标签结算结果（固定持有期 + 可选止盈/止损障碍 + MFE/MAE + 毛/净收益）。
type labelOutcome struct {
	Status    string // traded / skip_limit_up / skip_cash / skip_suspend / pending
	BuyDate   string
	BuyPrice  float64
	SellDate  string
	SellPrice float64

	GrossPct float64 // 价格收益 %
	NetPct   float64 // 扣佣金印花税后 %
	MfePct   float64 // 持有期内（入场→出场）最大有利波动 %（≥0）
	MaePct   float64 // 最大不利波动 %（≤0）

	HitTakeProfit bool
	HitStopLoss   bool
	Deferred      int
	Forced        bool
}

// simulateLabelHold 标签结算：信号日下标 i 次日开盘买入，卖出根 = 买入根 + horizon
//（买入根之后第 horizon 个交易日收盘卖出，horizon=1 即买入次日卖出，满足 A 股 T+1）；
// takeProfit/stopLoss > 0 时启用三重障碍——自买入次日（T+1）起按当日 high/low 判盘中
// 触达，先触者出场、同日双触保守取止损、成交按障碍价（不取更优的 high/low）。
// 止损出场日若一字跌停按五件套③顺延。horizon 未走完且数据未覆盖返回 pending。
// sellDate 语义同 simulateHold：市场日轴上的到期卖出日（个股停牌不拉长持有跨度，
// 到期日停牌顺延复牌卖出）；marketLastDate 为市场轴末日（非空时个股末根 < sellDate
// 但市场已过 sellDate = 退市/长停，按末根收盘强平）；空 sellDate 回退按个股 K 线数推进。
func simulateLabelHold(bars []datasource.Bar, i int, symbol, name string, horizon int, perCap, takeProfit, stopLoss float64, nextDate, sellDate, marketLastDate string) labelOutcome {
	buyIdx, qty, cost, skip := simEntry(bars, i, symbol, name, perCap, nextDate)
	if skip != "" {
		return labelOutcome{Status: skip}
	}
	buy := bars[buyIdx]
	buyPrice := buy.Open
	limitPct := limitUpPctFor(symbol, name)
	out := labelOutcome{Status: btTraded, BuyDate: buy.TradeDate, BuyPrice: buyPrice}

	// 到期卖出根定位（endOK=false 表示数据未覆盖到期日，障碍扫描到末根后 pending）。
	end, endOK := 0, false
	endDeferred := 0
	forcedEnd := false // 个股退市/长停：到期出场按末根收盘强平
	if sellDate != "" {
		if bars[len(bars)-1].TradeDate >= sellDate {
			end = buyIdx
			for end < len(bars) && bars[end].TradeDate < sellDate {
				end++
			}
			endOK = true
			if bars[end].TradeDate != sellDate {
				endDeferred = 1 // 到期日个股停牌：顺延到复牌首根
			}
		} else if marketLastDate != "" && marketLastDate >= sellDate {
			// 市场已过到期日、个股停更 → 退市/长停：末根收盘强平（无障碍时）。
			end = len(bars) - 1
			endOK = true
			forcedEnd = true
		} else {
			end = len(bars) // 未覆盖：障碍扫描扫完已有窗口
		}
	} else {
		end = i + horizon + 1 // 到期卖出根 = 买入根(i+1) + horizon
		endOK = end < len(bars)
		if !endOK {
			end = len(bars)
		}
	}

	// 障碍扫描：T+1——买入日（buyIdx）当日不可卖出，从 buyIdx+1 起判。
	exitIdx, exitPrice := -1, 0.0
	barrierIdx := -1 // 障碍触发根（未被顺延时出场日=触发日，MFE/MAE 须截断到出场价）
	if takeProfit > 0 || stopLoss > 0 {
		for k := buyIdx + 1; k <= end && k < len(bars); k++ {
			b := bars[k]
			hitSL := stopLoss > 0 && b.Low > 0 && b.Low <= stopLoss
			hitTP := takeProfit > 0 && b.High >= takeProfit
			if !hitSL && !hitTP {
				continue
			}
			if hitSL { // 同日双触保守取止损
				out.HitStopLoss = true
				exitIdx, exitPrice = k, stopLoss
			} else {
				out.HitTakeProfit = true
				exitIdx, exitPrice = k, takeProfit
			}
			barrierIdx = k
			break
		}
	}
	if exitIdx < 0 {
		// 无障碍触发：到期收盘卖出（含一字跌停顺延）。
		if !endOK {
			// 数据未覆盖到期日：MFE/MAE 记录已有窗口后返回 pending。
			out.Status = btPending
			out.MfePct, out.MaePct = excursion(bars, buyIdx, len(bars)-1, buyPrice)
			return out
		}
		out.Deferred += endDeferred
		if forcedEnd {
			out.Forced = true // 个股退市/长停，末根收盘强平
		}
		j := end
		for j < len(bars) && isOneWordLimitDown(bars[j], bars[j-1], limitPct) {
			out.Deferred++
			j++
		}
		if j >= len(bars) {
			j = len(bars) - 1
			out.Forced = true
		}
		exitIdx, exitPrice = j, bars[j].Close
	} else if out.HitStopLoss && isOneWordLimitDown(bars[exitIdx], bars[exitIdx-1], limitPct) {
		// 止损触发日本身一字跌停（卖不出）：顺延到下一可交易日按收盘卖出。
		j := exitIdx
		for j < len(bars) && isOneWordLimitDown(bars[j], bars[j-1], limitPct) {
			out.Deferred++
			j++
		}
		if j >= len(bars) {
			j = len(bars) - 1
			out.Forced = true
		}
		exitIdx, exitPrice = j, bars[j].Close
	}

	if exitPrice <= 0 {
		// 出场根坏数据（停牌/未同步的 0 价）：不伪造成熟收益，返回 pending 等待（与
		// simEntry 买入侧 Open<=0 判 skip_suspend 对称，出场侧保守取 pending）。
		out.Status = btPending
		out.SellDate, out.SellPrice = "", 0
		return out
	}
	sellAmount := exitPrice * qty
	sellFee, sellTax := tradeFee("cn", model.PaperSideSell, symbol, sellAmount)
	proceeds := sellAmount - sellFee - sellTax
	out.SellDate = bars[exitIdx].TradeDate
	out.SellPrice = exitPrice
	out.GrossPct = round2((exitPrice - buyPrice) / buyPrice * 100)
	out.NetPct = round2((proceeds - cost) / cost * 100)
	if barrierIdx >= 0 && exitIdx == barrierIdx {
		// 障碍出场日盘中离场：出场后的同日行情未经历，当日只计入确定经历的价位
		//（开盘价与障碍成交价），不得用整根 High/Low 夸大 MFE/MAE。
		out.MfePct, out.MaePct = excursionToExit(bars, buyIdx, exitIdx, buyPrice, exitPrice)
	} else {
		out.MfePct, out.MaePct = excursion(bars, buyIdx, exitIdx, buyPrice)
	}
	return out
}

// excursion 持有窗口 [from, to] 内相对入场价的最大有利/不利波动（MFE ≥0 / MAE ≤0）。
func excursion(bars []datasource.Bar, from, to int, entry float64) (mfe, mae float64) {
	if entry <= 0 {
		return 0, 0
	}
	for k := from; k <= to && k < len(bars); k++ {
		if bars[k].High > 0 {
			if v := (bars[k].High - entry) / entry * 100; v > mfe {
				mfe = v
			}
		}
		if bars[k].Low > 0 {
			if v := (bars[k].Low - entry) / entry * 100; v < mae {
				mae = v
			}
		}
	}
	return round2(mfe), round2(mae)
}

// excursionToExit 障碍出场（出场日=触发日）的 MFE/MAE：[from, exitIdx-1] 按整根
// 计入；出场日只计入确定经历的价位——开盘价与障碍成交价（盘中离场后的行情不属于
// 本笔持有，日线粒度下无法还原触发前的真实高低点，保守取已知点）。
func excursionToExit(bars []datasource.Bar, from, exitIdx int, entry, exitPrice float64) (mfe, mae float64) {
	if entry <= 0 {
		return 0, 0
	}
	for k := from; k < exitIdx && k < len(bars); k++ {
		if bars[k].High > 0 {
			if v := (bars[k].High - entry) / entry * 100; v > mfe {
				mfe = v
			}
		}
		if bars[k].Low > 0 {
			if v := (bars[k].Low - entry) / entry * 100; v < mae {
				mae = v
			}
		}
	}
	for _, p := range []float64{bars[exitIdx].Open, exitPrice} {
		if p <= 0 {
			continue
		}
		v := (p - entry) / entry * 100
		if v > mfe {
			mfe = v
		}
		if v < mae {
			mae = v
		}
	}
	return round2(mfe), round2(mae)
}

// adjustSuspect 复权自洽校验：相邻收盘涨跌超越板块涨停幅 ×1.5（前复权序列不应出现
// 的断层）判可疑。跳过头部 btAdjustSanityHeadSkip 根（新股上市初期无涨跌幅限制）。
func adjustSuspect(bars []datasource.Bar, symbol, name string) bool {
	tol := limitUpPctFor(symbol, name) * 1.5
	start := btAdjustSanityHeadSkip + 1
	if start < 1 {
		start = 1
	}
	for i := start; i < len(bars); i++ {
		prev := bars[i-1].Close
		if prev <= 0 || bars[i].Close <= 0 {
			continue
		}
		if math.Abs((bars[i].Close/prev-1)*100) > tol {
			return true
		}
	}
	return false
}

// cnDailyBarsAsc 读单只 A 股的 daily_bars 全序列（升序；全市场地基每股约 250 根）。
func cnDailyBarsAsc(symbol string) []datasource.Bar {
	var rows []model.DailyBar
	if err := common.DB.Where("market = ? AND symbol = ?", "cn", symbol).
		Order("trade_date").Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]datasource.Bar, 0, len(rows))
	for _, r := range rows {
		out = append(out, datasource.Bar{
			TradeDate: r.TradeDate, Open: r.Open, High: r.High, Low: r.Low, Close: r.Close,
			Volume: r.Volume, Amount: r.Amount, TurnoverRate: r.TurnoverRate, Source: r.Source,
		})
	}
	return out
}
