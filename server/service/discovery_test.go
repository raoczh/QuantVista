package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func discoveryTestTable(date string, symbols ...string) *FactorTable {
	if len(symbols) == 0 {
		symbols = []string{"600003", "600001", "600002"}
	}
	keys := []string{"close", "chg_pct", "chg_5d", "chg_20d", "ma20", "ma60", "vol_boost", "vol_5v20", "pos_60", "volatility_20", "div_yield", "atr_pct", "amount_yi", "turnover_rate", "high_20d", "bull_align", "above_ma20", "above_ma60", "is_st"}
	cols := make(map[string][]float64, len(keys))
	for _, key := range keys {
		cols[key] = make([]float64, len(symbols))
	}
	for i := range symbols {
		values := map[string]float64{
			"close": 10, "chg_pct": 4, "chg_5d": -2, "chg_20d": 12, "ma20": 9, "ma60": 8,
			"vol_boost": 2, "vol_5v20": 0.8, "pos_60": 40, "volatility_20": 2, "div_yield": 3,
			"atr_pct": 2, "amount_yi": 3, "turnover_rate": 5, "high_20d": 1, "bull_align": 1,
			"above_ma20": 1, "above_ma60": 1, "is_st": 0,
		}
		for key, value := range values {
			cols[key][i] = value
		}
	}
	names := make([]string, len(symbols))
	lastDates := make([]string, len(symbols))
	for i := range symbols {
		names[i] = "测试股份" + symbols[i]
		lastDates[i] = date
	}
	return &FactorTable{TradeDate: date, ExpectedDate: date, FreshCoverage: 1, LagOpenDays: 0, BuiltAt: time.Now(), Symbols: symbols, Names: names, LastDates: lastDates, cols: cols}
}

func installDiscoveryTestTable(t *testing.T, table *FactorTable) {
	t.Helper()
	factorTableMu.Lock()
	old := factorTableCur
	factorTableCur = table
	factorTableMu.Unlock()
	t.Cleanup(func() {
		factorTableMu.Lock()
		factorTableCur = old
		factorTableMu.Unlock()
	})
}

func cleanDiscoveryDate(t *testing.T, date string) {
	t.Helper()
	setupTestDB(t)
	common.DB.Where("trade_date = ?", date).Delete(&model.CandidateDiscoveryItem{})
	common.DB.Where("trade_date = ?", date).Delete(&model.CandidateDiscoveryRun{})
	common.DB.Where("market = ? AND trade_date = ?", "cn", date).Delete(&model.TradingCalendar{})
}

func addDiscoveryCalendar(t *testing.T, dates ...string) {
	t.Helper()
	for _, date := range dates {
		common.DB.Where("market = ? AND trade_date = ?", "cn", date).Delete(&model.TradingCalendar{})
		if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}).Error; err != nil {
			t.Fatalf("插入交易日 %s 失败: %v", date, err)
		}
	}
}

func TestBuildDiscoverySignalsStableSortAndChannelQuota(t *testing.T) {
	table := discoveryTestTable("2043-01-03", "600003", "600001", "600002")
	got := BuildDiscoverySignals(table, 2)
	for _, channel := range discoveryChannels {
		rows := got[channel]
		if len(rows) != 2 {
			t.Fatalf("通道 %s 应固定取 Top2，得到 %d", channel, len(rows))
		}
		if rows[0].Symbol != "600001" || rows[1].Symbol != "600002" {
			t.Fatalf("通道 %s tie-breaker 不稳定: %+v", channel, rows)
		}
		if rows[0].Score != rows[1].Score {
			t.Fatalf("测试数据应产生相同分数以验证 tie-breaker: %+v", rows)
		}
	}
}

