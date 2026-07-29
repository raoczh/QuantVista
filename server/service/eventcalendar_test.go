package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// B9 事件提醒与事件日历 + AI 链路联动（解禁进快照与证据核验值域）的测试。

func addDays(n int) string {
	return time.Now().AddDate(0, 0, n).Format("2006-01-02")
}

// TestEvalPosLift 解禁提醒：窗口内推、窗口外不推、同日多批合并、比例带上。
func TestEvalPosLift(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	d5, d20 := addDays(5), addDays(20)

	// 窗口内（5 天后）：两批同日解禁合并成一条。
	rows := []model.RestrictedRelease{
		{Symbol: "600000", Name: "浦发银行", FreeDate: d5, FreeType: "首发原股东限售股份",
			FreeShares: 1e7, LiftMarketCap: 1e8, FreeRatio: 6},
		{Symbol: "600000", Name: "浦发银行", FreeDate: d5, FreeType: "定向增发机构配售股份",
			FreeShares: 5e6, LiftMarketCap: 5e7, FreeRatio: 3},
	}
	h := evalPosLift("600000", "浦发银行", rows, today)
	if h == nil {
		t.Fatal("窗口内解禁应命中")
	}
	if h.Kind != model.GuardKindPosLift || h.EventDate != d5 {
		t.Fatalf("事件类型/去重日期错: %+v", h)
	}
	// 合并后 1500 万股、1.5 亿元、占流通 9%。
	if !strings.Contains(h.Message, "1500 万股") || !strings.Contains(h.Message, "1.50 亿元") ||
		!strings.Contains(h.Message, "9.00%") {
		t.Fatalf("同日多批未合并或数字错: %s", h.Message)
	}
	if !strings.Contains(h.Message, "首发原股东限售股份") || !strings.Contains(h.Message, "定向增发机构配售股份") {
		t.Fatalf("解禁类型未合并进文案: %s", h.Message)
	}

	// 窗口外（20 天后 > guardLiftAheadDays=10）：不推。
	if h := evalPosLift("600000", "浦发银行",
		[]model.RestrictedRelease{{Symbol: "600000", FreeDate: d20, FreeShares: 1e7}}, today); h != nil {
		t.Fatalf("窗口外解禁不应推送: %+v", h)
	}
	// 已过去：不推。
	if h := evalPosLift("600000", "浦发银行",
		[]model.RestrictedRelease{{Symbol: "600000", FreeDate: addDays(-1), FreeShares: 1e7}}, today); h != nil {
		t.Fatalf("已过去的解禁不应推送: %+v", h)
	}
	// 空清单：不推。
	if h := evalPosLift("600000", "浦发银行", nil, today); h != nil {
		t.Fatal("无解禁不应推送")
	}
	// 窗口内有多个解禁日时取最近的一个。
	multi := []model.RestrictedRelease{
		{Symbol: "600000", FreeDate: addDays(8), FreeShares: 2e7, FreeRatio: 12},
		{Symbol: "600000", FreeDate: addDays(3), FreeShares: 1e7, FreeRatio: 5},
	}
	h = evalPosLift("600000", "浦发银行", multi, today)
	if h == nil || h.EventDate != addDays(3) {
		t.Fatalf("应取窗口内最近的解禁日: %+v", h)
	}
}

// TestEvalPosExDiv 除权除息提醒：提前 3 天内推、空方案不推、文案点明账面会变。
func TestEvalPosExDiv(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	d2 := addDays(2)

	rows := []model.CorporateAction{
		{Symbol: "600000", Name: "浦发银行", ExDate: d2, TransferRatio: 10, DividendPretax: 1,
			PlanProfile: "10转10派1.00元(含税)"},
	}
	h := evalPosExDiv("600000", "浦发银行", rows, today)
	if h == nil || h.Kind != model.GuardKindPosExDiv || h.EventDate != d2 {
		t.Fatalf("窗口内除权应命中: %+v", h)
	}
	if !strings.Contains(h.Message, "10转10派1.00元(含税)") || !strings.Contains(h.Message, "折算") {
		t.Fatalf("文案应含方案与折算提示: %s", h.Message)
	}

	// 窗口外（5 天后 > guardExDivAheadDays=3）。
	if h := evalPosExDiv("600000", "浦发银行",
		[]model.CorporateAction{{Symbol: "600000", ExDate: addDays(5), TransferRatio: 10}}, today); h != nil {
		t.Fatalf("窗口外除权不应推送: %+v", h)
	}
	// 空方案（送转派全 0，如「不分配」）：不推。
	if h := evalPosExDiv("600000", "浦发银行",
		[]model.CorporateAction{{Symbol: "600000", ExDate: d2}}, today); h != nil {
		t.Fatalf("空方案不应推送: %+v", h)
	}
	// 未定除权日（预案阶段）：不推。
	if h := evalPosExDiv("600000", "浦发银行",
		[]model.CorporateAction{{Symbol: "600000", ExDate: "", TransferRatio: 10}}, today); h != nil {
		t.Fatalf("未定除权日不应推送: %+v", h)
	}
}

