package service

import (
	"context"
	"strings"
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
	tables := []string{"recommendation_labels", "recommendation_candidate_events", "recommendations", "analysis_records", "daily_bars"}
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

// ---------- 第五十六批②：原始置信度双口径 ----------

// TestSnapshotRawConfidence 快照点语义：复核改写前同时快照动作与置信度；已有快照
// 幂等不覆盖；与 applyReviews 组合后最终 buy→watch，而原始快照保持 buy/原置信度。
func TestSnapshotRawConfidence(t *testing.T) {
	picks := []recPick{
		{Symbol: "600000", Action: model.RecActionBuy, Confidence: 85},
		{Symbol: "000001", Action: model.RecActionBuy, Confidence: 70},
	}
	snapshotRawConfidence(picks)
	if picks[0].RawConfidence == nil || *picks[0].RawConfidence != 85 {
		t.Fatalf("快照应记录复核前置信度: %+v", picks[0].RawConfidence)
	}
	if picks[0].RawAction == nil || *picks[0].RawAction != model.RecActionBuy {
		t.Fatalf("快照应记录复核前动作: %+v", picks[0].RawAction)
	}
	// 幂等：二次调用不覆盖（防 repair 重入等异常路径把改写后值误当原始值）。
	picks[0].Confidence = 25
	picks[0].Action = model.RecActionWatch
	snapshotRawConfidence(picks)
	if *picks[0].RawConfidence != 85 || *picks[0].RawAction != model.RecActionBuy {
		t.Fatalf("已有快照不得被二次快照覆盖: action=%s confidence=%d", *picks[0].RawAction, *picks[0].RawConfidence)
	}
	// 与复核级联组合：reject 后最终 watch/25，原始 buy/70 仍在。
	out := applyReviews(picks, []pickReview{{Symbol: "000001", Verdict: "reject", Confidence: 0}})
	if out[1].Action != model.RecActionWatch || out[1].Confidence != 25 ||
		out[1].RawAction == nil || *out[1].RawAction != model.RecActionBuy ||
		out[1].RawConfidence == nil || *out[1].RawConfidence != 70 {
		t.Fatalf("reject 后应为最终 watch/25、原始 buy/70: %+v", out[1])
	}
	// 模型自附 raw_action/raw_confidence 均剥除（服务端快照字段不可伪造）。
	fake := 99
	fakeAction := model.RecActionWatch
	p := normalizePick(recPick{Action: model.RecActionBuy, Confidence: 60, RawAction: &fakeAction, RawConfidence: &fake},
		"600036", candidate{Symbol: "600036", Price: 10})
	if p.RawAction != nil || p.RawConfidence != nil {
		t.Fatalf("normalizePick 应剥除模型自附 raw 字段: action=%v confidence=%v", p.RawAction, p.RawConfidence)
	}
}

// TestCalibRawSummary 双口径分列验算：有快照（diverged/一致）、无快照（missing）、
// 原始 watch 不进；门槛与主口径一致（≥ calibEvalMinSample 才产出 Brier/ECE）。
func TestCalibRawSummary(t *testing.T) {
	iptr := func(v int) *int { return &v }
	sptr := func(v string) *string { return &v }
	mk := func(action string, rawAction *string, conf int, raw *int, hit bool) calibSample {
		net := -1.0
		if hit {
			net = 1
		}
		return calibSample{
			label: model.RecommendationLabel{Action: action, NetReturnPct: net},
			meta:  calibRecMeta{conf: conf, rawAction: rawAction, rawConf: raw},
		}
	}
	samples := []calibSample{
		mk(model.RecActionWatch, sptr(model.RecActionBuy), 25, iptr(85), false), // 真实 reject：原始 buy→最终 watch
		mk(model.RecActionBuy, sptr(model.RecActionBuy), 70, iptr(70), true),    // 未改写：一致
		mk(model.RecActionBuy, nil, 80, nil, true),                              // 旧记录：按最终 buy 回退，confidence missing
		mk(model.RecActionWatch, sptr(model.RecActionBuy), 25, nil, false),      // 历史 reject：动作可回填、置信度缺失
		mk(model.RecActionBuy, sptr(model.RecActionWatch), 60, iptr(60), true),  // 原始 watch：即使最终 buy 也不进
	}
	raw := calibRawSummary(samples)
	if raw.Sample != 2 || raw.Missing != 2 || raw.Diverged != 2 {
		t.Fatalf("分列计数不符: %+v", raw)
	}
	if raw.Brier != nil || raw.ECE != nil {
		t.Fatalf("样本 2 < %d 不得产出 Brier/ECE: %+v", calibEvalMinSample, raw)
	}
	// 达门槛：100 条 raw=80 全命中 → Brier=(0.8-1)²=0.04、ECE=0.2（终值 conf=25 全部
	// diverged——原始口径的 Brier 不吃终值，这正是双口径要测的分界）。
	big := make([]calibSample, 0, calibEvalMinSample)
	for i := 0; i < calibEvalMinSample; i++ {
		big = append(big, mk(model.RecActionWatch, sptr(model.RecActionBuy), 25, iptr(80), true))
	}
	raw = calibRawSummary(big)
	if raw.Sample != calibEvalMinSample || raw.Diverged != calibEvalMinSample {
		t.Fatalf("达门槛计数不符: %+v", raw)
	}
	if raw.Brier == nil || *raw.Brier != 0.04 || raw.ECE == nil || *raw.ECE != 0.2 {
		t.Fatalf("原始口径应按快照值算 Brier=0.04/ECE=0.2: brier=%v ece=%v", raw.Brier, raw.ECE)
	}
}

// TestRecCalibReportRawCalibEndToEnd DB 端到端：真实 buy→reject 以最终 watch 落标签，
// DetailJSON 保留 raw_action/raw_confidence；并验证旧记录无 raw 字段的回退兼容。
func TestRecCalibReportRawCalibEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)
	seed := func(recID int64, action string, conf int, detail string, net float64) {
		if err := common.DB.Create(&model.Recommendation{
			ID: recID, BatchID: 1, UserID: 1, Symbol: "600000", Market: "cn",
			Action: action, Confidence: conf, DetailJSON: detail,
		}).Error; err != nil {
			t.Fatalf("seed rec: %v", err)
		}
		if err := common.DB.Create(&model.RecommendationLabel{
			RecommendationID: recID, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
			Type: model.RecTypeShortTerm, Action: action,
			NetReturnPct: net, GrossReturnPct: net + 0.4,
			MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
		}).Error; err != nil {
			t.Fatalf("seed label: %v", err)
		}
	}
	seed(1, model.RecActionWatch, 25, `{"sys_confidence":"low","raw_action":"buy","raw_confidence":85}`, -2)

	// 全部原始 buy 均被 reject 时最终 buy 池为空，raw_calib 仍必须输出。
	rep, err := buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("build reject-only report: %v", err)
	}
	if rep.BuySample != 0 || rep.RawCalib == nil || rep.RawCalib.Sample != 1 || rep.RawCalib.Diverged != 1 {
		t.Fatalf("reject-only 仍应保留原始 buy 校准: %+v", rep)
	}

	seed(2, model.RecActionBuy, 70, `{"sys_confidence":"high","raw_action":"buy","raw_confidence":70}`, 3)
	seed(3, model.RecActionBuy, 80, `{"sys_confidence":"high"}`, 3) // 旧记录无快照：按最终 buy 回退

	rep, err = buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("buildRecCalibReport: %v", err)
	}
	if rep.RawCalib == nil {
		t.Fatalf("buy 样本非空应输出 raw_calib 分列")
	}
	if rep.RawCalib.Sample != 2 || rep.RawCalib.Missing != 1 || rep.RawCalib.Diverged != 1 {
		t.Fatalf("raw_calib 分列不符: %+v", rep.RawCalib)
	}
	if rep.RawCalib.Brier != nil {
		t.Fatalf("快照样本 2 < 门槛不得产出原始口径 Brier: %+v", rep.RawCalib)
	}
	// raw_confidence=0 是合法值域（非 missing）：终值 0 vs 原始 0 一致。
	seed(4, model.RecActionBuy, 0, `{"sys_confidence":"low","raw_action":"buy","raw_confidence":0}`, -1)
	rep, err = buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rep.RawCalib.Sample != 3 || rep.RawCalib.Missing != 1 || rep.RawCalib.Diverged != 1 {
		t.Fatalf("raw_confidence=0 应计入快照样本非 missing: %+v", rep.RawCalib)
	}
	// Notes 应声明双口径（旧「口径限制」声明已换）。
	joined := ""
	for _, n := range rep.Notes {
		joined += n + "\n"
	}
	if !strings.Contains(joined, "双口径") || strings.Contains(joined, "口径限制（如实声明）") {
		t.Fatalf("Notes 应换双口径声明: %s", joined)
	}
}

