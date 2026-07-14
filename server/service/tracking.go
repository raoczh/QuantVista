package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// TrackingService 推荐追踪：复用 daily_bars 作为价格序列，按当日 high/low 判止盈/止损盘中触达，
// 按 trading_calendar 交易日判有效期，以上证指数为基准算超额收益(alpha)。
// 不做主动推送，仅刷新状态供查询提示（后台定时 + 手动触发）。
type TrackingService struct {
	market *MarketService
}

func NewTrackingService(market *MarketService) *TrackingService {
	return &TrackingService{market: market}
}

const (
	trackWindowDays = 90  // 只追踪近 90 天内生成的推荐，控制数据源调用量
	trackBarLimit   = 250 // 拉取的日线条数（覆盖追踪窗口 + 缓冲）
	benchBarLimit   = 250
	// longRecReviewDays 长线推荐超过该交易日数提示复盘（时间型触发，与持仓
	// longHoldReviewDays 同口径；财报/估值等数据型触发待外部数据源）。
	longRecReviewDays = 60
)

// trackInput 追踪计算输入（纯函数，便于测试；bars 需按日期升序、且均为推荐日之后的日线）。
type trackInput struct {
	RefPrice         float64
	TakeProfit       float64 // 短线止盈价（<=0 视为未设）
	StopLoss         float64 // 短线止损价（<=0 视为未设）
	ValidDays        int     // 短线有效期（交易日；<=0 视为不过期）
	IsShort          bool
	ReviewAfterDays  int              // 长线：超过该交易日数提示复盘（<=0 不提示）
	Bars             []datasource.Bar // 追踪期日线（升序）
	ElapsedTradeDays int              // 从推荐日到今的已过交易日
	BenchStart       float64          // 基准区间起点收盘（推荐日或其后首个交易日）
	BenchEnd         float64          // 基准最新收盘
}

// trackResult 追踪计算结果。
type trackResult struct {
	CurrentPrice   float64
	PeriodHigh     float64
	PeriodLow      float64
	ReturnPct      float64
	MaxGainPct     float64
	MaxDrawdownPct float64 // 正数表示回撤幅度
	BenchReturnPct float64
	AlphaPct       float64
	HitTakeProfit  bool
	HitStopLoss    bool
	Outcome        string
	ReviewNeeded   bool
	BarsCount      int
	HasBench       bool
	LastDate       string

	Return7d  *float64 // 第 7 交易日收盘相对 ref 收益 %（nil=未到节点）
	Return14d *float64
	Return30d *float64
}

