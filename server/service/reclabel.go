package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

// S0-1/S0-5 标签事实服务：批次生成后创建 pending 标签（含影子标签），由后台任务
// 按统一执行模拟器（execution_sim.go）结算成熟标签。
//
// 两类事实分表铁律：
//   - entry_mode=next_open：统一模拟成交（模型结果事实，供训练与评估）；
//   - entry_mode=actual_position：用户实际建仓价（执行差异分析用，**禁作训练标签**）。
//
// 影子标签：candidate_events 中被过滤/被门控标记/落选的标的同样建 next_open 标签——
// 评估一切门控/复核/候选来源的地基（错失机会率、gated vs ungated 配对、risk-coverage）。

const (
	// labelVersion 执行结算语义版本。l2（2026-07）：持有期按「卖出根=买入根+horizon」
	// 修正（原 l1 的 buyIdx+horizon-1 在 horizon=1 时同日买卖违反 T+1）+ 退市/长停市场
	// 轴强平 + 退出价守卫。旧 l1 pending 行用新逻辑结算后回写 l2；消费方按版本过滤防混池。
	labelVersion = "l2"
	// labelPerCap 标签结算的每标的拨款（与回测默认一致，元）。
	labelPerCap = float64(btDefaultPerCap)
	// labelNoDataAfterDays 信号日之后超过该自然日仍无任何日线 → no_data（退市/长停）。
	labelNoDataAfterDays = 90
	// labelAdvanceBatch 单轮推进的标签行上限（控制单轮耗时；幂等，下轮继续）。
	labelAdvanceBatch = 4000
)

// gateNote 阶段③/④产生的门控记录（影子或名单裁剪），随事件落库。
type gateNote struct {
	Symbol      string
	GateType    string // model.GateRegimeShadow / GateBearShadow / GateQualityShadow / GateCorrelation / GateIndustryCap
	GateVersion string // 产生该门控的判定版本（空=沿用 regimeVersion，兼容第一批落库口径）
	Reason      string
	WouldBeAction string // 影子门控「若强制会改写成」；名单裁剪/质量封顶类为空
	// AllTypes 合并后携带的全部命中门控（含主门控，按优先级排序）——mergeGateNotes
	// 填充，落库进事件行 GateTypes；单门控时为 nil（落库回退 GateType 单值）。
	AllTypes []string
}

// gatePriority 同一标的命中多个门控时的主 gate_type 选择优先级（值小者优先）：
// 会改写动作的影子门控（regime/bear）强于只封顶置信度的质量门控，强于名单裁剪。
var gatePriority = map[string]int{
	model.GateRegimeShadow:  0,
	model.GateBearShadow:    1,
	model.GateQualityShadow: 2,
	model.GateCorrelation:   3,
	model.GateIndustryCap:   4,
	model.GateQuoteStale:    5, // 硬门排除的候选不会 picked，优先级仅在与名单裁剪同现时兜底
}

// mergeGateNotes 按 symbol 归并门控记录：事件表每候选恰一行（影子标签唯一键依赖此
// 约束，同一标的多行事件会让 picked 条目生成重复标签撞唯一索引），主 gate_type 按
// 优先级取最强，其余门控说明并入 Reason 透明保留、类型全量记入 AllTypes（影子对照
// 报表按 GateTypes 分别归组——只留主类型会让次要门控永久丢失样本）。
func mergeGateNotes(gates []gateNote) map[string]gateNote {
	out := make(map[string]gateNote, len(gates))
	for _, g := range gates {
		cur, ok := out[g.Symbol]
		if !ok {
			out[g.Symbol] = g
			continue
		}
		if gatePriority[g.GateType] < gatePriority[cur.GateType] {
			g.Reason = g.Reason + "；另有[" + cur.GateType + "] " + cur.Reason
			g.AllTypes = mergeGateTypes(g, cur)
			out[g.Symbol] = g
		} else {
			cur.Reason = cur.Reason + "；另有[" + g.GateType + "] " + g.Reason
			cur.AllTypes = mergeGateTypes(cur, g)
			out[g.Symbol] = cur
		}
	}
	return out
}

