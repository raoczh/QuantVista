package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// ---------- P1-7 校准内核纯函数手工验算 ----------

// TestCalibBrierECE Brier/ECE 手工验算：
// obs = (conf80,hit) + (conf60,miss)：
//   - Brier = ((0.8-1)² + (0.6-0)²)/2 = (0.04+0.36)/2 = 0.2
//   - ECE：桶[60,80) 1 条 acc=0 avgConf=0.6 → |0-0.6|=0.6 权重 1/2；
//     桶[80,100] 1 条 acc=1 avgConf=0.8 → |1-0.8|=0.2 权重 1/2；ECE = 0.3+0.1 = 0.4
//
// 空观测 → nil（未评估 ≠ 0 分）。
func TestCalibBrierECE(t *testing.T) {
	obs := []calibObs{{Conf: 80, Hit: true}, {Conf: 60, Hit: false}}
	if b := calibBrier(obs); b == nil || *b != 0.2 {
		t.Fatalf("Brier 应 0.2，得到 %v", b)
	}
	if e := calibECE(obs); e == nil || *e != 0.4 {
		t.Fatalf("ECE 应 0.4，得到 %v", e)
	}
	if calibBrier(nil) != nil || calibECE(nil) != nil {
		t.Fatalf("空观测应返回 nil（未评估），不得当 0 分")
	}
	// 完美校准反例：conf=100 全命中 → Brier=0、ECE=0（非 nil）。
	perfect := []calibObs{{Conf: 100, Hit: true}, {Conf: 100, Hit: true}}
	if b := calibBrier(perfect); b == nil || *b != 0 {
		t.Fatalf("完美校准 Brier 应 0，得到 %v", b)
	}
}

// TestCalibBucketIdx 桶边界：左闭右开、末桶右闭、越界钳制。
func TestCalibBucketIdx(t *testing.T) {
	cases := []struct {
		conf float64
		want int
	}{{0, 0}, {19.9, 0}, {20, 1}, {59.9, 2}, {60, 3}, {79.9, 3}, {80, 4}, {100, 4}, {-5, 0}, {120, 4}}
	for _, c := range cases {
		if got := calibBucketIdx(c.conf); got != c.want {
			t.Fatalf("conf=%v 应落桶 %d，得到 %d", c.conf, c.want, got)
		}
	}
}

// TestCalibReliability 可靠性曲线：桶聚合/命中率/Gap 手工验算，空桶不输出。
// 桶[60,80)：conf 60/70 各一条，1 命中 → avgConf=65、hit=50%、gap=-15。
func TestCalibReliability(t *testing.T) {
	obs := []calibObs{
		{Conf: 60, Hit: true, Net: 2},
		{Conf: 70, Hit: false, Net: -4},
		{Conf: 90, Hit: true, Net: 6},
	}
	buckets := calibReliability(obs)
	if len(buckets) != 2 {
		t.Fatalf("应只输出 2 个非空桶，得到 %d: %+v", len(buckets), buckets)
	}
	b60 := buckets[0]
	if b60.Label != "60-80" || b60.Sample != 2 || b60.AvgConf != 65 || b60.HitRatePct != 50 {
		t.Fatalf("60-80 桶验算失败: %+v", b60)
	}
	if b60.GapPct != -15 || b60.AvgNetPct != -1 {
		t.Fatalf("60-80 桶 gap/net 应 -15/-1: %+v", b60)
	}
	b80 := buckets[1]
	if b80.Label != "80-100" || b80.Sample != 1 || b80.HitRatePct != 100 || b80.GapPct != 10 {
		t.Fatalf("80-100 桶验算失败: %+v", b80)
	}
	if calibReliability(nil) != nil {
		t.Fatalf("空观测应返回 nil")
	}
}

// TestCalibTierMonotone 单调性描述三态：成立/不成立/样本不足——只描述不判定。
func TestCalibTierMonotone(t *testing.T) {
	mk := func(h, m, l float64, n int) []CalibTierCell {
		return []CalibTierCell{
			{Tier: "high", Sample: n, HitRatePct: h},
			{Tier: "medium", Sample: n, HitRatePct: m},
			{Tier: "low", Sample: n, HitRatePct: l},
		}
	}
	if s := calibTierMonotone(mk(70, 60, 50, 10)); s != "high ≥ medium ≥ low 命中率单调，档位区分度在当前样本上成立" {
		t.Fatalf("单调应成立: %s", s)
	}
	if s := calibTierMonotone(mk(50, 70, 60, 10)); s == "" || s == "high ≥ medium ≥ low 命中率单调，档位区分度在当前样本上成立" {
		t.Fatalf("非单调不应报成立: %s", s)
	}
	if s := calibTierMonotone(mk(70, 60, 50, calibMinBucket-1)); s != "档位样本不足，单调性暂无法判读" {
		t.Fatalf("样本不足应无法判读: %s", s)
	}
	// 缺档同样无法判读（只有 high/low 两档）。
	two := []CalibTierCell{{Tier: "high", Sample: 10, HitRatePct: 70}, {Tier: "low", Sample: 10, HitRatePct: 50}}
	if s := calibTierMonotone(two); s != "档位样本不足，单调性暂无法判读" {
		t.Fatalf("缺档应无法判读: %s", s)
	}
}