// TestEvalIpoToday 打新提醒：当日申购的才推，Symbol 用申购代码（同日多只各一条）。
func TestEvalIpoToday(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	rows := []model.IpoSubscription{
		{Kind: model.IpoKindStock, Code: "301717", Name: "超纯应材", ApplyCode: "301717",
			ApplyDate: today, ApplyUpper: 5000, Board: "深交所创业板"},
		{Kind: model.IpoKindCb, Code: "113709", Name: "振26转债", ApplyCode: "754067",
			ApplyDate: today, IssuePrice: 100, StockCode: "603067", StockName: "振华股份", Rating: "AA"},
		{Kind: model.IpoKindStock, Code: "603123", Name: "明日新股", ApplyCode: "732123",
			ApplyDate: addDays(1)},
	}
	hits := evalIpoToday(rows, today)
	if len(hits) != 2 {
		t.Fatalf("今日应有 2 条打新（明日的不算）: %d", len(hits))
	}
	// Symbol=申购代码，保证同日多只各自去重。
	if hits[0].Symbol != "301717" || hits[1].Symbol != "754067" {
		t.Fatalf("Symbol 应为申购代码: %q %q", hits[0].Symbol, hits[1].Symbol)
	}
	if !strings.Contains(hits[0].Message, "发行价待定") {
		t.Fatalf("未定价应如实说明: %s", hits[0].Message)
	}
	if !strings.Contains(hits[1].Message, "正股 振华股份(603067)") || !strings.Contains(hits[1].Message, "AA") {
		t.Fatalf("可转债应带正股与评级: %s", hits[1].Message)
	}
	if len(evalIpoToday(nil, today)) != 0 {
		t.Fatal("空清单不应有命中")
	}
}

// TestGuardEventDedupe 守护台账去重：同一事件重复评估只在首次算「新事件」。
// 解禁/除权/打新三类都依赖它做窗口内幂等。
func TestGuardEventDedupe(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	d5 := addDays(5)
	h := guardHit{Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.GuardKindPosLift, Message: "解禁提醒"}
	if !recordGuardEvent(1, d5, h) {
		t.Fatal("首次应记为新事件")
	}
	if recordGuardEvent(1, d5, h) {
		t.Fatal("同日同标的同类事件重复评估不应再算新事件")
	}
	// 不同用户互不影响。
	if !recordGuardEvent(2, d5, h) {
		t.Fatal("另一用户应独立记新事件")
	}
	// 不同事件日期（解禁日变更）应重新推。
	if !recordGuardEvent(1, addDays(6), h) {
		t.Fatal("事件日期变更应重新算新事件")
	}
}

