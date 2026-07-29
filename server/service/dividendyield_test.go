package service

import (
	"math"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// C10 股息率口径测试（第六十二批）。
//
// 本组锁定三条不可退化的诚实性：
//  1. `dividend_yield = 0` 的方案行（预案/纯送转，上游不给股息率）**绝不能被选中**
//     ——0 是「这条方案没有股息率」不是「股息率为零」；
//  2. 过期报告期（超回看窗口）宁可缺失也不给；
//  3. 缺失时返回 nil / 键不在 map 里，调用方按缺席处理，**不得回退 0**。

func mkAction(report, ex string, yield float64) model.CorporateAction {
	return model.CorporateAction{
		Symbol: "600000", Market: "cn", Name: "浦发银行",
		ReportDate: report, ExDate: ex, DividendYield: yield,
		Progress: model.CorpActionProgressImplemented, PlanProfile: "10派3.20元(含税)",
	}
}

// TestPickLatestDividendYield 手工验算：取最近一期**有股息率**的方案。
func TestPickLatestDividendYield(t *testing.T) {
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.Local)

	// 场景①：最新一期是预案（无股息率），应回退到上一期有股息率的年报方案，
	// **不能因为「最新一期是 0」就报 0%**。
	got := pickLatestDividendYield([]model.CorporateAction{
		mkAction("2026-06-30", "", 0),              // 中报预案，上游无股息率
		mkAction("2025-12-31", "2026-06-18", 3.25), // 年报已实施
		mkAction("2024-12-31", "2025-06-20", 2.10),
	}, now)
	if got == nil {
		t.Fatal("应取到 2025-12-31 那期的股息率")
	}
	if got.YieldPct != 3.25 || got.ReportDate != "2025-12-31" || got.ExDate != "2026-06-18" {
		t.Fatalf("应取最近一期有股息率的方案，got %+v", got)
	}
	if got.Note == "" || !strings.Contains(got.Note, "2025-12-31") {
		t.Fatalf("口径声明必须带报告期时点，got %q", got.Note)
	}

	// 场景②：全部为 0（只有送转方案）→ 缺失，不是 0%。
	if v := pickLatestDividendYield([]model.CorporateAction{
		mkAction("2025-12-31", "2026-06-18", 0),
	}, now); v != nil {
		t.Fatalf("无股息率的方案不得被选中，got %+v", v)
	}

	// 场景③：报告期超出回看窗口（800 天前）→ 缺失。
	old := now.AddDate(0, 0, -dividendYieldMaxAgeDays-1).Format("2006-01-02")
	if v := pickLatestDividendYield([]model.CorporateAction{mkAction(old, "", 5.5)}, now); v != nil {
		t.Fatalf("过期报告期不得作为当前股息率，got %+v", v)
	}
	// 边界内一天应仍可用（窗口本身不能悄悄收窄）。
	inWindow := now.AddDate(0, 0, -dividendYieldMaxAgeDays+1).Format("2006-01-02")
	if v := pickLatestDividendYield([]model.CorporateAction{mkAction(inWindow, "", 5.5)}, now); v == nil {
		t.Fatal("窗口内的报告期应可用")
	}

	// 场景④：报告期缺失的脏行不参与。
	if v := pickLatestDividendYield([]model.CorporateAction{mkAction("", "", 4.4)}, now); v != nil {
		t.Fatalf("报告期缺失的行不得被选中，got %+v", v)
	}

	// 场景⑤：空清单 → nil。
	if v := pickLatestDividendYield(nil, now); v != nil {
		t.Fatal("空清单应返回 nil")
	}

	// 场景⑥：同一报告期两次实施（中期+末期），取除权日更晚的那条。
	got = pickLatestDividendYield([]model.CorporateAction{
		mkAction("2025-12-31", "2026-03-10", 1.10),
		mkAction("2025-12-31", "2026-07-10", 2.20),
	}, now)
	if got == nil || got.YieldPct != 2.2 || got.ExDate != "2026-07-10" {
		t.Fatalf("同报告期应取除权日更晚的一条，got %+v", got)
	}
}