// mergeGateTypes 主门控 primary 吸收 other 后的全类型清单（去重、按优先级排序）。
func mergeGateTypes(primary, other gateNote) []string {
	seen := map[string]bool{}
	var all []string
	add := func(ts ...string) {
		for _, t := range ts {
			if t != "" && !seen[t] {
				seen[t] = true
				all = append(all, t)
			}
		}
	}
	add(primary.GateType)
	add(primary.AllTypes...)
	add(other.GateType)
	add(other.AllTypes...)
	sort.Slice(all, func(i, j int) bool { return gatePriority[all[i]] < gatePriority[all[j]] })
	return all
}

// recordBatchFacts 批次落库后记录反事实事件 + 创建 pending 标签。返回 error 供调用方
// 判定完整性（成功后置 RecommendationBatch.FactsRecorded=true）：事件/标签 CreateInBatches
// 任一失败上抛，调用方按需重试并如实标记 facts_recorded 供人工排查。
//
// picks 为最终入选（items 已带 ID）；rawActionBySym 为复核前的 LLM 原始动作快照
// （Symbol→Action，#11）：picked 事件 RawAction 记该原始值，PostGateAction 记复核后终值，
// 二者才能构成真正的门控前后对照（verify reject 的 buy→watch 差异得以体现）。
//
// ⚠️ 幂等性：candidate_events 表无唯一索引、影子标签以每次新生成的 candidate_event_id
// 为唯一键——同批次重跑会产生重复事件与重复影子标签（只有 picked 标签受
// idx_rl_key 保护）。故本函数不为跨进程「持久化补建」设计；失败批次靠调用方进程内
// 同步重试恢复，无法恢复的标 facts_recorded=false 人工排查（tracking 扫描只记录不重建）。
func recordBatchFacts(batch *model.RecommendationBatch, pool []candidate, items []model.Recommendation, picks []recPick, rejected []recReject, gates []gateNote, industryBy map[string]string, rawActionBySym map[string]string) error {
	if common.DB == nil || len(pool) == 0 {
		return nil
	}
	pickBySym := make(map[string]recPick, len(picks))
	for _, p := range picks {
		pickBySym[p.Symbol] = p
	}
	itemBySym := make(map[string]model.Recommendation, len(items))
	for _, it := range items {
		itemBySym[it.Symbol] = it
	}
	rejReason := make(map[string]string, len(rejected))
	for _, r := range rejected {
		rejReason[r.Symbol] = r.Reason
	}
	gateBySym := mergeGateNotes(gates)

	events := make([]model.RecommendationCandidateEvent, 0, len(pool))
	for _, c := range pool {
		ev := model.RecommendationCandidateEvent{
			BatchID: batch.ID, UserID: batch.UserID,
			Symbol: c.Symbol, Market: c.Market, Name: c.Name,
			RawScore: c.Score, Source: firstSource(c), SentToLLM: c.SentToLLM,
			RefPrice: c.Price,
		}
		switch {
		case c.SentToLLM:
			if p, ok := pickBySym[c.Symbol]; ok {
				ev.CandidateStage = model.CandStagePicked
				// RawAction 用复核前快照（#11）：p.Action 已被 applyReviews 的 reject 强制
				// 改写为 watch，直接用它会让 RawAction==PostGateAction 恒等、门控前后对照
				// 失效。缺快照（旧调用点/降级路径）回退 p.Action。
				raw := rawActionBySym[c.Symbol]
				if raw == "" {
					raw = p.Action
				}
				ev.RawAction = raw
				ev.PostGateAction = p.Action
			} else {
				ev.CandidateStage = model.CandStageLLMList
				ev.RejectionReason = truncateRunes(rejReason[c.Symbol], 250)
			}
		case c.Excluded != "":
			ev.CandidateStage = model.CandStageFiltered
			if hasPoolFullPrefix(c.Excluded) {
				ev.CandidateStage = model.CandStagePoolFull
			}
			ev.RejectionReason = truncateRunes(c.Excluded, 250)
		default:
			ev.CandidateStage = model.CandStageScored
		}
		if g, ok := gateBySym[c.Symbol]; ok {
			ev.GateType = g.GateType
			ev.GateTypes = strings.Join(g.AllTypes, ",") // 多门控全量（单门控为空，回退 GateType）
			ev.GateVersion = g.GateVersion
			if ev.GateVersion == "" {
				ev.GateVersion = regimeVersion // 第一批门控（regime/correlation/industry_cap）落库口径
			}
			ev.WouldBeAction = g.WouldBeAction
			if ev.RejectionReason == "" {
				ev.RejectionReason = truncateRunes(g.Reason, 250)
			}
		}
		events = append(events, ev)
	}

	// 事件 + 标签同事务原子落库：任一失败整体回滚，杜绝「事件写入成功但标签失败」的
	// 半成品——调用方进程内同步重试重跑本函数时不会因半成品残留而重复插入事件
	// （candidate_events 无唯一索引，重复插入无从去重）。
	return common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.CreateInBatches(events, 200).Error; err != nil {
			return fmt.Errorf("反事实事件落库失败: %w", err)
		}

		// 标签行：picked 条目挂 recommendation_id；其余候选挂事件行（影子标签）。
		signalDate := batch.CreatedAt.In(time.Local).Format("2006-01-02")
		labels := make([]model.RecommendationLabel, 0, len(events)*len(model.LabelHorizons))
		candBySym := make(map[string]candidate, len(pool))
		for _, c := range pool {
			candBySym[c.Symbol] = c
		}
		for _, ev := range events {
			c := candBySym[ev.Symbol]
			recID := int64(0)
			evID := ev.ID
			action := ev.PostGateAction
			if ev.CandidateStage == model.CandStagePicked {
				it, ok := itemBySym[ev.Symbol]
				if !ok {
					continue
				}
				recID, evID = it.ID, 0
			} else if action == "" {
				action = "shadow" // 未入选标的无 LLM 动作，影子口径统一标记
			}
			for _, h := range model.LabelHorizons {
				labels = append(labels, model.RecommendationLabel{
					RecommendationID: recID, CandidateEventID: evID,
					HorizonDays: h, EntryMode: model.EntryModeNextOpen,
					BatchID: batch.ID, UserID: batch.UserID,
					Symbol: ev.Symbol, Market: ev.Market, Type: batch.Type, Action: action,
					SignalDate: signalDate, SignalAsOf: batch.CreatedAt,
					MaturityStatus: model.LabelPending,
					Strategy:       batch.Strategy, Source: ev.Source,
					Industry: industryBy[ev.Symbol], Regime: batch.Regime,
					EntryChg5dPct: candChg5d(c), EntryTurnover: c.TurnoverRate, EntryScore: c.Score,
					RefDate: c.lastBarDate, RefClose: c.lastBarClose,
					LabelVersion: labelVersion,
				})
			}
		}
		if err := tx.CreateInBatches(labels, 200).Error; err != nil {
			return fmt.Errorf("标签事实落库失败: %w", err)
		}
		return nil
	})
}