// TestRecCalibReportLegacyRawActionFromPickedEvent 历史记录只有 raw_confidence、没有
// raw_action 时，以同 batch+symbol 的 picked 事件 RawAction 还原复核前动作。
func TestRecCalibReportLegacyRawActionFromPickedEvent(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)

	const (
		recID   int64 = 101
		batchID int64 = 77
		symbol        = "600777"
	)
	if err := common.DB.Create(&model.Recommendation{
		ID: recID, BatchID: batchID, UserID: 1, Symbol: symbol, Market: "cn",
		Action: model.RecActionWatch, Confidence: 25,
		DetailJSON: `{"sys_confidence":"low","raw_confidence":85}`,
	}).Error; err != nil {
		t.Fatalf("seed legacy recommendation: %v", err)
	}
	if err := common.DB.Create(&model.RecommendationLabel{
		RecommendationID: recID, BatchID: batchID, UserID: 1, Symbol: symbol, Market: "cn",
		HorizonDays: 10, EntryMode: model.EntryModeNextOpen, Type: model.RecTypeShortTerm,
		Action: model.RecActionWatch, NetReturnPct: -2, GrossReturnPct: -1.6,
		MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
	}).Error; err != nil {
		t.Fatalf("seed legacy label: %v", err)
	}
	events := []model.RecommendationCandidateEvent{
		// 同 symbol 不同 batch 不能串到目标推荐。
		{BatchID: batchID - 1, UserID: 1, Symbol: symbol, Market: "cn", CandidateStage: model.CandStagePicked, RawAction: model.RecActionWatch},
		// 同 batch+symbol 但非 picked 也不能作为原动作来源。
		{BatchID: batchID, UserID: 1, Symbol: symbol, Market: "cn", CandidateStage: model.CandStageLLMList, RawAction: model.RecActionWatch},
		{BatchID: batchID, UserID: 1, Symbol: symbol, Market: "cn", CandidateStage: model.CandStagePicked,
			RawAction: model.RecActionBuy, PostGateAction: model.RecActionWatch},
	}
	if err := common.DB.Create(&events).Error; err != nil {
		t.Fatalf("seed candidate events: %v", err)
	}

	rep, err := buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("build legacy report: %v", err)
	}
	if rep.BuySample != 0 {
		t.Fatalf("最终 watch 不应进入主 buy 校准，得到 %d", rep.BuySample)
	}
	if rep.RawCalib == nil || rep.RawCalib.Sample != 1 || rep.RawCalib.Missing != 0 || rep.RawCalib.Diverged != 1 {
		t.Fatalf("历史原始 buy 应由 picked 事件回填并计入 raw 校准: %+v", rep.RawCalib)
	}

	// 权威 picked 事件也缺失时维持旧行为：回退最终 watch，不硬造原始 buy。
	if err := common.DB.Delete(&model.RecommendationCandidateEvent{}, events[2].ID).Error; err != nil {
		t.Fatalf("delete target picked event: %v", err)
	}
	rep, err = buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("rebuild legacy report without picked event: %v", err)
	}
	if rep.RawCalib != nil {
		t.Fatalf("缺 picked 事件时应回退最终 watch，不得伪造 raw buy: %+v", rep.RawCalib)
	}
}

