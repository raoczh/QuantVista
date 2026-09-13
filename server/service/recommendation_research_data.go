package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

const rankingResearchVersion = "rr2"

type RankingResearchRequest struct {
	Source     string `json:"source" form:"source"` // recommendations / snapshots
	RecType    string `json:"rec_type" form:"rec_type"`
	Profile    string `json:"profile" form:"profile"`
	Horizon    int    `json:"horizon" form:"horizon"`
	Target     string `json:"target" form:"target"` // net / alpha
	AsOf       string `json:"as_of" form:"as_of"`
	MaxDates   int    `json:"max_dates" form:"max_dates"`
	MaxSymbols int    `json:"max_symbols" form:"max_symbols"`
	TopK       int    `json:"top_k" form:"top_k"`
}

func normalizeRankingResearchRequest(req RankingResearchRequest, now time.Time) (RankingResearchRequest, error) {
	if req.Source == "" {
		req.Source = "recommendations"
	}
	if req.Source != "recommendations" && req.Source != "snapshots" {
		return req, errors.New("评估来源须为 recommendations 或 snapshots")
	}
	if req.RecType == "" {
		req.RecType = model.RecTypeShortTerm
	}
	if req.RecType != model.RecTypeShortTerm && req.RecType != model.RecTypeLongTerm {
		return req, errors.New("推荐周期无效")
	}
	if req.Profile == "" {
		req.Profile = "momentum"
		if req.RecType == model.RecTypeLongTerm {
			req.Profile = "growth"
		}
	}
	if !model.ValidStrategyScoreProfile(req.Profile) {
		return req, errors.New("评分侧重无效")
	}
	if req.Horizon == 0 {
		req.Horizon = 10
		if req.RecType == model.RecTypeLongTerm {
			req.Horizon = 20
		}
	}
	if req.Horizon != 5 && req.Horizon != 10 && req.Horizon != 20 && req.Horizon != 60 {
		return req, errors.New("持有期须为 5、10、20 或 60 个交易日")
	}
	if req.RecType == model.RecTypeShortTerm && req.Horizon > 10 || req.RecType == model.RecTypeLongTerm && req.Horizon < 20 {
		return req, errors.New("短线评估使用 5/10 日，长线评估使用 20/60 日")
	}
	if req.Target == "" {
		req.Target = "alpha"
	}
	if req.Target != "net" && req.Target != "alpha" {
		return req, errors.New("学习目标须为扣费收益或超额收益")
	}
	if req.AsOf == "" {
		req.AsOf = now.In(time.Local).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", req.AsOf); err != nil || req.AsOf > now.In(time.Local).Format("2006-01-02") {
		return req, errors.New("评估截止日无效或位于未来")
	}
	// 当天未收盘时，结果只能计算到前一完整日期；交易日历再处理周末和节假日。
	localNow := now.In(time.Local)
	if req.AsOf == localNow.Format("2006-01-02") && localNow.Hour()*60+localNow.Minute() < 15*60 {
		req.AsOf = localNow.AddDate(0, 0, -1).Format("2006-01-02")
	}
	if req.MaxDates == 0 {
		req.MaxDates = 480
	}
	if req.MaxSymbols == 0 {
		req.MaxSymbols = 200
	}
	if req.TopK == 0 {
		req.TopK = 5
	}
	if req.MaxDates < 120 || req.MaxDates > 1080 || req.MaxSymbols < 10 || req.MaxSymbols > 500 || req.TopK < 1 || req.TopK > 10 {
		return req, errors.New("日期窗口须为 120~1080、标的上限 10~500、TopK 为 1~10")
	}
	if req.Source == "snapshots" && req.MaxDates*req.MaxSymbols > 120000 {
		return req, errors.New("单次历史评估最多 12 万个股日样本，请缩小日期或标的范围")
	}
	return req, nil
}