func firstSource(c candidate) string {
	if len(c.Sources) > 0 {
		return c.Sources[0]
	}
	return c.Source
}

func hasPoolFullPrefix(reason string) bool {
	return len(reason) >= len(poolFullPrefix) && reason[:len(poolFullPrefix)] == poolFullPrefix
}

func candChg5d(c candidate) float64 {
	if c.Factors != nil {
		return c.Factors.Chg5d
	}
	return 0
}

// industriesFor 从宇宙快照（S0-3）查一批标的的最新行业归属；快照未积累时返回空 map。
func industriesFor(symbols []string) map[string]string {
	out := map[string]string{}
	if common.DB == nil || len(symbols) == 0 {
		return out
	}
	var latest string
	common.DB.Model(&model.StockUniverseDaily{}).Select("MAX(trade_date)").Scan(&latest)
	if latest == "" {
		return out
	}
	var rows []model.StockUniverseDaily
	common.DB.Select("symbol", "industry").
		Where("trade_date = ? AND symbol IN ?", latest, symbols).Find(&rows)
	for _, r := range rows {
		if r.Industry != "" {
			out[r.Symbol] = r.Industry
		}
	}
	return out
}

// ---------- 标签推进（后台任务，幂等） ----------

// AdvanceRecommendationLabels 推进 pending 标签：按统一执行模拟器结算已走完持有期
// 的标签；顺带为有持仓血缘的推荐补建 actual_position 行。幂等，可反复调用。
func AdvanceRecommendationLabels(ctx context.Context, market *MarketService) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	backfillActualLabels()

	today := time.Now().Format("2006-01-02")
	// 市场日轴 + 基准收盘（基准优先、回退交易日历，与回测同源）：
	// 轴用于定位「信号日的市场次日」（推荐日次日停牌须判 skip_suspend，不得用复牌
	// 远期价格假装成交）与「到期卖出日」（个股中途停牌不拉长实际持有跨度）。
	// 轴不可得时回退按个股 K 线数推进（历史兼容口径）。
	axis, benchClose, _ := NewBacktestService(market).marketAxis(ctx, today)

	// keyset 分页（按 id 游标）遍历全部 pending：单批上限 labelAdvanceBatch 只控单页
	// 内存/耗时，不再是「pending 总数 >4000 时字母序靠后的成熟标签永远饿死」的天花板
	//（旧的一次性 LIMIT 4000 会漏结算）。幂等：下轮从 id=0 重扫只挑仍 pending 的行。
	settled := 0
	var lastID int64
	for {
		if ctx.Err() != nil {
			break
		}
		var page []model.RecommendationLabel
		if err := common.DB.Where("maturity_status = ? AND id > ?", model.LabelPending, lastID).
			Order("id").Limit(labelAdvanceBatch).Find(&page).Error; err != nil {
			return settled, err
		}
		if len(page) == 0 {
			break
		}
		lastID = page[len(page)-1].ID

		// 页内按 symbol 分组：一只股读一次日线。
		bySym := map[string][]*model.RecommendationLabel{}
		var syms []string
		for i := range page {
			l := &page[i]
			if _, ok := bySym[l.Symbol]; !ok {
				syms = append(syms, l.Symbol)
			}
			bySym[l.Symbol] = append(bySym[l.Symbol], l)
		}
		sort.Strings(syms)
		nameBy := stateNamesFor(syms)
		for _, sym := range syms {
			if ctx.Err() != nil {
				break
			}
			bars := cnDailyBarsAsc(sym)
			for _, l := range bySym[sym] {
				if advanceOneLabel(l, bars, nameBy[sym], axis, benchClose, today) {
					if err := common.DB.Save(l).Error; err != nil {
						common.SysWarn("标签结算落库失败 label=%d: %v", l.ID, err)
						continue
					}
					settled++
				}
			}
		}
	}
	if settled > 0 {
		common.SysLog("推荐标签结算完成：本轮成熟/终态 %d 条", settled)
	}
	return settled, nil
}

