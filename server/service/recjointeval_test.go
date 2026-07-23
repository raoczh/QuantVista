package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// P2-5 组合/回测联合评估测试（recjointeval.go）：切分/净值回撤/换手纯函数手工验算 +
// 锁定段纪律（默认不出指标、显式请求登记审计、版本对照只吃 dev 段）+ DB 端到端。

func TestJointSplitDays(t *testing.T) {
	// 15 天：cut=int(15*0.7)=10 → dev 10 / locked 5。
	days := make([]string, 0, 15)
	for i := 1; i <= 15; i++ {
		days = append(days, time.Date(2026, 6, i, 0, 0, 0, 0, time.Local).Format("2006-01-02"))
	}
	dev, locked := jointSplitDays(days)
	if len(dev) != 10 || len(locked) != 5 {
		t.Fatalf("15 天应切 10/5，得到 %d/%d", len(dev), len(locked))
	}
	if dev[len(dev)-1] >= locked[0] {
		t.Fatalf("dev 段末日必须早于 locked 段首日: %s vs %s", dev[len(dev)-1], locked[0])
	}
	// 时间切分不随机：乱序输入排序后同一结果。
	shuffled := []string{days[8], days[2], days[14], days[0], days[5], days[1], days[3],
		days[4], days[6], days[7], days[9], days[10], days[11], days[12], days[13]}
	dev2, locked2 := jointSplitDays(shuffled)
	if strings.Join(dev2, ",") != strings.Join(dev, ",") || strings.Join(locked2, ",") != strings.Join(locked, ",") {
		t.Fatalf("乱序输入切分应一致")
	}
	// 不足 jointMinSplitDays 不切（locked 如实缺席）。
	dev3, locked3 := jointSplitDays(days[:9])
	if len(dev3) != 9 || len(locked3) != 0 {
		t.Fatalf("9 天不应切分，得到 %d/%d", len(dev3), len(locked3))
	}
	// 恰好 10 天：cut=7 → 7/3。
	dev4, locked4 := jointSplitDays(days[:10])
	if len(dev4) != 7 || len(locked4) != 3 {
		t.Fatalf("10 天应切 7/3，得到 %d/%d", len(dev4), len(locked4))
	}
}

func TestJointNavStats(t *testing.T) {
	// 手工验算：[+10, -5, +2] → nav=1.1×0.95×1.02=1.0659（+6.59%）；
	// 峰值 1.1，谷 1.045 → 最大回撤 (1.1-1.045)/1.1=5%。
	ret, dd := jointNavStats([]float64{10, -5, 2})
	if ret != 6.59 || dd != 5 {
		t.Fatalf("净值验算失败: ret=%v dd=%v（期望 6.59/5）", ret, dd)
	}
	// 单调上涨无回撤。
	ret, dd = jointNavStats([]float64{1, 2, 3})
	if dd != 0 || ret <= 0 {
		t.Fatalf("单调上涨不应有回撤: ret=%v dd=%v", ret, dd)
	}
	// 空序列零值。
	if ret, dd = jointNavStats(nil); ret != 0 || dd != 0 {
		t.Fatalf("空序列应零值: %v/%v", ret, dd)
	}
}

