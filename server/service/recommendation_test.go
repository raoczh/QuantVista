package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func testPool() map[string]candidate {
	return map[string]candidate{
		"600000": {Symbol: "600000", Market: "cn", Name: "浦发银行", Price: 8.5},
		"000001": {Symbol: "000001", Market: "cn", Name: "平安银行", Price: 11.2},
	}
}

// TestParseAndFilterPicks_DropsFabricated 反编造核心：池外/杜撰标的必须被丢弃。
func TestParseAndFilterPicks_DropsFabricated(t *testing.T) {
	pool := testPool()
	content := `{"picks":[
		{"symbol":"600000","action":"buy","confidence":70,"reason":["站上均线"],"risks":["大盘系统性风险"],"evidence":["现价8.5"]},
		{"symbol":"999999","action":"buy","confidence":90,"reason":["编造的票"]},
		{"symbol":"000001","action":"watch","confidence":55,"reason":["观察"]}
	]}`
	picks, err := parseAndFilterPicks(content, pool, 5)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(picks) != 2 {
		t.Fatalf("期望丢弃池外 999999 后剩 2 个，得到 %d", len(picks))
	}
	for _, p := range picks {
		if p.Symbol == "999999" {
			t.Fatalf("池外标的 999999 未被过滤（反编造失效）")
		}
	}
}

// TestParseAndFilterPicks_AllFabricated 全部越池 → 返回错误（触发 repair/降级）。
func TestParseAndFilterPicks_AllFabricated(t *testing.T) {
	pool := testPool()
	content := `{"picks":[{"symbol":"AAAAAA","action":"buy","confidence":80}]}`
	if _, err := parseAndFilterPicks(content, pool, 5); err == nil {
		t.Fatalf("全部越池时应返回错误")
	}
}

// TestParseAndFilterPicks_Dedup 同一标的重复只保留一个。
func TestParseAndFilterPicks_Dedup(t *testing.T) {
	pool := testPool()
	content := `{"picks":[
		{"symbol":"600000","action":"buy","confidence":70},
		{"symbol":"600000","action":"watch","confidence":40}
	]}`
	picks, err := parseAndFilterPicks(content, pool, 5)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(picks) != 1 {
		t.Fatalf("重复标的应去重为 1，得到 %d", len(picks))
	}
}

// TestParseAndFilterPicks_CountCap 输出条数不得超过用户请求的 count（PRD 3.6）。
func TestParseAndFilterPicks_CountCap(t *testing.T) {
	pool := map[string]candidate{
		"600000": {Symbol: "600000", Name: "浦发银行", Price: 8.5},
		"000001": {Symbol: "000001", Name: "平安银行", Price: 11.2},
		"600036": {Symbol: "600036", Name: "招商银行", Price: 35},
		"601318": {Symbol: "601318", Name: "中国平安", Price: 50},
	}
	content := `{"picks":[
		{"symbol":"600000","action":"buy","confidence":70},
		{"symbol":"000001","action":"buy","confidence":65},
		{"symbol":"600036","action":"buy","confidence":60},
		{"symbol":"601318","action":"buy","confidence":55}
	]}`
	picks, err := parseAndFilterPicks(content, pool, 3)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(picks) != 3 {
		t.Fatalf("用户请求 3 条时应截断到 3，得到 %d", len(picks))
	}
}