func TestExecuteDailyDiscoveryIdempotentConcurrentAndSystemOwner(t *testing.T) {
	setupTestDB(t)
	date := "2043-02-03"
	cleanDiscoveryDate(t, date)
	installDiscoveryTestTable(t, discoveryTestTable(date))
	got := make([]*model.CandidateDiscoveryRun, 2)
	var errs = make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gotRun, err := ExecuteDailyDiscovery(context.Background(), int64(i+1), "cn", date, "test-param")
			got[i], errs[i] = gotRun, err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("并发发现失败: %v", err)
		}
	}
	var runs []model.CandidateDiscoveryRun
	common.DB.Where("trade_date = ?", date).Find(&runs)
	if len(runs) != 1 {
		t.Fatalf("同日并发触发应只有一个 run，得到 %d", len(runs))
	}
	if runs[0].OwnerType != model.JobOwnerSystem || runs[0].OwnerUserID != nil || runs[0].Status != DiscoveryRunStatusOK {
		t.Fatalf("system/global run 归属或状态错误: %+v", runs[0])
	}
	var count int64
	common.DB.Model(&model.CandidateDiscoveryItem{}).Where("trade_date = ?", date).Count(&count)
	if count != int64(len(discoveryChannels)*len(discoveryTestTable(date).Symbols)) {
		t.Fatalf("发现 item 数量错误: %d", count)
	}
	second, err := ExecuteDailyDiscovery(context.Background(), 99, "cn", date, "test-param")
	if err != nil || second.ID != runs[0].ID {
		t.Fatalf("同日 success 重复执行应直接复用，run=%+v err=%v", second, err)
	}
	var countAfter int64
	common.DB.Model(&model.CandidateDiscoveryItem{}).Where("trade_date = ?", date).Count(&countAfter)
	if countAfter != count {
		t.Fatalf("同日幂等重复插入 item: before=%d after=%d", count, countAfter)
	}
}