func TestJointTurnoverStats(t *testing.T) {
	mk := func(user, batch int64, date string, syms ...string) jointBatchPicks {
		set := map[string]bool{}
		for _, s := range syms {
			set[s] = true
		}
		return jointBatchPicks{UserID: user, BatchID: batch, SignalDate: date, Buys: set}
	}
	// user1：A{600,601,602}→B{600,603}：新进 1/2=50%、重合 1/4=25%。
	// user2 单批不成对；user3 空名单批次跳过。
	got := jointTurnoverStats([]jointBatchPicks{
		mk(1, 11, "2026-06-01", "600000", "600001", "600002"),
		mk(1, 12, "2026-06-02", "600000", "600003"),
		mk(2, 21, "2026-06-01", "600100"),
		mk(3, 31, "2026-06-01"),
		mk(3, 32, "2026-06-02", "600200"),
	})
	if got.Pairs != 1 || got.AvgNewPct != 50 || got.AvgOverlapPct != 25 {
		t.Fatalf("换手验算失败: %+v（期望 pairs=1 new=50 overlap=25）", got)
	}
	// 同用户三批：A→B（上面 50/25）+ B{600,603}→C{603}：新进 0/1=0%、重合 1/2=50%。
	// 均值 new=(50+0)/2=25、overlap=(25+50)/2=37.5。
	got = jointTurnoverStats([]jointBatchPicks{
		mk(1, 11, "2026-06-01", "600000", "600001", "600002"),
		mk(1, 12, "2026-06-02", "600000", "600003"),
		mk(1, 13, "2026-06-03", "600003"),
	})
	if got.Pairs != 2 || got.AvgNewPct != 25 || got.AvgOverlapPct != 37.5 {
		t.Fatalf("三批换手验算失败: %+v", got)
	}
}

// jointSeedLabel 联合评估端到端的标签种子（带 signal_date/batch_id 维度；
// 复用 seedCalibLabel 的 detail 形态但独立控制批次与日期）。
func jointSeedLabel(t *testing.T, recID, batchID int64, signalDate, action string, conf int,
	net float64, mae float64, status string) {
	t.Helper()
	if err := common.DB.Create(&model.Recommendation{
		ID: recID, BatchID: batchID, UserID: 1, Symbol: "60" + signalDate[8:] + "0", Market: "cn",
		Action: action, Confidence: conf, DetailJSON: `{"sys_confidence":"high"}`,
	}).Error; err != nil {
		t.Fatalf("seed recommendation: %v", err)
	}
	if err := common.DB.Create(&model.RecommendationLabel{
		RecommendationID: recID, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
		BatchID: batchID, UserID: 1, Symbol: "60" + signalDate[8:] + "0", Market: "cn",
		Type: model.RecTypeShortTerm, Action: action, SignalDate: signalDate,
		NetReturnPct: net, GrossReturnPct: net + 0.4, AlphaPct: net - 1, HasBench: true,
		MaePct: mae, MaturityStatus: status, LabelVersion: labelVersion,
	}).Error; err != nil {
		t.Fatalf("seed label: %v", err)
	}
}

func cleanJointTables(t *testing.T) {
	t.Helper()
	wipe := func() {
		for _, tbl := range []string{"recommendation_labels", "recommendations", "recommendation_batches", "options"} {
			common.DB.Exec("DELETE FROM " + tbl)
		}
		jointCacheMu.Lock()
		jointCache = nil
		jointCacheMu.Unlock()
	}
	wipe()
	t.Cleanup(wipe)
}

