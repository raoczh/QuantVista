package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// ---------- S3-2 召回评估端到端（假日线 + 事件 + 影子标签，手工验算） ----------
//
// 构造：12 只股 × 30 个交易日，股 k 日增长率 g_k=2.0%−(k−1)×0.1%（600001 最高、
// 600012 最低）→ 任意窗口收益秩恒等于编号序，Top-10 = 600001..600010。
// 批次建在第 10 个交易日；池内事件 3 只：
//   600001 picked（source=watchlist）、600002 llm_list（source=gainer）、
//   600003 scored（source=gainer）；其余 9 只从未进池（absent）。
// 影子标签：600002 horizon=5 已成熟净收益 8%。
// 期望（K 钳到最小 10、kEff=10）：
//   RecallPool=3/10=30%、RecallLLM=2/10=20%（picked+llm_list）、RecallPicked=1/10=10%；
//   MissedRate=(7 absent + 2 进池未入选)/10=90%；
//   分桶 picked=1/llm_list=1/scored=1/absent=7；
//   消融：去 watchlist（1 命中）→20%（掉 10）；去 gainer（2 命中）→10%（掉 20）。
func seedRecallFixture(t *testing.T) (batchID int64) {
	t.Helper()
	for _, tbl := range []string{"daily_bars", "trading_calendars", "stock_universe_dailies",
		"recommendation_batches", "recommendation_candidate_events", "recommendation_labels"} {
		common.DB.Exec("DELETE FROM " + tbl)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"daily_bars", "trading_calendars", "stock_universe_dailies",
			"recommendation_batches", "recommendation_candidate_events", "recommendation_labels"} {
			common.DB.Exec("DELETE FROM " + tbl)
		}
	})

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	var dates []string
	for d := 0; d < 30; d++ {
		dates = append(dates, base.AddDate(0, 0, d).Format("2006-01-02"))
	}
	for _, d := range dates {
		common.DB.Exec("INSERT INTO trading_calendars (market, trade_date, is_open) VALUES ('cn', ?, 1)", d)
	}
	signalIdx := 9 // 第 10 个交易日
	signalDate := dates[signalIdx]

	var bars []model.DailyBar
	var uni []model.StockUniverseDaily
	for k := 1; k <= 12; k++ {
		sym := fmt.Sprintf("6000%02d", k)
		g := 0.020 - float64(k-1)*0.001
		price := 10.0
		for d := 0; d < 30; d++ {
			open := price
			price = price * (1 + g)
			bars = append(bars, model.DailyBar{
				Symbol: sym, Market: "cn", TradeDate: dates[d],
				Open: open, High: price * 1.001, Low: open * 0.999, Close: price,
				Volume: 5_000_000, Amount: 5e7, Source: "eastmoney",
			})
			if d == signalIdx {
				uni = append(uni, model.StockUniverseDaily{
					TradeDate: dates[d], Symbol: sym, Market: "cn", Name: "股" + sym,
					Amount: 5e7, Close: price,
				})
			}
		}
	}
	if err := common.DB.CreateInBatches(bars, 500).Error; err != nil {
		t.Fatalf("seed bars 失败: %v", err)
	}
	if err := common.DB.CreateInBatches(uni, 100).Error; err != nil {
		t.Fatalf("seed universe 失败: %v", err)
	}

	batch := model.RecommendationBatch{
		UserID: 7, Type: model.RecTypeShortTerm, Market: "cn", Status: model.RecStatusSuccess,
	}
	if err := common.DB.Create(&batch).Error; err != nil {
		t.Fatalf("seed batch 失败: %v", err)
	}
	// CreatedAt 显式改到信号日中午（Create 默认写 now）。
	created := base.AddDate(0, 0, signalIdx).Add(12 * time.Hour)
	common.DB.Model(&model.RecommendationBatch{}).Where("id = ?", batch.ID).Update("created_at", created)

	events := []model.RecommendationCandidateEvent{
		{BatchID: batch.ID, UserID: 7, Symbol: "600001", Market: "cn", CandidateStage: model.CandStagePicked, Source: "watchlist", SentToLLM: true},
		{BatchID: batch.ID, UserID: 7, Symbol: "600002", Market: "cn", CandidateStage: model.CandStageLLMList, Source: "gainer", SentToLLM: true},
		{BatchID: batch.ID, UserID: 7, Symbol: "600003", Market: "cn", CandidateStage: model.CandStageScored, Source: "gainer"},
	}
	if err := common.DB.Create(&events).Error; err != nil {
		t.Fatalf("seed events 失败: %v", err)
	}
	label := model.RecommendationLabel{
		RecommendationID: 0, CandidateEventID: events[1].ID, BatchID: batch.ID, UserID: 7,
		Symbol: "600002", Market: "cn", Type: model.RecTypeShortTerm,
		HorizonDays: 5, EntryMode: model.EntryModeNextOpen,
		SignalDate: signalDate, NetReturnPct: 8.0, MaturityStatus: model.LabelMatured,
	}
	if err := common.DB.Create(&label).Error; err != nil {
		t.Fatalf("seed label 失败: %v", err)
	}
	return batch.ID
}