// evaluateTracking 纯计算：给定基准价、止盈止损/有效期、升序日线与已过交易日，产出追踪结果。
// 止盈/止损按当日 high/low 判断（避免漏判盘中触达），取最早触发者为主结局；
// 触发判定仅在有效期窗口内进行——过期后再触达价位不算止盈/止损（PRD 3.7 expired 为独立终态）。
func evaluateTracking(in trackInput) trackResult {
	var r trackResult
	r.BarsCount = len(in.Bars)
	if in.RefPrice <= 0 || len(in.Bars) == 0 {
		r.Outcome = model.RecOutcomeNoData
		return r
	}

	// 基本序列统计。
	r.CurrentPrice = in.Bars[len(in.Bars)-1].Close
	r.LastDate = in.Bars[len(in.Bars)-1].TradeDate
	high, low := in.Bars[0].High, 0.0
	peak := in.RefPrice // 回撤相对追踪期内峰值（起点为 ref）
	worstDD := 0.0      // 最负的 (low-peak)/peak
	takeIdx, stopIdx := -1, -1
	for i, b := range in.Bars {
		if b.High > high {
			high = b.High
		}
		// Low<=0 视为坏数据（解析失败产 0），不参与低点/回撤/止损判定。
		if b.Low > 0 && (low <= 0 || b.Low < low) {
			low = b.Low
		}
		// 回撤：先用当日低点相对当前峰值，再用当日高点抬升峰值。
		if peak > 0 && b.Low > 0 {
			if dd := (b.Low - peak) / peak; dd < worstDD {
				worstDD = dd
			}
		}
		if b.High > peak {
			peak = b.High
		}
		// 触发判定（仅短线、设了价位、且在有效期窗口内——bars[i] 为推荐日后第 i+1 个交易日）。
		if in.IsShort && (in.ValidDays <= 0 || i < in.ValidDays) {
			if in.TakeProfit > 0 && b.High >= in.TakeProfit && takeIdx < 0 {
				takeIdx = i
			}
			if in.StopLoss > 0 && b.Low > 0 && b.Low <= in.StopLoss && stopIdx < 0 {
				stopIdx = i
			}
		}
	}
	r.PeriodHigh = round2(high)
	r.PeriodLow = round2(low)
	r.ReturnPct = round2((r.CurrentPrice - in.RefPrice) / in.RefPrice * 100)
	r.MaxGainPct = round2((high - in.RefPrice) / in.RefPrice * 100)
	r.MaxDrawdownPct = round2(-worstDD * 100)
	r.CurrentPrice = round2(r.CurrentPrice)

	// 时间节点收益：第 N 交易日收盘相对 ref（bars[i] 为推荐日后第 i+1 个交易日；
	// 已过节点且日线足够时记录，停牌缺 bar 时顺延到日线补齐后计入）。
	nodeReturn := func(n int) *float64 {
		if in.ElapsedTradeDays < n || len(in.Bars) < n || in.Bars[n-1].Close <= 0 {
			return nil
		}
		v := round2((in.Bars[n-1].Close - in.RefPrice) / in.RefPrice * 100)
		return &v
	}
	r.Return7d = nodeReturn(7)
	r.Return14d = nodeReturn(14)
	r.Return30d = nodeReturn(30)

	// 基准超额收益。
	if in.BenchStart > 0 && in.BenchEnd > 0 {
		r.BenchReturnPct = round2((in.BenchEnd - in.BenchStart) / in.BenchStart * 100)
		r.AlphaPct = round2(r.ReturnPct - r.BenchReturnPct)
		r.HasBench = true
	}

	// 结局。
	if !in.IsShort {
		r.Outcome = model.RecOutcomeTracking
		// 长线时间型复盘触发：超过复盘周期提示复盘（PRD 3.8；财报/估值等数据型触发待外部源）。
		if in.ReviewAfterDays > 0 && in.ElapsedTradeDays >= in.ReviewAfterDays {
			r.ReviewNeeded = true
		}
		return r
	}
	r.HitTakeProfit = takeIdx >= 0
	r.HitStopLoss = stopIdx >= 0
	switch {
	case takeIdx >= 0 && stopIdx >= 0:
		// 同期均触发：以更早者为主结局；同一日则保守取止损。
		if stopIdx <= takeIdx {
			r.Outcome = model.RecOutcomeStopLoss
		} else {
			r.Outcome = model.RecOutcomeTakeProfit
		}
	case takeIdx >= 0:
		r.Outcome = model.RecOutcomeTakeProfit
	case stopIdx >= 0:
		r.Outcome = model.RecOutcomeStopLoss
	case in.ValidDays > 0 && in.ElapsedTradeDays > in.ValidDays:
		r.Outcome = model.RecOutcomeExpired
	default:
		r.Outcome = model.RecOutcomeActive
	}
	r.ReviewNeeded = r.Outcome != model.RecOutcomeActive
	return r
}

// RefreshUser 刷新某用户近 trackWindowDays 天内成功批次的全部推荐追踪状态。返回处理条目数。
func (s *TrackingService) RefreshUser(ctx context.Context, userID int64) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	cutoff := time.Now().AddDate(0, 0, -trackWindowDays)
	var batches []model.RecommendationBatch
	// degraded 一并追踪：AI 超时量化降级的批次同样有条目与规则计划价（老式 degraded
	// 无条目，读到空列表自然跳过）。
	if err := common.DB.Where("user_id = ? AND status IN ? AND created_at >= ?",
		userID, []string{model.RecStatusSuccess, model.RecStatusDegraded}, cutoff).Find(&batches).Error; err != nil {
		return 0, err
	}
	return s.refreshBatches(ctx, batches), nil
}