// TestEventCalendar 事件日历：持仓/自选/全市场三类关系、排序、窗口边界、用户隔离。
func TestEventCalendar(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")

	// user 1 持仓 600000、自选 000001；user 2 只有自选 600519（隔离验证）。
	common.DB.Create(&model.Position{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100})
	common.DB.Create(&model.WatchlistItem{UserID: 1, WatchlistID: 1, Symbol: "000001", Market: "cn", Name: "平安银行"})
	common.DB.Create(&model.WatchlistItem{UserID: 2, WatchlistID: 2, Symbol: "600519", Market: "cn", Name: "贵州茅台"})

	common.DB.Create(&model.RestrictedRelease{Symbol: "600000", Market: "cn", Name: "浦发银行",
		FreeDate: addDays(10), FreeType: "首发原股东限售股份", FreeShares: 1e7, LiftMarketCap: 1e8, FreeRatio: 6})
	common.DB.Create(&model.RestrictedRelease{Symbol: "600519", Market: "cn", Name: "贵州茅台",
		FreeDate: addDays(10), FreeType: "首发", FreeShares: 1e6, LiftMarketCap: 1e9, FreeRatio: 1})
	common.DB.Create(&model.CorporateAction{Symbol: "000001", Market: "cn", Name: "平安银行",
		ReportDate: "2025-12-31", ExDate: addDays(3), DividendPretax: 2.46, PlanProfile: "10派2.46元(含税)"})
	common.DB.Create(&model.IpoSubscription{Kind: model.IpoKindStock, Code: "301717", Name: "超纯应材",
		ApplyCode: "301717", ApplyDate: today, ApplyUpper: 5000, Board: "深交所创业板"})
	// 窗口外（40 天后 > 默认 30 天）：不应出现。
	common.DB.Create(&model.RestrictedRelease{Symbol: "600000", Market: "cn",
		FreeDate: addDays(40), FreeType: "定增", FreeShares: 1e7, LiftMarketCap: 1e8})

	res, err := EventCalendar(1, 30)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if !res.Complete {
		t.Fatalf("全部读取成功时 Complete 应为 true: %v", res.Errors)
	}
	if res.Total != 3 {
		t.Fatalf("应有 3 条事件（打新+除权+解禁），got %d: %+v", res.Total, res.Events)
	}
	// 排序：日期升序（今日打新 → 3 天后除权 → 10 天后解禁）。
	if res.Events[0].Kind != EventKindIpo || res.Events[1].Kind != EventKindExDiv ||
		res.Events[2].Kind != EventKindLift {
		t.Fatalf("排序错: %+v", res.Events)
	}
	if res.Events[0].DaysLeft != 0 || res.Events[1].DaysLeft != 3 || res.Events[2].DaysLeft != 10 {
		t.Fatalf("DaysLeft 计算错: %d %d %d",
			res.Events[0].DaysLeft, res.Events[1].DaysLeft, res.Events[2].DaysLeft)
	}
	// 关系标注：解禁=持仓、除权=自选、打新=全市场。
	if res.Events[2].Relation != EventRelPosition || res.Events[1].Relation != EventRelWatch ||
		res.Events[0].Relation != EventRelMarket {
		t.Fatalf("关系标注错: %+v", res.Events)
	}
	// **用户隔离**：user 2 的自选 600519 解禁不得出现在 user 1 的日历里。
	for _, e := range res.Events {
		if e.Symbol == "600519" {
			t.Fatalf("他人自选的事件泄漏进本用户日历: %+v", e)
		}
	}
	// 窗口边界：40 天后的解禁不在 30 天窗口内。
	for _, e := range res.Events {
		if e.DaysLeft > 30 {
			t.Fatalf("超窗口事件出现: %+v", e)
		}
	}
	// user 2 只看到自己的解禁 + 全市场打新。
	res2, _ := EventCalendar(2, 30)
	if res2.Total != 2 {
		t.Fatalf("user2 应有 2 条: %+v", res2.Events)
	}
	// days 上限钳制（不承诺我们没有的数据）。
	resBig, _ := EventCalendar(1, 999)
	if resBig.Days != eventCalendarMaxDays {
		t.Fatalf("days 应被钳制到 %d，got %d", eventCalendarMaxDays, resBig.Days)
	}
}

// TestStockCorpEventsFor 个股解禁/分红块：A 股返回数据、非 A 股显式 unavailable。
func TestStockCorpEventsFor(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	common.DB.Create(&model.RestrictedRelease{Symbol: "600000", Market: "cn", Name: "浦发银行",
		FreeDate: addDays(20), FreeType: "首发", FreeShares: 1e7, LiftMarketCap: 1e8, FreeRatio: 6})
	common.DB.Create(&model.CorporateAction{Symbol: "600000", Market: "cn", Name: "浦发银行",
		ReportDate: "2025-12-31", ExDate: addDays(3), DividendPretax: 1, DividendYield: 3.25})

	ev, err := StockCorpEventsFor("cn", "600000")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(ev.Lifts) != 1 || len(ev.Actions) != 1 {
		t.Fatalf("应各有 1 条: %+v", ev)
	}
	if ev.LiftUnavailable || ev.ActionUnavailable {
		t.Fatal("数据可用时不应标 unavailable")
	}

	// 非 A 股：**显式 unavailable**（不是「无解禁」）。
	us, _ := StockCorpEventsFor("us", "AAPL")
	if !us.LiftUnavailable || !us.ActionUnavailable || us.Note == "" {
		t.Fatalf("非 A 股应显式声明不可用: %+v", us)
	}

	// A 股但无数据：可用 + 空数组（这是有依据的「确实没有」）。
	empty, _ := StockCorpEventsFor("cn", "000002")
	if empty.LiftUnavailable || len(empty.Lifts) != 0 {
		t.Fatalf("无数据应是可用+空数组: %+v", empty)
	}
}