// ---------- 推荐侧端到端（DB） ----------

func cleanCalibTables(t *testing.T) {
	t.Helper()
	tables := []string{"recommendation_labels", "recommendations", "analysis_records", "daily_bars"}
	wipe := func() {
		for _, tbl := range tables {
			common.DB.Exec("DELETE FROM " + tbl)
		}
		calibCacheMu.Lock()
		calibCache = nil
		calibCacheMu.Unlock()
	}
	wipe()
	t.Cleanup(wipe)
}

// seedCalibLabel 落一条推荐标签 + 对应推荐条目。
func seedCalibLabel(t *testing.T, recID int64, action string, conf int, sysConf string,
	net float64, status string, forced bool, degraded bool) {
	t.Helper()
	detail := `{"sys_confidence":"` + sysConf + `"}`
	if degraded {
		detail = `{"sys_confidence":"` + sysConf + `","degraded_source":"quant_fallback"}`
	}
	if err := common.DB.Create(&model.Recommendation{
		ID: recID, BatchID: 1, UserID: 1, Symbol: "600000", Market: "cn",
		Action: action, Confidence: conf, DetailJSON: detail,
	}).Error; err != nil {
		t.Fatalf("seed recommendation: %v", err)
	}
	gross := net + 0.4 // 成本前收益恒高于净收益，供 gross/net 分开统计验算
	if err := common.DB.Create(&model.RecommendationLabel{
		RecommendationID: recID, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
		BatchID: 1, UserID: 1, Symbol: "600000", Market: "cn",
		Type: model.RecTypeShortTerm, Action: action,
		NetReturnPct: net, GrossReturnPct: gross, AlphaPct: net - 1, HasBench: true,
		MaturityStatus: status, Forced: forced, LabelVersion: labelVersion,
	}).Error; err != nil {
		t.Fatalf("seed label: %v", err)
	}
}

