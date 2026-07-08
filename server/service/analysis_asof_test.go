package service

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// M2 回溯诊断测试：as_of 快照无未来泄露（改未来数据快照不变）、缺失声明、
// 非交易日回退、hindsight 事后核验（节点收益/价位首触/评级命中/隔离）。

// seedAsOfBars 铺单只股连续上涨日线：close=10+0.1*i、high=close+0.05、low=close-0.05，
// 日期 2025-01-01 起逐自然日 n 根。内存库 cache=shared 测试间共享，进场清 +
// t.Cleanup 退场清（含本测试族落库的 AnalysisRecord），防污染其他测试。
func seedAsOfBars(t *testing.T, symbol string, n int) []string {
	t.Helper()
	cleanAsOfTables := func() {
		common.DB.Where("symbol = ?", symbol).Delete(&model.DailyBar{})
		common.DB.Where("1=1").Delete(&model.AnalysisRecord{})
	}
	cleanAsOfTables()
	t.Cleanup(cleanAsOfTables)
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dates := make([]string, n)
	for i := 0; i < n; i++ {
		c := round2(10 + 0.1*float64(i))
		dates[i] = day.AddDate(0, 0, i).Format("2006-01-02")
		row := model.DailyBar{
			Symbol: symbol, Market: "cn", TradeDate: dates[i],
			Open: round2(c - 0.02), High: round2(c + 0.05), Low: round2(c - 0.05), Close: c,
			Volume: 10000 + int64(i)*100, Amount: c * 1000000, TurnoverRate: 2, Source: "eastmoney",
		}
		if err := common.DB.Create(&row).Error; err != nil {
			t.Fatalf("铺日线失败: %v", err)
		}
	}
	return dates
}

// TestAsOfSnapshotNoFutureLeak 把 as_of 之后的日线改成疯狂值，快照必须逐字节不变。
func TestAsOfSnapshotNoFutureLeak(t *testing.T) {
	setupTestDB(t)
	dates := seedAsOfBars(t, "600100", 40)
	asOf := dates[19] // 第 20 根

	_, snap1, err := buildStockSnapshotAsOf("600100", "cn", asOf)
	if err != nil {
		t.Fatalf("构建 as_of 快照失败: %v", err)
	}
	j1, _ := json.Marshal(snap1)

	// 污染未来数据。
	if err := common.DB.Model(&model.DailyBar{}).
		Where("symbol = ? AND trade_date > ?", "600100", asOf).
		Updates(map[string]any{"close": 999.0, "high": 1000.0, "low": 998.0}).Error; err != nil {
		t.Fatal(err)
	}
	_, snap2, err := buildStockSnapshotAsOf("600100", "cn", asOf)
	if err != nil {
		t.Fatal(err)
	}
	j2, _ := json.Marshal(snap2)
	if string(j1) != string(j2) {
		t.Fatalf("as_of 快照受未来数据影响（未来泄露）:\n%s\nvs\n%s", j1, j2)
	}
}