// TestLiftRiskFlags 解禁风险闸门：比例达阈值记 warn、未达记 info、窗口外不记。
func TestLiftRiskFlags(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	flags := evalLiftRiskFlags([]model.RestrictedRelease{
		{Symbol: "600000", FreeDate: addDays(10), FreeShares: 2e7, LiftMarketCap: 2e8, FreeRatio: 15},
		{Symbol: "600000", FreeDate: addDays(30), FreeShares: 1e6, LiftMarketCap: 1e7, FreeRatio: 2},
		{Symbol: "600000", FreeDate: addDays(90), FreeShares: 5e7, LiftMarketCap: 5e8, FreeRatio: 30},
	}, today)
	if len(flags) != 2 {
		t.Fatalf("窗口内应有 2 条（90 天后的超窗）: %+v", flags)
	}
	if flags[0].Level != "warn" || flags[0].Code != "lift_release" {
		t.Fatalf("15%% 占比应记 warn: %+v", flags[0])
	}
	if flags[1].Level != "info" {
		t.Fatalf("2%% 占比应记 info: %+v", flags[1])
	}
	if !strings.Contains(flags[0].Text, "2000 万股") || !strings.Contains(flags[0].Text, "15.00%") {
		t.Fatalf("闸门文本数字错: %s", flags[0].Text)
	}
}

// TestRiskGateNoteLiftSemantics **核心诚实性测试**：解禁「查不到」与「确实没有」
// 必须给出完全不同的声明——绝不把未知说成无解禁。
func TestRiskGateNoteLiftSemantics(t *testing.T) {
	unavailable := riskGateNoteFor(false, 0)
	none := riskGateNoteFor(true, 0)
	some := riskGateNoteFor(true, 2)

	if unavailable == none {
		t.Fatal("「数据不可用」与「确实无解禁」的声明必须不同")
	}
	if !strings.Contains(unavailable, "不可用") || !strings.Contains(unavailable, "自行核查") {
		t.Fatalf("不可用时须声明无法判断: %s", unavailable)
	}
	if strings.Contains(unavailable, "无解禁") {
		t.Fatalf("不可用时绝不能说「无解禁」: %s", unavailable)
	}
	if !strings.Contains(none, "无解禁安排") {
		t.Fatalf("确实无解禁时应给出有依据的结论: %s", none)
	}
	if !strings.Contains(some, "corp_events.lifts") {
		t.Fatalf("有解禁时应指引明细段: %s", some)
	}
	// 恒未接入的维度（质押/商誉）在三种情形下都要声明。
	for _, s := range []string{unavailable, none, some} {
		if !strings.Contains(s, "股权质押") {
			t.Fatalf("质押声明缺失: %s", s)
		}
		if strings.Contains(s, "限售解禁、商誉") {
			t.Fatalf("解禁已接入，不应再出现在「未接入」清单里: %s", s)
		}
	}
}

