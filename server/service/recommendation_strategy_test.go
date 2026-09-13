package service

import (
	"strconv"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestStrategiesForUserCatalog 推荐工作台下拉包含内置推荐策略 + 选股页全部策略
// （内置选股/新手模板/本人自建；他人与已归档策略不列出），key 命名空间不冲突且 ≤64。
func TestStrategiesForUserCatalog(t *testing.T) {
	setupTestDB(t)
	mine := model.ScreenerStrategy{UserID: 7, Name: "我的策略", Period: "short", Risk: "high", TreeJSON: `{"all":[{"factor":"chg_pct","op":">","value":2}]}`}
	if err := common.DB.Create(&mine).Error; err != nil {
		t.Fatal(err)
	}
	rev := model.ScreenerStrategyRevision{UserID: 7, StrategyID: mine.ID, Revision: 1, ContentHash: "h", Name: mine.Name, Period: "short", Risk: "high", TreeJSON: mine.TreeJSON}
	if err := common.DB.Create(&rev).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&mine).Update("current_revision_id", rev.ID).Error; err != nil {
		t.Fatal(err)
	}
	other := model.ScreenerStrategy{UserID: 8, Name: "别人的", Period: "mid", Risk: "low", TreeJSON: mine.TreeJSON, CurrentRevisionID: 1}
	common.DB.Create(&other)

	list, err := StrategiesForUser(7, model.RecTypeShortTerm)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	groups := map[string]int{}
	for _, s := range list {
		if seen[s.Key] {
			t.Fatalf("key 重复: %s", s.Key)
		}
		seen[s.Key] = true
		if len(s.Key) > 64 {
			t.Fatalf("key 超长: %s", s.Key)
		}
		if s.guide != "" || s.baseKey != "" || s.screen != nil {
			t.Fatalf("下拉视图泄漏内部字段: %+v", s)
		}
		groups[s.Group]++
	}
	if groups["rec"] != 3 || groups["screen"] != len(builtinScreens) || groups["template"] != len(retailTemplates) || groups["custom"] != 1 {
		t.Fatalf("分组数量异常: %+v", groups)
	}
	if !seen["screen:u"+strconv.FormatInt(mine.ID, 10)] || seen["screen:u"+strconv.FormatInt(other.ID, 10)] {
		t.Fatalf("自建策略隔离失败: %v", seen)
	}
	// 短线目录里选股类策略按 short→swing→mid 排在同组内。
	lastPeriod := -1
	order := map[string]int{"short": 0, "swing": 1, "mid": 2}
	for _, s := range list {
		if s.Group != "screen" {
			continue
		}
		if p := order[s.Period]; p < lastPeriod {
			t.Fatalf("内置选股策略周期排序错乱: %s(%s)", s.Key, s.Period)
		} else {
			lastPeriod = p
		}
	}
	// 归档后不再列出，但历史批次仍可解析（重试/回显）。
	if err := (&ScreenerService{}).DeleteStrategy(7, mine.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = StrategiesForUser(7, model.RecTypeShortTerm)
	for _, s := range list {
		if s.Key == "screen:u"+strconv.FormatInt(mine.ID, 10) {
			t.Fatalf("归档策略仍出现在下拉")
		}
	}
	strat, err := resolveRecStrategy(7, model.RecTypeShortTerm, "screen:u"+strconv.FormatInt(mine.ID, 10))
	if err != nil || strat.screen == nil || strat.screen.strategyID != mine.ID || strat.tree == nil {
		t.Fatalf("归档自建策略应仍可解析: %+v %v", strat, err)
	}
	if _, err := resolveRecStrategy(8, model.RecTypeShortTerm, "screen:u"+strconv.FormatInt(mine.ID, 10)); err == nil {
		t.Fatalf("他人不得解析我的自建策略")
	}
	if strat.baseKey != "balanced" || !strings.Contains(strat.guide, "我的策略") {
		t.Fatalf("自建策略基础映射/导向异常: base=%s guide=%s", strat.baseKey, strat.guide)
	}
}

// TestResolveRecStrategyBuiltinScreen 内置选股策略/新手模板作为推荐策略：key 解析、
// 明确的评分配置、非法 key 报错。
func TestResolveRecStrategyBuiltinScreen(t *testing.T) {
	cases := []struct{ recType, key, base string }{
		{model.RecTypeShortTerm, "screen:vol-break-20d", "momentum"},
		{model.RecTypeShortTerm, "screen:shrink-pullback-ma20", "pullback"},
		{model.RecTypeShortTerm, "screen:macd-gold-water", "momentum"},  // 水上金叉侧重趋势
		{model.RecTypeShortTerm, "screen:bull-align-trend", "momentum"}, // 多头排列侧重趋势
		{model.RecTypeLongTerm, "screen:bull-align-trend", "momentum"},  // 周期不改变趋势侧重
		{model.RecTypeLongTerm, "screen:vol-break-20d", "momentum"},
		{model.RecTypeLongTerm, "screen:year-line-stand", "pullback"},
		{model.RecTypeLongTerm, "tpl:low-price-steady", "pullback"},
		{model.RecTypeShortTerm, "tpl:volume-breakout", "momentum"},
	}
	for _, c := range cases {
		s, err := resolveRecStrategy(1, c.recType, c.key)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.recType, c.key, err)
		}
		if s.baseKey != c.base || s.screen == nil || s.tree == nil || s.Key != c.key {
			t.Fatalf("%s/%s: base=%s screen=%v tree=%v", c.recType, c.key, s.baseKey, s.screen, s.tree != nil)
		}
		req := s.scanRequest(10)
		if (req.StrategyKey == "") == (req.TemplateKey == "") {
			t.Fatalf("%s: scanRequest 必须恰好一种来源: %+v", c.key, req)
		}
		if !strings.Contains(s.guide, "strategy_signal") || !strings.Contains(s.guide, s.Name) {
			t.Fatalf("%s: prompt 导向缺失: %s", c.key, s.guide)
		}
	}
	for _, bad := range []string{"screen:nope", "tpl:nope", "screen:u0", "screen:uabc", "value"} {
		if _, err := resolveRecStrategy(1, model.RecTypeShortTerm, bad); err == nil {
			t.Fatalf("非法 key %q 应报错", bad)
		}
	}
	// 内置推荐策略仍按原路解析，且 baseKey=自身。
	s, err := resolveRecStrategy(1, model.RecTypeLongTerm, "value")
	if err != nil || s.baseKey != "value" || s.screen != nil {
		t.Fatalf("内置推荐策略解析异常: %+v %v", s, err)
	}
}

