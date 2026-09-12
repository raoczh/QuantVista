package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func researchSyntheticDataset(days int) *rankingResearchDataset {
	d := &rankingResearchDataset{Coverage: RankingResearchCoverage{Reasons: map[string]int{}}}
	start := time.Date(2022, 1, 1, 16, 0, 0, 0, time.Local)
	for i := 0; i < days; i++ {
		d.Axis = append(d.Axis, start.AddDate(0, 0, i).Format("2006-01-02"))
	}
	for i, date := range d.Axis {
		for j := 0; j < 6; j++ {
			x := make([]float64, len(rankingFeatureNames))
			for k := range x {
				x[k] = math.NaN()
			}
			x[0] = float64(j-3) + 0.1*math.Sin(float64(i))
			x[1] = 12 // 常量列不能参与推断
			if (i+j)%4 == 0 {
				x[3] = float64(j)
			}
			o := model.RecommendationSelectionOutcome{MaturityStatus: model.LabelPending}
			if i+11 < days {
				y := 1.5*x[0] + 2
				o = model.RecommendationSelectionOutcome{MaturityStatus: model.LabelMatured, EntryDate: d.Axis[i+1], ExitDate: d.Axis[i+11], NetReturnPct: y, AlphaPct: y - 1, HasBench: true}
			}
			d.Samples = append(d.Samples, rankingResearchSample{Key: fmt.Sprintf("%s:%d", date, j), Group: date, Date: date, Symbol: fmt.Sprintf("60010%d", j), AvailableAt: start.AddDate(0, 0, i), X: x, Legacy: float64(6 - j), Quality: float64(j), Outcome: o})
		}
	}
	d.Coverage.FeatureRows = len(d.Samples)
	return d
}