// TestAsOfSnapshotContent 快照内容：quote 由末根合成、缺失声明恒在、
// 因子与实时链路同函数对拍（喂相同截断序列结果一致）。
func TestAsOfSnapshotContent(t *testing.T) {
	setupTestDB(t)
	dates := seedAsOfBars(t, "600100", 40)
	asOf := dates[19]

	label, snap, err := buildStockSnapshotAsOf("600100", "cn", asOf)
	if err != nil {
		t.Fatal(err)
	}
	if label == "" {
		t.Fatal("label 为空")
	}
	quote := snap["quote"].(map[string]any)
	// 第 20 根 close=10+0.1×19=11.9；prev=11.8 → chg=(11.9/11.8-1)*100=0.85。
	if quote["price"].(float64) != 11.9 {
		t.Fatalf("quote.price 应为 as_of 收盘 11.9: %v", quote["price"])
	}
	if quote["trade_date"].(string) != asOf {
		t.Fatalf("quote.trade_date 应为 %s: %v", asOf, quote["trade_date"])
	}
	if math.Abs(quote["change_pct"].(float64)-0.85) > 0.01 {
		t.Fatalf("change_pct 错误: %v", quote["change_pct"])
	}
	if _, ok := snap["unavailable_note"]; !ok {
		t.Fatal("缺失声明 unavailable_note 必须存在（估值/新闻/财务不可得）")
	}
	if snap["as_of"].(string) != asOf {
		t.Fatalf("as_of 字段错误: %v", snap["as_of"])
	}
	if _, ok := snap["as_of_note"]; ok {
		t.Fatal("as_of 恰为交易日不应有回退说明")
	}

	// 与实时链路同函数对拍：technicals 喂相同截断序列必须一致。
	bars := cnBarsUpTo("600100", asOf, asOfBarLimit)
	if len(bars) != 20 {
		t.Fatalf("截断读取应 20 根: %d", len(bars))
	}
	tech := snap["technicals"].(map[string]any)
	want := computeTechnicals(bars) // 20 根 < asOfTechBars，全量即计算窗
	wj, _ := json.Marshal(want)
	gj, _ := json.Marshal(tech)
	if string(wj) != string(gj) {
		t.Fatalf("technicals 与实时链路函数不一致:\n%s\nvs\n%s", gj, wj)
	}

	// 非交易日回退：as_of 取一个铺库范围后的日期字符串之间的空档不存在——
	// 用最后日期+1 天之外无数据场景另测；此处测中间非交易日（铺的是连续自然日
	// 无空档，改用早于首根的日期报错分支）。
	if _, _, err := buildStockSnapshotAsOf("600100", "cn", "2024-06-01"); err == nil {
		t.Fatal("首根之前的日期应报错（无日线数据）")
	}
}

// TestAsOfSnapshotFallbackNote 回溯日期为非交易日时回退最近交易日并声明。
func TestAsOfSnapshotFallbackNote(t *testing.T) {
	setupTestDB(t)
	dates := seedAsOfBars(t, "600100", 10)
	// 删除中间一根制造非交易日（2025-01-05，index 4）。
	common.DB.Where("symbol = ? AND trade_date = ?", "600100", dates[4]).Delete(&model.DailyBar{})

	_, snap, err := buildStockSnapshotAsOf("600100", "cn", dates[4])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap["as_of_note"]; !ok {
		t.Fatal("非交易日回退必须带 as_of_note 声明")
	}
	quote := snap["quote"].(map[string]any)
	if quote["trade_date"].(string) != dates[3] {
		t.Fatalf("应回退到前一交易日 %s: %v", dates[3], quote["trade_date"])
	}
}