// TestJointEvalEndToEnd DB 端到端：12 个信号日（dev 8/locked 4 → cut=int(12*0.7)=8）；
// 锁定段默认只有 preview 无指标；include_locked=1 出指标并登记审计；版本对照只吃 dev 段
// （locked 段专属 prompt 版本不得出现在对照表）；换手/成本拖累/净值回撤验算。
func TestJointEvalEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanJointTables(t)

	// 两个批次归因（dev 段 p13 / locked 段 p13-custom.deadbeef）——版本对照隔离的探针。
	common.DB.Create(&model.RecommendationBatch{ID: 101, UserID: 1, Type: model.RecTypeShortTerm,
		Provider: "openai", Model: "gpt-x", PromptVersion: "p13", Status: model.RecStatusSuccess})
	common.DB.Create(&model.RecommendationBatch{ID: 102, UserID: 1, Type: model.RecTypeShortTerm,
		Provider: "openai", Model: "gpt-x", PromptVersion: "p13-custom.deadbeef", Status: model.RecStatusSuccess})

	id := int64(0)
	next := func() int64 { id++; return id }
	// 12 个信号日各 1 条 buy：01~08 归 dev（净收益 +2/-1 交替），09~12 归 locked（全 +5）。
	for i := 1; i <= 12; i++ {
		date := time.Date(2026, 6, i, 0, 0, 0, 0, time.Local).Format("2006-01-02")
		batch := int64(101)
		net := 2.0
		if i%2 == 0 {
			net = -1
		}
		if i > 8 {
			batch = 102
			net = 5
		}
		jointSeedLabel(t, next(), batch, date, model.RecActionBuy, 80, net, -2, model.LabelMatured)
	}
	// 干扰：pending 不进段样本、watch 不进收益。
	jointSeedLabel(t, next(), 101, "2026-06-01", model.RecActionBuy, 70, 9, -1, model.LabelPending)
	jointSeedLabel(t, next(), 101, "2026-06-02", model.RecActionWatch, 50, 9, -1, model.LabelMatured)

	rep, err := RunJointEval(false)
	if err != nil {
		t.Fatalf("RunJointEval: %v", err)
	}
	var sec *JointEvalSection
	for _, s := range rep.Sections {
		if s.Type == model.RecTypeShortTerm {
			sec = s
		}
	}
	if sec == nil || sec.Dev == nil {
		t.Fatalf("短线段应存在且有 dev 段: %+v", sec)
	}
	// dev 段：8 个信号日、buy 8 条（+2×4、-1×4）+ watch 1 → Sample=9。
	if sec.Dev.SignalDays != 8 || sec.Dev.BuySample != 8 || sec.Dev.Sample != 9 {
		t.Fatalf("dev 段样本不符: %+v", sec.Dev)
	}
	if sec.Dev.WinRatePct != 50 || sec.Dev.AvgNetPct != 0.5 {
		t.Fatalf("dev 收益验算失败（期望 win 50%%、avg 0.5）: %+v", sec.Dev)
	}
	// 成本拖累恒 0.4（gross=net+0.4 种子）。
	if sec.Dev.CostDragPct != 0.4 {
		t.Fatalf("成本拖累应 0.4: %+v", sec.Dev)
	}
	if sec.Dev.AvgMaePct != -2 || sec.Dev.WorstMaePct != -2 {
		t.Fatalf("MAE 验算失败: %+v", sec.Dev)
	}
	// 锁定段：默认只有 preview（4 天/4 样本），无指标。
	if sec.Locked != nil {
		t.Fatalf("默认不得计算锁定段指标")
	}
	if sec.LockedPreview == nil || sec.LockedPreview.SignalDays != 4 || sec.LockedPreview.Sample != 4 {
		t.Fatalf("锁定段 preview 不符: %+v", sec.LockedPreview)
	}
	// 版本对照只吃 dev：p13 在、locked 段专属 p13-custom.deadbeef 不得出现。
	var pvGroup *CalibSliceGroup
	for i := range sec.Slices {
		if sec.Slices[i].Dim == "prompt_version" {
			pvGroup = &sec.Slices[i]
		}
	}
	if pvGroup == nil || len(pvGroup.Rows) != 1 || pvGroup.Rows[0].Key != "p13" || pvGroup.Rows[0].Sample != 8 {
		t.Fatalf("版本对照应只含 dev 段 p13×8: %+v", pvGroup)
	}
	// 审计：未读锁定段 count=0（响应不带 locked_audit 或 count=0）。
	if rep.LockedAudit != nil && rep.LockedAudit.Count != 0 {
		t.Fatalf("未读过锁定段不应有审计计数: %+v", rep.LockedAudit)
	}

	// include_locked=1：出指标 + 审计 +1；恒重算不写缓存。
	rep2, err := RunJointEval(true)
	if err != nil {
		t.Fatalf("RunJointEval(locked): %v", err)
	}
	var sec2 *JointEvalSection
	for _, s := range rep2.Sections {
		if s.Type == model.RecTypeShortTerm {
			sec2 = s
		}
	}
	if sec2.Locked == nil || sec2.Locked.BuySample != 4 || sec2.Locked.WinRatePct != 100 {
		t.Fatalf("锁定段指标不符（4 条全 +5）: %+v", sec2.Locked)
	}
	if rep2.LockedAudit == nil || rep2.LockedAudit.Count != 1 || len(rep2.LockedAudit.Log) != 1 {
		t.Fatalf("锁定段读取应登记审计: %+v", rep2.LockedAudit)
	}
	// 再读一次 +1，且审计持久化在 options 表。
	rep3, _ := RunJointEval(true)
	if rep3.LockedAudit == nil || rep3.LockedAudit.Count != 2 {
		t.Fatalf("第二次读取审计应为 2: %+v", rep3.LockedAudit)
	}
	var opt model.Option
	if err := common.DB.Where("`key` = ?", jointLockedReadsKey).First(&opt).Error; err != nil || !strings.Contains(opt.Value, `"count":2`) {
		t.Fatalf("审计应持久化到 options: %+v err=%v", opt, err)
	}
	// 缓存只存常规视图：CachedJointEvalReport 不含锁定段指标。
	if c := CachedJointEvalReport(); c == nil || c.IncludeLocked {
		t.Fatalf("缓存应为常规视图: %+v", c)
	}
	// 常规视图重算后审计计数可见（提醒「已读过 N 次」）。
	rep4, _ := RunJointEval(false)
	if rep4.LockedAudit == nil || rep4.LockedAudit.Count != 2 {
		t.Fatalf("常规视图应透出既有审计计数: %+v", rep4.LockedAudit)
	}
}