// TestEvaluateStrategyHitAndBonus 条件命中评估与选股引擎同口径；加分随命中度分档。
func TestEvaluateStrategyHitAndBonus(t *testing.T) {
	strat, err := resolveRecStrategy(1, model.RecTypeLongTerm, "screen:bull-align-trend")
	if err != nil {
		t.Fatal(err)
	}
	trend := genTrendBars(80, 10, 0.4) // 稳步上行：多头排列 + 站上 MA60
	hit := evaluateStrategyHit(strat, "600100", wideStockMeta{Name: "甲股"}, trend)
	if hit == nil || !hit.Full || hit.Hit != hit.Total || hit.Total != 3 {
		t.Fatalf("上升趋势应全部命中: %+v", hit)
	}
	// 与选股引擎对拍：同一根日线在宽表求值也应命中。
	vals := computeWideRow("600100", wideStockMeta{Name: "甲股"}, trend)
	if !evalCondRow(singleRowFactorTable(vals), strat.tree, 0) {
		t.Fatalf("宽表求值与命中评估不一致")
	}
	d, notes := screenStrategyBonus(hit)
	if d != 12 || len(notes) != 1 {
		t.Fatalf("全中应 +12: %v %v", d, notes)
	}
	down := genTrendBars(80, 10, -0.4)
	hit = evaluateStrategyHit(strat, "600200", wideStockMeta{}, down)
	if hit == nil || hit.Full || len(hit.Missed) == 0 {
		t.Fatalf("下跌趋势不应全中: %+v", hit)
	}
	if d, _ := screenStrategyBonus(hit); d > 0 {
		t.Fatalf("多数未命中不应加分: %v (%+v)", d, hit)
	}
	// 非选股类策略 / 无日线：nil，且加分为 0。
	base, _ := resolveRecStrategy(1, model.RecTypeLongTerm, "value")
	if evaluateStrategyHit(base, "600100", wideStockMeta{}, trend) != nil {
		t.Fatalf("内置推荐策略不评估命中")
	}
	if evaluateStrategyHit(strat, "600100", wideStockMeta{}, nil) != nil {
		t.Fatalf("无日线应 nil")
	}
	if d, n := screenStrategyBonus(nil); d != 0 || n != nil {
		t.Fatalf("nil 命中应零分")
	}
	// any 组作为一个单元。
	anyStrat := &strategyTemplate{screen: &recScreenBinding{builtinKey: "x"}, tree: &CondNode{All: []CondNode{
		{Any: []CondNode{leafV("chg_pct", ">", 1000), leafTrue("above_ma20")}},
		leafV("close", ">", 0),
	}}}
	hit = evaluateStrategyHit(anyStrat, "600100", wideStockMeta{}, trend)
	if hit == nil || hit.Total != 2 || !hit.Full {
		t.Fatalf("any 组应计 1 单元且命中: %+v", hit)
	}
}