// TestDividendYieldsForBatch 批量查询：按 symbol 归并取最新，零值行不进 map，
// **不在 map 里 = 无数据**（因子列保持 NaN），与「股息率 0」严格区分。
func TestDividendYieldsForBatch(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.Local)

	rows := []model.CorporateAction{
		{Symbol: "600000", Market: "cn", ReportDate: "2025-12-31", ExDate: "2026-06-18", DividendYield: 3.25},
		{Symbol: "600000", Market: "cn", ReportDate: "2024-12-31", ExDate: "2025-06-20", DividendYield: 2.10},
		{Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ExDate: "2026-06-25", DividendYield: 1.88},
		// 只有送转、无股息率：该股整只都不该出现在结果里。
		{Symbol: "300750", Market: "cn", ReportDate: "2025-12-31", ExDate: "2026-05-11", DividendYield: 0},
	}
	for i := range rows {
		if err := common.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("建方案失败: %v", err)
		}
	}

	all := DividendYieldsFor(nil, now)
	if len(all) != 2 {
		t.Fatalf("应只有两只票有股息率，got %d: %+v", len(all), all)
	}
	if all["600000"] != 3.25 {
		t.Fatalf("600000 应取最新报告期 3.25，got %v", all["600000"])
	}
	if _, ok := all["300750"]; ok {
		t.Fatal("无股息率的票不得出现在结果里（缺失≠0%）")
	}

	// 指定 symbols 过滤生效。
	sub := DividendYieldsFor([]string{"600519"}, now)
	if len(sub) != 1 || sub["600519"] != 1.88 {
		t.Fatalf("按 symbol 过滤结果不对: %+v", sub)
	}
}

// TestStockCorpEventsDividendYield 个股解禁/分红接口带出规范化股息率；
// 读取失败（ActionUnavailable）时**不给**股息率——空结果不得冒充「无分红」。
func TestStockCorpEventsDividendYield(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now()
	report := now.AddDate(0, -7, 0).Format("2006-01-02")
	if err := common.DB.Create(&model.CorporateAction{
		Symbol: "600000", Market: "cn", Name: "浦发银行",
		ReportDate: report, ExDate: now.AddDate(0, -1, 0).Format("2006-01-02"),
		DividendYield: 4.12, DividendPretax: 3.2,
		Progress: model.CorpActionProgressImplemented, PlanProfile: "10派3.20元(含税)",
	}).Error; err != nil {
		t.Fatalf("建方案失败: %v", err)
	}

	ev, err := StockCorpEventsFor("cn", "600000")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if ev.DividendYield == nil {
		t.Fatal("应带出股息率")
	}
	if ev.DividendYield.YieldPct != 4.12 || ev.DividendYield.ReportDate != report {
		t.Fatalf("股息率或报告期不对: %+v", ev.DividendYield)
	}

	// 无任何方案的票：整项缺席（不是 0%）。
	ev2, err := StockCorpEventsFor("cn", "600001")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if ev2.DividendYield != nil {
		t.Fatalf("无方案的票不得给出股息率，got %+v", ev2.DividendYield)
	}

	// 非 A 股：两个维度都不可用，股息率同样缺席。
	ev3, _ := StockCorpEventsFor("us", "AAPL")
	if ev3.DividendYield != nil {
		t.Fatal("非 A 股不得给出股息率")
	}
}

