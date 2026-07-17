package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// S3-2 候选池召回评估（RECOMMENDATION_ACCURACY_PLAN §5 S3-2）：排序优化之前先回答
// 「好股票有没有进池」。
//
//   - Recall@K：未来 N 日全市场 Top-K 收益股中，多少进过当日候选池 / LLM 名单 / 最终入选；
//   - 机会集（opportunity_set）限定当日可交易 + 基本流动性门槛——由宇宙快照
//     （!ST、!停牌、成交额 ≥ 门槛）+ 统一执行模拟器 simEntry（一字板买不进/次日停牌
//     剔除）共同定义，连续一字涨停等买不进的标的不污染召回率；
//   - 来源消融：逐路来源去掉后 Recall 掉多少（按候选首来源归属，多源重叠归首来源，
//     诚实声明）；
//   - 错失机会率：Top-K 中进过池但未入选者按 stage 分桶（吃 S0-5 candidate_events），
//     其已成熟影子标签（S0-5 账本）给出错失收益；
//   - 全市场机会集收益分布 vs 候选池收益分布对比。
//
// 口径纪律：收益一律用统一执行模拟器 simulateHold（次日开盘成交、扣费净收益、
// 五件套保守语义），与 recommendation_labels 的 next_open 口径可比；
// 纯程序批次零 LLM 调用；全库流式单遍扫描（数秒级），全局互斥防并发重跑。

const (
	recallMaxBatches = 12   // 最多评估最近 N 个成功批次（每批一次全市场重建，控制耗时）
	recallDefaultK   = 50   // Top-K 默认值
	recallPerCap     = 2e4  // 模拟每标的拨款（与回测默认一致的量级，够一手即可）
	recallMinAmount  = 3e7  // 机会集流动性门槛：成交额 ≥3000 万（对齐 riskgate 警示阈值）
)

// recallHorizons 支持的收益窗口。
var recallHorizons = map[int]bool{5: true, 10: true, 20: true}

// RecallDist 收益分布摘要。
type RecallDist struct {
	N         int     `json:"n"`
	MeanPct   float64 `json:"mean_pct"`
	MedianPct float64 `json:"median_pct"`
	P10Pct    float64 `json:"p10_pct"`
	P90Pct    float64 `json:"p90_pct"`
}

// RecallSourceAblation 单来源消融行。
type RecallSourceAblation struct {
	Source        string  `json:"source"`
	Label         string  `json:"label"`
	PoolCount     int     `json:"pool_count"`      // 该来源贡献的池内候选数（首来源口径，跨批累计）
	RecallPct     float64 `json:"recall_pct"`      // 保留全部来源的池召回（对照基线，各行相同）
	AblatedPct    float64 `json:"ablated_pct"`     // 去掉该来源后的池召回
	DropPct       float64 `json:"drop_pct"`        // 召回损失（RecallPct − AblatedPct）
}

// RecallBatchRow 单批次明细行。
type RecallBatchRow struct {
	BatchID    int64   `json:"batch_id"`
	SignalDate string  `json:"signal_date"` // 落到交易日轴后的信号日
	OppSize    int     `json:"opp_size"`    // 当日机会集规模（可交易+流动性达标）
	PoolSize   int     `json:"pool_size"`
	KEff       int     `json:"k_eff"`
	HitPool    int     `json:"hit_pool"`
	HitLLM     int     `json:"hit_llm"`
	HitPicked  int     `json:"hit_picked"`
}

