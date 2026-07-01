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
	picks, err := parseAndFilterPicks(content, pool)
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
	if _, err := parseAndFilterPicks(content, pool); err == nil {
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
	picks, err := parseAndFilterPicks(content, pool)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(picks) != 1 {
		t.Fatalf("重复标的应去重为 1，得到 %d", len(picks))
	}
}

// TestNormalizePick_Clamps action/confidence/负价 归一。
func TestNormalizePick_Clamps(t *testing.T) {
	p := normalizePick(recPick{
		Action: "STRONG_BUY", Confidence: 130, StopLoss: -5, TakeProfit: 12.345,
	}, "600000")
	if p.Action != model.RecActionWatch {
		t.Fatalf("非法 action 应回退 watch，得到 %s", p.Action)
	}
	if p.Confidence != 100 {
		t.Fatalf("confidence 应钳制到 100，得到 %d", p.Confidence)
	}
	if p.StopLoss != 0 {
		t.Fatalf("负价应归零，得到 %v", p.StopLoss)
	}
	if p.TakeProfit != 12.35 && p.TakeProfit != 12.34 {
		t.Fatalf("价格应两位小数，得到 %v", p.TakeProfit)
	}
	if p.Disclaimer == "" {
		t.Fatalf("空 disclaimer 应回退默认")
	}
	if p.Reason == nil || p.Risks == nil || p.Evidence == nil || p.KeyMetrics == nil {
		t.Fatalf("数组字段应兜底非 nil")
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