// labelFarFuture 到期日尚未到来的哨兵日期（比任何真实交易日都晚，令模拟器返回 pending）。
const labelFarFuture = "9999-12-31"

// labelAxisDates 市场轴定位：signalDate 之后第 1 个交易日（计划买入日）与卖出日
//（卖出根 = 买入根 + horizon，即买入根之后第 horizon 个交易日）。轴为空返回两个空串
//（回退旧口径）；轴覆盖不足时到期日返回哨兵（数据未到，必 pending）。
func labelAxisDates(axis []string, signalDate string, horizon int) (nextDate, sellDate string) {
	if len(axis) == 0 {
		return "", ""
	}
	pos := sort.SearchStrings(axis, signalDate) // 第一个 ≥ signalDate
	nextIdx := pos
	if pos < len(axis) && axis[pos] == signalDate {
		nextIdx = pos + 1
	}
	if nextIdx < len(axis) {
		nextDate = axis[nextIdx]
	}
	if sellIdx := nextIdx + horizon; sellIdx < len(axis) {
		sellDate = axis[sellIdx]
	} else {
		sellDate = labelFarFuture
	}
	return nextDate, sellDate
}

// advanceOneLabel 结算单条标签；返回是否有状态变化需要落库。
func advanceOneLabel(l *model.RecommendationLabel, bars []datasource.Bar, name string, axis []string, benchClose map[string]float64, today string) bool {
	// 定位信号根：<= signal_date 的最后一根（推荐日盘中/盘后生成都算当日信号，
	// 与 BatchBacktest 同口径）。
	i := -1
	for k := len(bars) - 1; k >= 0; k-- {
		if bars[k].TradeDate <= l.SignalDate {
			i = k
			break
		}
	}
	if i < 0 || len(bars) == 0 {
		// 无信号日及之前的日线：超过窗口仍无数据判 no_data，否则继续等。
		if daysBetween(l.SignalDate, today) > labelNoDataAfterDays {
			l.MaturityStatus = model.LabelNoData
			l.LabelVersion = labelVersion
			return true
		}
		return false
	}

	// S0-4 防前复权重锚：生成时点收盘价版本与当前序列比对，偏差超容差说明序列已
	// 重锚——计划价（止盈/止损快照价）按复权因子调整后再结算。
	tp, sl := labelBarriers(l)
	if l.RefDate != "" && l.RefClose > 0 {
		for _, b := range bars {
			if b.TradeDate == l.RefDate {
				if b.Close > 0 && relDiff(b.Close, l.RefClose) > rebaseTolerance {
					factor := b.Close / l.RefClose
					tp, sl = round2(tp*factor), round2(sl*factor)
				}
				break
			}
		}
	}

	// 市场轴末日：供结算区分「个股退市/长停」（末根 < 到期日但市场已过）与「真未成熟」。
	marketLast := ""
	if len(axis) > 0 {
		marketLast = axis[len(axis)-1]
	}
	var out labelOutcome
	if l.EntryMode == model.EntryModeActual {
		out = settleFromActualEntry(bars, l.EntryDate, l.ActualBuyPrice, l.HorizonDays, actualSellDate(axis, l.EntryDate, l.HorizonDays), marketLast)
	} else {
		nextDate, sellDate := labelAxisDates(axis, l.SignalDate, l.HorizonDays)
		out = simulateLabelHold(bars, i, l.Symbol, name, l.HorizonDays, labelPerCap, tp, sl, nextDate, sellDate, marketLast)
	}
	switch out.Status {
	case btPending:
		if daysBetween(l.SignalDate, today) > labelNoDataAfterDays {
			// 超窗仍 pending：已入场（BuyDate 非空）且无市场轴可判到期 → 用个股末根
			// 收盘强平（Forced），落入下方成熟写回；从未入场或补不出 → no_data 终态。
			if out.BuyDate != "" && forceCloseStaleLabel(l, &out, bars) {
				l.MaturityStatus = model.LabelMatured
			} else {
				l.MaturityStatus = model.LabelNoData
				l.LabelVersion = labelVersion
				return true
			}
		} else {
			return false // 持有期未走完，下轮再看
		}
	case btTraded:
		l.MaturityStatus = model.LabelMatured
	default:
		l.MaturityStatus = model.LabelSkipped
		l.SkipReason = out.Status
		l.LabelVersion = labelVersion
		return true
	}
	l.EntryDate, l.EntryPrice = out.BuyDate, round2(out.BuyPrice)
	l.ExitDate, l.ExitPrice = out.SellDate, round2(out.SellPrice)
	l.GrossReturnPct, l.NetReturnPct = out.GrossPct, out.NetPct
	l.MfePct, l.MaePct = out.MfePct, out.MaePct
	l.HitTakeProfit, l.HitStopLoss = out.HitTakeProfit, out.HitStopLoss
	l.Forced = out.Forced
	l.LabelVersion = labelVersion // 旧 l1 pending 行用新逻辑结算后回写当前版本
	if b0, ok0 := benchClose[out.BuyDate]; ok0 && b0 > 0 {
		if b1, ok1 := benchClose[out.SellDate]; ok1 {
			l.BenchReturnPct = round2((b1 - b0) / b0 * 100)
			l.AlphaPct = round2(l.NetReturnPct - l.BenchReturnPct) // 扣成本 Alpha
			l.HasBench = true
		}
	}
	return true
}