// TestRecCalibReportEndToEnd 推荐侧端到端手工验算：
// buy 6 条（high 命中 2、medium 1 命中 1 未命中、low 未命中 2）+ watch 2 条（1 命中）
// + 干扰项：forced 1、degraded 1、pending 1、影子标签（rec_id=0）、l1 旧版本、h=5 别档。
// 验算：Sample=8、buy 命中率、precision/recall、coverage、SysTiers、gross/net 分开。
func TestRecCalibReportEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)

	id := int64(0)
	next := func() int64 { id++; return id }
	// buy：high×2 命中（conf 80）、medium 命中+未命中（conf 60）、low×2 未命中（conf 40）。
	seedCalibLabel(t, next(), model.RecActionBuy, 80, "high", 5, model.LabelMatured, false, false)
	seedCalibLabel(t, next(), model.RecActionBuy, 80, "high", 3, model.LabelMatured, false, false)
	seedCalibLabel(t, next(), model.RecActionBuy, 60, "medium", 2, model.LabelMatured, false, false)
	seedCalibLabel(t, next(), model.RecActionBuy, 60, "medium", -2, model.LabelMatured, false, false)
	seedCalibLabel(t, next(), model.RecActionBuy, 40, "low", -6, model.LabelMatured, false, false)
	seedCalibLabel(t, next(), model.RecActionBuy, 40, "low", -1, model.LabelMatured, false, false)
	// watch 对照：1 命中 1 未命中。
	seedCalibLabel(t, next(), model.RecActionWatch, 50, "medium", 4, model.LabelMatured, false, false)
	seedCalibLabel(t, next(), model.RecActionWatch, 50, "low", -3, model.LabelMatured, false, false)
	// 干扰项：forced（强平剔除）、degraded（量化降级剔除）、pending。
	seedCalibLabel(t, next(), model.RecActionBuy, 90, "high", 9, model.LabelMatured, true, false)
	seedCalibLabel(t, next(), model.RecActionBuy, 35, "low", 8, model.LabelMatured, false, true)
	seedCalibLabel(t, next(), model.RecActionBuy, 70, "high", 0, model.LabelPending, false, false)
	// 影子标签（rec_id=0 事件挂载）不进本报表。
	common.DB.Create(&model.RecommendationLabel{
		RecommendationID: 0, CandidateEventID: 99, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
		Type: model.RecTypeShortTerm, Action: model.RecActionBuy, NetReturnPct: 99,
		MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
	})
	// 旧执行语义 l1 不混池。
	rid := next()
	common.DB.Create(&model.Recommendation{ID: rid, BatchID: 1, UserID: 1, Action: model.RecActionBuy, Confidence: 80, DetailJSON: `{"sys_confidence":"high"}`})
	common.DB.Create(&model.RecommendationLabel{
		RecommendationID: rid, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
		Type: model.RecTypeShortTerm, Action: model.RecActionBuy, NetReturnPct: 50,
		MaturityStatus: model.LabelMatured, LabelVersion: "l1",
	})
	// 别的 horizon（h=5）不进短线代表持有期报表。
	rid = next()
	common.DB.Create(&model.Recommendation{ID: rid, BatchID: 1, UserID: 1, Action: model.RecActionBuy, Confidence: 80, DetailJSON: `{"sys_confidence":"high"}`})
	common.DB.Create(&model.RecommendationLabel{
		RecommendationID: rid, HorizonDays: 5, EntryMode: model.EntryModeNextOpen,
		Type: model.RecTypeShortTerm, Action: model.RecActionBuy, NetReturnPct: 50,
		MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
	})

	rep, err := buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("buildRecCalibReport: %v", err)
	}
	// coverage：total=11（l2/next_open/rec_id>0/h10/short），matured=10、pending=1、
	// forced=1、degraded=1 → 进统计 8。
	if rep.Coverage.Total != 11 || rep.Coverage.Matured != 10 || rep.Coverage.Pending != 1 {
		t.Fatalf("coverage 计数错: %+v", rep.Coverage)
	}
	if rep.Coverage.Forced != 1 || rep.Coverage.DegradedExcl != 1 {
		t.Fatalf("forced/degraded 剔除计数错: %+v", rep.Coverage)
	}
	if rep.Sample != 8 || rep.BuySample != 6 {
		t.Fatalf("样本应 8（buy 6），得到 %d/%d", rep.Sample, rep.BuySample)
	}
	if rep.Evaluated {
		t.Fatalf("样本 8 < %d 不应标已评估", calibMinSample)
	}
	if rep.Brier != nil || rep.ECE != nil {
		t.Fatalf("样本不足 Brier/ECE 应 nil（未评估≠0 分）")
	}
	// precision = buy 命中 3/6 = 50%；recall = buy 命中 3 / 全部命中 4 = 75%；watch 命中 1/2=50%。
	if rep.ActionPR.PrecisionNetPct == nil || *rep.ActionPR.PrecisionNetPct != 50 {
		t.Fatalf("precision 应 50: %+v", rep.ActionPR)
	}
	if rep.ActionPR.RecallNetPct == nil || *rep.ActionPR.RecallNetPct != 75 {
		t.Fatalf("recall 应 75: %+v", rep.ActionPR)
	}
	if rep.ActionPR.WatchHitPct == nil || *rep.ActionPR.WatchHitPct != 50 {
		t.Fatalf("watch 命中率应 50: %+v", rep.ActionPR)
	}
	// SysTiers（buy 限定）：high 2 条全命中 avgNet=4 avgGross=4.4；medium 1/2；low 0/2。
	if len(rep.SysTiers) != 3 {
		t.Fatalf("应 3 档，得到 %+v", rep.SysTiers)
	}
	high := rep.SysTiers[0]
	if high.Tier != "high" || high.Sample != 2 || high.HitRatePct != 100 || high.AvgNetPct != 4 {
		t.Fatalf("high 档验算失败: %+v", high)
	}
	if high.AvgGrossPct != 4.4 {
		t.Fatalf("high 档成本前收益应 4.4（gross/net 分开统计）: %+v", high)
	}
	low := rep.SysTiers[2]
	if low.Tier != "low" || low.Sample != 2 || low.HitRatePct != 0 || low.SevereLossPct != 50 {
		t.Fatalf("low 档验算失败: %+v", low)
	}
	// 可靠性曲线仍输出（样本不足只是不出 Brier/ECE 汇总值）：
	// conf40×2 miss、conf60×2 一中一失、conf80×2 全中。
	if len(rep.Reliability) != 3 {
		t.Fatalf("可靠性曲线应 3 桶: %+v", rep.Reliability)
	}
	if rep.Reliability[2].Label != "80-100" || rep.Reliability[2].HitRatePct != 100 {
		t.Fatalf("80-100 桶应全命中: %+v", rep.Reliability[2])
	}
}