// TestJointEvalNoSplitFewDays 信号日不足 jointMinSplitDays：全部归 dev、锁定段缺席，
// include_locked 请求也不产生审计（无锁定段可读不烧额度）。
func TestJointEvalNoSplitFewDays(t *testing.T) {
	setupTestDB(t)
	cleanJointTables(t)

	common.DB.Create(&model.RecommendationBatch{ID: 201, UserID: 1, Type: model.RecTypeShortTerm,
		Provider: "openai", Model: "m", PromptVersion: "p13", Status: model.RecStatusSuccess})
	id := int64(1000)
	for i := 1; i <= 5; i++ {
		id++
		jointSeedLabel(t, id, 201, time.Date(2026, 6, i, 0, 0, 0, 0, time.Local).Format("2006-01-02"),
			model.RecActionBuy, 80, 2, -1, model.LabelMatured)
	}
	rep, err := RunJointEval(true)
	if err != nil {
		t.Fatalf("RunJointEval: %v", err)
	}
	var sec *JointEvalSection
	for _, s := range rep.Sections {
		if s.Type == model.RecTypeShortTerm {
			sec = s
		}
	}
	if sec.Dev == nil || sec.Dev.SignalDays != 5 || sec.LockedPreview != nil || sec.Locked != nil {
		t.Fatalf("5 天不应切分: dev=%+v preview=%+v", sec.Dev, sec.LockedPreview)
	}
	if rep.LockedAudit != nil && rep.LockedAudit.Count != 0 {
		t.Fatalf("无锁定段可读不应登记审计: %+v", rep.LockedAudit)
	}
}

// TestJointSegStatsCalibThreshold 段内校准与校准报表同门槛：不足 calibEvalMinSample
// 时 Brier/ECE 为 nil（未评估≠0 分），calib_sample 如实计数。
func TestJointSegStatsCalibThreshold(t *testing.T) {
	mk := func(n int) []calibSample {
		out := make([]calibSample, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, calibSample{
				label: model.RecommendationLabel{Action: model.RecActionBuy, NetReturnPct: 1,
					SignalDate: "2026-06-01", GrossReturnPct: 1.4, MaePct: -1},
				meta: calibRecMeta{conf: 80, sysConf: "high"},
			})
		}
		return out
	}
	seg := jointSegStats("dev", []string{"2026-06-01"}, mk(calibEvalMinSample-1))
	if seg.Brier != nil || seg.ECE != nil || seg.CalibSample != calibEvalMinSample-1 {
		t.Fatalf("样本不足不应产出 Brier/ECE: %+v", seg)
	}
	seg = jointSegStats("dev", []string{"2026-06-01"}, mk(calibEvalMinSample))
	// conf=80 全命中：Brier=(0.8-1)²=0.04、ECE=0.2（校准报表同款验算锚）。
	if seg.Brier == nil || *seg.Brier != 0.04 || seg.ECE == nil || *seg.ECE != 0.2 {
		t.Fatalf("达门槛应产出 Brier=0.04/ECE=0.2: %+v", seg)
	}
}