// TestCorpEventsEvidenceDomain **证据核验值域联动**：
// 进入快照的解禁/分红数值（股数/市值/比例/派息/股息率）必须可被模型引用而不判幻觉；
// 伪造的数值必须落在值域外被标记 unmatched。
func TestCorpEventsEvidenceDomain(t *testing.T) {
	snap := map[string]any{
		"symbol": "600000",
		"corp_events": map[string]any{
			"lifts": []map[string]any{{
				"symbol":             "600000",
				"free_date":          "2026-08-15",
				"free_type":          "首发原股东限售股份",
				"free_shares_wan":    2127.66,
				"lift_market_cap_yi": 6.94,
				"free_ratio_pct":     19.2,
				"total_ratio_pct":    16.11,
			}},
			"dividends": []map[string]any{{
				"report_date":        "2025-12-31",
				"bonus_per10":        2.0,
				"transfer_per10":     3.0,
				"dividend_per10":     1.5,
				"dividend_yield_pct": 3.25,
			}},
			"latest_dividend_yield_pct": 3.25,
			// 口径基数：模型写「每 10 股派 1.5 元」时的 10 必须在值域内，
			// 否则正确复述会被判幻觉（核验侧对「10 股」这类带单位整数不跳过）。
			"ratio_base_shares": 10,
		},
	}
	vals := snapshotLabeledValues(snap, stockFieldHints(snap))

	// 真实值可引用：模型复述解禁股数/市值/比例、股息率与派息，全部应命中值域。
	real := []evidenceSection{{Module: "风险", Text: "该股 2026-08-15 解禁 2127.66 万股，市值约 6.94 亿元，" +
		"占流通股 19.2%，占总股本 16.11%；最新股息率 3.25%，方案每 10 股送 2.0 转 3.0 派 1.5 元。"}}
	check := verifyEvidenceLabeled(real, vals)
	if check.Total == 0 {
		t.Fatal("真实解禁/分红数字应被提取核验")
	}
	if check.UnmatchedTotal != 0 {
		var miss []string
		for _, it := range check.Items {
			if !it.Matched {
				miss = append(miss, it.Raw)
			}
		}
		t.Fatalf("真实值应全部命中值域，未命中: %v", miss)
	}
	// 命中项须标为「被数据快照佐证」（Origin 空），不是复述。
	if check.SnapshotMatched != check.Matched {
		t.Fatalf("解禁/分红数值应算快照佐证: snapshot=%d matched=%d", check.SnapshotMatched, check.Matched)
	}

	// 伪造值被拒：编造的解禁比例与股息率必须落在值域外。
	fake := []evidenceSection{{Module: "风险", Text: "该股解禁 8888.88 万股，占流通股 66.66%，股息率高达 12.34%。"}}
	fakeCheck := verifyEvidenceLabeled(fake, vals)
	if fakeCheck.UnmatchedTotal != fakeCheck.Total || fakeCheck.Total == 0 {
		t.Fatalf("伪造数值必须全部未命中: total=%d unmatched=%d items=%+v",
			fakeCheck.Total, fakeCheck.UnmatchedTotal, fakeCheck.Items)
	}

	// 数据源标注：证据链要能回指「这个解禁数字是谁给的」。
	hints := stockFieldHints(snap)
	if hints == nil || hints.source["corp_events."] != "eastmoney_datacenter" {
		t.Fatalf("corp_events 段应标注数据源: %+v", hints)
	}
	// 日期不应被当成幻觉：年份 2026 与月日均在跳过规则内。
	dateOnly := []evidenceSection{{Module: "风险", Text: "解禁日为 2026-08-15。"}}
	if dc := verifyEvidenceLabeled(dateOnly, vals); dc.UnmatchedTotal != 0 {
		t.Fatalf("日期不应被判为未命中数值: %+v", dc.Items)
	}
}

// TestCorpEventsSnapshotUnknown 解禁数据不可用时快照必须显式落 unknowns，
// 否则模型看到没有 lifts 会自行脑补「无解禁风险」。
func TestCorpEventsSnapshotUnknown(t *testing.T) {
	snap := map[string]any{
		"corp_events": map[string]any{"lifts_unavailable": true, "lifts_note": "不可用"},
	}
	b, _ := json.Marshal(snap)
	var round map[string]any
	_ = json.Unmarshal(b, &round)
	ce, ok := round["corp_events"].(map[string]any)
	if !ok {
		t.Fatal("corp_events 段丢失")
	}
	if un, _ := ce["lifts_unavailable"].(bool); !un {
		t.Fatal("不可用标志必须能经 JSON 往返（问答复用落库快照的路径）")
	}
}