// TestRecCalibReportEvaluatedThreshold 「已评估」硬门槛（审查修复批统一为 buy 校准样本
// ≥ calibEvalMinSample=100，与 Brier/ECE 产出同门槛）：100 条 buy conf=80 全命中 →
// evaluated=true、Brier=(0.8-1)²=0.04、ECE=|1-0.8|=0.2；30 条（旧分级门槛）只算分级
// 参考，evaluated=false 且 Brier/ECE 为 nil（未评估≠0 分）。
func TestRecCalibReportEvaluatedThreshold(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)
	for i := 1; i <= calibMinSample; i++ {
		seedCalibLabel(t, int64(i), model.RecActionBuy, 80, "high", 5, model.LabelMatured, false, false)
	}
	rep, err := buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("buildRecCalibReport: %v", err)
	}
	if rep.Evaluated || rep.Brier != nil || rep.ECE != nil {
		t.Fatalf("样本 %d < %d 不得标已评估或产出 Brier/ECE: eval=%v brier=%v", rep.Sample, calibEvalMinSample, rep.Evaluated, rep.Brier)
	}
	for i := calibMinSample + 1; i <= calibEvalMinSample; i++ {
		seedCalibLabel(t, int64(i), model.RecActionBuy, 80, "high", 5, model.LabelMatured, false, false)
	}
	rep, err = buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("buildRecCalibReport: %v", err)
	}
	if !rep.Evaluated || rep.Sample != calibEvalMinSample {
		t.Fatalf("样本 %d 应标已评估: %+v", rep.Sample, rep.Evaluated)
	}
	if rep.Brier == nil || *rep.Brier != 0.04 {
		t.Fatalf("Brier 应 0.04，得到 %v", rep.Brier)
	}
	if rep.ECE == nil || *rep.ECE != 0.2 {
		t.Fatalf("ECE 应 0.2，得到 %v", rep.ECE)
	}
	if rep.TierMonotone != "档位样本不足，单调性暂无法判读" {
		t.Fatalf("只有 high 档应无法判读单调性: %s", rep.TierMonotone)
	}
}

// ---------- 分析侧端到端（DB） ----------

// seedCalibAnalysis 落一条个股标准分析记录。
func seedCalibAnalysis(t *testing.T, symbol, rating string, conf int, sysConf, mode string) int64 {
	t.Helper()
	resultJSON := `{"rating":"` + rating + `","sys_confidence":"` + sysConf + `"}`
	if sysConf == "" {
		resultJSON = `{"rating":"` + rating + `"}`
	}
	rec := &model.AnalysisRecord{
		UserID: 1, Module: model.AnalysisModuleStock, Market: "cn", Symbol: symbol,
		Status: model.AnalysisStatusSuccess, Mode: mode, Rating: rating, Confidence: conf,
		ResultJSON: resultJSON,
	}
	if err := common.DB.Create(rec).Error; err != nil {
		t.Fatalf("seed analysis: %v", err)
	}
	// created_at 固定为基准日 2026-06-01（AutoMigrate 的 CreatedAt 默认 now，显式改写）。
	common.DB.Model(rec).Update("created_at", time.Date(2026, 6, 1, 10, 0, 0, 0, time.Local))
	return rec.ID
}

// seedCalibBars 给标的落基准日 + 后续 n 根日线（线性走势，direction=+1 涨/-1 跌）。
func seedCalibBars(t *testing.T, symbol string, n int, direction float64) {
	t.Helper()
	base := 10.0
	rows := []model.DailyBar{{Market: "cn", Symbol: symbol, TradeDate: "2026-06-01", Close: base, Open: base, High: base, Low: base}}
	d := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	for i := 1; i <= n; i++ {
		price := base + direction*0.1*float64(i)
		td := d.AddDate(0, 0, i).Format("2006-01-02")
		rows = append(rows, model.DailyBar{Market: "cn", Symbol: symbol, TradeDate: td, Close: price, Open: price, High: price, Low: price})
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatalf("seed bars: %v", err)
	}
}