func TestRankingRidgeRegularizationAndTrainingOnlyPreprocess(t *testing.T) {
	d := researchSyntheticDataset(55)
	a, err := fitRankingRidge(d.Samples, "net", 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fitRankingRidge(d.Samples, "net", 100)
	if err != nil {
		t.Fatal(err)
	}
	if a.Active[1] || a.Active[2] || a.Weights[1].Weight != 0 || a.Weights[2].Weight != 0 {
		t.Fatal("常量和全缺失列必须失活")
	}
	if a.Weights[0].Weight <= 0 || math.Abs(b.Weights[0].Weight) >= math.Abs(a.Weights[0].Weight) {
		t.Fatal("应恢复正向关系，并由更强正则收缩系数")
	}
	meanResidual := 0.0
	n := 0
	for _, s := range d.Samples {
		if y, ok := rankingSampleTarget(s, "net"); ok {
			meanResidual += a.score(s.X) - y
			n++
		}
	}
	if math.Abs(meanResidual/float64(n)) > 1e-9 {
		t.Fatal("裁剪后的设计矩阵须重新中心化，不能改变截距口径")
	}
	reversed := append([]rankingResearchSample(nil), d.Samples...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	c, err := fitRankingRidge(reversed, "net", 1)
	if err != nil || !reflect.DeepEqual(a, c) {
		t.Fatal("同一训练样本的输入排列不能改变模型")
	}
	weights, err := solveRankingSPD([][]float64{{4, 1}, {1, 3}}, []float64{1, 2})
	if err != nil || math.Abs(weights[0]-1.0/11) > 1e-12 || math.Abs(weights[1]-7.0/11) > 1e-12 {
		t.Fatalf("线性解不正确: %v %v", weights, err)
	}
	if _, err := fitRankingRidge(d.Samples[:12], "net", 1); err == nil {
		t.Fatal("小样本不能训练假模型")
	}
}

func TestRankingResearchNoFutureFillOrFalseCoverage(t *testing.T) {
	d := researchSyntheticDataset(30)
	rows := researchPredictions(d.Samples[:6], "additive_sp1", nil)
	rows[0].Sample.Outcome = model.RecommendationSelectionOutcome{MaturityStatus: model.LabelSkipped}
	m := rankingResearchMetric(rows, "a", "net", 1, d.Axis)
	if m.Selected != 1 || m.Skipped != 1 || m.CoveragePct != 0 || m.Traded != 0 || m.AllocationMeanPct != nil {
		t.Fatalf("未走完持有期的现金不能算完整；不能补选未来可成交股: %+v", m)
	}
	rows[0].Sample.Outcome.ExitDate = d.Axis[11]
	m = rankingResearchMetric(rows, "a", "net", 1, d.Axis)
	if m.CoveragePct != 100 || m.AllocationMeanPct == nil || *m.AllocationMeanPct != 0 {
		t.Fatal("成熟未成交拨款应按现金零收益")
	}
	m = rankingResearchMetric(rows, "a", "alpha", 1, d.Axis)
	if m.CoveragePct != 0 || m.AllocationMeanPct != nil {
		t.Fatal("缺基准不能宣称超额结果已覆盖")
	}
	rows[0].Sample.Outcome.HasBench = true
	rows[0].Sample.Outcome.AlphaPct = -3
	m = rankingResearchMetric(rows, "a", "alpha", 1, d.Axis)
	if m.AllocationMeanPct == nil || *m.AllocationMeanPct != -3 {
		t.Fatal("未成交现金也有相对基准的机会成本")
	}
	rows[0].Sample.Outcome.ExitDate = labelFarFuture
	if _, ok := rankingSampleTarget(rows[0].Sample, "net"); ok {
		t.Fatal("哨兵不能成为成熟标签")
	}
}

func TestRankingResearchPurgesActualExitAndNeverTunesOnTestLabels(t *testing.T) {
	d := researchSyntheticDataset(280)
	s := d.Samples[0]
	s.Outcome.ExitDate = d.Axis[100] // 停牌延后，虽然理论持有期很早就结束，也必须 purge
	if got, n := researchMatureBefore([]rankingResearchSample{s}, d.Axis[90], "net"); len(got) != 0 || n != 1 {
		t.Fatal("应按实际退出日清除跨段标签")
	}
	s.Outcome.ExitDate = d.Axis[10]
	s.AvailableAt = researchDayEnd(d.Axis[95])
	if got, _ := researchMatureBefore([]rankingResearchSample{s}, d.Axis[90], "net"); len(got) != 0 {
		t.Fatal("信息尚不可知不能进入训练")
	}
	req := RankingResearchRequest{Source: "recommendations", RecType: model.RecTypeShortTerm, Profile: "momentum", Horizon: 10, Target: "net", TopK: 3}
	a := evaluateRankingResearch(req, d)
	if len(a.Folds) != 1 || a.Folds[0].Model == nil {
		t.Fatalf("合成足量样本应完成一折: %+v", a.Folds)
	}
	from := a.Folds[0].TestFrom
	for i := range d.Samples {
		if d.Samples[i].Date >= from {
			d.Samples[i].Outcome.NetReturnPct = -30 - float64(i%17)
		}
	}
	b := evaluateRankingResearch(req, d)
	if !reflect.DeepEqual(a.Folds[0].Model, b.Folds[0].Model) || a.Folds[0].Lambda != b.Folds[0].Lambda {
		t.Fatal("更改测试结果不能改变选参、标准化或拟合系数")
	}
	if reflect.DeepEqual(a.OutOfTime, b.OutOfTime) {
		t.Fatal("测试收益应真实反映变化")
	}
}

func TestRankingResearchOverlappingTestsAreCountedOnce(t *testing.T) {
	d := researchSyntheticDataset(920)
	req := RankingResearchRequest{Source: "recommendations", RecType: model.RecTypeShortTerm, Profile: "momentum", Horizon: 10, Target: "net", TopK: 2}
	rep := evaluateRankingResearch(req, d)
	unique := map[string]bool{}
	for _, f := range rep.Folds {
		for _, date := range d.Axis {
			if date >= f.TestFrom && date <= f.TestTo {
				unique[date] = true
			}
		}
	}
	if len(rep.Folds) < 2 || len(rep.OutOfTime) != 3 || rep.OutOfTime[0].Selected != len(unique)*2 {
		t.Fatal("滚动测试的重叠日期不能重复计数")
	}
	rep2 := evaluateRankingResearch(req, d)
	if !reflect.DeepEqual(rep.Comparisons, rep2.Comparisons) || !reflect.DeepEqual(rep.OutOfTime, rep2.OutOfTime) {
		t.Fatal("日期聚合与 bootstrap 应可重复")
	}
}

func researchSnapshotFixture(t *testing.T) (model.FactorSnapshotDaily, model.StockUniverseDaily) {
	t.Helper()
	bars := qualityScenarioBars(false)
	date := bars[len(bars)-1].TradeDate
	values := computeWideRowOpts("600100", wideStockMeta{Name: "样本"}, bars, false)
	factors := map[string]float64{}
	for i, def := range factorDefs {
		if finiteRecNumber(values[i]) {
			factors[def.Key] = values[i]
		}
	}
	b, err := json.Marshal(factors)
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.ParseInLocation("2006-01-02 15:04", date+" 16:10", time.Local)
	return model.FactorSnapshotDaily{Symbol: "600100", Market: "cn", TradeDate: date, LastBarDate: date, FactorVersion: factorSnapshotVersion, FactorsJSON: string(b), CreatedAt: at}, model.StockUniverseDaily{Symbol: "600100", Market: "cn", Name: "样本", TradeDate: date, Close: bars[len(bars)-1].Close, Amount: 1e8, TurnoverRate: 3, CreatedAt: at}
}

func TestRankingResearchSnapshotPITAndFrozenFeatures(t *testing.T) {
	row, u := researchSnapshotFixture(t)
	req := RankingResearchRequest{RecType: model.RecTypeShortTerm, Profile: "momentum"}
	c, ok := snapshotResearchCandidate(row, u, req)
	if !ok || c.SignalQuality.BreakoutConfirmed == nil || !*c.SignalQuality.BreakoutConfirmed || c.ScoreDims == nil {
		t.Fatal("fv6 应保存当时的技术维度和突破事实")
	}
	original := rankingCandidateFeatures(c)
	// 输入只能来自快照，今天重新锚定的历史价格不会参与重算。
	bars := qualityScenarioBars(false)
	for i := range bars {
		bars[i].Close *= 7
	}
	again, ok := snapshotResearchCandidate(row, u, req)
	if !ok || fmt.Sprint(original) != fmt.Sprint(rankingCandidateFeatures(again)) {
		t.Fatal("冻结特征不应随今日行情漂移")
	}
	for _, mutate := range []func(*model.FactorSnapshotDaily, *model.StockUniverseDaily){
		func(r *model.FactorSnapshotDaily, u *model.StockUniverseDaily) { r.FactorVersion = "fv5" },
		func(r *model.FactorSnapshotDaily, u *model.StockUniverseDaily) {
			r.CreatedAt = r.CreatedAt.AddDate(0, 0, 1)
		},
		func(r *model.FactorSnapshotDaily, u *model.StockUniverseDaily) {
			u.CreatedAt = u.CreatedAt.AddDate(0, 0, 1)
		},
		func(r *model.FactorSnapshotDaily, u *model.StockUniverseDaily) { u.TradeDate = "2026-12-31" },
		func(r *model.FactorSnapshotDaily, u *model.StockUniverseDaily) { u.IsST = true },
		func(r *model.FactorSnapshotDaily, u *model.StockUniverseDaily) {
			r.FactorsJSON = `{"close":10,"bar_count":90}`
		},
	} {
		a, b := row, u
		mutate(&a, &b)
		if _, ok := snapshotResearchCandidate(a, b, req); ok {
			t.Fatal("旧特征、未来入库与历史宇宙缺失不能伪装有效")
		}
	}
}

func TestRankingResearchReadOnlyDatabaseAndMissingSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "research.sqlite")
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	db, err := gorm.Open(sqlite.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.TradingCalendar{}, &model.StockUniverseDaily{}, &model.FactorSnapshotDaily{}); err != nil {
		t.Fatal(err)
	}
	row, universe := researchSnapshotFixture(t)
	for _, v := range []any{&row, &universe, &model.TradingCalendar{Market: "cn", TradeDate: row.TradeDate, IsOpen: true}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	sqlDB, _ := db.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery = "mode=ro"
	ro, err := gorm.Open(sqlite.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlRO, _ := ro.DB()
	defer sqlRO.Close()
	oldDB := common.DB
	common.DB = nil
	t.Cleanup(func() { common.DB = oldDB })
	rep, err := RunRankingResearch(context.Background(), ro, RankingResearchRequest{Source: "snapshots", AsOf: row.TradeDate})
	if err != nil || rep.Coverage.FeatureRows != 1 || rep.Coverage.NoData != 1 || rep.PromotionReady {
		t.Fatalf("无日线/基准的只读输入应返回真实缺口: %+v %v", rep, err)
	}
	if err := sqlRO.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("评估不能写入或迁移源库")
	}
}

func TestRankingResearchRejectsIncompleteOpportunityGroup(t *testing.T) {
	ev := optimizationFactEvent(t)
	ev.BatchID, ev.UserID = 1, 2
	var facts recOptimizationFacts
	if err := json.Unmarshal([]byte(ev.FeatureSnapshot), &facts); err != nil {
		t.Fatal(err)
	}
	b := model.RecommendationBatch{ID: 1, UserID: 2, Market: "cn", Type: model.RecTypeShortTerm, ScoringVersion: recommendationScoringVersion, CreatedAt: facts.Candidate.FactAsOf.Add(-time.Minute)}
	req := RankingResearchRequest{Profile: "momentum", AsOf: "2026-09-01"}
	d := &rankingResearchDataset{Coverage: RankingResearchCoverage{Reasons: map[string]int{}}}
	appendRecommendationResearchGroup([]model.RecommendationCandidateEvent{ev}, b, req, d)
	if len(d.Samples) != 1 {
		t.Fatal("完整机会集应能读取")
	}
	d.Samples = nil
	appendRecommendationResearchGroup([]model.RecommendationCandidateEvent{ev, ev}, b, req, d)
	if len(d.Samples) != 0 || d.Coverage.Reasons["incomplete_opportunity_batch"] != 1 {
		t.Fatal("重复事实不能只删坏行后假装机会集完整")
	}
}

func TestCompletedBenchmarkCacheIsImmutable(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 9, 11, 11, 0, 0, 0, time.Local)
	bars := []datasource.Bar{{TradeDate: "2026-09-10", Close: 3100}, {TradeDate: "2026-09-11", Close: 3500}, {TradeDate: "2026-09-12", Close: 3600}}
	persistCompletedBenchmark(context.Background(), "cn", bars, now)
	var rows []model.BenchmarkDaily
	if err := common.DB.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Close != 3100 || rows[0].Symbol != model.CNBenchmarkSymbol {
		t.Fatal("不能缓存盘中或未来指数为收盘基准")
	}
	bars[0].Close = 9999
	persistCompletedBenchmark(context.Background(), "cn", bars, now.Add(6*time.Hour))
	rows = nil
	if err := common.DB.Order("trade_date").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Close != 3100 || rows[1].Close != 3500 {
		t.Fatal("已收盘缓存首写后不可覆盖")
	}
}