type RankingResearchCoverage struct {
	InputRows       int            `json:"input_rows"`
	FeatureRows     int            `json:"feature_rows"`
	Matured         int            `json:"matured"`
	Pending         int            `json:"pending"`
	Skipped         int            `json:"skipped"`
	NoData          int            `json:"no_data"`
	Forced          int            `json:"forced"`
	BenchmarkRows   int            `json:"benchmark_rows"`
	FinanceRows     int            `json:"finance_rows"`
	TradeDates      int            `json:"trade_dates"`
	UniverseSymbols int            `json:"universe_symbols"`
	SampledSymbols  int            `json:"sampled_symbols"`
	Reasons         map[string]int `json:"reasons"`
	Sampling        string         `json:"sampling"`
}

var rankingFeatureNames = []string{"trend", "momentum", "position", "volume", "risk", "ma20_distance_atr", "breakout_distance_atr", "compression", "volume_contraction", "close_location", "efficiency_20", "demand_5", "pe_ttm", "annual_roe", "revenue_growth", "profit_growth"}

type rankingResearchSample struct {
	Key, Group, Date, Symbol, Industry string
	AvailableAt                        time.Time
	X                                  []float64
	Legacy, Quality                    float64
	Outcome                            model.RecommendationSelectionOutcome
	batch                              model.RecommendationBatch
	event                              model.RecommendationCandidateEvent
}

type rankingResearchDataset struct {
	Axis     []string
	Samples  []rankingResearchSample
	Coverage RankingResearchCoverage
	Hash     string
}

func rankingCandidateFeatures(c candidate) []float64 {
	x := make([]float64, len(rankingFeatureNames))
	for i := range x {
		x[i] = math.NaN()
	}
	if c.ScoreDims != nil {
		x[0], x[1], x[2], x[3], x[4] = c.ScoreDims.Trend, c.ScoreDims.Momentum, c.ScoreDims.Position, c.ScoreDims.Volume, c.ScoreDims.Risk
	}
	if q := c.SignalQuality; q != nil {
		for i, v := range []*float64{q.MA20DistanceATR, q.BreakoutDistanceATR, q.Compression, q.VolumeContraction, q.CloseLocation, q.Efficiency20, q.DemandBalance5} {
			if v != nil && finiteRecNumber(*v) {
				x[5+i] = *v
			}
		}
	}
	if c.PETTM > 0 && finiteRecNumber(c.PETTM) {
		x[12] = math.Log1p(math.Min(c.PETTM, 500))
	}
	if c.Fin != nil {
		if c.Fin.hasAnnualROE() {
			x[13] = bounded(*c.Fin.AnnualROE, -100, 200)
		}
		for i, v := range []*float64{c.Fin.RevenueYoY, c.Fin.NetProfitYoY} {
			if c.Fin.has(v) {
				x[14+i] = bounded(*v, -100, 200)
			}
		}
	}
	return x
}

func researchDayEnd(date string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	return t.AddDate(0, 0, 1).Add(-time.Nanosecond)
}