func TestDiscoveryJobRunDeduplicatesConcurrentSystemTriggers(t *testing.T) {
	resetDurableJobs(t)
	date := "2043-02-10"
	cleanDiscoveryDate(t, date)
	installDiscoveryTestTable(t, discoveryTestTable(date))
	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	handler := func(ctx context.Context, _ int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
		calls.Add(1)
		close(started)
		<-release
		var req discoveryJobRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return DurableJobResult{}, err
		}
		resultID, ok := currentJobResultID(ctx)
		if !ok {
			return DurableJobResult{}, context.Canceled
		}
		run, err := ExecuteDailyDiscovery(ctx, resultID, req.Market, req.TradeDate, req.ParameterHash)
		if err != nil {
			return DurableJobResult{}, err
		}
		return DurableJobResult{Status: model.JobStatusSuccess, Total: run.UniverseCount, Succeeded: run.SuccessCount}, nil
	}
	runtime.registerWithBinding(JobKindDailyDiscovery, time.Minute, handler, registerDiscoveryBinding(), false)
	req := discoveryJobRequest{Version: 1, Market: "cn", TradeDate: date, TriggerSource: "test", ParameterHash: "job-param"}
	first, created, err := runtime.startSystemWithBindingStatus(nil, JobKindDailyDiscovery, req, nil)
	if err != nil || !created {
		t.Fatalf("首次 system 发现作业创建失败: run=%+v created=%v err=%v", first, created, err)
	}
	<-started
	second, secondCreated, err := runtime.startSystemWithBindingStatus(nil, JobKindDailyDiscovery, req, nil)
	if err != nil || secondCreated || second == nil || second.ID != first.ID {
		t.Fatalf("并发重复触发必须复用同一 JobRun: first=%+v second=%+v created=%v err=%v", first, second, secondCreated, err)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := common.DB.First(first, first.ID).Error; err == nil && first.Status == model.JobStatusSuccess {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first.Status != model.JobStatusSuccess || calls.Load() != 1 || first.ResultID == nil {
		t.Fatalf("发现作业未单次收敛成功: run=%+v calls=%d", first, calls.Load())
	}
	var discoveryRun model.CandidateDiscoveryRun
	if err := common.DB.First(&discoveryRun, *first.ResultID).Error; err != nil || discoveryRun.Status != DiscoveryRunStatusOK || discoveryRun.JobRunID == nil || *discoveryRun.JobRunID != first.ID {
		t.Fatalf("JobRun 与 DiscoveryRun 结果引用不一致: discovery=%+v err=%v", discoveryRun, err)
	}
}

func TestExecuteDailyDiscoveryPartialAndHistory(t *testing.T) {
	setupTestDB(t)
	dates := []string{"2043-03-01", "2043-03-02", "2043-03-03", "2043-03-04"}
	for _, date := range dates {
		cleanDiscoveryDate(t, date)
	}
	addDiscoveryCalendar(t, dates...)
	for _, date := range dates {
		table := discoveryTestTable(date)
		if date == dates[0] {
			table.FreshCoverage, table.LagOpenDays = 0.9, 1
		}
		installDiscoveryTestTable(t, table)
		run, err := ExecuteDailyDiscovery(context.Background(), 0, "cn", date, "history-param")
		if err != nil {
			t.Fatalf("%s 发现失败: %v", date, err)
		}
		if date == dates[0] && run.Status != DiscoveryRunStatusPart {
			t.Fatalf("覆盖不足应 partial: %+v", run)
		}
	}
	var latest []model.CandidateDiscoveryItem
	common.DB.Where("trade_date = ? AND channel = ?", dates[3], "trend_breakout").Find(&latest)
	if len(latest) == 0 || latest[0].FirstSeenDate != dates[0] || latest[0].ConsecutiveDays != 4 {
		t.Fatalf("首次出现/连续天数未按历史事实固化: %+v", latest)
	}
	if latest[0].RankChange != 0 || latest[0].ScoreChange != 0 {
		t.Fatalf("相同排名/分数的变化量应为 0: %+v", latest[0])
	}
}

func TestDiscoveryFailedAndStartupBackfillDecision(t *testing.T) {
	setupTestDB(t)
	date := "2043-03-20"
	cleanDiscoveryDate(t, date)
	installDiscoveryTestTable(t, &FactorTable{TradeDate: date, ExpectedDate: date, FreshCoverage: 1, BuiltAt: time.Now(), cols: map[string][]float64{}})
	run, err := ExecuteDailyDiscovery(context.Background(), 0, "cn", date, "empty-param")
	if err == nil || run == nil || run.Status != DiscoveryRunStatusFail || run.PartialReason == "" {
		t.Fatalf("空宽表必须如实固化 failed 并返回错误: run=%+v err=%v", run, err)
	}
	if !discoveryNeedsBackfill(date) {
		t.Fatal("没有当前参数版本成功运行时，启动应判定需要补跑")
	}
	now := time.Now()
	startupRun := &model.CandidateDiscoveryRun{OwnerType: model.JobOwnerSystem, Market: "cn", TradeDate: date, AsOf: now, DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion, ParameterHash: discoveryParameterHash(), Status: DiscoveryRunStatusFail, Error: "测试失败", StartedAt: now, FinishedAt: &now}
	if err := common.DB.Create(startupRun).Error; err != nil {
		t.Fatalf("创建启动补跑事实失败: %v", err)
	}
	if !discoveryNeedsBackfill(date) {
		t.Fatal("failed 运行必须在启动时补跑")
	}
	if err := common.DB.Model(startupRun).Updates(map[string]any{"status": DiscoveryRunStatusOK, "error": "", "finished_at": now}).Error; err != nil {
		t.Fatalf("更新启动运行失败: %v", err)
	}
	if discoveryNeedsBackfill(date) {
		t.Fatal("同日同版本同参数已有完整 success 后不应启动重扫")
	}
}

func TestRecentDiscoveryWindowRecomputesScoreAndKeepsGlobalPool(t *testing.T) {
	setupTestDB(t)
	dates := []string{"2043-04-01", "2043-04-02", "2043-04-03", "2043-04-04", "2043-04-05", "2043-04-06", "2043-04-07"}
	for _, date := range dates {
		cleanDiscoveryDate(t, date)
	}
	addDiscoveryCalendar(t, dates...)
	for _, date := range dates {
		table := discoveryTestTable(date)
		if date == dates[0] {
			table.Symbols, table.Names, table.LastDates = []string{"600099"}, []string{"旧日股份"}, []string{date}
			for key := range table.cols {
				table.cols[key] = table.cols[key][:1]
			}
		}
		if date == dates[6] {
			table.Symbols, table.Names, table.LastDates = []string{"600001"}, []string{"当前股份"}, []string{date}
			for key := range table.cols {
				table.cols[key] = table.cols[key][:1]
			}
		}
		installDiscoveryTestTable(t, table)
		if _, err := ExecuteDailyDiscovery(context.Background(), 0, "cn", date, "window-param"); err != nil {
			t.Fatalf("%s 发现失败: %v", date, err)
		}
	}
	first := recentDiscoveryCandidates("cn", 20)
	if len(first) == 0 {
		t.Fatal("最近发现不应为空")
	}
	for _, item := range first {
		if item.candidate.Score != 0 {
			t.Fatalf("历史发现分数不能直接作为今日推荐分: %+v", item.candidate)
		}
		if item.candidate.Discovery == nil || item.candidate.Discovery.SeenDays5D > 5 {
			t.Fatalf("最近候选摘要越过 5 日边界: %+v", item.candidate.Discovery)
		}
	}
	for _, item := range first {
		if item.candidate.Symbol == "600099" {
			t.Fatal("仅第 6 个交易日前出现的候选不应进入最近 5 日池")
		}
	}
	var current *discoveryPoolCandidate
	for i := range first {
		if first[i].candidate.Symbol == "600001" {
			current = &first[i]
			break
		}
	}
	if current == nil || current.source != "daily_discovery" {
		t.Fatalf("最新日候选来源错误: %+v", current)
	}
	if current.candidate.Discovery.ConsecutiveDays != 6 {
		t.Fatalf("连续出现天数不应被近 5 日展示窗口截断: %+v", current.candidate.Discovery)
	}
	hasRecent := false
	for _, item := range first {
		if item.source == "recent_discovery" {
			hasRecent = true
			break
		}
	}
	if !hasRecent {
		t.Fatal("前一交易日仍出现的候选应保留 recent_discovery 来源")
	}
	if reason := applyFreshQuoteToCand(&current.candidate, &datasource.Quote{Price: 80, Amount: 3e8, ChangePct: 1, DataTime: time.Now()}, RecFilters{PriceMax: 50}); reason == "" {
		t.Fatal("近 5 日候选必须按当前价格重新执行用户过滤")
	}
	// 两个用户只读取同一份 global 事实，不应改变 run 数量。
	var before, after int64
	common.DB.Model(&model.CandidateDiscoveryRun{}).Where("trade_date IN ?", dates).Count(&before)
	_ = recentDiscoveryCandidates("cn", 20)
	_ = recentDiscoveryCandidates("cn", 20)
	common.DB.Model(&model.CandidateDiscoveryRun{}).Where("trade_date IN ?", dates).Count(&after)
	if before != after {
		t.Fatalf("用户消费发现事实不应重复创建 run: %d -> %d", before, after)
	}
}

func TestDiscoveryCandidateEventSourcesAndBoundedNews(t *testing.T) {
	setupTestDB(t)
	batch := &model.RecommendationBatch{UserID: 911, Market: "cn", Type: model.RecTypeShortTerm, Status: model.RecStatusSuccess, CreatedAt: time.Now()}
	if err := common.DB.Create(batch).Error; err != nil {
		t.Fatalf("创建测试批次失败: %v", err)
	}
	pool := []candidate{{Symbol: "600001", Market: "cn", Name: "测试股份", Price: 10, Amount: 3e8, Sources: []string{"daily_discovery", "recent_discovery"}, Source: "daily_discovery"}}
	if err := recordBatchFacts(batch, pool, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("CandidateEvent 落库失败: %v", err)
	}
	var event model.RecommendationCandidateEvent
	if err := common.DB.Where("batch_id = ? AND symbol = ?", batch.ID, "600001").First(&event).Error; err != nil {
		t.Fatalf("读取 CandidateEvent 失败: %v", err)
	}
	if event.Source != "daily_discovery" || event.SourceSet != "daily_discovery,recent_discovery" {
		t.Fatalf("候选来源归因不完整: %+v", event)
	}
	now := time.Now()
	news := []model.News{
		{Title: "有效新闻一", Content: "SECRET正文一", Source: "cls", RelatedSymbols: `["600001"]`, PublishTime: now.Add(-time.Hour), ContentHash: "discovery-news-1"},
		{Title: "有效新闻二", Content: "SECRET正文二", Source: "cls", RelatedSymbols: `["600001"]`, PublishTime: now.Add(-2 * time.Hour), ContentHash: "discovery-news-2"},
		{Title: "有效新闻三", Content: "SECRET正文三", Source: "cls", RelatedSymbols: `["600001"]`, PublishTime: now.Add(-3 * time.Hour), ContentHash: "discovery-news-3"},
		{Title: "超出条数", Content: "SECRET正文四", Source: "cls", RelatedSymbols: `["600001"]`, PublishTime: now.Add(-4 * time.Hour), ContentHash: "discovery-news-4"},
		{Title: "非精确标的", Content: "SECRET正文五", Source: "cls", RelatedSymbols: `["1600001"]`, PublishTime: now.Add(-time.Hour), ContentHash: "discovery-news-5"},
		{Title: "窗口外", Content: "SECRET正文六", Source: "cls", RelatedSymbols: `["600001"]`, PublishTime: now.AddDate(0, 0, -8), ContentHash: "discovery-news-6"},
	}
	for i := range news {
		if err := common.DB.Create(&news[i]).Error; err != nil {
			t.Fatalf("写入新闻 %d 失败: %v", i, err)
		}
	}
	c := []candidate{{Symbol: "600001", NewsBrief: nil}}
	enrichCandidatePromptContext(c, now)
	if len(c[0].NewsBrief) != 3 {
		t.Fatalf("新闻摘要必须限制为 3 条窗口内精确新闻，得到 %d", len(c[0].NewsBrief))
	}
	b, _ := json.Marshal(compactRawCandidateForLLM(c[0]))
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "正文") {
		t.Fatalf("LLM 上下文不应包含新闻正文: %s", b)
	}
}