// RecallReport 召回评估报表。
type RecallReport struct {
	Type        string `json:"type"`
	HorizonDays int    `json:"horizon_days"`
	K           int    `json:"k"`
	Batches     int    `json:"batches"`

	RecallPoolPct   float64 `json:"recall_pool_pct"`   // 批次平均：Top-K 进过候选池比例
	RecallLLMPct    float64 `json:"recall_llm_pct"`    // Top-K 进过 LLM 名单比例
	RecallPickedPct float64 `json:"recall_picked_pct"` // Top-K 被最终入选比例

	// TopKStageCounts Top-K 标的的去向分桶（跨批累计）：picked/llm_list/scored/
	// pool_full/filtered/absent（absent=从未进池）。
	TopKStageCounts map[string]int `json:"topk_stage_counts"`

	// MissedInPool 错失机会（进过池但未入选的 Top-K 标的）已成熟影子标签统计；
	// MissedRatePct = 未入选 Top-K / 全部 Top-K。
	MissedRatePct float64     `json:"missed_rate_pct"`
	MissedLabels  *RecallDist `json:"missed_labels,omitempty"`

	OppDist  RecallDist `json:"opp_dist"`  // 机会集净收益分布（各批日期合并）
	PoolDist RecallDist `json:"pool_dist"` // 池内候选净收益分布

	Forced int `json:"forced"` // 退市/长停末根强平的观测数（收益不可靠，已排除出机会集）

	SourceAblation []RecallSourceAblation `json:"source_ablation"`
	BatchRows      []RecallBatchRow       `json:"batch_rows"`
	Notes          []string               `json:"notes"`
	ElapsedMs      int64                  `json:"elapsed_ms"`
}

var recallInflight atomic.Bool

// recallPoolEntry 池内候选的事件摘要。
type recallPoolEntry struct {
	stage     string
	source    string
	sentToLLM bool
}