// TestTodoCorpAdjustAndIpo 待办聚合：除权待确认（优先级 1）与今日打新（优先级 2）都进清单。
func TestTodoCorpAdjustAndIpo(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1, today, 0, 10, 1)
	// 先把建议生成出来，再删掉持仓行：TodoService 的持仓块需要真实 MarketService 取行情
	//（`&PositionService{market: nil}` 只在无持仓时可用，见 todo_test.go 同款限制）。
	// 本用例要验的是「除权建议与打新是否接进待办」，不是持仓富化，删仓可绕开行情依赖
	// 而不影响待办里那条 pending 建议。
	if n, err := GenerateCorpAdjusts(1, today); err != nil || n != 1 {
		t.Fatalf("生成建议失败: n=%d err=%v", n, err)
	}
	common.DB.Where("id = ?", p.ID).Delete(&model.Position{})

	common.DB.Create(&model.IpoSubscription{Kind: model.IpoKindCb, Code: "113709", Name: "振26转债",
		ApplyCode: "754067", ApplyDate: today, IssuePrice: 100,
		StockCode: "603067", StockName: "振华股份", Rating: "AA"})

	svc := NewTodoService(&AlertService{}, &PositionService{market: nil}, nil)
	// 用 all 范围断言两类都在：D18 起除权折算属 ledger、打新属 market，
	// 默认范围（ledger）看不到打新——本用例验的是「有没有生成」，不是消费出口分流。
	res, err := svc.Build(context.Background(), 1, TodoScopeAll)
	if err != nil {
		t.Fatalf("待办聚合失败: %v", err)
	}
	var hasAdjust, hasIpo bool
	for _, it := range res.Items {
		switch it.Kind {
		case TodoKindCorpAdjust:
			hasAdjust = true
			if it.Priority != 1 {
				t.Fatalf("除权待确认应为最高优先级: %+v", it)
			}
			if !strings.Contains(it.Detail, "1000") || !strings.Contains(it.Detail, "2000") {
				t.Fatalf("待办应写清折算前后数量: %s", it.Detail)
			}
		case TodoKindIpo:
			hasIpo = true
			if it.Symbol != "754067" {
				t.Fatalf("打新待办应用申购代码: %+v", it)
			}
			if !strings.Contains(it.Detail, "振华股份") {
				t.Fatalf("可转债待办应带正股: %s", it.Detail)
			}
		}
	}
	if !hasAdjust {
		t.Fatal("除权待确认未进待办")
	}
	if !hasIpo {
		t.Fatal("今日打新未进待办")
	}
}

// TestLiftSignalsFor 候选池解禁信号：可用性与「窗口内确实没有」严格区分。
func TestLiftSignalsFor(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	common.DB.Create(&model.RestrictedRelease{Symbol: "600000", Market: "cn",
		FreeDate: addDays(10), FreeType: "首发", FreeShares: 2e7, LiftMarketCap: 2e8, FreeRatio: 12})
	common.DB.Create(&model.RestrictedRelease{Symbol: "600000", Market: "cn",
		FreeDate: addDays(10), FreeType: "定增", FreeShares: 1e7, LiftMarketCap: 1e8, FreeRatio: 6})
	// 窗口外的不参与。
	common.DB.Create(&model.RestrictedRelease{Symbol: "600000", Market: "cn",
		FreeDate: addDays(90), FreeType: "其它", FreeShares: 9e7, LiftMarketCap: 9e8, FreeRatio: 50})

	sigs, ok := liftSignalsFor([]string{"600000", "000001"})
	if !ok {
		t.Fatal("查询成功时 available 应为 true")
	}
	sig, has := sigs["600000"]
	if !has {
		t.Fatal("600000 应有解禁信号")
	}
	// 同日两批合并：3000 万股、3 亿元、18%。
	if sig.SharesWan != 3000 || sig.CapYi != 3 || sig.RatioPct != 18 {
		t.Fatalf("同日多批未合并: %+v", sig)
	}
	if sig.Days != 10 {
		t.Fatalf("距今天数错: %d", sig.Days)
	}
	// 000001 无解禁：表里没有它——这是「确实没有」（available=true 已表明数据可用）。
	if _, has := sigs["000001"]; has {
		t.Fatal("无解禁的标的不应出现在信号表里")
	}
	// 空入参：无意义查询，available=false（不是「都没解禁」）。
	if _, ok := liftSignalsFor(nil); ok {
		t.Fatal("空入参不应声称数据可用")
	}
}
