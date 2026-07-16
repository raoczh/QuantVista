package service

import (
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// ---------- S2 多门控合并（事件表每候选恰一行的前提） ----------

// TestMergeGateNotes 同一标的命中多个门控：主 gate_type 按优先级取最强
//（regime > bear > quality），其余说明并入 reason；与顺序无关。
func TestMergeGateNotes(t *testing.T) {
	regime := gateNote{Symbol: "600100", GateType: model.GateRegimeShadow, WouldBeAction: model.RecActionWatch, Reason: "防守档"}
	quality := gateNote{Symbol: "600100", GateType: model.GateQualityShadow, GateVersion: qualityGateVersion, Reason: "缺财务"}
	bear := gateNote{Symbol: "600100", GateType: model.GateBearShadow, GateVersion: bearReviewVersion, WouldBeAction: model.RecActionWatch, Reason: "反方high"}

	// 三重叠加（任意顺序）：主 gate 恒 regime_shadow，其余并入 reason。
	for _, gates := range [][]gateNote{
		{regime, quality, bear},
		{quality, bear, regime},
		{bear, regime, quality},
	} {
		merged := mergeGateNotes(gates)
		if len(merged) != 1 {
			t.Fatalf("同标的应合并为一行: %+v", merged)
		}
		g := merged["600100"]
		if g.GateType != model.GateRegimeShadow || g.WouldBeAction != model.RecActionWatch {
			t.Fatalf("主 gate 应 regime_shadow: %+v", g)
		}
		if !strings.Contains(g.Reason, "另有["+model.GateQualityShadow+"]") ||
			!strings.Contains(g.Reason, "另有["+model.GateBearShadow+"]") {
			t.Fatalf("次要门控应并入 reason: %s", g.Reason)
		}
	}

	// 单门控与不同标的原样保留。
	merged := mergeGateNotes([]gateNote{quality, {Symbol: "600200", GateType: model.GateCorrelation, Reason: "相关"}})
	if len(merged) != 2 || merged["600100"].GateType != model.GateQualityShadow ||
		merged["600200"].GateType != model.GateCorrelation {
		t.Fatalf("无冲突时应原样: %+v", merged)
	}
}

// ---------- S2-4 影子对照报表（DB 端到端验算） ----------

// seedShadowLabel 落一条标签行（recID>0=入选挂 rec；recID=0 时挂事件行 evID）。
func seedShadowLabel(t *testing.T, recID, evID, batchID int64, symbol, status string, net, alpha float64) {
	t.Helper()
	l := model.RecommendationLabel{
		RecommendationID: recID, CandidateEventID: evID, HorizonDays: 10,
		EntryMode: model.EntryModeNextOpen, BatchID: batchID, UserID: 11,
		Symbol: symbol, Market: "cn", Type: model.RecTypeShortTerm,
		Action: model.RecActionBuy, SignalDate: "2026-06-01",
		MaturityStatus: status, NetReturnPct: net, AlphaPct: alpha,
		HasBench: status == model.LabelMatured, LabelVersion: labelVersion,
	}
	if err := common.DB.Create(&l).Error; err != nil {
		t.Fatalf("seed label 失败: %v", err)
	}
}

// seedShadowEvent 落一条候选事件行，返回 id。
func seedShadowEvent(t *testing.T, batchID int64, symbol, stage, gateType, wouldBe string) int64 {
	t.Helper()
	ev := model.RecommendationCandidateEvent{
		BatchID: batchID, UserID: 11, Symbol: symbol, Market: "cn",
		CandidateStage: stage, GateType: gateType, WouldBeAction: wouldBe,
	}
	if err := common.DB.Create(&ev).Error; err != nil {
		t.Fatalf("seed event 失败: %v", err)
	}
	return ev.ID
}

// TestRecShadowReport gated vs ungated 分组统计与覆盖率手工验算。
func TestRecShadowReport(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)

	const batchID = int64(501)
	// 入选五只（rec_id>0）：600100 无门控 +10；600200 regime 影子 -6；600300 反方影子 -2；
	// 600400 质量影子 +1；600600 无门控 pending。名单外 600500 被相关性挤出，影子标签 +3。
	seedShadowEvent(t, batchID, "600100", model.CandStagePicked, "", "")
	seedShadowEvent(t, batchID, "600200", model.CandStagePicked, model.GateRegimeShadow, model.RecActionWatch)
	seedShadowEvent(t, batchID, "600300", model.CandStagePicked, model.GateBearShadow, model.RecActionWatch)
	seedShadowEvent(t, batchID, "600400", model.CandStagePicked, model.GateQualityShadow, "")
	corrEv := seedShadowEvent(t, batchID, "600500", model.CandStageScored, model.GateCorrelation, "")
	seedShadowEvent(t, batchID, "600600", model.CandStagePicked, "", "")

	seedShadowLabel(t, 1, 0, batchID, "600100", model.LabelMatured, 10, 8)
	seedShadowLabel(t, 2, 0, batchID, "600200", model.LabelMatured, -6, -7)
	seedShadowLabel(t, 3, 0, batchID, "600300", model.LabelMatured, -2, -3)
	seedShadowLabel(t, 4, 0, batchID, "600400", model.LabelMatured, 1, 0.5)
	seedShadowLabel(t, 0, corrEv, batchID, "600500", model.LabelMatured, 3, 2)
	seedShadowLabel(t, 6, 0, batchID, "600600", model.LabelPending, 0, 0)

	rep, err := RecShadowReport(11, model.RecTypeShortTerm, 10)
	if err != nil {
		t.Fatalf("报表失败: %v", err)
	}
	// 覆盖率：入选 buy 6 只中 rec_id>0 的 6 行——600500 是影子（rec_id=0）不算 → 5；成熟 4。
	if rep.PickedBuy != 5 || rep.PickedBuyMatured != 4 {
		t.Fatalf("picked_buy 应 5/成熟 4，得到 %d/%d", rep.PickedBuy, rep.PickedBuyMatured)
	}
	byType := map[string]ShadowGateGroup{}
	for _, g := range rep.Groups {
		byType[g.GateType] = g
	}
	if len(rep.Groups) != 4 {
		t.Fatalf("应 4 个门控分组，得到 %d: %+v", len(rep.Groups), rep.Groups)
	}
	// 对照组（各组共用）：未被任何门控标记的入选成熟标的——只有 600100（600600 pending）。
	for gt, g := range byType {
		if g.Ungated.Sample != 1 || g.Ungated.AvgNetPct != 10 || g.Ungated.WinRate != 100 {
			t.Fatalf("%s 对照组应只含 600100(+10): %+v", gt, g.Ungated)
		}
	}
	rg := byType[model.GateRegimeShadow]
	if rg.Marked != 1 || rg.WouldRewrite != 1 || rg.Gated.Sample != 1 || rg.Gated.AvgNetPct != -6 {
		t.Fatalf("regime 组不符: %+v", rg)
	}
	br := byType[model.GateBearShadow]
	if br.Marked != 1 || br.WouldRewrite != 1 || br.Gated.AvgNetPct != -2 {
		t.Fatalf("bear 组不符: %+v", br)
	}
	qg := byType[model.GateQualityShadow]
	if qg.WouldRewrite != 0 || qg.Gated.AvgNetPct != 1 {
		t.Fatalf("质量组不改动作 would_rewrite 应 0: %+v", qg)
	}
	// 相关性组吃影子标签（挂事件行）。
	cr := byType[model.GateCorrelation]
	if cr.Marked != 1 || cr.Gated.Sample != 1 || cr.Gated.AvgNetPct != 3 {
		t.Fatalf("correlation 组应吃影子标签(+3): %+v", cr)
	}
	// 严重亏损率：regime 组 -6 命中 <-5。
	if rg.Gated.SevereLossPct != 100 {
		t.Fatalf("regime 组严重亏损率应 100: %+v", rg.Gated)
	}

	// 类型过滤：长线口径无标签 → 空报表。
	repLong, err := RecShadowReport(11, model.RecTypeLongTerm, 10)
	if err != nil {
		t.Fatalf("长线报表失败: %v", err)
	}
	if repLong.PickedBuy != 0 || len(repLong.Groups) != 0 {
		t.Fatalf("长线口径应为空: %+v", repLong)
	}
	// 非法持有期拒绝。
	if _, err := RecShadowReport(11, "", 7); err == nil {
		t.Fatalf("非法持有期应报错")
	}
	// 用户隔离。
	repOther, _ := RecShadowReport(12, model.RecTypeShortTerm, 10)
	if repOther.PickedBuy != 0 || len(repOther.Groups) != 0 {
		t.Fatalf("跨用户应为空: %+v", repOther)
	}
}