// RecRecallReport 生成召回评估报表（当前用户最近成功批次；数秒级重活，全局互斥）。
func (s *RecommendationService) RecRecallReport(ctx context.Context, userID int64, recType string, horizon, k int) (*RecallReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !recallHorizons[horizon] {
		return nil, errors.New("持有期须为 5/10/20 之一")
	}
	if k <= 0 {
		k = recallDefaultK
	}
	if k < 10 {
		k = 10
	}
	if k > 100 {
		k = 100
	}
	if !recallInflight.CompareAndSwap(false, true) {
		return nil, errors.New("召回评估进行中，请稍后再试")
	}
	defer recallInflight.Store(false)
	start := time.Now()

	// 1. 最近成功批次（cn 市场；degraded 无 LLM 名单语义，排除）。
	bq := common.DB.Where("user_id = ? AND market = ? AND status = ?", userID, "cn", model.RecStatusSuccess)
	if recType == model.RecTypeShortTerm || recType == model.RecTypeLongTerm {
		bq = bq.Where("type = ?", recType)
	}
	var batches []model.RecommendationBatch
	if err := bq.Select("id", "created_at", "type").
		Order("created_at DESC").Limit(recallMaxBatches).Find(&batches).Error; err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return nil, errors.New("暂无可评估的成功推荐批次")
	}

	// 2. 交易日轴（基准优先，回退日历）+ 批次信号日落轴。
	freshDate, err := wideFreshDate()
	if err != nil {
		return nil, err
	}
	bt := NewBacktestService(s.market)
	axis, _, _ := bt.marketAxis(ctx, freshDate)
	if len(axis) < horizon+3 {
		return nil, errors.New("交易日轴数据不足，无法评估")
	}
	marketLast := axis[len(axis)-1] // 市场轴末日：区分个股退市/长停与真未成熟
	axisIndex := make(map[string]int, len(axis))
	for i, d := range axis {
		axisIndex[d] = i
	}
	// 信号日=批次日期（或其前最近交易日）；且须留足持有期走完
	//（信号日 i 的卖出根 = 买入根(i+1)+horizon = i+horizon+1，须 ≤ 轴末）。
	lastEligible := len(axis) - (horizon + 2)
	if lastEligible < 0 {
		return nil, errors.New("交易日轴数据不足，无法评估")
	}
	snapDate := func(d string) string {
		i := sort.SearchStrings(axis, d)
		if i < len(axis) && axis[i] == d {
			// 精确命中
		} else {
			i-- // 其前最近交易日
		}
		if i < 0 || i > lastEligible {
			return ""
		}
		return axis[i]
	}
	batchDate := map[int64]string{}
	dateSet := map[string]bool{}
	for _, b := range batches {
		d := snapDate(b.CreatedAt.In(time.Local).Format("2006-01-02"))
		if d == "" {
			continue // 持有期未走完的近期批次：本轮不评（等成熟）
		}
		batchDate[b.ID] = d
		dateSet[d] = true
	}
	if len(batchDate) == 0 {
		return nil, fmt.Errorf("最近批次的 %d 日持有期尚未走完，暂无可评估样本", horizon)
	}
	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	// 3. 事件与影子标签账本。
	batchIDs := make([]int64, 0, len(batchDate))
	for id := range batchDate {
		batchIDs = append(batchIDs, id)
	}
	var events []model.RecommendationCandidateEvent
	if err := common.DB.Where("batch_id IN ?", batchIDs).Find(&events).Error; err != nil {
		return nil, err
	}
	poolBy := map[int64]map[string]recallPoolEntry{} // batchID → symbol → entry
	for _, ev := range events {
		if poolBy[ev.BatchID] == nil {
			poolBy[ev.BatchID] = map[string]recallPoolEntry{}
		}
		poolBy[ev.BatchID][ev.Symbol] = recallPoolEntry{
			stage: ev.CandidateStage, source: ev.Source,
			sentToLLM: ev.SentToLLM || ev.CandidateStage == model.CandStageLLMList || ev.CandidateStage == model.CandStagePicked,
		}
	}
	var labels []model.RecommendationLabel
	if err := common.DB.Where("batch_id IN ? AND horizon_days = ? AND entry_mode = ? AND maturity_status = ?",
		batchIDs, horizon, model.EntryModeNextOpen, model.LabelMatured).Find(&labels).Error; err != nil {
		return nil, err
	}
	labelNet := map[string]float64{} // "batch|symbol" → 成熟净收益
	for _, l := range labels {
		labelNet[fmt.Sprintf("%d|%s", l.BatchID, l.Symbol)] = l.NetReturnPct
	}

	// 4. 机会集资格：宇宙快照优先（!ST/!停牌/成交额门槛），无快照日回退日线口径。
	eligByDate := map[string]map[string]bool{}
	fallbackDates := map[string]bool{}
	for _, d := range dates {
		var rows []model.StockUniverseDaily
		if err := common.DB.Select("symbol", "is_st", "suspended", "amount").
			Where("trade_date = ?", d).Find(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			fallbackDates[d] = true // S0-3 部署前的旧批次：回退到日线成交额+states 名称判 ST
			continue
		}
		m := make(map[string]bool, len(rows))
		for _, r := range rows {
			if !r.IsST && !r.Suspended && r.Amount >= recallMinAmount {
				m[r.Symbol] = true
			}
		}
		eligByDate[d] = m
	}

	// 宇宙元数据（回退日的 ST 判定）。
	var states []model.MarketSyncState
	if err := common.DB.Select("symbol", "name").Where("market = ?", "cn").Find(&states).Error; err != nil {
		return nil, err
	}
	nameBy := make(map[string]string, len(states))
	for _, st := range states {
		nameBy[st.Symbol] = st.Name
	}

	// 5. 全库流式单遍扫描：每 (股, 评估日) 用统一执行模拟器算净收益。
	retByDate := map[string]map[string]float64{}
	for _, d := range dates {
		retByDate[d] = map[string]float64{}
	}
	forcedCount := 0 // 退市/长停末根强平的观测数（不进机会集，单独计数；streamCNDailyBars 顺序执行）
	process := func(symbol string, bars []datasource.Bar) {
		if ctx.Err() != nil {
			return
		}
		name := nameBy[symbol]
		var idxOf map[string]int
		for _, d := range dates {
			// 资格：快照日按快照，回退日按日线口径（!ST + 当日成交额门槛）。
			if elig, ok := eligByDate[d]; ok {
				if !elig[symbol] {
					continue
				}
			} else if isSTName(name) {
				continue
			}
			if idxOf == nil {
				idxOf = make(map[string]int, len(dates))
				for i, b := range bars {
					idxOf[b.TradeDate] = i
				}
			}
			i, ok := idxOf[d]
			if !ok {
				continue // 当日无 bar（停牌）
			}
			if fallbackDates[d] && bars[i].Amount < recallMinAmount {
				continue
			}
			nextDate, sellDate := "", ""
			if ai, ok := axisIndex[d]; ok {
				if ai+1 < len(axis) {
					nextDate = axis[ai+1]
				}
				if ai+horizon+1 < len(axis) {
					sellDate = axis[ai+horizon+1] // 卖出根=买入根(ai+1)+horizon；停牌不拉长持有跨度
				}
			}
			o := simulateHold(bars, i, symbol, name, horizon, recallPerCap, nextDate, sellDate, marketLast)
			if o.Status != btTraded {
				continue // 一字板买不进/停牌/坏数据：不属可交易机会集
			}
			if o.Forced {
				forcedCount++ // 退市/长停末根强平：收益不可靠，不进机会集收益主统计与 Top-K
				continue
			}
			retByDate[d][symbol] = o.ReturnPct
		}
	}
	if err := streamCNDailyBars(ctx, process); err != nil {
		return nil, err
	}

	// 6. 逐批次 Recall@K + 分桶 + 消融。
	rep := &RecallReport{Type: recType, HorizonDays: horizon, K: k, TopKStageCounts: map[string]int{}}
	rep.Forced = forcedCount
	sourceCount := map[string]int{}
	// 消融：去掉来源 s 后的池召回命中 = 总命中 − 首来源为 s 的命中。
	var basePoolHits, baseKTotal float64
	hitsBySource := map[string]float64{}
	var oppRets, poolRets, missedRets []float64
	missedTotal := 0
	skippedNoPool := 0 // 无候选事件的批次（S0-5 部署前旧批次 / 事实写入失败）：非未召回，排除

	for _, b := range batches {
		d, ok := batchDate[b.ID]
		if !ok {
			continue
		}
		rets := retByDate[d]
		if len(rets) == 0 {
			continue
		}
		type sr struct {
			sym string
			ret float64
		}
		all := make([]sr, 0, len(rets))
		for sym, r := range rets {
			all = append(all, sr{sym, r})
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].ret != all[j].ret {
				return all[i].ret > all[j].ret
			}
			return all[i].sym < all[j].sym // 并列按代码稳定排序（结果可复现）
		})
		kEff := k
		if kEff > len(all) {
			kEff = len(all)
		}
		pool := poolBy[b.ID]
		if len(pool) == 0 {
			// 无候选事件（S0-5 部署前旧批次 / 事实写入失败）：poolBy 空时每个 Top-K 都记
			// absent、贡献满额 baseKTotal 但 0 命中，把「数据缺失」当「全量未召回」拉低
			// Recall。整批排除并声明——与 recrecall 诚实缺失原则一致。
			skippedNoPool++
			continue
		}
		row := RecallBatchRow{BatchID: b.ID, SignalDate: d, OppSize: len(all), PoolSize: len(pool), KEff: kEff}
		for sym, e := range pool {
			sourceCount[e.source]++
			if r, ok := rets[sym]; ok {
				poolRets = append(poolRets, r)
			}
		}
		for _, x := range all {
			oppRets = append(oppRets, x.ret)
		}
		for i := 0; i < kEff; i++ {
			sym := all[i].sym
			baseKTotal++
			e, inPool := pool[sym]
			if !inPool {
				rep.TopKStageCounts["absent"]++
				missedTotal++
				// 消融：本就不在池内，去掉任何来源都不变——ablatedHits 不加。
				continue
			}
			row.HitPool++
			basePoolHits++
			hitsBySource[e.source]++
			rep.TopKStageCounts[e.stage]++
			if e.sentToLLM {
				row.HitLLM++
			}
			if e.stage == model.CandStagePicked {
				row.HitPicked++
			} else {
				missedTotal++
				if net, ok := labelNet[fmt.Sprintf("%d|%s", b.ID, sym)]; ok {
					missedRets = append(missedRets, net)
				}
			}
		}
		rep.Batches++
		rep.BatchRows = append(rep.BatchRows, row)
	}
	if rep.Batches == 0 || baseKTotal == 0 {
		return nil, errors.New("评估日均无机会集数据（日线覆盖不足）")
	}

	var sumLLM, sumPicked float64
	for _, r := range rep.BatchRows {
		sumLLM += float64(r.HitLLM)
		sumPicked += float64(r.HitPicked)
	}
	rep.RecallPoolPct = round2(basePoolHits / baseKTotal * 100)
	rep.RecallLLMPct = round2(sumLLM / baseKTotal * 100)
	rep.RecallPickedPct = round2(sumPicked / baseKTotal * 100)
	rep.MissedRatePct = round2(float64(missedTotal) / baseKTotal * 100)

	rep.OppDist = summarizeRecallDist(oppRets)
	rep.PoolDist = summarizeRecallDist(poolRets)
	if len(missedRets) > 0 {
		d := summarizeRecallDist(missedRets)
		rep.MissedLabels = &d
	}

	// 来源消融行（只列池内实际出现过的来源）。
	srcs := make([]string, 0, len(sourceCount))
	for s := range sourceCount {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		ab := round2((basePoolHits - hitsBySource[src]) / baseKTotal * 100)
		rep.SourceAblation = append(rep.SourceAblation, RecallSourceAblation{
			Source: src, Label: sourceLabelOrKey(src), PoolCount: sourceCount[src],
			RecallPct: rep.RecallPoolPct, AblatedPct: ab, DropPct: round2(rep.RecallPoolPct - ab),
		})
	}

	rep.Notes = append(rep.Notes,
		"机会集=当日可交易（非 ST/非停牌/次日非一字板可成交）且成交额 ≥3000 万；收益=统一执行模拟净收益（次日开盘成交、扣费），与标签 next_open 口径一致",
		"来源消融按候选首来源归属——多源重叠标的归首来源，消融值是保守下界",
		"错失收益吃 S0-5 影子标签账本（已成熟 next_open 净收益）；样本随标签积累增长",
		fmt.Sprintf("评估最近 %d 个成功批次（持有期未走完的批次自动跳过）", recallMaxBatches),
	)
	if skippedNoPool > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d 个批次无候选事件（S0-5 部署前旧批次或事实落库失败）已整批排除，非未召回——未计入 Recall 分母", skippedNoPool))
	}
	for d := range fallbackDates {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s 无宇宙快照（S0-3 部署前），机会集回退日线口径（ST 按当前名称判定，存在轻微幸存者偏差）", d))
	}
	if rep.Forced > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d 个观测因退市/长停按末根收盘强平（真实中卖不出，收益不可靠）——已排除出机会集与收益分布，单独计数", rep.Forced))
	}
	rep.ElapsedMs = time.Since(start).Milliseconds()
	common.SysLog("召回评估完成: %d 批次，%d 评估日，Recall@%d 池 %.1f%%/名单 %.1f%%/入选 %.1f%%，耗时 %dms",
		rep.Batches, len(dates), k, rep.RecallPoolPct, rep.RecallLLMPct, rep.RecallPickedPct, rep.ElapsedMs)
	return rep, nil
}