// forceCloseStaleLabel 已入场但长期停牌/退市（无市场轴可判到期）超窗：按个股末根收盘
// 强平结算 out（Forced=true）。out 为 simulateLabelHold/settleFromActualEntry 返回的
// pending（已带 BuyDate/BuyPrice 与已有窗口 MFE/MAE）。末根坏数据/买不起一手返回 false
// 交上层 no_data。qty 口径随 EntryMode（next_open 按拨款、actual 名义一手，各自与结算同源）。
func forceCloseStaleLabel(l *model.RecommendationLabel, out *labelOutcome, bars []datasource.Bar) bool {
	if out.BuyPrice <= 0 || len(bars) == 0 {
		return false
	}
	last := bars[len(bars)-1]
	if last.Close <= 0 {
		return false
	}
	qty := float64(int(labelPerCap/(out.BuyPrice*100))) * 100 // 整百股（正数 int 截断=向下取整）
	if l.EntryMode == model.EntryModeActual {
		qty = 100 // 名义一手（与 settleFromActualEntry 同口径）
	}
	if qty < 100 {
		return false
	}
	buyAmount := out.BuyPrice * qty
	buyFee, buyTax := tradeFee("cn", model.PaperSideBuy, l.Symbol, buyAmount)
	cost := buyAmount + buyFee + buyTax
	sellAmount := last.Close * qty
	sellFee, sellTax := tradeFee("cn", model.PaperSideSell, l.Symbol, sellAmount)
	out.Status = btTraded
	out.Forced = true
	out.SellDate, out.SellPrice = last.TradeDate, last.Close
	out.GrossPct = round2((last.Close - out.BuyPrice) / out.BuyPrice * 100)
	out.NetPct = round2((sellAmount - sellFee - sellTax - cost) / cost * 100)
	// MFE/MAE 已在 pending 返回时算到末根（excursion），保留 out.MfePct/MaePct。
	return true
}

