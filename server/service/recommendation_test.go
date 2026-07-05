package service

import (
	"encoding/json"
	"fmt"
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
	picks, _, err := parseAndFilterPicks(content, pool, 5)
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

// TestParseAndFilterPicks_Rejected 落选理由：仅认池内且未入选的标的，越池/已入选/空理由剔除。
func TestParseAndFilterPicks_Rejected(t *testing.T) {
	pool := testPool()
	content := `{"picks":[
		{"symbol":"600000","action":"buy","confidence":70}
	],"rejected":[
		{"symbol":"000001","reason":"量价背离，与动量策略不符"},
		{"symbol":"600000","reason":"已入选不应出现在落选"},
		{"symbol":"999999","reason":"池外杜撰"},
		{"symbol":"000001","reason":"重复条目"},
		{"symbol":"","reason":"空代码"}
	]}`
	_, rejected, err := parseAndFilterPicks(content, pool, 5)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(rejected) != 1 {
		t.Fatalf("期望仅保留 1 条有效落选理由，得到 %d: %+v", len(rejected), rejected)
	}
	if rejected[0].Symbol != "000001" || rejected[0].Name != "平安银行" {
		t.Fatalf("落选条目应为池内 000001 并补齐名称: %+v", rejected[0])
	}
}

// TestParseAndFilterPicks_RejectedMissing 模型未给 rejected 不构成不合格（best-effort）。
func TestParseAndFilterPicks_RejectedMissing(t *testing.T) {
	pool := testPool()
	content := `{"picks":[{"symbol":"600000","action":"buy","confidence":70}]}`
	picks, rejected, err := parseAndFilterPicks(content, pool, 5)
	if err != nil || len(picks) != 1 {
		t.Fatalf("缺 rejected 不应报错: err=%v picks=%d", err, len(picks))
	}
	if len(rejected) != 0 {
		t.Fatalf("无 rejected 时应为空，得到 %d", len(rejected))
	}
}

// TestParseAndFilterPicks_AllFabricated 全部越池 → 返回错误（触发 repair/降级）。
func TestParseAndFilterPicks_AllFabricated(t *testing.T) {
	pool := testPool()
	content := `{"picks":[{"symbol":"AAAAAA","action":"buy","confidence":80}]}`
	if _, _, err := parseAndFilterPicks(content, pool, 5); err == nil {
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
	picks, _, err := parseAndFilterPicks(content, pool, 5)
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
	picks, _, err := parseAndFilterPicks(content, pool, 3)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(picks) != 3 {
		t.Fatalf("用户请求 3 条时应截断到 3，得到 %d", len(picks))
	}
}

// TestParseAndFilterPicks_EmptyPicksLegal 显式空 picks = 模型行使「宁缺毋滥」拒选权：
// 不报错（不得触发 repair 强迫硬凑标的），rejected 照常保留供展示落选理由。
func TestParseAndFilterPicks_EmptyPicksLegal(t *testing.T) {
	pool := testPool()
	content := `{"picks":[],"rejected":[{"symbol":"000001","reason":"量价背离，短线无安全买点"}]}`
	picks, rejected, err := parseAndFilterPicks(content, pool, 5)
	if err != nil {
		t.Fatalf("显式空 picks 是合法拒选，不应报错: %v", err)
	}
	if len(picks) != 0 {
		t.Fatalf("picks 应为空，得到 %d", len(picks))
	}
	if len(rejected) != 1 || rejected[0].Symbol != "000001" {
		t.Fatalf("拒选时落选理由应保留: %+v", rejected)
	}
}

// TestParseAndFilterPicks_MissingPicksField 缺 picks 字段 ≠ 显式空数组：
// 模型没按 schema 输出，应报错触发 repair（指针语义区分两者）。
func TestParseAndFilterPicks_MissingPicksField(t *testing.T) {
	pool := testPool()
	if _, _, err := parseAndFilterPicks(`{"rejected":[]}`, pool, 5); err == nil {
		t.Fatalf("缺 picks 字段应报错（区别于显式空数组的合法拒选）")
	}
}

// TestMarshalPoolSnapshot 池快照容量保护：≤上限原样序列化；超限时参与排名者
// 全保留、被排除者按序补位截断并计数（防大自选用户批次 INSERT 超 TEXT 容量失败）。
func TestMarshalPoolSnapshot(t *testing.T) {
	mk := func(n, excludedFrom int) []candidate {
		pool := make([]candidate, 0, n)
		for i := 0; i < n; i++ {
			c := candidate{Symbol: fmt.Sprintf("6%05d", i), Name: "x", Price: 10}
			if i >= excludedFrom {
				c.Excluded = "测试排除"
			}
			pool = append(pool, c)
		}
		return pool
	}

	// ≤150：原样序列化，无省略。
	small := mk(3, 2)
	gotJSON, omitted := marshalPoolSnapshot(small)
	wantJSON, _ := json.Marshal(small)
	if gotJSON != string(wantJSON) || omitted != 0 {
		t.Fatalf("池 ≤%d 应原样序列化且无省略: omitted=%d", poolSnapshotMax, omitted)
	}

	// >150（100 参与排名 + 60 被排除）：排名者全保留，排除者补位到上限，省略 10。
	big := mk(160, 100)
	gotJSON, omitted = marshalPoolSnapshot(big)
	if omitted != 10 {
		t.Fatalf("160 条（100 排名+60 排除）应省略 10 条，得到 %d", omitted)
	}
	var kept []candidate
	if err := json.Unmarshal([]byte(gotJSON), &kept); err != nil {
		t.Fatalf("快照应为合法 JSON: %v", err)
	}
	if len(kept) != poolSnapshotMax {
		t.Fatalf("快照条数应为上限 %d，得到 %d", poolSnapshotMax, len(kept))
	}
	keptRanked := 0
	for _, c := range kept {
		if c.Excluded == "" {
			keptRanked++
		}
	}
	if keptRanked != 100 {
		t.Fatalf("参与排名的 100 只应全部保留，得到 %d", keptRanked)
	}
}

// TestCandidateEligible PRD 3.6 前置筛选：ST/退市/停牌/流动性不足/黑名单剔除。
func TestCandidateEligible(t *testing.T) {
	def := defaultCandidateFilter()
	cases := []struct {
		name string
		c    candidate
		f    candidateFilter
		want bool
	}{
		{"正常标的", candidate{Symbol: "600000", Name: "浦发银行", Price: 8.5}, def, true},
		{"带成交额的活跃标的", candidate{Symbol: "600036", Name: "招商银行", Price: 35, Amount: 5e9}, def, true},
		{"ST 股剔除", candidate{Symbol: "600001", Name: "ST某某", Price: 3.2}, def, false},
		{"*ST 股剔除", candidate{Symbol: "600002", Name: "*ST某某", Price: 1.5}, def, false},
		{"退市整理剔除", candidate{Symbol: "600003", Name: "某某退", Price: 0.8}, def, false},
		{"停牌/无行情剔除", candidate{Symbol: "600004", Name: "某某股份", Price: 0}, def, false},
		{"流动性不足剔除", candidate{Symbol: "600005", Name: "某某股份", Price: 5, Amount: 3e7}, def, false},
		{"无成交额数据不按流动性剔除", candidate{Symbol: "600006", Name: "某某股份", Price: 5}, def, true},
		{"黑名单剔除", candidate{Symbol: "600007", Market: "cn", Name: "某某股份", Price: 5},
			candidateFilter{blacklist: map[string]bool{"cn:600007": true}, minAmount: minCandidateAmount}, false},
		{"用户调高门槛后剔除", candidate{Symbol: "600008", Name: "某某股份", Price: 5, Amount: 2e8},
			candidateFilter{minAmount: 5e8}, false},
		{"门槛 0 表示不过滤", candidate{Symbol: "600009", Name: "某某股份", Price: 5, Amount: 3e7},
			candidateFilter{minAmount: 0}, true},
	}
	for _, tc := range cases {
		if got := candidateEligible(tc.c, tc.f); got != tc.want {
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

// TestRecommendationPositionLink 血缘：一键建仓写入 recommendation_id 后，
// Get 详情应附带对应持仓（仅本人可见；多笔取最早一笔）。
func TestRecommendationPositionLink(t *testing.T) {
	setupTestDB(t)
	svc := &RecommendationService{}

	batch := &model.RecommendationBatch{UserID: 1, Type: model.RecTypeShortTerm, Market: "cn", Status: model.RecStatusSuccess}
	common.DB.Create(batch)
	item := &model.Recommendation{BatchID: batch.ID, UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行", RefPrice: 8.5}
	common.DB.Create(item)

	// 未建仓时无血缘。
	v, err := svc.Get(1, batch.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if v.Items[0].Position != nil {
		t.Fatalf("未建仓时不应附带持仓")
	}

	// 两笔建仓（先后），应取最早一笔；他人的同 recommendation_id 不可见。
	common.DB.Create(&model.Position{UserID: 1, Symbol: "600000", Market: "cn", Status: model.PositionStatusHolding,
		BuyPrice: 8.6, BuyDate: "2026-07-01", Quantity: 1000, RecommendationID: item.ID})
	common.DB.Create(&model.Position{UserID: 1, Symbol: "600000", Market: "cn", Status: model.PositionStatusHolding,
		BuyPrice: 8.8, BuyDate: "2026-07-02", Quantity: 500, RecommendationID: item.ID})
	common.DB.Create(&model.Position{UserID: 2, Symbol: "600000", Market: "cn", Status: model.PositionStatusHolding,
		BuyPrice: 9.9, Quantity: 100, RecommendationID: item.ID})

	v, err = svc.Get(1, batch.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	pl := v.Items[0].Position
	if pl == nil {
		t.Fatalf("建仓后应附带持仓血缘")
	}
	if pl.BuyPrice != 8.6 || pl.Quantity != 1000 {
		t.Fatalf("多笔建仓应取最早一笔（8.6×1000），得到 %+v", pl)
	}
}

// TestResolveRecommendationLink 建仓血缘归属校验：
// 本人推荐落血缘；他人/不存在的推荐静默清零（不阻断建仓）。
func TestResolveRecommendationLink(t *testing.T) {
	setupTestDB(t)

	rec := &model.Recommendation{BatchID: 1, UserID: 1, Symbol: "600000", Market: "cn"}
	common.DB.Create(rec)

	if got := resolveRecommendationLink(1, rec.ID); got != rec.ID {
		t.Fatalf("本人推荐应保留血缘，得到 %d", got)
	}
	if got := resolveRecommendationLink(2, rec.ID); got != 0 {
		t.Fatalf("他人推荐应清零，得到 %d", got)
	}
	if got := resolveRecommendationLink(1, 99999); got != 0 {
		t.Fatalf("不存在的推荐应清零，得到 %d", got)
	}
	if got := resolveRecommendationLink(1, 0); got != 0 {
		t.Fatalf("0 应原样返回 0，得到 %d", got)
	}
}

// TestLoadCandidateFilter 用户偏好中的回避规则加载：黑名单与门槛生效、缺偏好回退默认。
func TestLoadCandidateFilter(t *testing.T) {
	setupTestDB(t)

	// 无偏好行：回退默认（1e8 门槛、无黑名单）。
	f := loadCandidateFilter(42)
	if f.minAmount != minCandidateAmount || len(f.blacklist) != 0 {
		t.Fatalf("缺偏好应回退默认: %+v", f)
	}

	common.DB.Create(&model.UserPreference{
		UserID: 1, RiskLevel: "balanced", DefaultMarket: "cn", HorizonPref: "long_term", DefaultRecCount: 3,
		BlacklistJSON: `[{"symbol":"600000","market":"cn","reason":"历史亏损严重"}]`, MinCandidateAmount: 5e8,
	})
	f = loadCandidateFilter(1)
	if f.minAmount != 5e8 {
		t.Fatalf("门槛应为用户配置 5e8，得到 %v", f.minAmount)
	}
	if !f.blacklist["cn:600000"] {
		t.Fatalf("黑名单应含 cn:600000: %+v", f.blacklist)
	}
	if !candidateEligible(candidate{Symbol: "000001", Market: "cn", Name: "平安银行", Price: 11}, f) {
		t.Fatalf("非黑名单标的应通过")
	}
	if candidateEligible(candidate{Symbol: "600000", Market: "cn", Name: "浦发银行", Price: 8.5}, f) {
		t.Fatalf("黑名单标的应被剔除")
	}
}

// TestNormalizeBlacklist 黑名单归一化：去空/去重/市场缺省/理由截断/坏格式报错。
func TestNormalizeBlacklist(t *testing.T) {
	if s, err := normalizeBlacklist(""); err != nil || s != "" {
		t.Fatalf("空串应通过并存空: %v %q", err, s)
	}
	if s, err := normalizeBlacklist("[]"); err != nil || s != "" {
		t.Fatalf("空数组应存空: %v %q", err, s)
	}
	if _, err := normalizeBlacklist("{bad"); err == nil {
		t.Fatalf("坏格式应报错")
	}
	s, err := normalizeBlacklist(`[{"symbol":" 600000 ","reason":"r1"},{"symbol":"600000","market":"cn","reason":"dup"},{"symbol":""}]`)
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	var entries []BlacklistEntry
	if json.Unmarshal([]byte(s), &entries) != nil || len(entries) != 1 {
		t.Fatalf("应去空去重剩 1 条: %s", s)
	}
	if entries[0].Symbol != "600000" || entries[0].Market != "cn" {
		t.Fatalf("应 trim symbol 并缺省 market=cn: %+v", entries[0])
	}
}