// TestCandidateEligible PRD 3.6 前置筛选：ST/退市/停牌/流动性不足剔除。
func TestCandidateEligible(t *testing.T) {
	cases := []struct {
		name string
		c    candidate
		want bool
	}{
		{"正常标的", candidate{Symbol: "600000", Name: "浦发银行", Price: 8.5}, true},
		{"带成交额的活跃标的", candidate{Symbol: "600036", Name: "招商银行", Price: 35, Amount: 5e9}, true},
		{"ST 股剔除", candidate{Symbol: "600001", Name: "ST某某", Price: 3.2}, false},
		{"*ST 股剔除", candidate{Symbol: "600002", Name: "*ST某某", Price: 1.5}, false},
		{"退市整理剔除", candidate{Symbol: "600003", Name: "某某退", Price: 0.8}, false},
		{"停牌/无行情剔除", candidate{Symbol: "600004", Name: "某某股份", Price: 0}, false},
		{"流动性不足剔除", candidate{Symbol: "600005", Name: "某某股份", Price: 5, Amount: 3e7}, false},
		{"无成交额数据不按流动性剔除", candidate{Symbol: "600006", Name: "某某股份", Price: 5}, true},
	}
	for _, tc := range cases {
		if got := candidateEligible(tc.c); got != tc.want {
			t.Errorf("%s: candidateEligible=%v, 期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestNormalizePick_Clamps action/confidence/负价 归一；无效价位组合被清零（不驱动追踪）。
func TestNormalizePick_Clamps(t *testing.T) {
	p := normalizePick(recPick{
		Action: "STRONG_BUY", Confidence: 130, StopLoss: -5, TakeProfit: 12.345,
	}, "600000", candidate{Price: 10})
	if p.Action != model.RecActionWatch {
		t.Fatalf("非法 action 应回退 watch，得到 %s", p.Action)
	}
	if p.Confidence != 100 {
		t.Fatalf("confidence 应钳制到 100，得到 %d", p.Confidence)
	}
	if p.StopLoss != 0 {
		t.Fatalf("负价应归零，得到 %v", p.StopLoss)
	}
	if p.TakeProfit != 0 {
		t.Fatalf("价位组合无效（缺买入区间/止损）时应整体清零，得到 %v", p.TakeProfit)
	}
	if p.Disclaimer == "" {
		t.Fatalf("空 disclaimer 应回退默认")
	}
	if p.Reason == nil || p.Risks == nil || p.Evidence == nil || p.KeyMetrics == nil {
		t.Fatalf("数组字段应兜底非 nil")
	}
}

// TestNormalizePick_ValidShortPlan 合法短线计划保留价位并 round2。
func TestNormalizePick_ValidShortPlan(t *testing.T) {
	p := normalizePick(recPick{
		Action: "buy", Confidence: 70,
		BuyZoneLow: 9.8, BuyZoneHigh: 10.2, TakeProfit: 11.555, StopLoss: 9.2, ValidDays: 5,
	}, "600000", candidate{Price: 10})
	if p.Action != model.RecActionBuy {
		t.Fatalf("合法计划不应降级，得到 %s", p.Action)
	}
	if p.TakeProfit != 11.56 && p.TakeProfit != 11.55 {
		t.Fatalf("价格应两位小数，得到 %v", p.TakeProfit)
	}
	if p.StopLoss != 9.2 {
		t.Fatalf("止损应保留，得到 %v", p.StopLoss)
	}
}

// TestNormalizePick_DetachedPlan 价位次序合法但整体悬空于现价之外 → 降级并清零。
func TestNormalizePick_DetachedPlan(t *testing.T) {
	p := normalizePick(recPick{
		Action: "buy", Confidence: 60,
		BuyZoneLow: 12.5, BuyZoneHigh: 13, TakeProfit: 15, StopLoss: 12, ValidDays: 5,
	}, "600000", candidate{Price: 10}) // 现价 10 < 止损 12：悬空计划
	if p.Action != model.RecActionWatch {
		t.Fatalf("悬空计划应降级为观察，得到 %s", p.Action)
	}
	if p.TakeProfit != 0 || p.StopLoss != 0 {
		t.Fatalf("悬空计划价位应清零（否则追踪首日即误报止损），得到 TP=%v SL=%v", p.TakeProfit, p.StopLoss)
	}
}

// TestNormalizePick_ShortPlan 校验短线计划价位关系、有效期和交易约束提示。
func TestNormalizePick_ShortPlan(t *testing.T) {
	p := normalizePick(recPick{
		Action: "buy", Confidence: 80,
		BuyZoneLow: 10, BuyZoneHigh: 10, TakeProfit: 11, StopLoss: 9, ValidDays: 30,
	}, "600000", candidate{Price: 10.2})
	if p.Action != model.RecActionWatch {
		t.Fatalf("买入区间上下沿相等应降级为观察，得到 %s", p.Action)
	}
	if p.ValidDays != 10 {
		t.Fatalf("短线有效期应限制到 10 个交易日，得到 %d", p.ValidDays)
	}
	if len(p.Risks) == 0 {
		t.Fatalf("短线计划无效时应追加风险说明")
	}
	if len(p.Evidence) == 0 {
		t.Fatalf("短线计划应追加 A 股交易约束提示")
	}
}

// TestStrategiesFor 短线/长线策略清单不空且不外泄 guide。
func TestStrategiesFor(t *testing.T) {
	for _, tp := range []string{model.RecTypeShortTerm, model.RecTypeLongTerm} {
		ss := StrategiesFor(tp)
		if len(ss) == 0 {
			t.Fatalf("%s 策略清单不应为空", tp)
		}
		for _, s := range ss {
			if s.Key == "" || s.Name == "" {
				t.Fatalf("策略字段缺失: %+v", s)
			}
			if s.guide != "" {
				t.Fatalf("guide 不应外泄到对外清单")
			}
		}
	}
}

// TestRecommendationHistoryGetDelete DB 集成：列名/隔离/条目关联/级联删除。
func TestRecommendationHistoryGetDelete(t *testing.T) {
	setupTestDB(t)
	svc := &RecommendationService{}

	batch := &model.RecommendationBatch{
		UserID: 1, Type: model.RecTypeShortTerm, Market: "cn", Strategy: "momentum",
		Status: model.RecStatusSuccess, CandidateCount: 5, CandidatePool: `[{"symbol":"600000"}]`,
		Model: "gpt-x", Provider: "openai", PromptVersion: "p1", StrategyVersion: "s1", TotalTokens: 200,
	}
	if err := common.DB.Create(batch).Error; err != nil {
		t.Fatalf("插入批次失败: %v", err)
	}
	item := &model.Recommendation{
		BatchID: batch.ID, UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Action: model.RecActionBuy, Confidence: 70, Summary: "站上均线", RefPrice: 8.5,
		DetailJSON: `{"symbol":"600000","action":"buy","confidence":70}`, SortOrder: 0,
	}
	common.DB.Create(item)
	// 他人批次。
	common.DB.Create(&model.RecommendationBatch{UserID: 2, Type: model.RecTypeLongTerm, Status: model.RecStatusSuccess})

	// History：仅本人、不含重字段。
	rows, err := svc.History(1, "", 30)
	if err != nil {
		t.Fatalf("History 失败（列名可能拼错）: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("期望 1 条（用户隔离），得到 %d", len(rows))
	}
	if rows[0].CandidatePool != "" || rows[0].DataSnapshot != "" {
		t.Fatalf("列表不应返回重字段")
	}

	// Get：含条目 + 明细解析。
	v, err := svc.Get(1, batch.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if len(v.Items) != 1 || v.Items[0].Detail == nil || v.Items[0].Detail.Symbol != "600000" {
		t.Fatalf("条目/明细解析失败: %+v", v.Items)
	}

	// 跨用户 Get/Delete 隔离。
	if _, err := svc.Get(2, batch.ID); err == nil {
		t.Fatalf("跨用户 Get 应失败")
	}
	if err := svc.Delete(2, batch.ID); err == nil {
		t.Fatalf("跨用户 Delete 应失败")
	}

	// 本人 Delete 级联删除条目。
	if err := svc.Delete(1, batch.ID); err != nil {
		t.Fatalf("本人 Delete 应成功: %v", err)
	}
	var itemCnt int64
	common.DB.Model(&model.Recommendation{}).Where("batch_id = ?", batch.ID).Count(&itemCnt)
	if itemCnt != 0 {
		t.Fatalf("删除批次应级联删除条目，剩 %d", itemCnt)
	}
}