// RefreshBatch 刷新单个批次（手动触发，校验归属）。
func (s *TrackingService) RefreshBatch(ctx context.Context, userID, batchID int64) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	var batch model.RecommendationBatch
	if err := common.DB.Where("id = ? AND user_id = ?", batchID, userID).First(&batch).Error; err != nil {
		return 0, errors.New("推荐记录不存在")
	}
	if batch.Status != model.RecStatusSuccess && batch.Status != model.RecStatusDegraded {
		return 0, nil // 无条目可追踪（processing/failed；老式 degraded 空条目由下游自然跳过）
	}
	return s.refreshBatches(ctx, []model.RecommendationBatch{batch}), nil
}

// refreshBatches 逐批评估并 upsert 状态；基准日线按市场缓存一次，避免重复请求。
func (s *TrackingService) refreshBatches(ctx context.Context, batches []model.RecommendationBatch) int {
	benchCache := map[string][]datasource.Bar{}
	benchTried := map[string]bool{}
	getBench := func(market string) []datasource.Bar {
		if benchTried[market] {
			return benchCache[market]
		}
		benchTried[market] = true
		if _, bars, err := s.market.GetBenchmarkBars(ctx, market, benchBarLimit); err == nil {
			benchCache[market] = bars
		}
		return benchCache[market]
	}

	count := 0
	for _, b := range batches {
		var recs []model.Recommendation
		if err := common.DB.Where("batch_id = ? AND user_id = ?", b.ID, b.UserID).Find(&recs).Error; err != nil {
			common.SysWarn("追踪读取推荐条目失败 batch=%d: %v", b.ID, err)
			continue
		}
		if len(recs) == 0 {
			continue
		}
		recDate := b.CreatedAt.In(time.Local).Format("2006-01-02")
		bench := getBench(b.Market)
		// 条目受控并发评估（2026-07-14）：手动刷新是前端同步等待的操作，逐条串行时
		// 5 只 × 每只(日线+实时行情)两次上游请求很容易顶到前端超时；并发 4 与数据源
		// 免费额度平衡。upsert 收集后串行落库（SQLite 测试库不吃写并发）。
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		results := make([]*model.RecommendationStatus, len(recs))
		for i, rec := range recs {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, rec model.Recommendation) {
				defer wg.Done()
				defer func() { <-sem }()
				results[i] = s.evaluateOne(ctx, b, rec, recDate, bench)
			}(i, rec)
		}
		wg.Wait()
		for _, st := range results {
			if st == nil {
				continue
			}
			if err := s.upsertStatus(st); err != nil {
				common.SysWarn("追踪落库失败 rec=%d: %v", st.RecommendationID, err)
				continue
			}
			count++
		}
	}
	return count
}