// TestAsOfDateValidation 日期参数校验。
func TestAsOfDateValidation(t *testing.T) {
	if _, err := asOfDate("2025/01/01"); err == nil {
		t.Fatal("非法格式应报错")
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	if _, err := asOfDate(today); err == nil {
		t.Fatal("当天应报错（用实时分析）")
	}
	if d, err := asOfDate("2025-01-15"); err != nil || d != "2025-01-15" {
		t.Fatalf("合法日期被拒: %v %v", d, err)
	}
}

// TestHindsight 事后核验：节点收益/最大涨跌幅/价位首触/评级命中/用户隔离。
func TestHindsight(t *testing.T) {
	setupTestDB(t)
	dates := seedAsOfBars(t, "600100", 40)
	asOf := dates[9] // 第 10 根 close=10.9

	rec := &model.AnalysisRecord{
		UserID: 1, Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600100",
		Target: "回溯股", Title: "个股分析 · 回溯股 · 回溯@" + asOf,
		Status: model.AnalysisStatusSuccess, Rating: model.AnalysisRatingBullish,
		AsOf: asOf,
	}
	if err := common.DB.Create(rec).Error; err != nil {
		t.Fatal(err)
	}

	svc := &AnalysisService{} // market=nil：无基准，alpha 字段应缺席
	v, err := svc.Hindsight(context.Background(), 1, rec.ID, 11.0, 9.0)
	if err != nil {
		t.Fatal(err)
	}
	if v.BaseDate != asOf || v.BasePrice != 10.9 {
		t.Fatalf("基准根错误: %+v", v)
	}
	// after=bars[10:]（30 根）。d5=第 15 根 close=11.4 → (11.4/10.9-1)*100=4.59。
	d5 := v.Returns["d5"]
	if d5 == nil || math.Abs(d5.ReturnPct-4.59) > 0.01 || d5.Date != dates[14] {
		t.Fatalf("d5 节点错误: %+v", d5)
	}
	d20 := v.Returns["d20"]
	// d20=第 30 根 close=12.9 → 18.35%。
	if d20 == nil || math.Abs(d20.ReturnPct-18.35) > 0.01 {
		t.Fatalf("d20 节点错误: %+v", d20)
	}
	if v.Returns["d60"] != nil {
		t.Fatal("仅 30 根后续数据，d60 应为 null")
	}
	// 最大涨幅=末根 high 13.95 → 27.98%；连续上涨最低 low=after 首根 11.0-0.05=10.95 → 高于基准，回撤≈0。
	if math.Abs(v.MaxGainPct-27.98) > 0.05 {
		t.Fatalf("最大涨幅错误: %v", v.MaxGainPct)
	}
	if v.MaxDrawdownPct > 0.01 {
		t.Fatalf("连续上涨不应有回撤: %v", v.MaxDrawdownPct)
	}
	// 目标价 11.0 首触：after 首根（第 11 根）high=11.05 ≥ 11 → day_index=1。
	if v.TargetTouch == nil || v.TargetTouch.DayIndex != 1 || v.TargetTouch.Date != dates[10] {
		t.Fatalf("目标价首触错误: %+v", v.TargetTouch)
	}
	// 止损 9.0 永不触及。
	if v.StopTouch != nil {
		t.Fatalf("止损价不应触及: %+v", v.StopTouch)
	}
	// bullish 且 d20>0 → 命中。
	if v.RatingHit == nil || !*v.RatingHit {
		t.Fatalf("评级命中错误: %+v", v.RatingHit)
	}
	// 无基准注入时 alpha 缺席。
	if v.AlphaPct != nil {
		t.Fatal("无基准时 alpha 应缺席")
	}

	// 用户隔离。
	if _, err := svc.Hindsight(context.Background(), 2, rec.ID, 0, 0); err == nil {
		t.Fatal("跨用户核验应报错")
	}
	// 非个股模块拒绝。
	mrec := &model.AnalysisRecord{UserID: 1, Module: model.AnalysisModuleMarket, Status: model.AnalysisStatusSuccess}
	common.DB.Create(mrec)
	if _, err := svc.Hindsight(context.Background(), 1, mrec.ID, 0, 0); err == nil {
		t.Fatal("非个股模块应报错")
	}
}

// TestAnalyzeAsOfValidation Analyze 入口的 as_of 组合校验（不发起真实调用，
// 校验发生在 LLM 解析之前……实际在配置解析之后，这里只测参数组合拒绝路径）。
func TestAnalyzeAsOfValidation(t *testing.T) {
	setupTestDB(t)
	svc := &AnalysisService{llm: NewLLMService()}
	// panel + as_of 拒绝。
	_, err := svc.Analyze(context.Background(), 1, false, AnalyzeRequest{
		Module: model.AnalysisModuleStock, Mode: model.AnalysisModePanel,
		Symbol: "600100", AsOf: "2025-01-10",
	})
	if err == nil || err.Error() != "回溯诊断仅支持个股模块的标准分析" {
		t.Fatalf("panel+as_of 应拒绝: %v", err)
	}
	// 非个股模块 + as_of 拒绝。
	_, err = svc.Analyze(context.Background(), 1, false, AnalyzeRequest{
		Module: model.AnalysisModuleMarket, AsOf: "2025-01-10",
	})
	if err == nil || err.Error() != "回溯诊断仅支持个股模块的标准分析" {
		t.Fatalf("market+as_of 应拒绝: %v", err)
	}
	// 非法日期拒绝。
	_, err = svc.Analyze(context.Background(), 1, false, AnalyzeRequest{
		Module: model.AnalysisModuleStock, Symbol: "600100", AsOf: "not-a-date",
	})
	if err == nil {
		t.Fatal("非法日期应拒绝")
	}
}