// TestDivYieldFactorMissingSemantics 因子宽表的 div_yield 缺失语义：
// **没取到 → NaN（缺失），不是 0**；NaN 在条件树里连 `<=` 都不命中，
// 「本地没有这只票的分红数据」不会被「股息率 ≤ 1%」这类条件选出来。
// 同时锁住回测/IC/walk-forward 的 as-of 路径（不填 meta）恒为 NaN——
// 把今天的股息率贴到历史信号日上是未来函数。
func TestDivYieldFactorMissingSemantics(t *testing.T) {
	bars := genTrendBars(80, 10, 0.2)

	// ① 未装载（回测/IC as-of 路径的 meta 零值）→ NaN。
	vals := computeWideRow("600001", wideStockMeta{Name: "无分红数据"}, bars)
	if !math.IsNaN(wideVal(vals, "div_yield")) {
		t.Fatalf("未装载股息率时应为 NaN（缺失≠0%%），got %v", wideVal(vals, "div_yield"))
	}
	tbl := &FactorTable{
		TradeDate: "2026-07-29", Symbols: []string{"600001"}, Names: []string{"无分红数据"},
		LastDates: []string{"2026-07-29"}, cols: map[string][]float64{},
	}
	for _, d := range factorDefs {
		tbl.cols[d.Key] = []float64{vals[factorIndex[d.Key]]}
	}
	lo := 1.0
	node := CondNode{Factor: "div_yield", Op: "<=", Value: &lo}
	if evalCondRow(tbl, &node, 0) {
		t.Fatal("NaN 的 div_yield 不得被「股息率 ≤ 1%」命中（缺失≠低股息）")
	}

	// ② 已装载 → 取到值，且 round2 口径。
	vals2 := computeWideRow("600000", wideStockMeta{Name: "红利股", DivYield: 4.567, DivYieldOK: true}, bars)
	if got := wideVal(vals2, "div_yield"); got != 4.57 {
		t.Fatalf("div_yield 应为 round2 后的 4.57，got %v", got)
	}

	// ③ 真实为 0 的情况不存在——DividendYieldsFor 只收 >0 的行，
	//    故不会出现「DivYieldOK=true 且值为 0」这种把缺失伪装成 0 的组合。
	def, ok := factorByKey("div_yield")
	if !ok || def.Kind != fkPct || def.Group != "估值" {
		t.Fatalf("div_yield 注册信息不对: %+v", def)
	}
}

// TestCorpEventsBlockDividendYield AI 个股快照的股息率取值与**核验值域联动**：
//   - 最新一期是无股息率的预案时，快照给的是上一期的真实股息率而不是 0；
//   - 该数值进入证据核验值域，模型忠实引用不判幻觉；伪造值仍被拒。
func TestCorpEventsBlockDividendYield(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now()
	planReport := now.AddDate(0, -1, 0).Format("2006-01-02")   // 最新一期：预案，无股息率
	annualReport := now.AddDate(0, -8, 0).Format("2006-01-02") // 上一期：年报，有股息率
	for _, a := range []model.CorporateAction{
		{Symbol: "600000", Market: "cn", Name: "浦发银行", ReportDate: planReport, Progress: "董事会预案", DividendYield: 0},
		{Symbol: "600000", Market: "cn", Name: "浦发银行", ReportDate: annualReport,
			ExDate: now.AddDate(0, -3, 0).Format("2006-01-02"), Progress: model.CorpActionProgressImplemented,
			DividendPretax: 3.2, DividendYield: 3.25, PlanProfile: "10派3.20元(含税)"},
	} {
		row := a
		if err := common.DB.Create(&row).Error; err != nil {
			t.Fatalf("建方案失败: %v", err)
		}
	}

	block, _, _ := stockCorpEventsBlock("cn", "600000", now)
	if block == nil {
		t.Fatal("应生成 corp_events 段")
	}
	if got := block["latest_dividend_yield_pct"]; got != 3.25 {
		t.Fatalf("最新一期无股息率时应回退到上一期真实值 3.25，got %v", got)
	}
	if got := block["latest_dividend_yield_as_of"]; got != annualReport {
		t.Fatalf("股息率必须带报告期 as-of，got %v", got)
	}

	snap := map[string]any{"symbol": "600000", "corp_events": block}
	vals := snapshotLabeledValues(snap, stockFieldHints(snap))
	real := []evidenceSection{{Module: "估值", Text: "最新股息率 3.25%，每 10 股派 3.2 元。"}}
	check := verifyEvidenceLabeled(real, vals)
	if check.Total == 0 || check.UnmatchedTotal != 0 {
		t.Fatalf("真实股息率应可被引用: total=%d unmatched=%d items=%+v",
			check.Total, check.UnmatchedTotal, check.Items)
	}
	// 伪造值取 6.66：与值域里的 3.25 / 3.2 / 10（口径基数）都拉开足够距离，
	// 避免撞上 ratio_base_shares=10 的容差带（B9 已知的值域代价）。
	fake := []evidenceSection{{Module: "估值", Text: "股息率高达 6.66%。"}}
	if fc := verifyEvidenceLabeled(fake, vals); fc.Total == 0 || fc.UnmatchedTotal != fc.Total {
		t.Fatalf("伪造股息率必须未命中: %+v", fc.Items)
	}
}