// evaluateOne 评估单条推荐：拉日线（落库）+ 追加当日实时行情 + 交易日历计过期 + 基准 → 状态。
func (s *TrackingService) evaluateOne(ctx context.Context, batch model.RecommendationBatch, rec model.Recommendation, recDate string, bench []datasource.Bar) *model.RecommendationStatus {
	isShort := batch.Type == model.RecTypeShortTerm
	var detail recPick
	if rec.DetailJSON != "" {
		_ = json.Unmarshal([]byte(rec.DetailJSON), &detail)
	}

	st := &model.RecommendationStatus{
		RecommendationID: rec.ID,
		BatchID:          batch.ID,
		UserID:           rec.UserID,
		Symbol:           rec.Symbol,
		Market:           rec.Market,
		Type:             batch.Type,
		Action:           rec.Action,
		RefPrice:         rec.RefPrice,
		ValidDays:        detail.ValidDays,
	}

	// 推荐日之后的日线（升序）。
	bars := s.symbolBarsAfter(ctx, rec.Market, rec.Symbol, recDate)

	// 追加当日实时行情为一根 bar（用于盘中触达判定与最新价刷新）。
	if q, err := s.market.GetQuote(ctx, rec.Market, rec.Symbol); err == nil && q.Price > 0 {
		today := time.Now().In(time.Local).Format("2006-01-02")
		if today > recDate && (len(bars) == 0 || bars[len(bars)-1].TradeDate < today) {
			high, low := q.High, q.Low
			if high <= 0 {
				high = q.Price
			}
			if low <= 0 {
				low = q.Price
			}
			bars = append(bars, datasource.Bar{
				TradeDate: today, Open: q.Open, High: high, Low: low, Close: q.Price, Source: "quote",
			})
		}
	}

	elapsed, hasCal := countOpenTradeDaysAfter(rec.Market, recDate)
	if !hasCal {
		elapsed = len(bars) // 日历不可用时以日线条数近似
	}
	st.ElapsedTradeDays = elapsed

	benchStart, benchEnd := benchRange(bench, recDate)
	res := evaluateTracking(trackInput{
		RefPrice: rec.RefPrice, TakeProfit: detail.TakeProfit, StopLoss: detail.StopLoss,
		ValidDays: detail.ValidDays, IsShort: isShort, ReviewAfterDays: longRecReviewDays,
		Bars:             bars,
		ElapsedTradeDays: elapsed, BenchStart: benchStart, BenchEnd: benchEnd,
	})

	st.CurrentPrice = res.CurrentPrice
	st.PeriodHigh = res.PeriodHigh
	st.PeriodLow = res.PeriodLow
	st.ReturnPct = res.ReturnPct
	st.MaxGainPct = res.MaxGainPct
	st.MaxDrawdownPct = res.MaxDrawdownPct
	st.BenchReturnPct = res.BenchReturnPct
	st.AlphaPct = res.AlphaPct
	st.Outcome = res.Outcome
	st.ReviewNeeded = res.ReviewNeeded
	st.HitTakeProfit = res.HitTakeProfit
	st.HitStopLoss = res.HitStopLoss
	st.BarsCount = res.BarsCount
	st.LastEvalDate = res.LastDate
	st.Return7d = res.Return7d
	st.Return14d = res.Return14d
	st.Return30d = res.Return30d
	if res.Outcome == model.RecOutcomeNoData {
		st.Note = "暂无推荐日之后的日线数据，无法评估表现"
	} else {
		// 无 corporate_actions 复权表，评估基于原始日线（PRD 3.9 要求复权，此为已知边界）。
		notes := []string{nonAdjustedNote}
		if !res.HasBench {
			notes = append(notes, benchUnavailableNote)
		}
		st.Note = strings.Join(notes, "；")
	}
	return st
}

// symbolBarsAfter 拉取标的日线（落库），返回按日期升序、trade_date > afterDate 的部分。
func (s *TrackingService) symbolBarsAfter(ctx context.Context, market, symbol, afterDate string) []datasource.Bar {
	bars, err := s.market.GetDailyBars(ctx, market, symbol, trackBarLimit)
	if err != nil {
		return nil
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].TradeDate < bars[j].TradeDate })
	out := make([]datasource.Bar, 0, len(bars))
	for _, b := range bars {
		if b.TradeDate > afterDate {
			out = append(out, b)
		}
	}
	return out
}

// benchRange 返回基准区间起点（推荐日或其后首个交易日收盘）与最新收盘。
func benchRange(bench []datasource.Bar, recDate string) (start, end float64) {
	if len(bench) == 0 {
		return 0, 0
	}
	sort.Slice(bench, func(i, j int) bool { return bench[i].TradeDate < bench[j].TradeDate })
	for _, b := range bench {
		if b.TradeDate >= recDate {
			start = b.Close
			break
		}
	}
	end = bench[len(bench)-1].Close
	return start, end
}