func TestRecRecallReportEndToEnd(t *testing.T) {
	setupTestDB(t)
	batchID := seedRecallFixture(t)
	factorFreshMu.Lock()
	factorFreshVal = ""
	factorFreshMu.Unlock()

	svc := &RecommendationService{}
	rep, err := svc.RecRecallReport(context.Background(), 7, model.RecTypeShortTerm, 5, 10)
	if err != nil {
		t.Fatalf("RecRecallReport 失败: %v", err)
	}
	if rep.Batches != 1 || len(rep.BatchRows) != 1 {
		t.Fatalf("应评估 1 个批次: %+v", rep)
	}
	row := rep.BatchRows[0]
	if row.BatchID != batchID || row.OppSize != 12 || row.PoolSize != 3 || row.KEff != 10 {
		t.Fatalf("批次行不符: %+v", row)
	}
	if row.HitPool != 3 || row.HitLLM != 2 || row.HitPicked != 1 {
		t.Fatalf("命中数应 3/2/1: %+v", row)
	}
	if rep.RecallPoolPct != 30 || rep.RecallLLMPct != 20 || rep.RecallPickedPct != 10 {
		t.Fatalf("Recall 应 30/20/10: %+v", rep)
	}
	if rep.MissedRatePct != 90 {
		t.Fatalf("错失率应 90，得到 %v", rep.MissedRatePct)
	}
	if rep.TopKStageCounts["picked"] != 1 || rep.TopKStageCounts["llm_list"] != 1 ||
		rep.TopKStageCounts["scored"] != 1 || rep.TopKStageCounts["absent"] != 7 {
		t.Fatalf("分桶不符: %v", rep.TopKStageCounts)
	}
	if rep.MissedLabels == nil || rep.MissedLabels.N != 1 || rep.MissedLabels.MeanPct != 8 {
		t.Fatalf("错失影子标签应 1 条净收益 8%%: %+v", rep.MissedLabels)
	}
	// 消融：watchlist 掉 10 个点、gainer 掉 20 个点。
	drops := map[string]float64{}
	for _, a := range rep.SourceAblation {
		drops[a.Source] = a.DropPct
	}
	if drops["watchlist"] != 10 || drops["gainer"] != 20 {
		t.Fatalf("消融应 watchlist 10/gainer 20: %+v", rep.SourceAblation)
	}
	// 分布：机会集 12 样本、池 3 样本；单调构造下机会集 P90 ≥ 池中位。
	if rep.OppDist.N != 12 || rep.PoolDist.N != 3 {
		t.Fatalf("分布样本应 12/3: %+v %+v", rep.OppDist, rep.PoolDist)
	}
	// 用户隔离：其他用户无批次。
	if _, err := svc.RecRecallReport(context.Background(), 8, "", 5, 10); err == nil {
		t.Fatal("其他用户应报无批次")
	}
	// 非法持有期。
	if _, err := svc.RecRecallReport(context.Background(), 7, "", 7, 10); err == nil {
		t.Fatal("持有期 7 应被拒绝")
	}
}

// TestRecRecallSkipsNoPoolBatch #23：无候选事件的批次（S0-5 部署前/事实落库失败）整批
// 排除，不把「数据缺失」当「全量未召回」拉低 Recall——Recall 仍由有池批次决定，
// 且 Notes 声明排除数。
func TestRecRecallSkipsNoPoolBatch(t *testing.T) {
	setupTestDB(t)
	seedRecallFixture(t) // 一个有 3 只候选事件的批次（Recall 池 30%）
	factorFreshMu.Lock()
	factorFreshVal = ""
	factorFreshMu.Unlock()

	// 追加一个「无候选事件」批次，落在同一信号日（第 10 个交易日中午）。
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	created := base.AddDate(0, 0, 9).Add(12 * time.Hour)
	noPool := model.RecommendationBatch{UserID: 7, Type: model.RecTypeShortTerm, Market: "cn",
		Status: model.RecStatusSuccess}
	if err := common.DB.Create(&noPool).Error; err != nil {
		t.Fatalf("建无池批次失败: %v", err)
	}
	common.DB.Model(&model.RecommendationBatch{}).Where("id = ?", noPool.ID).Update("created_at", created)

	svc := &RecommendationService{}
	rep, err := svc.RecRecallReport(context.Background(), 7, model.RecTypeShortTerm, 5, 10)
	if err != nil {
		t.Fatalf("RecRecallReport 失败: %v", err)
	}
	// 只评估有池批次；无池批次被排除。
	if rep.Batches != 1 || len(rep.BatchRows) != 1 {
		t.Fatalf("无池批次应被排除，仅评 1 批: batches=%d rows=%d", rep.Batches, len(rep.BatchRows))
	}
	if rep.BatchRows[0].BatchID == noPool.ID {
		t.Fatalf("被评估的不应是无池批次")
	}
	// Recall 未被 10 个 absent 稀释（仍 30/20/10）。
	if rep.RecallPoolPct != 30 || rep.MissedRatePct != 90 {
		t.Fatalf("Recall 不应被无池批次稀释: pool=%v missed=%v", rep.RecallPoolPct, rep.MissedRatePct)
	}
	// Notes 声明排除。
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "无候选事件") && strings.Contains(n, "已整批排除") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Notes 应声明无池批次排除: %v", rep.Notes)
	}
}