// TestRecCalibReportLegacyActionDivergesWithoutRawConfidence 覆盖更早期的历史记录：
// picked 事件能还原原始 buy，但 DetailJSON 没有 raw_confidence。它应同时计入 missing
// 与动作 diverged，不能因为缺置信度就漏掉复核改写事实。
func TestRecCalibReportLegacyActionDivergesWithoutRawConfidence(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)

	const (
		recID   int64 = 102
		batchID int64 = 78
		symbol        = "600778"
	)
	if err := common.DB.Create(&model.Recommendation{
		ID: recID, BatchID: batchID, UserID: 1, Symbol: symbol, Market: "cn",
		Action: model.RecActionWatch, Confidence: 25, DetailJSON: `{"sys_confidence":"low"}`,
	}).Error; err != nil {
		t.Fatalf("seed legacy recommendation: %v", err)
	}
	if err := common.DB.Create(&model.RecommendationLabel{
		RecommendationID: recID, BatchID: batchID, UserID: 1, Symbol: symbol, Market: "cn",
		HorizonDays: 10, EntryMode: model.EntryModeNextOpen, Type: model.RecTypeShortTerm,
		Action: model.RecActionWatch, NetReturnPct: -2, GrossReturnPct: -1.6,
		MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
	}).Error; err != nil {
		t.Fatalf("seed legacy label: %v", err)
	}
	if err := common.DB.Create(&model.RecommendationCandidateEvent{
		BatchID: batchID, UserID: 1, Symbol: symbol, Market: "cn", CandidateStage: model.CandStagePicked,
		RawAction: model.RecActionBuy, PostGateAction: model.RecActionWatch,
	}).Error; err != nil {
		t.Fatalf("seed picked event: %v", err)
	}

	rep, err := buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("build legacy report: %v", err)
	}
	if rep.RawCalib == nil || rep.RawCalib.Sample != 0 || rep.RawCalib.Missing != 1 || rep.RawCalib.Diverged != 1 {
		t.Fatalf("缺 raw confidence 仍应记录动作分歧: %+v", rep.RawCalib)
	}
}