// labelBarriers 读取该标签对应推荐条目的止盈/止损计划价（仅 picked 条目有）。
func labelBarriers(l *model.RecommendationLabel) (tp, sl float64) {
	if l.RecommendationID <= 0 {
		return 0, 0
	}
	var rec model.Recommendation
	if err := common.DB.Select("detail_json").First(&rec, l.RecommendationID).Error; err != nil {
		return 0, 0
	}
	var d recPick
	if rec.DetailJSON == "" || json.Unmarshal([]byte(rec.DetailJSON), &d) != nil {
		return 0, 0
	}
	return d.TakeProfit, d.StopLoss
}

// actualSellDate 实际建仓行的市场轴到期日：建仓日（或其后首个交易日）为买入根，
// 卖出根 = 买入根 + horizon（第 horizon 个交易日收盘卖出）。轴空返回空串（回退按个股
// K 线数推进）。
func actualSellDate(axis []string, entryDate string, horizon int) string {
	if len(axis) == 0 || entryDate == "" {
		return ""
	}
	p := sort.SearchStrings(axis, entryDate) // 第一个 ≥ entryDate（建仓日为买入根）
	if sellIdx := p + horizon; sellIdx < len(axis) {
		return axis[sellIdx]
	}
	return labelFarFuture
}

// settleFromActualEntry 用户执行事实结算：从实际建仓日/价起持有 horizon 交易日按收盘
// 卖出（不判障碍——用户实际操作自主，本口径只量化「按固定周期持有」的执行结果，
// 用于与统一模拟并列对比，禁作训练标签）。sellDate 语义同 simulateHold（市场轴到期
// 日，个股停牌不拉长持有跨度；空串回退按个股 K 线数推进）；marketLastDate 为市场轴
// 末日（非空时个股末根 < sellDate 但市场已过 = 退市/长停，末根收盘强平）。
func settleFromActualEntry(bars []datasource.Bar, entryDate string, entryPrice float64, horizon int, sellDate, marketLastDate string) labelOutcome {
	if entryPrice <= 0 || entryDate == "" {
		return labelOutcome{Status: btSkipSuspend}
	}
	e := -1
	for k, b := range bars {
		if b.TradeDate >= entryDate {
			e = k
			break
		}
	}
	if e < 0 {
		return labelOutcome{Status: btPending}
	}
	// 建仓根为买入根 → 卖出根 = e + horizon（建仓根之后第 horizon 个交易日，与统一
	// 模拟 buyIdx+holdN 同口径）。
	var j int
	forced := false
	if sellDate != "" {
		if bars[len(bars)-1].TradeDate < sellDate {
			if marketLastDate != "" && marketLastDate >= sellDate {
				j = len(bars) - 1 // 市场已过到期日、个股停更 → 退市/长停：末根收盘强平
				forced = true
			} else {
				return labelOutcome{Status: btPending}
			}
		} else {
			j = e
			for j < len(bars) && bars[j].TradeDate < sellDate {
				j++
			}
		}
	} else {
		j = e + horizon
		if j >= len(bars) {
			return labelOutcome{Status: btPending}
		}
	}
	sell := bars[j]
	if sell.Close <= 0 {
		return labelOutcome{Status: btPending} // 出场根坏数据：不伪造成熟收益
	}
	out := labelOutcome{Status: btTraded, BuyDate: entryDate, BuyPrice: entryPrice, Forced: forced}
	qty := 100.0 // 名义一手：净收益率与股数无关（费率含最低佣金 5 元，按一手口径计）
	buyAmount := entryPrice * qty
	buyFee, buyTax := tradeFee("cn", model.PaperSideBuy, "", buyAmount)
	cost := buyAmount + buyFee + buyTax
	sellAmount := sell.Close * qty
	sellFee, sellTax := tradeFee("cn", model.PaperSideSell, "", sellAmount)
	out.SellDate, out.SellPrice = sell.TradeDate, sell.Close
	out.GrossPct = round2((sell.Close - entryPrice) / entryPrice * 100)
	out.NetPct = round2((sellAmount - sellFee - sellTax - cost) / cost * 100)
	out.MfePct, out.MaePct = excursion(bars, e, j, entryPrice)
	return out
}

