package service

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

type RankingResearchMetric struct {
	Algorithm              string             `json:"algorithm"`
	Target                 string             `json:"target"`
	Groups                 int                `json:"groups"`
	Selected               int                `json:"selected"`
	Traded                 int                `json:"traded"`
	Skipped                int                `json:"skipped"`
	Pending                int                `json:"pending"`
	NoData                 int                `json:"no_data"`
	Forced                 int                `json:"forced"`
	KnownTargets           int                `json:"known_targets"`
	CoveragePct            float64            `json:"coverage_pct"`
	BenchmarkCoveragePct   float64            `json:"benchmark_coverage_pct"`
	FillRatePct            float64            `json:"fill_rate_pct"`
	MeanNetPct             float64            `json:"mean_net_pct"`
	MedianNetPct           float64            `json:"median_net_pct"`
	P10NetPct              float64            `json:"p10_net_pct"`
	WinRatePct             float64            `json:"win_rate_pct"`
	SevereLossPct          float64            `json:"severe_loss_pct"`
	MeanAlphaPct           *float64           `json:"mean_alpha_pct,omitempty"`
	MeanMAEPct             float64            `json:"mean_mae_pct"`
	MeanHoldingDays        float64            `json:"mean_holding_days"`
	AllocationMeanPct      *float64           `json:"allocation_mean_pct,omitempty"` // 含真实未成交现金，缺行情不算零
	AllocationNetMeanPct   *float64           `json:"allocation_net_mean_pct,omitempty"`
	AllocationAlphaMeanPct *float64           `json:"allocation_alpha_mean_pct,omitempty"`
	CompleteDates          int                `json:"complete_dates"`
	Daily                  map[string]float64 `json:"-"`
	DailyNet               map[string]float64 `json:"-"`
	DailyAlpha             map[string]float64 `json:"-"`
	allocationMean         float64
}

type RankingResearchFold struct {
	TrainFrom    string                  `json:"train_from"`
	TrainTo      string                  `json:"train_to"`
	ValidateFrom string                  `json:"validate_from"`
	ValidateTo   string                  `json:"validate_to"`
	TestFrom     string                  `json:"test_from"`
	TestTo       string                  `json:"test_to"`
	TrainingRows int                     `json:"training_rows"`
	PurgedRows   int                     `json:"purged_rows"`
	Lambda       float64                 `json:"lambda,omitempty"`
	Status       string                  `json:"status"`
	Reason       string                  `json:"reason,omitempty"`
	Model        *RankingRidgeModel      `json:"model,omitempty"`
	Metrics      []RankingResearchMetric `json:"metrics"`
}

type RankingResearchComparison struct {
	Challenger string  `json:"challenger"`
	Baseline   string  `json:"baseline"`
	Target     string  `json:"target"`
	Dates      int     `json:"dates"`
	BlockDays  int     `json:"block_days"`
	DeltaPct   float64 `json:"delta_pct"`
	Low95      float64 `json:"low_95"`
	High95     float64 `json:"high_95"`
}

type RankingResearchReport struct {
	Version          string                      `json:"version"`
	Request          RankingResearchRequest      `json:"request"`
	DatasetHash      string                      `json:"dataset_hash"`
	OutcomeVersion   string                      `json:"outcome_version"`
	FeatureVersion   string                      `json:"feature_version"`
	Evaluated        bool                        `json:"evaluated"`
	Reason           string                      `json:"reason,omitempty"`
	Coverage         RankingResearchCoverage     `json:"coverage"`
	Spec             wfSpec                      `json:"split"`
	Adapted          bool                        `json:"adapted"`
	Folds            []RankingResearchFold       `json:"folds"`
	Descriptive      []RankingResearchMetric     `json:"descriptive"`
	OutOfTime        []RankingResearchMetric     `json:"out_of_time"`
	Comparisons      []RankingResearchComparison `json:"comparisons"`
	PromotionReady   bool                        `json:"promotion_ready"`
	PromotionReasons []string                    `json:"promotion_reasons"`
	Notes            []string                    `json:"notes"`
	GeneratedAt      time.Time                   `json:"generated_at"`
}