func readRankingResearchDataset(ctx context.Context, db *gorm.DB, req RankingResearchRequest) (*rankingResearchDataset, error) {
	if db == nil {
		return nil, errors.New("只读评估数据库不可用")
	}
	d := &rankingResearchDataset{Coverage: RankingResearchCoverage{Reasons: map[string]int{}}}
	opts := &sql.TxOptions{}
	if db.Dialector.Name() == "mysql" {
		opts.Isolation, opts.ReadOnly = sql.LevelRepeatableRead, true
	}
	// 本路径仅 SELECT，不调用行情服务、模型、AutoMigrate、标签推进或缓存回写。
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !tx.Migrator().HasTable(&model.TradingCalendar{}) {
			d.Coverage.Reasons["calendar_missing"]++
			return nil
		}
		if err := tx.Model(&model.TradingCalendar{}).Where("market = ? AND is_open = ? AND trade_date <= ?", "cn", true, req.AsOf).Order("trade_date DESC").Limit(req.MaxDates).Pluck("trade_date", &d.Axis).Error; err != nil {
			return err
		}
		sort.Strings(d.Axis)
		if len(d.Axis) == 0 {
			d.Coverage.Reasons["calendar_missing"]++
			return nil
		}
		var err error
		if req.Source == "recommendations" {
			err = readRecommendationResearchSamples(tx, req, d)
		} else {
			err = readSnapshotResearchSamples(ctx, tx, req, d)
		}
		if err != nil {
			return err
		}
		return fillRankingResearchOutcomes(ctx, tx, req, d)
	}, opts)
	if err != nil {
		return nil, err
	}
	d.Coverage.FeatureRows = len(d.Samples)
	dates := map[string]bool{}
	symbols := map[string]bool{}
	h := sha256.New()
	requestJSON, _ := json.Marshal(req)
	fmt.Fprintf(h, "%s|%s|%s|%v|%v\n", rankingResearchVersion, model.SelectionOutcomeVersion, requestJSON, d.Axis, rankingFeatureNames)
	sort.Slice(d.Samples, func(i, j int) bool { return d.Samples[i].Key < d.Samples[j].Key })
	for _, s := range d.Samples {
		dates[s.Date] = true
		symbols[s.Symbol] = true
		outcomeJSON, _ := json.Marshal(s.Outcome)
		fmt.Fprintf(h, "%s|%s|%s|%s|%.17g|%.17g|%s|%v\n", s.Key, s.Group, s.Date, s.AvailableAt.Format(time.RFC3339Nano), s.Legacy, s.Quality, outcomeJSON, s.X)
		if finiteRecNumber(s.X[13]) && finiteRecNumber(s.X[14]) && finiteRecNumber(s.X[15]) {
			d.Coverage.FinanceRows++
		}
		switch s.Outcome.MaturityStatus {
		case model.LabelMatured:
			if s.Outcome.Forced {
				d.Coverage.Forced++
			} else {
				d.Coverage.Matured++
			}
		case model.LabelSkipped:
			d.Coverage.Skipped++
		case model.LabelNoData:
			d.Coverage.NoData++
		default:
			d.Coverage.Pending++
		}
		if s.Outcome.HasBench {
			d.Coverage.BenchmarkRows++
		}
	}
	d.Coverage.TradeDates = len(dates)
	d.Coverage.SampledSymbols = len(symbols)
	d.Hash = hex.EncodeToString(h.Sum(nil))
	return d, nil
}