// countOpenTradeDaysAfter 统计 trading_calendar 中 (recDate, today] 的开市日数。
// 第二返回值表示日历是否可用（无任何该市场日历行时为 false，触发近似回退）。
func countOpenTradeDaysAfter(market, recDate string) (int, bool) {
	if common.DB == nil {
		return 0, false
	}
	var total int64
	common.DB.Model(&model.TradingCalendar{}).Where("market = ?", market).Count(&total)
	if total == 0 {
		return 0, false
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	var n int64
	common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date > ? AND trade_date <= ?", market, true, recDate, today).
		Count(&n)
	return int(n), true
}

// upsertStatus 幂等落库追踪状态（按 recommendation_id 覆盖更新）。
func (s *TrackingService) upsertStatus(st *model.RecommendationStatus) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	st.UpdatedAt = time.Now()
	return common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "recommendation_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"batch_id", "user_id", "symbol", "market", "type", "action",
			"ref_price", "current_price", "period_high", "period_low",
			"return_pct", "max_gain_pct", "max_drawdown_pct", "bench_return_pct", "alpha_pct",
			"outcome", "review_needed", "hit_take_profit", "hit_stop_loss",
			"elapsed_trade_days", "valid_days", "bars_count", "last_eval_date", "note",
			"return_7d", "return_14d", "return_30d", "updated_at",
		}),
	}).Create(st).Error
}

// StatusesForBatch 取某批次全部条目的追踪状态，按 recommendation_id 索引（供视图拼装）。
func (s *TrackingService) StatusesForBatch(userID, batchID int64) map[int64]model.RecommendationStatus {
	out := map[int64]model.RecommendationStatus{}
	if common.DB == nil {
		return out
	}
	var rows []model.RecommendationStatus
	common.DB.Where("batch_id = ? AND user_id = ?", batchID, userID).Find(&rows)
	for _, r := range rows {
		out[r.RecommendationID] = r
	}
	return out
}

// PerformanceStats 推荐历史表现统计（带样本量）。
type PerformanceStats struct {
	Type              string  `json:"type"`   // "" 全部 / short_term / long_term
	Sample            int     `json:"sample"` // 样本量 n（有价格数据的条目）
	WinRate           float64 `json:"win_rate"`
	AvgReturnPct      float64 `json:"avg_return_pct"`
	AvgAlphaPct       float64 `json:"avg_alpha_pct"`
	AvgMaxGainPct     float64 `json:"avg_max_gain_pct"`
	AvgMaxDrawdownPct float64 `json:"avg_max_drawdown_pct"`
	TakeProfit        int     `json:"take_profit"` // 触发止盈数（短线）
	StopLoss          int     `json:"stop_loss"`
	Expired           int     `json:"expired"`
	Active            int     `json:"active"`
	BenchSample       int     `json:"bench_sample"` // 有基准数据、alpha 有效的样本量

	// 时间节点均值：推荐后第 7/14/30 交易日的平均收益（仅统计已到节点的样本）。
	Avg7dPct  float64 `json:"avg_7d_pct"`
	Avg14dPct float64 `json:"avg_14d_pct"`
	Avg30dPct float64 `json:"avg_30d_pct"`
	Sample7d  int     `json:"sample_7d"`
	Sample14d int     `json:"sample_14d"`
	Sample30d int     `json:"sample_30d"`
}

const benchUnavailableNote = "基准指数数据不可得，超额收益(alpha)按 0 计"

// nonAdjustedNote 按 PRD 3.9 价格序列需处理除权除息；当前日线为前复权（东财 fqt=1，
// 以最新价重锚），公司行为发生后历史序列会整体重刷，而 RefPrice/止盈止损是生成时点
// 的快照价——两者可能错位，note 中如实标注。待复权因子表后彻底解决。
const nonAdjustedNote = "基于前复权日线评估，除权除息后历史价格重锚，结果可能与生成时价位错位"