// sourceLabelOrKey 来源中文名（未知 key 原样返回）。
func sourceLabelOrKey(src string) string {
	if l, ok := sourceLabelCN[src]; ok {
		return l
	}
	if src == "" {
		return "（未记录）"
	}
	return src
}

// summarizeRecallDist 收益分布摘要（均值/中位/P10/P90）。
func summarizeRecallDist(rets []float64) RecallDist {
	d := RecallDist{N: len(rets)}
	if d.N == 0 {
		return d
	}
	sorted := append([]float64(nil), rets...)
	sort.Float64s(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	pct := func(p float64) float64 {
		i := int(math.Round(p * float64(len(sorted)-1)))
		return sorted[i]
	}
	d.MeanPct = round2(sum / float64(d.N))
	d.MedianPct = round2(median(sorted))
	d.P10Pct = round2(pct(0.10))
	d.P90Pct = round2(pct(0.90))
	return d
}

// streamCNDailyBars 全市场日线流式单遍扫描（ORDER BY symbol, trade_date 恰合唯一
// 索引序免 filesort），逐股回调 process。召回评估等轻计算消费方共用。
func streamCNDailyBars(ctx context.Context, process func(symbol string, bars []datasource.Bar)) error {
	rows, err := common.DB.Model(&model.DailyBar{}).
		Select("symbol", "trade_date", "open", "high", "low", "close", "volume", "amount", "turnover_rate").
		Where("market = ?", "cn").
		Order("symbol, trade_date").Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	var cur []datasource.Bar
	curSymbol := ""
	flush := func() {
		if curSymbol != "" && len(cur) > 0 {
			process(curSymbol, cur)
		}
		cur = nil
	}
	for rows.Next() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var sym, td string
		var open, high, low, closeP, amount, turnover float64
		var volume int64
		if err := rows.Scan(&sym, &td, &open, &high, &low, &closeP, &volume, &amount, &turnover); err != nil {
			return err
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
	if err := rows.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