// backfillActualLabels 为已建仓（血缘）的推荐补建 actual_position 标签行（幂等：
// 已存在的 (rec, horizon, actual) 行不重复建）。
func backfillActualLabels() {
	// 有 next_open 标签、且持仓血缘存在、但尚无 actual 行的推荐。
	var recIDs []int64
	common.DB.Model(&model.RecommendationLabel{}).
		Where("recommendation_id > 0 AND entry_mode = ?", model.EntryModeNextOpen).
		Distinct().Pluck("recommendation_id", &recIDs)
	if len(recIDs) == 0 {
		return
	}
	var positions []model.Position
	common.DB.Where("recommendation_id IN ? AND buy_price > 0", recIDs).Order("id").Find(&positions)
	if len(positions) == 0 {
		return
	}
	posByRec := map[int64]model.Position{}
	for _, p := range positions {
		if _, ok := posByRec[p.RecommendationID]; !ok {
			posByRec[p.RecommendationID] = p // 同一推荐多笔建仓取最早一笔（与视图口径一致）
		}
	}
	var existing []int64
	common.DB.Model(&model.RecommendationLabel{}).
		Where("recommendation_id IN ? AND entry_mode = ?", recIDs, model.EntryModeActual).
		Distinct().Pluck("recommendation_id", &existing)
	done := map[int64]bool{}
	for _, id := range existing {
		done[id] = true
	}

	var seeds []model.RecommendationLabel
	common.DB.Where("recommendation_id > 0 AND entry_mode = ? AND horizon_days = ?",
		model.EntryModeNextOpen, model.LabelHorizons[0]).
		Where("recommendation_id IN ?", recIDs).Find(&seeds)
	var rows []model.RecommendationLabel
	for _, seed := range seeds {
		pos, ok := posByRec[seed.RecommendationID]
		if !ok || done[seed.RecommendationID] {
			continue
		}
		for _, h := range model.LabelHorizons {
			l := seed
			l.ID = 0
			l.HorizonDays = h
			l.EntryMode = model.EntryModeActual
			l.MaturityStatus = model.LabelPending
			l.PositionID = pos.ID
			l.ActualBuyPrice = pos.BuyPrice
			l.EntryDate = pos.BuyDate
			l.EntryPrice = 0
			l.CreatedAt, l.UpdatedAt = time.Time{}, time.Time{}
			rows = append(rows, l)
		}
	}
	if len(rows) > 0 {
		if err := common.DB.CreateInBatches(rows, 200).Error; err != nil {
			common.SysWarn("actual_position 标签补建失败: %v", err)
		} else {
			common.SysLog("补建实际建仓标签 %d 条（%d 个推荐）", len(rows), len(rows)/len(model.LabelHorizons))
		}
	}
}

// stateNamesFor 从宇宙字典批量取标的名称（涨停幅度判定需要 ST 名称）。
func stateNamesFor(symbols []string) map[string]string {
	out := map[string]string{}
	if len(symbols) == 0 {
		return out
	}
	var rows []model.MarketSyncState
	common.DB.Select("symbol", "name").Where("market = ? AND symbol IN ?", "cn", symbols).Find(&rows)
	for _, r := range rows {
		out[r.Symbol] = r.Name
	}
	return out
}

// daysBetween 两个 YYYY-MM-DD 间的自然日数（from < to 为正）。解析失败返回 0。
func daysBetween(from, to string) int {
	f, err1 := time.Parse("2006-01-02", from)
	t, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(t.Sub(f).Hours() / 24)
}