// TestCalibSliceRows 分层分组纯函数：空 key 归（未知）、样本降序、超上限合并
// （其他）行且合并行不产出 Brier、达门槛层产出 Brier/ECE。
func TestCalibSliceRows(t *testing.T) {
	var obs []calibSliceObs
	// momentum 达门槛（conf 80 全命中 → Brier 0.04/ECE 0.2）；pullback 2 条；空 key 1 条。
	for i := 0; i < calibEvalMinSample; i++ {
		obs = append(obs, calibSliceObs{Key: "momentum", Conf: 80, Hit: true, Net: 2, Alpha: 1, HasBench: true})
	}
	obs = append(obs,
		calibSliceObs{Key: "pullback", Conf: 60, Hit: false, Net: -3},
		calibSliceObs{Key: "pullback", Conf: 60, Hit: true, Net: 1},
		calibSliceObs{Key: "", Conf: 50, Hit: true, Net: 4},
	)
	rows := calibSliceRows(obs)
	if len(rows) != 3 {
		t.Fatalf("应 3 行: %+v", rows)
	}
	if rows[0].Key != "momentum" || rows[0].Sample != calibEvalMinSample ||
		rows[0].Brier == nil || *rows[0].Brier != 0.04 || rows[0].ECE == nil || *rows[0].ECE != 0.2 {
		t.Fatalf("momentum 层验算失败: %+v", rows[0])
	}
	if rows[1].Key != "pullback" || rows[1].Sample != 2 || rows[1].Brier != nil ||
		rows[1].HitRatePct != 50 || rows[1].AvgNetPct != -1 {
		t.Fatalf("pullback 层验算失败: %+v", rows[1])
	}
	if rows[2].Key != "（未知）" || rows[2].Sample != 1 {
		t.Fatalf("空 key 应归（未知）: %+v", rows[2])
	}

	// 超上限合并：14 个不同 key 各 1 条 → 12 行 +（其他 2 项）合并行（无 Brier）。
	obs = nil
	for i := 0; i < calibSliceMaxRows+2; i++ {
		obs = append(obs, calibSliceObs{Key: string(rune('a' + i)), Conf: 50, Hit: true, Net: 1})
	}
	rows = calibSliceRows(obs)
	if len(rows) != calibSliceMaxRows+1 {
		t.Fatalf("应 %d 行: %d", calibSliceMaxRows+1, len(rows))
	}
	last := rows[len(rows)-1]
	if !strings.Contains(last.Key, "其他 2 项") || last.Sample != 2 || last.Brier != nil {
		t.Fatalf("合并行不符: %+v", last)
	}
}