type rankingPrediction struct {
	Sample rankingResearchSample
	Score  float64
}

func rankingResearchMetric(predictions []rankingPrediction, algorithm, target string, topK int, axis []string) RankingResearchMetric {
	m := RankingResearchMetric{Algorithm: algorithm, Target: target, Daily: map[string]float64{}}
	groups := map[string][]rankingPrediction{}
	dateGroups := map[string][]float64{}
	invalidDate := map[string]bool{}
	netGroups, alphaGroups := map[string][]float64{}, map[string][]float64{}
	invalidNet, invalidAlpha := map[string]bool{}, map[string]bool{}
	knownAlpha := 0
	for _, p := range predictions {
		groups[p.Sample.Group] = append(groups[p.Sample.Group], p)
	}
	axisIdx := map[string]int{}
	for i, date := range axis {
		axisIdx[date] = i
	}
	var nets, alphas []float64
	wins, severe := 0, 0
	mae, hold := 0.0, 0.0
	groupKeys := make([]string, 0, len(groups))
	for key := range groups {
		groupKeys = append(groupKeys, key)
	}
	sort.Strings(groupKeys)
	for _, key := range groupKeys {
		rows := groups[key]
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Score != rows[j].Score {
				return rows[i].Score > rows[j].Score
			}
			return researchSampleLess(rows[i].Sample, rows[j].Sample)
		})
		if len(rows) > topK {
			rows = rows[:topK]
		}
		if len(rows) == 0 {
			continue
		}
		m.Groups++
		date := rows[0].Sample.Date
		complete := true
		allocation := 0.0
		netSum, alphaSum := 0.0, 0.0
		netComplete, alphaComplete := true, true
		for _, p := range rows {
			s := p.Sample
			o := s.Outcome
			m.Selected++
			y, known := rankingSampleTarget(s, target)
			if net, ok := rankingSampleTarget(s, "net"); ok {
				netSum += net
			} else {
				netComplete = false
			}
			if alpha, ok := rankingSampleTarget(s, "alpha"); ok {
				alphaSum += alpha
				knownAlpha++
			} else {
				alphaComplete = false
			}
			if !known {
				complete = false
			} else {
				allocation += y
				m.KnownTargets++
			}
			switch o.MaturityStatus {
			case model.LabelMatured:
				if o.Forced {
					m.Forced++
					continue
				}
				m.Traded++
				nets = append(nets, o.NetReturnPct)
				mae += o.MaePct
				if o.NetReturnPct > 0 {
					wins++
				}
				if o.NetReturnPct < -5 {
					severe++
				}
				if o.HasBench {
					alphas = append(alphas, o.AlphaPct)
				}
				if a, ok := axisIdx[o.EntryDate]; ok {
					if b, ok := axisIdx[o.ExitDate]; ok && b >= a {
						hold += float64(b - a)
					}
				}
			case model.LabelSkipped:
				m.Skipped++
			case model.LabelNoData:
				m.NoData++
			default:
				m.Pending++
			}
		}
		if complete {
			dateGroups[date] = append(dateGroups[date], allocation/float64(len(rows)))
		} else {
			invalidDate[date] = true
		}
		if netComplete {
			netGroups[date] = append(netGroups[date], netSum/float64(len(rows)))
		} else {
			invalidNet[date] = true
		}
		if alphaComplete {
			alphaGroups[date] = append(alphaGroups[date], alphaSum/float64(len(rows)))
		} else {
			invalidAlpha[date] = true
		}
	}
	if m.Selected > 0 {
		m.CoveragePct = round2(float64(m.KnownTargets) / float64(m.Selected) * 100)
		m.FillRatePct = round2(float64(m.Traded) / float64(m.Selected) * 100)
		m.BenchmarkCoveragePct = round2(float64(knownAlpha) / float64(m.Selected) * 100)
	}
	if len(nets) > 0 {
		sort.Float64s(nets)
		m.MeanNetPct = round2(meanRankingValues(nets))
		m.MedianNetPct = round2(quantileRankingValues(nets, 0.5))
		m.P10NetPct = round2(quantileRankingValues(nets, 0.1))
		m.WinRatePct = round2(float64(wins) / float64(len(nets)) * 100)
		m.SevereLossPct = round2(float64(severe) / float64(len(nets)) * 100)
		m.MeanMAEPct = round2(mae / float64(len(nets)))
		m.MeanHoldingDays = round2(hold / float64(len(nets)))
	}
	if len(alphas) > 0 {
		v := round2(meanRankingValues(alphas))
		m.MeanAlphaPct = &v
	}
	var daily []float64
	dates := make([]string, 0, len(dateGroups))
	for date := range dateGroups {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates {
		values := dateGroups[date]
		if !invalidDate[date] {
			v := meanRankingValues(values)
			m.Daily[date] = v
			daily = append(daily, v)
		}
	}
	m.CompleteDates = len(daily)
	if len(daily) > 0 {
		m.allocationMean = meanRankingValues(daily)
		v := round2(m.allocationMean)
		m.AllocationMeanPct = &v
	}
	m.DailyNet, m.AllocationNetMeanPct = aggregateRankingDates(netGroups, invalidNet)
	m.DailyAlpha, m.AllocationAlphaMeanPct = aggregateRankingDates(alphaGroups, invalidAlpha)
	return m
}