// TestAnalysisCalibReportEndToEnd 分析侧端到端：
// bullish+涨=命中(high)、bullish+跌=未命中(low)、bearish+跌=命中(无 sys_conf→未知档)、
// neutral 跳过、无日线跳过、不足 20 根跳过、panel 不扫描。
func TestAnalysisCalibReportEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)

	seedCalibBars(t, "600001", 25, 1)  // 涨
	seedCalibBars(t, "600002", 25, -1) // 跌
	seedCalibBars(t, "600003", 25, -1) // 跌
	seedCalibBars(t, "600004", 5, 1)   // 不足 20 根

	seedCalibAnalysis(t, "600001", model.AnalysisRatingBullish, 80, "high", "")      // 命中
	seedCalibAnalysis(t, "600002", model.AnalysisRatingBullish, 60, "low", "")       // 未命中
	seedCalibAnalysis(t, "600003", model.AnalysisRatingBearish, 70, "", "")          // 命中，无 sys_conf
	seedCalibAnalysis(t, "600001", model.AnalysisRatingNeutral, 50, "high", "")      // neutral 跳过
	seedCalibAnalysis(t, "688888", model.AnalysisRatingBullish, 50, "high", "")      // 无日线跳过
	seedCalibAnalysis(t, "600004", model.AnalysisRatingBullish, 50, "high", "")      // 不足 20 根跳过
	seedCalibAnalysis(t, "600001", model.AnalysisRatingBullish, 50, "high", "panel") // panel 不扫

	rep, err := buildAnalysisCalibReport(context.Background())
	if err != nil {
		t.Fatalf("buildAnalysisCalibReport: %v", err)
	}
	if rep.Scanned != 6 {
		t.Fatalf("应扫描 6 条（panel 不进查询），得到 %d", rep.Scanned)
	}
	if rep.Judged != 3 || rep.NeutralSkipped != 1 || rep.NoDataSkipped != 1 || rep.ImmatureSkipped != 1 {
		t.Fatalf("judged/skip 计数错: %+v", rep)
	}
	if rep.NoSysConf != 1 {
		t.Fatalf("无 sys_confidence 应计 1: %d", rep.NoSysConf)
	}
	if rep.Evaluated {
		t.Fatalf("3 条 < %d 不应标已评估", calibMinSample)
	}
	// 档位：high 1 命中、low 1 未命中、（未知）1 命中。
	if len(rep.SysTiers) != 3 {
		t.Fatalf("应 3 档（high/low/未知）: %+v", rep.SysTiers)
	}
	if rep.SysTiers[0].Tier != "high" || rep.SysTiers[0].HitRatePct != 100 {
		t.Fatalf("high 档应 100%% 命中: %+v", rep.SysTiers[0])
	}
	if rep.SysTiers[1].Tier != "low" || rep.SysTiers[1].HitRatePct != 0 {
		t.Fatalf("low 档应 0%% 命中: %+v", rep.SysTiers[1])
	}
	if rep.SysTiers[2].Tier != "（未知）" || rep.SysTiers[2].Sample != 1 {
		t.Fatalf("未知档应 1 条: %+v", rep.SysTiers[2])
	}
	// bearish 命中方向验证：600003 跌、rating=bearish → hit（可靠性曲线 60-80 桶 2 条全命中）。
	found := false
	for _, b := range rep.Reliability {
		if b.Label == "60-80" {
			found = true
			if b.Sample != 2 || b.HitRatePct != 50 {
				t.Fatalf("60-80 桶应 2 条 50%%（conf60 未命中+conf70 命中）: %+v", b)
			}
		}
	}
	if !found {
		t.Fatalf("应有 60-80 桶: %+v", rep.Reliability)
	}
}

// TestRunLLMCalibrationCacheAndShape 总入口：跑通、缓存生效、报表形态完整。
func TestRunLLMCalibrationCacheAndShape(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)
	seedCalibLabel(t, 1, model.RecActionBuy, 80, "high", 5, model.LabelMatured, false, false)

	rep, err := RunLLMCalibration(context.Background())
	if err != nil {
		t.Fatalf("RunLLMCalibration: %v", err)
	}
	if len(rep.Recommendation) != 2 {
		t.Fatalf("应含短线+长线两组: %d", len(rep.Recommendation))
	}
	if rep.Recommendation[0].Type != model.RecTypeShortTerm || rep.Recommendation[0].HorizonDays != 10 {
		t.Fatalf("第一组应短线 h=10: %+v", rep.Recommendation[0])
	}
	if rep.Recommendation[1].Type != model.RecTypeLongTerm || rep.Recommendation[1].HorizonDays != 20 {
		t.Fatalf("第二组应长线 h=20: %+v", rep.Recommendation[1])
	}
	if rep.Analysis == nil {
		t.Fatalf("应含分析侧报表")
	}
	if rep.LabelVersion != labelVersion {
		t.Fatalf("应声明标签口径版本 %s: %s", labelVersion, rep.LabelVersion)
	}
	if CachedLLMCalibrationReport() != rep {
		t.Fatalf("缓存应返回同一报表")
	}
}