// TestRecCalibReportSlices 校准报表分层端到端：策略/regime 取标签冗余、provider·model
// 与 prompt_version 关联批次；批次已删归（未知）；watch 不进分层（buy 口径）。
func TestRecCalibReportSlices(t *testing.T) {
	setupTestDB(t)
	cleanJointTables(t)

	common.DB.Create(&model.RecommendationBatch{ID: 301, UserID: 1, Type: model.RecTypeShortTerm,
		Provider: "openai", Model: "gpt-a", PromptVersion: "p13", Status: model.RecStatusSuccess})
	common.DB.Create(&model.RecommendationBatch{ID: 302, UserID: 1, Type: model.RecTypeShortTerm,
		Provider: "anthropic", Model: "claude-b", PromptVersion: "p13-custom.beef", Status: model.RecStatusSuccess})

	seed := func(recID, batchID int64, strategy, regime, action string, net float64) {
		common.DB.Create(&model.Recommendation{ID: recID, BatchID: batchID, UserID: 1,
			Action: action, Confidence: 80, DetailJSON: `{"sys_confidence":"high"}`})
		common.DB.Create(&model.RecommendationLabel{
			RecommendationID: recID, HorizonDays: 10, EntryMode: model.EntryModeNextOpen,
			BatchID: batchID, UserID: 1, Type: model.RecTypeShortTerm, Action: action,
			SignalDate: "2026-06-01", Strategy: strategy, Regime: regime,
			NetReturnPct: net, GrossReturnPct: net + 0.4, HasBench: false,
			MaturityStatus: model.LabelMatured, LabelVersion: labelVersion,
		})
	}
	seed(1, 301, "momentum", "offense", model.RecActionBuy, 3)
	seed(2, 301, "momentum", "offense", model.RecActionBuy, -1)
	seed(3, 302, "pullback", "defense", model.RecActionBuy, 2)
	seed(4, 999, "momentum", "", model.RecActionBuy, 1)          // 批次已删→provider（未知）
	seed(5, 301, "momentum", "offense", model.RecActionWatch, 9) // watch 不进分层

	rep, err := buildRecCalibReport(model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("buildRecCalibReport: %v", err)
	}
	groups := map[string]CalibSliceGroup{}
	for _, g := range rep.Slices {
		groups[g.Dim] = g
	}
	if len(groups) != 4 {
		t.Fatalf("应 4 个分层维度: %+v", rep.Slices)
	}
	st := groups["strategy"]
	if len(st.Rows) != 2 || st.Rows[0].Key != "momentum" || st.Rows[0].Sample != 3 ||
		st.Rows[1].Key != "pullback" || st.Rows[1].Sample != 1 {
		t.Fatalf("策略分层不符（watch 不得计入）: %+v", st.Rows)
	}
	rg := groups["regime"]
	if len(rg.Rows) != 3 { // offense 2 / defense 1 /（未知）1
		t.Fatalf("regime 分层应 3 行: %+v", rg.Rows)
	}
	pm := groups["provider_model"]
	pmKeys := map[string]int{}
	for _, r := range pm.Rows {
		pmKeys[r.Key] = r.Sample
	}
	if pmKeys["openai/gpt-a"] != 2 || pmKeys["anthropic/claude-b"] != 1 || pmKeys["（未知）"] != 1 {
		t.Fatalf("provider·model 分层不符: %+v", pm.Rows)
	}
	pv := groups["prompt_version"]
	pvKeys := map[string]int{}
	for _, r := range pv.Rows {
		pvKeys[r.Key] = r.Sample
	}
	if pvKeys["p13"] != 2 || pvKeys["p13-custom.beef"] != 1 || pvKeys["（未知）"] != 1 {
		t.Fatalf("prompt_version 分层不符: %+v", pv.Rows)
	}
	// momentum 层收益验算：3 条 buy（3/-1/1）→命中 2/3、均值 1。
	if st.Rows[0].HitRatePct != 66.67 || st.Rows[0].AvgNetPct != 1 {
		t.Fatalf("momentum 层收益验算失败: %+v", st.Rows[0])
	}
	// Notes 应声明分层口径（旧「未分层」限制声明已清）。
	joined := strings.Join(rep.Notes, "\n")
	if !strings.Contains(joined, "分层维度") || strings.Contains(joined, "未按 provider/model") {
		t.Fatalf("Notes 分层声明不符: %s", joined)
	}
}

// TestJointEvalMarshal 报表可序列化（前端契约冒烟）：含指针字段/嵌套分组。
func TestJointEvalMarshal(t *testing.T) {
	setupTestDB(t)
	cleanJointTables(t)
	rep, err := RunJointEval(false)
	if err != nil {
		t.Fatalf("空库应可出报表: %v", err)
	}
	if _, err := json.Marshal(rep); err != nil {
		t.Fatalf("报表应可序列化: %v", err)
	}
	if len(rep.Sections) != 2 {
		t.Fatalf("应两个 section（短线/长线）: %d", len(rep.Sections))
	}
}
