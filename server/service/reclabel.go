package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
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
	labelVersion = "l1"
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
}

// gatePriority 同一标的命中多个门控时的主 gate_type 选择优先级（值小者优先）：
// 会改写动作的影子门控（regime/bear）强于只封顶置信度的质量门控，强于名单裁剪。
var gatePriority = map[string]int{
	model.GateRegimeShadow:  0,
	model.GateBearShadow:    1,
	model.GateQualityShadow: 2,
	model.GateCorrelation:   3,
	model.GateIndustryCap:   4,
}

// mergeGateNotes 按 symbol 归并门控记录：事件表每候选恰一行（影子标签唯一键依赖此
// 约束，同一标的多行事件会让 picked 条目生成重复标签撞唯一索引），主 gate_type 按
// 优先级取最强，其余门控说明并入 Reason 透明保留。
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
			out[g.Symbol] = g
		} else {
			cur.Reason = cur.Reason + "；另有[" + g.GateType + "] " + g.Reason
			out[g.Symbol] = cur
		}
	}
	return out
}

// recordBatchFacts 批次落库后记录反事实事件 + 创建 pending 标签（best-effort：
// 失败只记日志，不影响推荐主流程）。picks 为最终入选（items 已带 ID）。
func recordBatchFacts(batch *model.RecommendationBatch, pool []candidate, items []model.Recommendation, picks []recPick, rejected []recReject, gates []gateNote, industryBy map[string]string) {
	if common.DB == nil || len(pool) == 0 {
		return
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
				ev.RawAction = p.Action
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
	if err := common.DB.CreateInBatches(events, 200).Error; err != nil {
		common.SysWarn("反事实事件落库失败 batch=%d: %v", batch.ID, err)
		return
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
	if err := common.DB.CreateInBatches(labels, 200).Error; err != nil {
		common.SysWarn("标签事实落库失败 batch=%d: %v", batch.ID, err)
	}
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

	var pending []model.RecommendationLabel
	if err := common.DB.Where("maturity_status = ?", model.LabelPending).
		Order("symbol, id").Limit(labelAdvanceBatch).Find(&pending).Error; err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	// 基准收盘 map（date → close）：一次拉取供全轮使用；缺失时 alpha 如实缺席。
	benchClose := map[string]float64{}
	if market != nil {
		if _, bars, err := market.GetBenchmarkBars(ctx, "cn", benchBarLimit); err == nil {
			for _, b := range bars {
				if b.Close > 0 {
					benchClose[b.TradeDate] = b.Close
				}
			}
		}
	}

	// 按 symbol 分组：一只股读一次日线。
	bySym := map[string][]*model.RecommendationLabel{}
	var syms []string
	for i := range pending {
		if ctx.Err() != nil {
			break
		}
		l := &pending[i]
		if _, ok := bySym[l.Symbol]; !ok {
			syms = append(syms, l.Symbol)
		}
		bySym[l.Symbol] = append(bySym[l.Symbol], l)
	}
	sort.Strings(syms)

	nameBy := stateNamesFor(syms)
	settled := 0
	today := time.Now().Format("2006-01-02")
	for _, sym := range syms {
		if ctx.Err() != nil {
			break
		}
		bars := cnDailyBarsAsc(sym)
		for _, l := range bySym[sym] {
			if advanceOneLabel(l, bars, nameBy[sym], benchClose, today) {
				if err := common.DB.Save(l).Error; err != nil {
					common.SysWarn("标签结算落库失败 label=%d: %v", l.ID, err)
					continue
				}
				settled++
			}
		}
	}
	if settled > 0 {
		common.SysLog("推荐标签结算完成：本轮成熟/终态 %d 条", settled)
	}
	return settled, nil
}

// advanceOneLabel 结算单条标签；返回是否有状态变化需要落库。
func advanceOneLabel(l *model.RecommendationLabel, bars []datasource.Bar, name string, benchClose map[string]float64, today string) bool {
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

	var out labelOutcome
	if l.EntryMode == model.EntryModeActual {
		out = settleFromActualEntry(bars, l.EntryDate, l.ActualBuyPrice, l.HorizonDays)
	} else {
		out = simulateLabelHold(bars, i, l.Symbol, name, l.HorizonDays, labelPerCap, tp, sl, "")
	}
	switch out.Status {
	case btPending:
		return false // 持有期未走完，下轮再看
	case btTraded:
		l.MaturityStatus = model.LabelMatured
	default:
		l.MaturityStatus = model.LabelSkipped
		l.SkipReason = out.Status
		return true
	}
	l.EntryDate, l.EntryPrice = out.BuyDate, round2(out.BuyPrice)
	l.ExitDate, l.ExitPrice = out.SellDate, round2(out.SellPrice)
	l.GrossReturnPct, l.NetReturnPct = out.GrossPct, out.NetPct
	l.MfePct, l.MaePct = out.MfePct, out.MaePct
	l.HitTakeProfit, l.HitStopLoss = out.HitTakeProfit, out.HitStopLoss
	if b0, ok0 := benchClose[out.BuyDate]; ok0 && b0 > 0 {
		if b1, ok1 := benchClose[out.SellDate]; ok1 {
			l.BenchReturnPct = round2((b1 - b0) / b0 * 100)
			l.AlphaPct = round2(l.NetReturnPct - l.BenchReturnPct) // 扣成本 Alpha
			l.HasBench = true
		}
	}
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

// settleFromActualEntry 用户执行事实结算：从实际建仓日/价起持有 horizon 交易日按收盘
// 卖出（不判障碍——用户实际操作自主，本口径只量化「按固定周期持有」的执行结果，
// 用于与统一模拟并列对比，禁作训练标签）。
func settleFromActualEntry(bars []datasource.Bar, entryDate string, entryPrice float64, horizon int) labelOutcome {
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
	// 建仓日为持有第 1 日 → 出场下标 e+horizon-1（与统一模拟 buyIdx+holdN-1 同口径）。
	j := e + horizon - 1
	if j >= len(bars) {
		return labelOutcome{Status: btPending}
	}
	out := labelOutcome{Status: btTraded, BuyDate: entryDate, BuyPrice: entryPrice}
	sell := bars[j]
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
