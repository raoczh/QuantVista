package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func recPreheatSymbols(rows []recPreheatCandidate) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].Symbol
	}
	return out
}

func TestPlanRecPreheatOrderInvariant(t *testing.T) {
	base := []recPreheatCandidate{
		{Idx: 0, Symbol: "600005", BaseScore: 70, NeedsFetch: true},
		{Idx: 1, Symbol: "600003", BaseScore: 90, NeedsFetch: true},
		{Idx: 2, Symbol: "600001", BaseScore: 90, NeedsFetch: true},
		{Idx: 3, Symbol: "600004", BaseScore: 100, NeedsFetch: false}, // fresh
		{Idx: 4, Symbol: "600002", BaseScore: 80, NeedsFetch: true},
	}
	tests := []struct {
		name     string
		budget   int
		cooldown map[string]bool
		want     []string
	}{
		{name: "预算为零", budget: 0, want: []string{}},
		{name: "全冷按基础分与代码", budget: 3, want: []string{"600001", "600003", "600002"}},
		{name: "部分热缓存不参与预算", budget: 4, want: []string{"600001", "600003", "600002", "600005"}},
		{name: "冷却命中不耗预算", budget: 3, cooldown: map[string]bool{"600001": true}, want: []string{"600003", "600002", "600005"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for seed := int64(0); seed < 20; seed++ {
				shuffled := append([]recPreheatCandidate(nil), base...)
				rng := rand.New(rand.NewSource(seed))
				rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
				reserveCalls := 0
				got := planRecPreheat(shuffled, tc.budget, func(c recPreheatCandidate) bool {
					reserveCalls++
					return !tc.cooldown[c.Symbol]
				})
				if syms := recPreheatSymbols(got); !reflect.DeepEqual(syms, tc.want) {
					t.Fatalf("seed=%d：补拉集合应为 %v，得到 %v", seed, tc.want, syms)
				}
				if tc.budget == 0 && reserveCalls != 0 {
					t.Fatalf("预算 0 不得占用冷却槽，reserveCalls=%d", reserveCalls)
				}
			}
		})
	}
}

func resetRecommendationPreheatState(t *testing.T) {
	t.Helper()
	for _, row := range []any{
		&model.FinanceIndicator{}, &model.DisclosureSchedule{}, &model.FundFlowDaily{},
	} {
		if err := common.DB.Where("1 = 1").Delete(row).Error; err != nil {
			t.Fatalf("清理预热测试表失败: %v", err)
		}
	}
	finSyncMu.Lock()
	finSyncTry = map[string]time.Time{}
	finSyncMu.Unlock()
	fflowTryMu.Lock()
	fflowTry = map[string]time.Time{}
	fflowTryMu.Unlock()
}

func recFlowSymbol(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	secid := u.Query().Get("secid")
	if dot := strings.IndexByte(secid, '.'); dot >= 0 {
		return secid[dot+1:]
	}
	return secid
}

func recFreshFlowBody(symbol string, now time.Time) []byte {
	fresh := prevOpenTradeDate(now.Format("2006-01-02"))
	end, err := time.ParseInLocation("2006-01-02", fresh, time.Local)
	if err != nil {
		end = now.AddDate(0, 0, -1)
	}
	n, _ := strconv.Atoi(symbol[len(symbol)-2:])
	mainPct := float64(n%9) - 4
	lines := make([]string, 0, 5)
	for i := 4; i >= 0; i-- {
		date := end.AddDate(0, 0, -i).Format("2006-01-02")
		lines = append(lines, fmt.Sprintf("%s,1000000,0,0,0,0,%.2f,0,0,0,0,10.00,0.10,0,0", date, mainPct))
	}
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"klines": lines}})
	return body
}