func TestRankingSnapshotCohortCannotUseFutureListings(t *testing.T) {
	setupTestDB(t)
	row, u := researchSnapshotFixture(t)
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	future := u.CreatedAt.AddDate(0, 0, 1)
	req := RankingResearchRequest{Source: "snapshots", RecType: model.RecTypeShortTerm, Profile: "momentum", AsOf: future.Format("2006-01-02"), MaxSymbols: 1}
	read := func() *rankingResearchDataset {
		d := &rankingResearchDataset{Axis: []string{row.TradeDate, req.AsOf}, Coverage: RankingResearchCoverage{Reasons: map[string]int{}}}
		if err := readSnapshotResearchSamples(context.Background(), common.DB, req, d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	before := read()
	if len(before.Samples) != 1 {
		t.Fatal("应读到当日已知股票")
	}
	var futureUniverse []model.StockUniverseDaily
	var futureFactors []model.FactorSnapshotDaily
	for i := 0; i < 100; i++ {
		x, y := u, row
		x.ID, y.ID = 0, 0
		x.Symbol = fmt.Sprintf("601%03d", i)
		y.Symbol = x.Symbol
		x.TradeDate, y.TradeDate, y.LastBarDate = req.AsOf, req.AsOf, req.AsOf
		x.CreatedAt, y.CreatedAt = future, future
		futureUniverse = append(futureUniverse, x)
		futureFactors = append(futureFactors, y)
	}
	if err := common.DB.CreateInBatches(futureUniverse, 100).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.CreateInBatches(futureFactors, 100).Error; err != nil {
		t.Fatal(err)
	}
	after := read()
	if len(after.Samples) != 1 || after.Samples[0].Key != before.Samples[0].Key || fmt.Sprint(after.Samples[0].X) != fmt.Sprint(before.Samples[0].X) {
		t.Fatal("后来上市的股票不能倒过来改变训练期群组或特征")
	}
}

func TestRankingRequestKeepsHorizonAlignedWithRecommendationType(t *testing.T) {
	now := time.Date(2026, 9, 11, 11, 0, 0, 0, time.Local)
	if _, err := normalizeRankingResearchRequest(RankingResearchRequest{RecType: model.RecTypeShortTerm, Horizon: 60}, now); err == nil {
		t.Fatal("不能用长线结果启用短线学习模型")
	}
	if _, err := normalizeRankingResearchRequest(RankingResearchRequest{RecType: model.RecTypeLongTerm, Horizon: 5}, now); err == nil {
		t.Fatal("不能用短线结果启用长线学习模型")
	}
	req, err := normalizeRankingResearchRequest(RankingResearchRequest{AsOf: "2026-09-11"}, now)
	if err != nil || req.AsOf != "2026-09-10" {
		t.Fatal("未收盘的当日不能作为成熟结果截止日")
	}
}