// Performance 聚合某用户的推荐追踪表现（可按类型过滤）。样本仅计有价格数据（outcome != no_data）的条目。
func (s *TrackingService) Performance(userID int64, recType string) (*PerformanceStats, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	q := common.DB.Where("user_id = ?", userID)
	if recType == model.RecTypeShortTerm || recType == model.RecTypeLongTerm {
		q = q.Where("type = ?", recType)
	}
	var rows []model.RecommendationStatus
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}

	stats := &PerformanceStats{Type: recType}
	var sumRet, sumGain, sumDD, sumAlpha float64
	var sum7, sum14, sum30 float64
	var wins int
	for _, r := range rows {
		if r.Outcome == model.RecOutcomeNoData {
			continue
		}
		stats.Sample++
		sumRet += r.ReturnPct
		sumGain += r.MaxGainPct
		sumDD += r.MaxDrawdownPct
		if r.ReturnPct > 0 {
			wins++
		}
		if !strings.Contains(r.Note, benchUnavailableNote) { // 基准有效才计入 alpha 样本
			stats.BenchSample++
			sumAlpha += r.AlphaPct
		}
		if r.Return7d != nil {
			stats.Sample7d++
			sum7 += *r.Return7d
		}
		if r.Return14d != nil {
			stats.Sample14d++
			sum14 += *r.Return14d
		}
		if r.Return30d != nil {
			stats.Sample30d++
			sum30 += *r.Return30d
		}
		switch r.Outcome {
		case model.RecOutcomeTakeProfit:
			stats.TakeProfit++
		case model.RecOutcomeStopLoss:
			stats.StopLoss++
		case model.RecOutcomeExpired:
			stats.Expired++
		case model.RecOutcomeActive:
			stats.Active++
		}
	}
	if stats.Sample > 0 {
		stats.WinRate = round2(float64(wins) / float64(stats.Sample) * 100)
		stats.AvgReturnPct = round2(sumRet / float64(stats.Sample))
		stats.AvgMaxGainPct = round2(sumGain / float64(stats.Sample))
		stats.AvgMaxDrawdownPct = round2(sumDD / float64(stats.Sample))
	}
	if stats.BenchSample > 0 {
		stats.AvgAlphaPct = round2(sumAlpha / float64(stats.BenchSample))
	}
	if stats.Sample7d > 0 {
		stats.Avg7dPct = round2(sum7 / float64(stats.Sample7d))
	}
	if stats.Sample14d > 0 {
		stats.Avg14dPct = round2(sum14 / float64(stats.Sample14d))
	}
	if stats.Sample30d > 0 {
		stats.Avg30dPct = round2(sum30 / float64(stats.Sample30d))
	}
	return stats, nil
}

// StartTrackingJobs 后台推荐追踪：启动后延迟一次全量刷新，之后每 2 小时刷新一次。
// 遍历所有有近 90 天成功批次的用户；失败仅记日志，不影响主流程。
func StartTrackingJobs(mgr *datasource.Manager) {
	svc := NewTrackingService(NewMarketService(mgr))
	refreshAll := func() {
		if common.DB == nil {
			return
		}
		cutoff := time.Now().AddDate(0, 0, -trackWindowDays)
		var userIDs []int64
		if err := common.DB.Model(&model.RecommendationBatch{}).
			Where("status = ? AND created_at >= ?", model.RecStatusSuccess, cutoff).
			Distinct().Pluck("user_id", &userIDs).Error; err != nil {
			common.SysWarn("追踪任务列举用户失败: %v", err)
			return
		}
		for _, uid := range userIDs {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if n, err := svc.RefreshUser(ctx, uid); err != nil {
				common.SysWarn("刷新用户 %d 推荐追踪失败: %v", uid, err)
			} else if n > 0 {
				common.SysLog("刷新用户 %d 推荐追踪完成，共 %d 条", uid, n)
			}
			cancel()
		}
	}

	go func() {
		time.Sleep(90 * time.Second) // 启动后错峰，避开首屏冷启
		refreshAll()
		t := time.NewTicker(2 * time.Hour)
		defer t.Stop()
		for range t.C {
			refreshAll()
		}
	}()
}