func seedRecFreshFinance(t *testing.T, symbol string, now time.Time) {
	t.Helper()
	if err := common.DB.Create(&model.FinanceIndicator{
		Symbol: symbol, Market: "cn", ReportDate: "2025-12-31", ReportName: "2025年报",
		NoticeDate: now.AddDate(0, 0, -30).Format("2006-01-02"), ROE: 12, RevenueYoY: 8, NetProfitYoY: 9,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func seedRecFreshFlow(t *testing.T, symbol string, now time.Time) {
	t.Helper()
	if err := common.DB.Create(&model.FundFlowDaily{
		Symbol: symbol, Market: "cn", TradeDate: prevOpenTradeDate(now.Format("2006-01-02")),
		MainNet: 1e6, MainPct: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestPreheatRecommendationRoundFreezesSelectionAndFailures(t *testing.T) {
	setupTestDB(t)
	oldF10 := fetchF10
	t.Cleanup(func() { fetchF10 = oldF10 })
	now := time.Now().In(time.Local)
	symbols := []string{"600101", "600102", "600103", "600104", "600105"}
	pool := make([]candidate, len(symbols))
	base := make([]recPreheatCandidate, len(symbols))
	for i, symbol := range symbols {
		pool[i] = candidate{Symbol: symbol, Market: "cn"}
		base[i] = recPreheatCandidate{Idx: i, Symbol: symbol, BaseScore: float64(100 - i*10)}
	}

	type availability struct {
		Flow bool
		Fin  bool
	}
	run := func(seed int64) ([]string, []string, map[string]availability) {
		resetRecommendationPreheatState(t)
		seedRecFreshFinance(t, "600101", now)
		seedRecFreshFlow(t, "600101", now)
		// 600102 两域均 stale；财务有明确的新报告证据，刷新失败后必须 fail-closed。
		if err := common.DB.Create(&model.FinanceIndicator{
			Symbol: "600102", Market: "cn", ReportDate: "2025-12-31", ReportName: "旧年报",
			NoticeDate: now.AddDate(0, 0, -60).Format("2006-01-02"), ROE: 30,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := common.DB.Create(&model.DisclosureSchedule{
			Symbol: "600102", Market: "cn", ReportDate: "2026-03-31", IsPublished: true,
			ActualDate: now.AddDate(0, 0, -1).Format("2006-01-02"),
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := common.DB.Create(&model.FundFlowDaily{
			Symbol: "600102", Market: "cn", TradeDate: "2020-01-02", MainNet: 9e9, MainPct: 9,
		}).Error; err != nil {
			t.Fatal(err)
		}
		// 600103 正在冷却，规划必须跳过并把预算留给 600104。
		finSyncMu.Lock()
		finSyncTry["ind:600103"] = now
		finSyncMu.Unlock()
		fflowTryMu.Lock()
		fflowTry["cn:600103"] = now
		fflowTryMu.Unlock()

		var mu sync.Mutex
		var finCalls, flowCalls []string
		fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
			mu.Lock()
			finCalls = append(finCalls, symbol)
			mu.Unlock()
			if symbol == "600102" {
				return nil, errors.New("财务预热失败")
			}
			return []datasource.DcRow{f10Row(t, "2025-12-31", "2025年报", 15, 12, 18)}, nil
		}
		svc := &RecommendationService{em: datasource.NewEastMoneyAdapter()}
		svc.em.SetFetchForTest(func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
			symbol := recFlowSymbol(rawURL)
			mu.Lock()
			flowCalls = append(flowCalls, symbol)
			mu.Unlock()
			if symbol == "600102" {
				return nil, 0, errors.New("资金流预热失败")
			}
			return recFreshFlowBody(symbol, now), 200, nil
		})
		shuffled := append([]recPreheatCandidate(nil), base...)
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		finBudget, flowBudget := 2, 2
		got := svc.preheatRecommendationRound(context.Background(), model.RecTypeLongTerm, pool, shuffled, &finBudget, &flowBudget, now)
		if finBudget != 0 || flowBudget != 0 {
			t.Fatalf("真实请求应各消耗 2 个预算：fin=%d flow=%d", finBudget, flowBudget)
		}
		out := make(map[string]availability, len(symbols))
		for i, symbol := range symbols {
			out[symbol] = availability{Flow: got.FlowAvailable[i], Fin: got.FinanceAvailable[i]}
		}
		return append([]string(nil), finCalls...), append([]string(nil), flowCalls...), out
	}

	wantAvailability := map[string]availability{
		"600101": {Flow: true, Fin: true},   // 本地 fresh
		"600102": {Flow: false, Fin: false}, // stale 请求失败，不能回退旧证据
		"600103": {Flow: false, Fin: false}, // 冷却跳过
		"600104": {Flow: true, Fin: true},   // 接续使用未被冷却消耗的预算
		"600105": {Flow: false, Fin: false}, // 预算已满，失败不得触发补选
	}
	for seed := int64(0); seed < 8; seed++ {
		finCalls, flowCalls, available := run(seed)
		if want := []string{"600102", "600104"}; !reflect.DeepEqual(finCalls, want) {
			t.Fatalf("seed=%d：财务补拉集合应冻结为 %v，得到 %v", seed, want, finCalls)
		}
		if want := []string{"600102", "600104"}; !reflect.DeepEqual(flowCalls, want) {
			t.Fatalf("seed=%d：资金流补拉集合应冻结为 %v，得到 %v", seed, want, flowCalls)
		}
		if !reflect.DeepEqual(available, wantAvailability) {
			t.Fatalf("seed=%d：可用性结果不一致\nwant=%+v\ngot=%+v", seed, wantAvailability, available)
		}
	}
}

func TestPreheatRecommendationRoundBudgetZero(t *testing.T) {
	setupTestDB(t)
	resetRecommendationPreheatState(t)
	oldF10 := fetchF10
	t.Cleanup(func() { fetchF10 = oldF10 })
	finCalls := 0
	fetchF10 = func(context.Context, string) ([]datasource.DcRow, error) {
		finCalls++
		return nil, nil
	}
	svc := &RecommendationService{em: datasource.NewEastMoneyAdapter()}
	flowCalls := 0
	svc.em.SetFetchForTest(func(context.Context, string, map[string]string) ([]byte, int, error) {
		flowCalls++
		return nil, 200, nil
	})
	pool := []candidate{{Symbol: "600201", Market: "cn"}}
	bases := []recPreheatCandidate{{Idx: 0, Symbol: "600201", BaseScore: 99}}
	finBudget, flowBudget := 0, 0
	got := svc.preheatRecommendationRound(context.Background(), model.RecTypeLongTerm, pool, bases, &finBudget, &flowBudget, time.Now())
	if finCalls != 0 || flowCalls != 0 {
		t.Fatalf("预算 0 不得请求上游：fin=%d flow=%d", finCalls, flowCalls)
	}
	if got.FlowAvailable[0] || got.FinanceAvailable[0] {
		t.Fatalf("冷缓存且预算 0 必须显式 missing：%+v", got)
	}
}

func TestRecommendationMissingEnrichmentIsExplicitAndNotEvidence(t *testing.T) {
	c := candidate{
		Symbol: "600201", Market: "cn", Name: "缺失样本", Price: 10, Score: 60, Rank: 1,
		Factors: &candFactors{BarCount: 90}, FlowStatus: recEnrichmentMissing, FinStatus: recEnrichmentMissing,
	}
	rows := compactForLLM(model.RecTypeLongTerm, []candidate{c})
	if len(rows) != 1 || rows[0]["flow_status"] != recEnrichmentMissing || rows[0]["finance_status"] != recEnrichmentMissing {
		t.Fatalf("LLM 候选必须显式记录两域 missing，得到 %+v", rows)
	}
	if _, ok := rows[0]["fin"]; ok {
		t.Fatalf("财务缺失不得包装成零值 fin 对象：%+v", rows[0]["fin"])
	}
	for _, value := range candidateLabeledValues(c) {
		if strings.HasPrefix(value.Path, "fin.") || strings.HasPrefix(value.Path, "factors.main_net_") {
			t.Fatalf("缺失的零值不得进入核验证据值域：%+v", value)
		}
	}
}

type recPreheatMarketAdapter struct {
	failDaily map[string]bool
}

func (a *recPreheatMarketAdapter) Name() string { return "rec-preheat-test" }

func (a *recPreheatMarketAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	bars := wfParityBars(chipBarLimit, 3)
	return &datasource.Quote{
		Symbol: symbol, Market: market, Name: "测试" + symbol, Price: bars[len(bars)-1].Close,
		PrevClose: bars[len(bars)-2].Close, ChangePct: 0.1, Amount: 1e8,
		Source: "rec-preheat-test", DataTime: recFreshQuoteTime(),
	}, nil
}

func (a *recPreheatMarketAdapter) GetDailyBars(_ context.Context, _, symbol string, limit int) ([]datasource.Bar, error) {
	if a.failDaily[symbol] {
		return nil, datasource.ErrNoData
	}
	return wfParityBars(limit, 3), nil
}

func recFreshQuoteTime() time.Time {
	now := time.Now().In(time.Local)
	expected := expectedQuoteDate(now, isTradingDayToday(now), prevOpenTradeDate(now.Format("2006-01-02")))
	if expected == now.Format("2006-01-02") {
		return now
	}
	if parsed, err := time.ParseInLocation("2006-01-02 15:04", expected+" 15:00", time.Local); err == nil {
		return parsed
	}
	return now
}

func recScorePoolCandidates(n int) []candidate {
	bars := wfParityBars(chipBarLimit, 3)
	price := bars[len(bars)-1].Close
	pool := make([]candidate, n)
	for i := 0; i < n; i++ {
		symbol := fmt.Sprintf("601%03d", i)
		pool[i] = candidate{
			Symbol: symbol, Market: "cn", Name: "测试" + symbol, Price: price,
			Amount: 1e8, TurnoverRate: 3, Sources: []string{"active"},
		}
	}
	return pool
}

type recScoreOutcome struct {
	Score        float64
	Rank         int
	SentToLLM    bool
	Excluded     string
	FlowStatus   string
	FinStatus    string
	FinReport    string
	MainNetDays  int
	MainNet5dYi  float64
	VolumeScore  float64
	StrategyNote string
}

func recScoreOutcomes(pool []candidate) map[string]recScoreOutcome {
	out := make(map[string]recScoreOutcome, len(pool))
	for _, c := range pool {
		row := recScoreOutcome{
			Score: c.Score, Rank: c.Rank, SentToLLM: c.SentToLLM, Excluded: c.Excluded,
			FlowStatus: c.FlowStatus, FinStatus: c.FinStatus,
			StrategyNote: strings.Join(c.Bonus, "|"),
		}
		if c.Fin != nil {
			row.FinReport = c.Fin.Report
		}
		if c.Factors != nil {
			row.MainNetDays = c.Factors.MainNetDays
			row.MainNet5dYi = c.Factors.MainNet5dYi
		}
		if c.ScoreDims != nil {
			row.VolumeScore = c.ScoreDims.Volume
		}
		out[c.Symbol] = row
	}
	return out
}

func shuffledCandidates(src []candidate, seed int64) []candidate {
	out := append([]candidate(nil), src...)
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func TestScorePoolDeterministicPreheatOrderInvariant(t *testing.T) {
	setupTestDB(t)
	oldF10 := fetchF10
	t.Cleanup(func() { fetchF10 = oldF10 })
	basePool := recScorePoolCandidates(18)
	now := time.Now().In(time.Local)

	type runResult struct {
		FinCalls  []string
		FlowCalls []string
		Outcomes  map[string]recScoreOutcome
	}
	run := func(seed int64) runResult {
		resetRecommendationPreheatState(t)
		pool := shuffledCandidates(basePool, seed)
		var mu sync.Mutex
		var finCalls, flowCalls []string
		fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
			mu.Lock()
			finCalls = append(finCalls, symbol)
			mu.Unlock()
			return []datasource.DcRow{f10Row(t, "2025-12-31", "2025年报", 15, 12, 18)}, nil
		}
		adapter := &recPreheatMarketAdapter{failDaily: map[string]bool{}}
		svc := NewRecommendationService(NewMarketService(datasource.NewManagerWithAdapters(adapter)), nil, nil)
		svc.em.SetFetchForTest(func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
			symbol := recFlowSymbol(rawURL)
			mu.Lock()
			flowCalls = append(flowCalls, symbol)
			mu.Unlock()
			return recFreshFlowBody(symbol, now), 200, nil
		})
		svc.scorePool(context.Background(), model.RecTypeLongTerm, &longStrategies[0], pool, RecFilters{}, map[string]string{})
		return runResult{
			FinCalls: append([]string(nil), finCalls...), FlowCalls: append([]string(nil), flowCalls...),
			Outcomes: recScoreOutcomes(pool),
		}
	}

	wantFin := make([]string, 0, finRecFetchBudget)
	wantFlow := make([]string, 0, fflowRecBudget)
	for i := 0; i < finRecFetchBudget; i++ {
		wantFin = append(wantFin, fmt.Sprintf("601%03d", i))
	}
	for i := 0; i < fflowRecBudget; i++ {
		wantFlow = append(wantFlow, fmt.Sprintf("601%03d", i))
	}
	var baseline map[string]recScoreOutcome
	for seed := int64(0); seed < 6; seed++ {
		got := run(seed)
		if !reflect.DeepEqual(got.FinCalls, wantFin) {
			t.Fatalf("seed=%d：全冷财务补拉集合应为 %v，得到 %v", seed, wantFin, got.FinCalls)
		}
		if !reflect.DeepEqual(got.FlowCalls, wantFlow) {
			t.Fatalf("seed=%d：全冷资金流补拉集合应为 %v，得到 %v", seed, wantFlow, got.FlowCalls)
		}
		flowAvailable, finAvailable := 0, 0
		for _, row := range got.Outcomes {
			if row.FlowStatus == recEnrichmentAvailable {
				flowAvailable++
			}
			if row.FinStatus == recEnrichmentAvailable {
				finAvailable++
			}
		}
		if flowAvailable != fflowRecBudget || finAvailable != finRecFetchBudget {
			t.Fatalf("seed=%d：available 标记数量错误 flow=%d fin=%d", seed, flowAvailable, finAvailable)
		}
		if baseline == nil {
			baseline = got.Outcomes
		} else if !reflect.DeepEqual(got.Outcomes, baseline) {
			t.Fatalf("seed=%d：同一候选集合乱序后最终评分事实发生变化\nwant=%+v\ngot=%+v", seed, baseline, got.Outcomes)
		}
	}
}

func TestScorePoolRefillSecondRoundUsesDeterministicPreheat(t *testing.T) {
	setupTestDB(t)
	oldF10 := fetchF10
	t.Cleanup(func() { fetchF10 = oldF10 })
	basePool := recScorePoolCandidates(maxScanCandidates + 1)
	failSymbol := basePool[0].Symbol
	refillSymbol := basePool[len(basePool)-1].Symbol
	basePool[len(basePool)-1].Excluded = poolFullPrefix + "，等待补位"
	now := time.Now().In(time.Local)

	type runResult struct {
		FinCalls  []string
		FlowCalls []string
		Outcomes  map[string]recScoreOutcome
	}
	run := func(seed int64) runResult {
		resetRecommendationPreheatState(t)
		// 第一轮候选全部已有 fresh 缓存；一只日线失败释放名额后，唯一 poolFull
		// 候选在第二轮变为冷缓存，必须独立执行同一两阶段规划并使用剩余预算。
		for i := 0; i < maxScanCandidates; i++ {
			seedRecFreshFinance(t, basePool[i].Symbol, now)
			seedRecFreshFlow(t, basePool[i].Symbol, now)
		}
		pool := shuffledCandidates(basePool, seed)
		var mu sync.Mutex
		var finCalls, flowCalls []string
		fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
			mu.Lock()
			finCalls = append(finCalls, symbol)
			mu.Unlock()
			return []datasource.DcRow{f10Row(t, "2025-12-31", "2025年报", 15, 12, 18)}, nil
		}
		adapter := &recPreheatMarketAdapter{failDaily: map[string]bool{failSymbol: true}}
		svc := NewRecommendationService(NewMarketService(datasource.NewManagerWithAdapters(adapter)), nil, nil)
		svc.em.SetFetchForTest(func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
			symbol := recFlowSymbol(rawURL)
			mu.Lock()
			flowCalls = append(flowCalls, symbol)
			mu.Unlock()
			return recFreshFlowBody(symbol, now), 200, nil
		})
		svc.scorePool(context.Background(), model.RecTypeLongTerm, &longStrategies[0], pool, RecFilters{}, map[string]string{})
		return runResult{
			FinCalls: append([]string(nil), finCalls...), FlowCalls: append([]string(nil), flowCalls...),
			Outcomes: recScoreOutcomes(pool),
		}
	}

	var baseline map[string]recScoreOutcome
	for seed := int64(0); seed < 4; seed++ {
		got := run(seed)
		if want := []string{refillSymbol}; !reflect.DeepEqual(got.FinCalls, want) {
			t.Fatalf("seed=%d：第二轮财务补拉应仅含 %v，得到 %v", seed, want, got.FinCalls)
		}
		if want := []string{refillSymbol}; !reflect.DeepEqual(got.FlowCalls, want) {
			t.Fatalf("seed=%d：第二轮资金流补拉应仅含 %v，得到 %v", seed, want, got.FlowCalls)
		}
		if row := got.Outcomes[failSymbol]; !strings.Contains(row.Excluded, "日线数据获取失败") {
			t.Fatalf("日线失败候选应透明排除，得到 %+v", row)
		}
		if row := got.Outcomes[refillSymbol]; row.Excluded != "" || row.FlowStatus != recEnrichmentAvailable || row.FinStatus != recEnrichmentAvailable {
			t.Fatalf("第二轮补位候选应完成统一富化，得到 %+v", row)
		}
		if baseline == nil {
			baseline = got.Outcomes
		} else if !reflect.DeepEqual(got.Outcomes, baseline) {
			t.Fatalf("seed=%d：第二轮补位结果受遍历顺序影响\nwant=%+v\ngot=%+v", seed, baseline, got.Outcomes)
		}
	}
}