func aggregateRankingDates(groups map[string][]float64, invalid map[string]bool) (map[string]float64, *float64) {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := map[string]float64{}
	var values []float64
	for _, key := range keys {
		if !invalid[key] {
			v := meanRankingValues(groups[key])
			out[key] = v
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return out, nil
	}
	mean := round2(meanRankingValues(values))
	return out, &mean
}

func researchSampleLess(a, b rankingResearchSample) bool {
	if a.Date != b.Date {
		return a.Date < b.Date
	}
	if a.Group != b.Group {
		return a.Group < b.Group
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	return a.Key < b.Key
}

func meanRankingValues(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
func quantileRankingValues(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	pos := p * float64(len(xs)-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= len(xs) {
		return xs[lo]
	}
	return xs[lo] + (xs[hi]-xs[lo])*(pos-float64(lo))
}

func researchPredictions(samples []rankingResearchSample, algorithm string, m *RankingRidgeModel) []rankingPrediction {
	out := make([]rankingPrediction, 0, len(samples))
	for _, s := range samples {
		score := s.Legacy
		if algorithm == recommendationScoringVersion {
			score = s.Quality
		} else if algorithm == rankingRidgeVersion {
			if m == nil {
				continue
			}
			score = m.score(s.X)
		}
		out = append(out, rankingPrediction{Sample: s, Score: score})
	}
	return out
}

func researchSamplesIn(samples []rankingResearchSample, lo, hi string) []rankingResearchSample {
	out := make([]rankingResearchSample, 0)
	for _, s := range samples {
		if s.Date >= lo && s.Date <= hi {
			out = append(out, s)
		}
	}
	return out
}

func researchMatureBefore(samples []rankingResearchSample, before string, target string) ([]rankingResearchSample, int) {
	out := make([]rankingResearchSample, 0)
	purged := 0
	cut, _ := time.ParseInLocation("2006-01-02", before, time.Local)
	for _, s := range samples {
		if _, ok := rankingSampleTarget(s, target); !ok || s.Outcome.ExitDate >= before || !s.AvailableAt.Before(cut) {
			purged++
			continue
		}
		out = append(out, s)
	}
	return out, purged
}

func researchValidationBefore(samples []rankingResearchSample, before string) []rankingResearchSample {
	out := append([]rankingResearchSample(nil), samples...)
	for i := range out {
		cut, _ := time.ParseInLocation("2006-01-02", before, time.Local)
		if out[i].Outcome.ExitDate == "" || out[i].Outcome.ExitDate >= before || !out[i].AvailableAt.Before(cut) {
			out[i].Outcome.MaturityStatus = model.LabelPending
		}
	}
	return out
}

func compareRankingResearch(a, b RankingResearchMetric, horizon int, targets ...string) RankingResearchComparison {
	target := a.Target
	if len(targets) > 0 {
		target = targets[0]
		if target == "net" {
			a.Daily, b.Daily = a.DailyNet, b.DailyNet
		} else {
			a.Daily, b.Daily = a.DailyAlpha, b.DailyAlpha
		}
	}
	comparison := RankingResearchComparison{Challenger: a.Algorithm, Baseline: b.Algorithm, Target: target, BlockDays: horizon + 1}
	dates := make([]string, 0)
	for date := range a.Daily {
		if _, ok := b.Daily[date]; ok {
			dates = append(dates, date)
		}
	}
	sort.Strings(dates)
	diffs := make([]float64, len(dates))
	for i, date := range dates {
		diffs[i] = a.Daily[date] - b.Daily[date]
	}
	comparison.Dates = len(diffs)
	if len(diffs) == 0 {
		return comparison
	}
	comparison.DeltaPct = round2(meanRankingValues(diffs))
	block := horizon + 1
	if block > len(diffs) {
		block = len(diffs)
	}
	rng := rand.New(rand.NewSource(20260912))
	boots := make([]float64, 1000)
	for i := range boots {
		sum, n := 0.0, 0
		for n < len(diffs) {
			start := rng.Intn(len(diffs))
			for j := 0; j < block && n < len(diffs); j++ {
				sum += diffs[(start+j)%len(diffs)]
				n++
			}
		}
		boots[i] = sum / float64(n)
	}
	sort.Float64s(boots)
	comparison.Low95 = round2(quantileRankingValues(boots, 0.025))
	comparison.High95 = round2(quantileRankingValues(boots, 0.975))
	return comparison
}

func evaluateRankingResearch(req RankingResearchRequest, d *rankingResearchDataset, contexts ...context.Context) *RankingResearchReport {
	ctx := jobSubmissionContext(contexts...)
	rep := &RankingResearchReport{Version: rankingResearchVersion, Request: req, DatasetHash: d.Hash, OutcomeVersion: model.SelectionOutcomeVersion, FeatureVersion: "of1 / fv6 / sq1", Coverage: d.Coverage, GeneratedAt: time.Now(), Notes: []string{
		"只读取冻结特征；日线只计算结果，不参与重建历史特征。缺少新字段的旧快照不会伪装完整样本。",
		"原加法评分、新质量评分与 ridge 基线使用相同机会集、TopK 和 so2 费用执行器。每组先排序再核对结果，未成交不补选下一只。",
		"未成交拨款按现金观察到同一持有期；行情缺失、未成熟和强制退出单列，不当作零收益。均值按信号日期聚合，不把重叠持仓复利成年化收益。",
		"训练标签必须在下一阶段开始前成熟；验证段仅选择正则参数，测试段不调参。区间采用持有期长度的日期块重采样。",
		"学习分仅用于排序，不是获利概率。训练目标按固定 ±50% 限制异常值，测试收益保持原值。",
		"学习模型有合法测试预测时，三种算法的汇总都限制在同一测试机会集；尚不能拟合模型时仍提供规则的时间外对照。",
	}}
	for _, alg := range []string{"additive_sp1", recommendationScoringVersion} {
		rep.Descriptive = append(rep.Descriptive, rankingResearchMetric(researchPredictions(d.Samples, alg, nil), alg, req.Target, req.TopK, d.Axis))
	}
	var ok bool
	rep.Spec, rep.Adapted, ok = wfAdaptSpec(len(d.Axis)-req.Horizon-1, req.Horizon)
	if !ok {
		rep.Reason = "历史跨度不足以隔开训练、验证、测试及持有期重叠"
		rep.PromotionReasons = []string{rep.Reason}
		return rep
	}
	folds := wfSplitFolds(len(d.Axis)-req.Horizon-1, rep.Spec)
	outPreds := map[string]map[string]rankingPrediction{"additive_sp1": {}, recommendationScoringVersion: {}, rankingRidgeVersion: {}}
	for _, f := range folds {
		if ctx.Err() != nil {
			rep.Reason = "研究已取消"
			return rep
		}
		fold := RankingResearchFold{TrainFrom: d.Axis[f.TrainLo], TrainTo: d.Axis[f.TrainHi], ValidateFrom: d.Axis[f.ValLo], ValidateTo: d.Axis[f.ValHi], TestFrom: d.Axis[f.TestLo], TestTo: d.Axis[f.TestHi], Status: "insufficient"}
		train := researchSamplesIn(d.Samples, fold.TrainFrom, fold.TrainTo)
		train, fold.PurgedRows = researchMatureBefore(train, fold.ValidateFrom, req.Target)
		fold.TrainingRows = len(train)
		validation := researchValidationBefore(researchSamplesIn(d.Samples, fold.ValidateFrom, fold.ValidateTo), fold.TestFrom)
		test := researchSamplesIn(d.Samples, fold.TestFrom, fold.TestTo)
		var best *RankingRidgeModel
		bestValue := math.Inf(-1)
		for _, lambda := range []float64{1, 10, 100} {
			m, err := fitRankingRidge(train, req.Target, lambda, ctx)
			if err != nil {
				fold.Reason = err.Error()
				continue
			}
			metric := rankingResearchMetric(researchPredictions(validation, rankingRidgeVersion, m), rankingRidgeVersion, req.Target, req.TopK, d.Axis)
			if metric.AllocationMeanPct == nil || metric.CompleteDates < 5 || metric.CoveragePct < 80 {
				fold.Reason = "验证段成熟覆盖或独立日期不足"
				continue
			}
			if metric.allocationMean > bestValue {
				best = m
				bestValue = metric.allocationMean
			}
		}
		if best != nil {
			// 参数已由验证段选定；测试前用已成熟的训练+验证样本重新拟合。
			refit := append(append([]rankingResearchSample(nil), train...), validation...)
			refit, _ = researchMatureBefore(refit, fold.TestFrom, req.Target)
			m, err := fitRankingRidge(refit, req.Target, best.Lambda, ctx)
			if err == nil {
				fold.Model = m
				fold.Lambda = m.Lambda
				fold.Status = "evaluated"
				fold.Reason = ""
			} else {
				fold.Reason = err.Error()
			}
		}
		for _, alg := range []string{"additive_sp1", recommendationScoringVersion, rankingRidgeVersion} {
			preds := researchPredictions(test, alg, fold.Model)
			metric := rankingResearchMetric(preds, alg, req.Target, req.TopK, d.Axis)
			fold.Metrics = append(fold.Metrics, metric)
			// 重叠测试日期只保留最近一次合法训练的预测，不能重复放大样本量。
			for _, p := range preds {
				outPreds[alg][p.Sample.Key] = p
			}
		}
		rep.Folds = append(rep.Folds, fold)
	}
	for _, alg := range []string{"additive_sp1", recommendationScoringVersion, rankingRidgeVersion} {
		preds := make([]rankingPrediction, 0, len(outPreds[alg]))
		for _, p := range outPreds[alg] {
			if len(outPreds[rankingRidgeVersion]) > 0 {
				if _, ok := outPreds[rankingRidgeVersion][p.Sample.Key]; !ok {
					continue
				}
			}
			preds = append(preds, p)
		}
		sort.Slice(preds, func(i, j int) bool { return researchSampleLess(preds[i].Sample, preds[j].Sample) })
		rep.OutOfTime = append(rep.OutOfTime, rankingResearchMetric(preds, alg, req.Target, req.TopK, d.Axis))
	}
	if len(rep.OutOfTime) == 3 {
		rep.Comparisons = append(rep.Comparisons, compareRankingResearch(rep.OutOfTime[1], rep.OutOfTime[0], req.Horizon), compareRankingResearch(rep.OutOfTime[2], rep.OutOfTime[1], req.Horizon), compareRankingResearch(rep.OutOfTime[2], rep.OutOfTime[0], req.Horizon))
		linear, quality := rep.OutOfTime[2], rep.OutOfTime[1]
		rep.Evaluated = linear.CompleteDates >= 10 && linear.Traded >= 100
		if !rep.Evaluated {
			rep.Reason = "成熟且可核验的时间外样本不足（至少 100 笔成交、10 个完整日期）"
		}
		cmp := rep.Comparisons[1]
		minDates := 30
		if 2*(req.Horizon+1) > minDates {
			minDates = 2 * (req.Horizon + 1)
		}
		if req.Source != "recommendations" {
			rep.PromotionReasons = append(rep.PromotionReasons, "收盘历史仅作研究，启用学习评分还需要真实推荐机会集对照")
		}
		if !rep.Evaluated || cmp.Dates < minDates {
			rep.PromotionReasons = append(rep.PromotionReasons, fmt.Sprintf("时间外有效日期少于 %d，或成交样本不足", minDates))
		}
		if cmp.Low95 <= 0 {
			rep.PromotionReasons = append(rep.PromotionReasons, "学习基线相对新规则的收益增量区间尚未整体高于零")
		}
		if rep.Comparisons[2].Low95 <= 0 {
			rep.PromotionReasons = append(rep.PromotionReasons, "学习基线相对原加法评分的增量区间尚未整体高于零")
		}
		otherTarget := "net"
		if req.Target == "net" {
			otherTarget = "alpha"
		}
		other := compareRankingResearch(linear, quality, req.Horizon, otherTarget)
		rep.Comparisons = append(rep.Comparisons, other)
		if other.Dates < minDates || other.DeltaPct < 0 {
			rep.PromotionReasons = append(rep.PromotionReasons, "扣费与超额收益的另一口径尚未取得足够配对覆盖或表现恶化")
		}
		if len(rep.Folds) == 0 || rep.Folds[len(rep.Folds)-1].Model == nil {
			rep.PromotionReasons = append(rep.PromotionReasons, "最近一折缺少可用于后续推荐的验证模型")
		}
		if linear.CoveragePct < 95 || linear.BenchmarkCoveragePct < 95 || linear.FillRatePct < 80 || linear.CoveragePct < quality.CoveragePct {
			rep.PromotionReasons = append(rep.PromotionReasons, "结果覆盖或可成交率不足，不能以少成交换取表面提升")
		}
		if linear.SevereLossPct > quality.SevereLossPct+1 || linear.P10NetPct < quality.P10NetPct-1 {
			rep.PromotionReasons = append(rep.PromotionReasons, "尾部损失相对规则基线恶化")
		}
		if profileUsesFinance(req.Profile) && d.Coverage.FinanceRows < d.Coverage.FeatureRows*8/10 {
			rep.PromotionReasons = append(rep.PromotionReasons, "财务型策略的财报覆盖不足")
		}
		rep.PromotionReady = len(rep.PromotionReasons) == 0
	}
	return rep
}

// RunRankingResearch 是只读离线研究入口，也供管理员页面和独立 CLI 共用。
func RunRankingResearch(ctx context.Context, db *gorm.DB, request RankingResearchRequest) (*RankingResearchReport, error) {
	req, err := normalizeRankingResearchRequest(request, time.Now())
	if err != nil {
		return nil, err
	}
	d, err := readRankingResearchDataset(ctx, db, req)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	report := evaluateRankingResearch(req, d, ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return report, nil
}