func readRecommendationResearchSamples(tx *gorm.DB, req RankingResearchRequest, d *rankingResearchDataset) error {
	if !tx.Migrator().HasTable(&model.RecommendationCandidateEvent{}) || !tx.Migrator().HasTable(&model.RecommendationBatch{}) {
		d.Coverage.Reasons["candidate_events_missing"]++
		return nil
	}
	var batches []model.RecommendationBatch
	if err := tx.Where("type = ? AND market = ? AND facts_recorded = ? AND created_at >= ? AND created_at <= ?", req.RecType, "cn", true, d.Axis[0], researchDayEnd(req.AsOf)).Order("created_at DESC, id DESC").Limit(1001).Find(&batches).Error; err != nil {
		return err
	}
	if len(batches) > 1000 {
		d.Coverage.Reasons["batch_budget_truncated"]++
		batches = batches[:1000]
	}
	byID := map[int64]model.RecommendationBatch{}
	ids := make([]int64, 0, len(batches))
	for _, b := range batches {
		byID[b.ID] = b
		ids = append(ids, b.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	for lo := 0; lo < len(ids); lo += 400 {
		hi := lo + 400
		if hi > len(ids) {
			hi = len(ids)
		}
		rows, err := tx.Model(&model.RecommendationCandidateEvent{}).Where("batch_id IN ? AND score_rank > ?", ids[lo:hi], 0).Order("batch_id, score_rank, symbol").Rows()
		if err != nil {
			return err
		}
		var group []model.RecommendationCandidateEvent
		for rows.Next() {
			var ev model.RecommendationCandidateEvent
			if err := tx.ScanRows(rows, &ev); err != nil {
				rows.Close()
				return err
			}
			if len(group) > 0 && group[0].BatchID != ev.BatchID {
				appendRecommendationResearchGroup(group, byID[group[0].BatchID], req, d)
				group = nil
			}
			group = append(group, ev)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(group) > 0 {
			appendRecommendationResearchGroup(group, byID[group[0].BatchID], req, d)
		}
	}
	d.Coverage.Sampling = "最近 1000 个有完整事实的批次；每批全部合格评分候选，保持原机会集；损坏或重复事实的批次整体排除"
	return nil
}

func appendRecommendationResearchGroup(events []model.RecommendationCandidateEvent, batch model.RecommendationBatch, req RankingResearchRequest, d *rankingResearchDataset) {
	var samples []rankingResearchSample
	invalid := false
	duplicates := map[string]int{}
	for _, ev := range events {
		duplicates[fmt.Sprintf("%d:%s", ev.BatchID, ev.Symbol)]++
	}
	for _, ev := range events {
		d.Coverage.InputRows++
		key := fmt.Sprintf("%d:%s", ev.BatchID, ev.Symbol)
		if duplicates[key] != 1 {
			d.Coverage.Reasons["duplicate_event"]++
			invalid = true
			continue
		}
		if len(ev.FeatureSnapshot) > 128*1024 {
			d.Coverage.Reasons["oversized_fact"]++
			invalid = true
			continue
		}
		facts, err := readOptimizationFacts(ev)
		if err != nil {
			d.Coverage.Reasons["incomplete_or_invalid_fact"]++
			invalid = true
			continue
		}
		c := facts.Candidate
		b := batch
		if c.Market != "cn" || b.Market != c.Market || b.ScoringVersion != ev.ScoringVersion || (ev.ScoringVersion == rankingRidgeVersion && b.ScoringArtifactHash != c.ScoringComparison.ModelHash) {
			d.Coverage.Reasons["batch_version_or_market_mismatch"]++
			invalid = true
			continue
		}
		if c.ScoreBreakdown.Profile != req.Profile {
			d.Coverage.Reasons["other_profile"]++
			continue
		}
		if c.Excluded != "" {
			d.Coverage.Reasons["known_filtered"]++
			continue
		}
		if c.FactAsOf.After(researchDayEnd(req.AsOf)) || c.FactAsOf.Before(b.CreatedAt) || b.UserID != ev.UserID {
			d.Coverage.Reasons["fact_time_or_owner_mismatch"]++
			invalid = true
			continue
		}
		date := c.FactAsOf.In(time.Local).Format("2006-01-02")
		// 固定持有结果以信息全部可知的日期为起点；跨日任务不沿用更早批次创建日。
		b.CreatedAt = *c.FactAsOf
		samples = append(samples, rankingResearchSample{Key: "rec:" + key, Group: fmt.Sprintf("rec:%d", b.ID), Date: date, Symbol: c.Symbol, Industry: c.Industry, AvailableAt: *c.FactAsOf, X: rankingCandidateFeatures(c), Legacy: c.ScoringComparison.LegacyScore, Quality: c.ScoringComparison.QualityScore, batch: b, event: ev})
	}
	if invalid {
		d.Coverage.Reasons["incomplete_opportunity_batch"]++
		return
	}
	d.Samples = append(d.Samples, samples...)
}

func snapshotResearchCandidate(row model.FactorSnapshotDaily, u model.StockUniverseDaily, req RankingResearchRequest) (candidate, bool) {
	if row.FactorVersion != factorSnapshotVersion || row.DataQuality != "verified_adjustment" || row.LastBarDate != row.TradeDate || row.Market != "cn" || u.Symbol != row.Symbol || u.TradeDate != row.TradeDate || u.IsST || u.Suspended || u.Market != "cn" || isCNFund(row.Symbol) || strings.HasPrefix(row.Symbol, "4") || strings.HasPrefix(row.Symbol, "8") || strings.HasPrefix(row.Symbol, "92") {
		return candidate{}, false
	}
	closeTime, err := time.ParseInLocation("2006-01-02 15:04", row.TradeDate+" 15:00", time.Local)
	if err != nil || row.CreatedAt.Before(closeTime) || u.CreatedAt.Before(closeTime) || row.CreatedAt.After(researchDayEnd(row.TradeDate)) || u.CreatedAt.After(researchDayEnd(row.TradeDate)) {
		return candidate{}, false
	}
	var v map[string]float64
	if json.Unmarshal([]byte(row.FactorsJSON), &v) != nil {
		return candidate{}, false
	}
	for _, key := range []string{"close", "bar_count", "vol_boost", "vol_5v20", "chg_5d", "chg_20d", "pos_60", "drawdown_20", "rq_atr", "rq_ma20_dist", "rq_score_trend", "rq_score_momentum", "rq_score_position", "rq_score_volume", "rq_score_risk"} {
		if x, ok := v[key]; !ok || !finiteRecNumber(x) {
			return candidate{}, false
		}
	}
	if v["bar_count"] < 60 || v["close"] <= 0 || v["rq_atr"] <= 0 || !finiteRecNumber(u.Amount) || u.Amount < 3e7 || !finiteRecNumber(u.TurnoverRate) || u.TurnoverRate > deadTurnoverHardPct {
		return candidate{}, false
	}
	ptr := func(key string) *float64 {
		if x, ok := v[key]; ok && finiteRecNumber(x) {
			return recNumber(x)
		}
		return nil
	}
	flag := func(key string) *bool {
		if x, ok := v[key]; ok && (x == 0 || x == 1) {
			return boolPtr(x == 1)
		}
		return nil
	}
	q := &recSignalQuality{Version: recommendationSignalVersion, AsOf: row.TradeDate, Bars: int(v["bar_count"]), ATR: ptr("rq_atr"), BreakoutLevel: ptr("rq_breakout_level"), BreakoutDistanceATR: ptr("rq_breakout_dist"), MA20DistanceATR: ptr("rq_ma20_dist"), Compression: ptr("rq_compression"), VolumeContraction: ptr("rq_volume_contract"), CloseLocation: ptr("rq_close_location"), UpperWick: ptr("rq_upper_wick"), RangeShock: ptr("rq_range_shock"), Efficiency20: ptr("rq_efficiency20"), DemandBalance5: ptr("rq_demand5"), PullbackDepthATR: ptr("rq_pullback_depth"), Stabilized: flag("rq_stabilized"), HigherLow: flag("rq_higher_low"), BreakoutConfirmed: flag("rq_breakout"), BreakoutRun: int(v["rq_breakout_run"]), Support: ptr("rq_support"), SupportDistanceATR: ptr("rq_support_dist"), Resistance: ptr("rq_resistance"), ResistanceDistanceATR: ptr("rq_resistance_dist")}
	q.BreakoutATR = ptr("rq_breakout_atr")
	f := &candFactors{BarCount: int(v["bar_count"]), MA5: v["ma5"], MA10: v["ma10"], MA20: v["ma20"], MA60: v["ma60"], Chg5d: v["chg_5d"], Chg20d: v["chg_20d"], High20d: v["high_20d"] == 1, BullAlign: v["bull_align"] == 1, AboveMA20: v["above_ma20"] == 1, VolBoost: v["vol_boost"], Vol5v20: v["vol_5v20"], Volatility20: v["volatility_20"], Drawdown20: v["drawdown_20"], Bias20: v["bias_20"], Pos60: v["pos_60"], RSI14: v["rsi_14"], MACDDif: v["macd_dif"], MACDGold: v["macd_gold"] == 1, MACDXUp: v["macd_cross_up"] == 1, BollMid: v["boll_mid"], BollPos: v["boll_pos"], ChipProfit: v["chip_profit"], ChipBars: int(v["chip_bars"])}
	c := candidate{Symbol: row.Symbol, Market: "cn", Name: u.Name, Price: v["close"], Amount: u.Amount, TurnoverRate: u.TurnoverRate, PETTM: u.PETTM, PB: u.PB, Industry: u.Industry, QuoteAsOf: row.TradeDate + " 15:00", FactAsOf: &row.CreatedAt, Factors: f, SignalQuality: q, ScoreDims: &scoreDims{Trend: v["rq_score_trend"], Momentum: v["rq_score_momentum"], Position: v["rq_score_position"], Volume: v["rq_score_volume"], Risk: v["rq_score_risk"]}}
	if u.CreatedAt.After(row.CreatedAt) {
		c.FactAsOf = &u.CreatedAt
	}
	if applyTurnoverPosFilter(c, f) != "" {
		return candidate{}, false
	}
	return c, true
}

func readSnapshotResearchSamples(ctx context.Context, tx *gorm.DB, req RankingResearchRequest, d *rankingResearchDataset) error {
	if !tx.Migrator().HasTable(&model.FactorSnapshotDaily{}) || !tx.Migrator().HasTable(&model.StockUniverseDaily{}) {
		d.Coverage.Reasons["pit_sources_missing"]++
		return nil
	}
	var symbols []string
	var cohortDates []string
	if err := tx.Model(&model.StockUniverseDaily{}).Where("market = ? AND trade_date >= ? AND trade_date <= ?", "cn", d.Axis[0], req.AsOf).Distinct("trade_date").Order("trade_date").Pluck("trade_date", &cohortDates).Error; err != nil {
		return err
	}
	cohortDate := ""
	// 在窗口内首个当时可知的宇宙上固定群组；未来上市股票不能反向挤掉训练期样本。
	for _, date := range cohortDates {
		closeAt, err := time.ParseInLocation("2006-01-02 15:04", date+" 15:00", time.Local)
		if err != nil {
			continue
		}
		if err := tx.Model(&model.StockUniverseDaily{}).Where("market = ? AND trade_date = ? AND created_at >= ? AND created_at <= ?", "cn", date, closeAt, researchDayEnd(date)).Pluck("symbol", &symbols).Error; err != nil {
			return err
		}
		if len(symbols) > 0 {
			cohortDate = date
			break
		}
	}
	if len(symbols) == 0 {
		d.Coverage.Reasons["cohort_no_point_in_time_universe"]++
		return nil
	}
	d.Coverage.UniverseSymbols = len(symbols)
	hash := func(s string) uint64 {
		h := fnv.New64a()
		_, _ = h.Write([]byte("qr-cohort-20260912:" + s))
		return h.Sum64()
	}
	sort.Slice(symbols, func(i, j int) bool {
		a, b := hash(symbols[i]), hash(symbols[j])
		if a != b {
			return a < b
		}
		return symbols[i] < symbols[j]
	})
	if len(symbols) > req.MaxSymbols {
		d.Coverage.Reasons["cohort_not_sampled"] = len(symbols) - req.MaxSymbols
		symbols = symbols[:req.MaxSymbols]
	}
	d.Coverage.Sampling = "在 " + cohortDate + " 当时已知的宇宙中按固定代码哈希选取有界群组；之后逐日核对宇宙状态，后上市股票不纳入该群组"
	if len(symbols) == 0 {
		return nil
	}
	var universe []model.StockUniverseDaily
	if err := tx.Where("market = ? AND symbol IN ? AND trade_date >= ? AND trade_date <= ?", "cn", symbols, d.Axis[0], req.AsOf).Find(&universe).Error; err != nil {
		return err
	}
	byKey := map[string]model.StockUniverseDaily{}
	for _, u := range universe {
		byKey[u.TradeDate+":"+u.Symbol] = u
	}
	rows, err := tx.Model(&model.FactorSnapshotDaily{}).Where("market = ? AND symbol IN ? AND trade_date >= ? AND trade_date <= ?", "cn", symbols, d.Axis[0], req.AsOf).Order("trade_date, symbol").Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	byDate := map[string][]candidate{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var row model.FactorSnapshotDaily
		if err := tx.ScanRows(rows, &row); err != nil {
			return err
		}
		d.Coverage.InputRows++
		u, ok := byKey[row.TradeDate+":"+row.Symbol]
		if !ok {
			d.Coverage.Reasons["universe_snapshot_missing"]++
			continue
		}
		c, ok := snapshotResearchCandidate(row, u, req)
		if !ok {
			d.Coverage.Reasons["pit_feature_or_eligibility_unavailable"]++
			continue
		}
		byDate[row.TradeDate] = append(byDate[row.TradeDate], c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	strat := &strategyTemplate{baseKey: req.Profile, ScoreProfile: req.Profile, Intent: profileIntent(req.Profile)}
	for _, date := range d.Axis {
		pool := byDate[date]
		industries := map[string]string{}
		for _, c := range pool {
			industries[c.Symbol] = c.Industry
		}
		attachRecommendationPeers(pool, industries)
		for _, c := range pool {
			sc := ScoreResult{Trend: c.ScoreDims.Trend, Momentum: c.ScoreDims.Momentum, Position: c.ScoreDims.Position, Volume: c.ScoreDims.Volume, Risk: c.ScoreDims.Risk, BarCount: c.Factors.BarCount}
			b, _ := scoreQualityCandidate(req.RecType, strat, c, c.Factors, sc)
			if !b.Valid {
				d.Coverage.Reasons["quality_core_missing"]++
				continue
			}
			comparison := scoreComparison(req.RecType, strat, c, c.Factors, sc, b)
			batch := model.RecommendationBatch{CreatedAt: *c.FactAsOf, Type: req.RecType, Market: "cn"}
			ev := model.RecommendationCandidateEvent{Symbol: c.Symbol, Market: "cn", Name: c.Name, RefPrice: c.Price, RankingVersion: candidateRankingVersion}
			d.Samples = append(d.Samples, rankingResearchSample{Key: "snapshot:" + date + ":" + c.Symbol, Group: "snapshot:" + date, Date: date, Symbol: c.Symbol, Industry: c.Industry, AvailableAt: *c.FactAsOf, X: rankingCandidateFeatures(c), Legacy: comparison.LegacyScore, Quality: comparison.QualityScore, batch: batch, event: ev})
		}
	}
	return nil
}

func fillRankingResearchOutcomes(ctx context.Context, tx *gorm.DB, req RankingResearchRequest, d *rankingResearchDataset) error {
	if len(d.Samples) == 0 {
		return nil
	}
	symbolSet := map[string]bool{}
	batchSet := map[int64]bool{}
	for _, s := range d.Samples {
		symbolSet[s.Symbol] = true
		if s.batch.ID > 0 {
			batchSet[s.batch.ID] = true
		}
	}
	symbols := make([]string, 0, len(symbolSet))
	for symbol := range symbolSet {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	from, _ := time.Parse("2006-01-02", d.Axis[0])
	from = from.AddDate(0, 0, -30)
	bySymbol := map[string][]datasource.Bar{}
	hasBars := tx.Migrator().HasTable(&model.DailyBar{})
	for lo := 0; hasBars && lo < len(symbols); lo += 300 {
		if err := ctx.Err(); err != nil {
			return err
		}
		hi := lo + 300
		if hi > len(symbols) {
			hi = len(symbols)
		}
		var rows []model.DailyBar
		if err := tx.Where("market = ? AND symbol IN ? AND trade_date >= ? AND trade_date <= ?", "cn", symbols[lo:hi], from.Format("2006-01-02"), req.AsOf).Order("symbol, trade_date").Find(&rows).Error; err != nil {
			return err
		}
		for _, b := range rows {
			bySymbol[b.Symbol] = append(bySymbol[b.Symbol], datasource.Bar{TradeDate: b.TradeDate, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Amount: b.Amount, TurnoverRate: b.TurnoverRate, Source: b.Source})
		}
	}
	bench := map[string]float64{}
	if tx.Migrator().HasTable(&model.BenchmarkDaily{}) {
		var rows []model.BenchmarkDaily
		if err := tx.Where("market = ? AND symbol = ? AND trade_date >= ? AND trade_date <= ?", "cn", model.CNBenchmarkSymbol, from.Format("2006-01-02"), req.AsOf).Find(&rows).Error; err != nil {
			return err
		}
		for _, b := range rows {
			if b.Close > 0 && finiteRecNumber(b.Close) {
				bench[b.TradeDate] = b.Close
			}
		}
	}
	// 已形成的当前版本 fixed-hold 结果可补足已过日线保留期的批次；不读计划标签。
	stored := map[string]model.RecommendationSelectionOutcome{}
	if len(batchSet) > 0 && tx.Migrator().HasTable(&model.RecommendationSelectionOutcome{}) {
		ids := make([]int64, 0, len(batchSet))
		for id := range batchSet {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for lo := 0; lo < len(ids); lo += 400 {
			hi := lo + 400
			if hi > len(ids) {
				hi = len(ids)
			}
			var rows []model.RecommendationSelectionOutcome
			if err := tx.Where("batch_id IN ? AND horizon_days = ? AND outcome_version = ?", ids[lo:hi], req.Horizon, model.SelectionOutcomeVersion).Find(&rows).Error; err != nil {
				return err
			}
			for _, r := range rows {
				stored[fmt.Sprintf("%d:%s", r.BatchID, r.Symbol)] = r
			}
		}
	}
	last := d.Axis[len(d.Axis)-1]
	for i := range d.Samples {
		s := &d.Samples[i]
		if r, ok := stored[fmt.Sprintf("%d:%s", s.batch.ID, s.Symbol)]; ok && r.CandidateEventID == s.event.ID && r.UserID == s.batch.UserID && r.Market == "cn" && r.Type == req.RecType && r.EntryMode == model.EntryModeNextOpen && r.SchemaVersion == model.SelectionOutcomeSchemaVersion && r.SignalDate == s.Date && r.ExitDate != "" && r.ExitDate <= req.AsOf && r.MaturityStatus == model.LabelMatured {
			s.Outcome = r
			continue
		}
		bars := bySymbol[s.Symbol]
		if len(bars) == 0 || validateAdjustedBars("cn", bars) != nil || adjustSuspect(bars, s.Symbol, s.event.Name) {
			s.Outcome = model.RecommendationSelectionOutcome{MaturityStatus: model.LabelNoData, NoDataReason: "historical_prices_missing_or_unverified"}
			continue
		}
		s.Outcome = computeSelectionOutcome(s.batch, s.event, req.Horizon, bars, d.Axis, bench, last, req.AsOf)
		if s.Outcome.MaturityStatus == model.LabelSkipped {
			entry, exit := labelAxisDates(d.Axis, s.Date, req.Horizon)
			if exit == "" || exit == labelFarFuture || exit > req.AsOf {
				s.Outcome.ExitDate = ""
				continue
			}
			s.Outcome.ExitDate = exit // 未成交的标准拨款按现金观察到相同持有期末
			if b0, b1 := bench[entry], bench[exit]; entry != "" && exit != "" && b0 > 0 && b1 > 0 {
				s.Outcome.BenchReturnPct = round2((b1/b0 - 1) * 100)
				s.Outcome.AlphaPct = -s.Outcome.BenchReturnPct
				s.Outcome.HasBench = true
			}
		}
	}
	return nil
}

func rankingSampleTarget(s rankingResearchSample, target string) (float64, bool) {
	if (s.Outcome.MaturityStatus != model.LabelMatured && s.Outcome.MaturityStatus != model.LabelSkipped) || s.Outcome.Forced || s.Outcome.ExitDate == "" || s.Outcome.ExitDate == labelFarFuture {
		return 0, false
	}
	if target == "alpha" {
		return s.Outcome.AlphaPct, s.Outcome.HasBench && finiteRecNumber(s.Outcome.AlphaPct)
	}
	return s.Outcome.NetReturnPct, finiteRecNumber(s.Outcome.NetReturnPct)
}
