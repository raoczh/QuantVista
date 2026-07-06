package service

import (
	"fmt"
	"strings"
	"testing"

	"quantvista/datasource"
	"quantvista/model"
)

// TestSanitizeRecFilters 归一化：负值归零、区间反转交换。
func TestSanitizeRecFilters(t *testing.T) {
	f := sanitizeRecFilters(RecFilters{
		PriceMin: 30, PriceMax: 5, // 反转
		TurnoverMin: -3, TurnoverMax: 200, // 负值/越界
		FloatCapMinYi: 500, FloatCapMaxYi: 100, // 反转
		MaxGain5dPct: -1,
	})
	if f.PriceMin != 5 || f.PriceMax != 30 {
		t.Fatalf("价格区间应交换为 [5,30]，得到 [%v,%v]", f.PriceMin, f.PriceMax)
	}
	if f.TurnoverMin != 0 || f.TurnoverMax != 30 {
		t.Fatalf("换手区间应钳在绝对硬顶内 [0,30]，得到 [%v,%v]", f.TurnoverMin, f.TurnoverMax)
	}
	if f.FloatCapMinYi != 100 || f.FloatCapMaxYi != 500 {
		t.Fatalf("市值区间应交换为 [100,500]，得到 [%v,%v]", f.FloatCapMinYi, f.FloatCapMaxYi)
	}
	if f.MaxGain5dPct != 0 {
		t.Fatalf("负的追高上限应归零，得到 %v", f.MaxGain5dPct)
	}
	// 换手下限额外钳到 25（给 [min,30] 留可行带，min=30 会退化成测度零区间）。
	f2 := sanitizeRecFilters(RecFilters{TurnoverMin: 28, TurnoverMax: 29})
	if f2.TurnoverMin != 25 || f2.TurnoverMax != 29 {
		t.Fatalf("换手下限应钳到 25，得到 [%v,%v]", f2.TurnoverMin, f2.TurnoverMax)
	}
}

// TestDefaultRecFilters 短线默认带追高保护与排除涨停，长线不带追高限制。
func TestDefaultRecFilters(t *testing.T) {
	s := defaultRecFilters(model.RecTypeShortTerm)
	if s.MaxGain5dPct != 25 || !s.ExcludeLimitUp {
		t.Fatalf("短线默认应为 追高25%%+排除涨停: %+v", s)
	}
	l := defaultRecFilters(model.RecTypeLongTerm)
	if l.MaxGain5dPct != 0 || !l.ExcludeLimitUp {
		t.Fatalf("长线默认应为 无追高限制+排除涨停: %+v", l)
	}
}

// TestApplyQuoteFilters 用户筛选逐条：价格/市值/换手/死亡换手/涨停；缺失字段跳过不判。
func TestApplyQuoteFilters(t *testing.T) {
	base := candidate{Symbol: "600000", Name: "浦发银行", Price: 8.5, TurnoverRate: 5, FloatCap: 100e8}
	cases := []struct {
		name    string
		c       candidate
		f       RecFilters
		blocked bool
		keyword string
	}{
		{"全通过", base, RecFilters{}, false, ""},
		{"价格超上限", candidate{Symbol: "601318", Name: "中国平安", Price: 55}, RecFilters{PriceMax: 30}, true, "上限"},
		{"价格低于下限", candidate{Symbol: "600000", Price: 2.5, Name: "x"}, RecFilters{PriceMin: 3}, true, "下限"},
		{"市值超上限", base, RecFilters{FloatCapMaxYi: 50}, true, "流通市值"},
		{"市值缺失不判", candidate{Symbol: "600001", Name: "x", Price: 8, TurnoverRate: 5}, RecFilters{FloatCapMaxYi: 50}, false, ""},
		{"换手低于下限", base, RecFilters{TurnoverMin: 8}, true, "换手率"},
		{"换手20~30放行待阶段③位置判定", candidate{Symbol: "600002", Name: "x", Price: 8, TurnoverRate: 25}, RecFilters{}, false, ""},
		{"极端换手硬拦", candidate{Symbol: "600004", Name: "x", Price: 8, TurnoverRate: 35}, RecFilters{}, true, "极端换手"},
		{"已涨停排除", candidate{Symbol: "600003", Name: "x", Price: 11, LimitUp: 11, ChangePct: 10.0}, RecFilters{ExcludeLimitUp: true}, true, "涨停"},
		{"涨停但未开启排除", candidate{Symbol: "600003", Name: "x", Price: 11, LimitUp: 11, ChangePct: 10.0}, RecFilters{}, false, ""},
		{"ETF不参与个股推荐", candidate{Symbol: "510300", Name: "沪深300ETF", Price: 4}, RecFilters{}, true, "ETF"},
		{"深ETF也排除", candidate{Symbol: "159915", Name: "创业板ETF", Price: 2}, RecFilters{}, true, "基金"},
	}
	for _, tc := range cases {
		reason := applyQuoteFilters(tc.c, tc.f)
		if (reason != "") != tc.blocked {
			t.Errorf("%s: 期望 blocked=%v，得到原因 %q", tc.name, tc.blocked, reason)
			continue
		}
		if tc.blocked && tc.keyword != "" && !strings.Contains(reason, tc.keyword) {
			t.Errorf("%s: 排除原因应含 %q，得到 %q", tc.name, tc.keyword, reason)
		}
	}
}

// TestLimitUpHelpers 按代码前缀判涨停幅度；一字/封板判定。
func TestLimitUpHelpers(t *testing.T) {
	cases := []struct {
		symbol, name string
		want         float64
	}{
		{"600000", "浦发银行", 10},
		{"000001", "平安银行", 10},
		{"300750", "宁德时代", 20},
		{"688981", "中芯国际", 20},
		{"830799", "北交某股", 30},
		{"600001", "ST某某", 5},
	}
	for _, tc := range cases {
		if got := limitUpPctFor(tc.symbol, tc.name); got != tc.want {
			t.Errorf("%s(%s): 涨停幅应为 %v，得到 %v", tc.symbol, tc.name, tc.want, got)
		}
	}
	// 有涨停价：达到即视为封板。
	if !isAtLimitUp(candidate{Symbol: "600000", Price: 11.0, LimitUp: 11.0}) {
		t.Fatalf("现价=涨停价应判为已涨停")
	}
	if isAtLimitUp(candidate{Symbol: "600000", Price: 10.5, LimitUp: 11.0}) {
		t.Fatalf("未及涨停价不应判为涨停")
	}
	// 无涨停价：按涨跌幅接近板块幅度近似。
	if !isAtLimitUp(candidate{Symbol: "600000", Name: "x", Price: 11, ChangePct: 9.99}) {
		t.Fatalf("主板 +9.99%% 无涨停价时应近似判为涨停")
	}
	if isAtLimitUp(candidate{Symbol: "300750", Name: "x", Price: 100, ChangePct: 9.99}) {
		t.Fatalf("创业板 +9.99%% 距 20%% 涨停远，不应判为涨停")
	}
}

// TestGainCapFor 追高上限对 20cm 板放大。
func TestGainCapFor(t *testing.T) {
	if got := gainCapFor(25, "600000"); got != 25 {
		t.Fatalf("主板追高上限应 25，得到 %v", got)
	}
	if got := gainCapFor(25, "300750"); got != 40 {
		t.Fatalf("创业板追高上限应 40（25×1.6），得到 %v", got)
	}
	if got := gainCapFor(0, "600000"); got != 0 {
		t.Fatalf("0=不限应保持 0，得到 %v", got)
	}
}

// TestApplyTurnoverPosFilter 换手 20~30% 区间的位置分档：高位死亡换手排除、低位放行。
func TestApplyTurnoverPosFilter(t *testing.T) {
	high := &candFactors{Pos60: 80}
	low := &candFactors{Pos60: 30}
	if r := applyTurnoverPosFilter(candidate{TurnoverRate: 25}, high); !strings.Contains(r, "高位死亡换手") {
		t.Fatalf("高位+高换手应排除，得到 %q", r)
	}
	if r := applyTurnoverPosFilter(candidate{TurnoverRate: 25}, low); r != "" {
		t.Fatalf("低位+高换手应放行（由评分扣分标注），得到 %q", r)
	}
	if r := applyTurnoverPosFilter(candidate{TurnoverRate: 15}, high); r != "" {
		t.Fatalf("换手 ≤20%% 不参与位置判定，得到 %q", r)
	}
	if r := applyTurnoverPosFilter(candidate{TurnoverRate: 25}, nil); r != "" {
		t.Fatalf("无因子（日线缺失路径）不判，得到 %q", r)
	}
}

// TestStrategySources 策略-来源映射：回踩带回调路（活跃榜×温和回调）、价值带低PB榜、行级过滤生效。
func TestStrategySources(t *testing.T) {
	srcs := strategySources(model.RecTypeShortTerm, "pullback")
	var hasDipper bool
	for _, s := range srcs {
		if s.label == "dipper" {
			hasDipper = true
			if s.asc || s.keep == nil {
				t.Fatalf("回调路应为热度榜降序深捞+行级过滤（升序跌幅榜前100永远是深跌段）: %+v", s)
			}
			if s.keep(datasource.StockRank{ChangePct: -8}) {
				t.Fatalf("深跌 -8%% 应被行级过滤")
			}
			if !s.keep(datasource.StockRank{ChangePct: -3}) {
				t.Fatalf("温和回调 -3%% 应保留")
			}
			if s.keep(datasource.StockRank{ChangePct: -0.2}) {
				t.Fatalf("近乎平盘 -0.2%% 不算回调")
			}
			if s.keep(datasource.StockRank{ChangePct: 1}) {
				t.Fatalf("上涨不算回调")
			}
		}
	}
	if !hasDipper {
		t.Fatalf("pullback 应含回调路来源: %+v", srcs)
	}
	srcs = strategySources(model.RecTypeLongTerm, "value")
	var hasLowPB bool
	for _, s := range srcs {
		if s.label == "lowpb" {
			hasLowPB = true
			if s.keep(datasource.StockRank{PB: -0.5, PE: 8}) {
				t.Fatalf("负 PB（资不抵债/退市）应被行级过滤")
			}
			if s.keep(datasource.StockRank{PB: 0.4, PE: -6}) {
				t.Fatalf("破净但亏损（价值陷阱）应被行级过滤")
			}
			if s.keep(datasource.StockRank{PB: 0.6, PE: 55}) {
				t.Fatalf("破净但 PE 55 与价值策略不符，应被行级过滤")
			}
			if !s.keep(datasource.StockRank{PB: 0.6, PE: 6}) {
				t.Fatalf("破净盈利低估值应保留")
			}
		}
	}
	if !hasLowPB {
		t.Fatalf("value 应含低PB榜来源: %+v", srcs)
	}
	// momentum 的换手路应深捞+温和带过滤（榜单前排极端换手大多被硬拦，直接取温和带）。
	for _, s := range strategySources(model.RecTypeShortTerm, "momentum") {
		if s.label == "turnover" {
			if s.keep == nil {
				t.Fatalf("换手路应带温和带过滤: %+v", s)
			}
			if s.keep(datasource.StockRank{TurnoverRate: 28}) || !s.keep(datasource.StockRank{TurnoverRate: 8}) {
				t.Fatalf("换手温和带应保留 8%%、过滤 28%%")
			}
		}
	}
	// 所有策略的来源标签都必须有中文映射（前端展示依赖）。
	for _, rt := range []string{model.RecTypeShortTerm, model.RecTypeLongTerm} {
		for _, st := range StrategiesFor(rt) {
			for _, s := range strategySources(rt, st.Key) {
				if sourceLabelCN[s.label] == "" {
					t.Errorf("来源 %q 缺少中文标签", s.label)
				}
				if s.limit <= 0 || s.limit > 100 {
					t.Errorf("来源 %q limit 越界: %d", s.label, s.limit)
				}
			}
		}
	}
}

// TestAssignScanQuota 评分名额轮转：自选整组优先，其余来源逐轮各出一只，防单路垄断。
func TestAssignScanQuota(t *testing.T) {
	pool := make([]candidate, 0, 60)
	// 2 只自选 + 单一来源 A 55 只 + 来源 B 3 只（B 排池尾，旧的先到先得下全灭）。
	for i := 0; i < 2; i++ {
		pool = append(pool, candidate{Symbol: fmt.Sprintf("W%02d", i), Sources: []string{"watchlist"}})
	}
	for i := 0; i < 55; i++ {
		pool = append(pool, candidate{Symbol: fmt.Sprintf("A%02d", i), Sources: []string{"gainer"}})
	}
	for i := 0; i < 3; i++ {
		pool = append(pool, candidate{Symbol: fmt.Sprintf("B%02d", i), Sources: []string{"active"}})
	}
	assignScanQuota(pool)
	var wOK, aOK, bOK, full int
	for _, c := range pool {
		switch {
		case c.Excluded == "" && c.Sources[0] == "watchlist":
			wOK++
		case c.Excluded == "" && c.Sources[0] == "gainer":
			aOK++
		case c.Excluded == "" && c.Sources[0] == "active":
			bOK++
		case strings.HasPrefix(c.Excluded, poolFullPrefix):
			full++
		}
	}
	if wOK != 2 {
		t.Fatalf("自选应整组优先拿名额，得到 %d", wOK)
	}
	if bOK != 3 {
		t.Fatalf("池尾小来源应经轮转拿到名额（防单路垄断），得到 %d", bOK)
	}
	if aOK+bOK+wOK != maxScanCandidates {
		t.Fatalf("总发放应为 %d，得到 %d", maxScanCandidates, aOK+bOK+wOK)
	}
	if full != 60-maxScanCandidates {
		t.Fatalf("落选者应标池满，得到 %d", full)
	}
	// 已被用户筛选排除者不占名额。
	pool2 := []candidate{
		{Symbol: "X", Sources: []string{"gainer"}, Excluded: "股价超上限"},
		{Symbol: "Y", Sources: []string{"gainer"}},
	}
	assignScanQuota(pool2)
	if pool2[0].Excluded != "股价超上限" || pool2[1].Excluded != "" {
		t.Fatalf("已排除者不参与名额分配: %+v", pool2)
	}
}

// TestCandidateEligibleBJ 北交所前缀（43/83/87/920）不进池（数据源不支持，挤占名额）。
func TestCandidateEligibleBJ(t *testing.T) {
	f := defaultCandidateFilter()
	for _, sym := range []string{"430047", "830799", "870357", "920193"} {
		if candidateEligible(candidate{Symbol: sym, Name: "北交某股", Price: 10, Amount: 5e8}, f) {
			t.Errorf("北交所 %s 应被基础准入排除", sym)
		}
	}
	if !candidateEligible(candidate{Symbol: "600000", Name: "浦发银行", Price: 8, Amount: 5e8}, f) {
		t.Fatalf("沪市正常标的应通过")
	}
}

// TestRecFiltersDescribe 条件回显文案。
func TestRecFiltersDescribe(t *testing.T) {
	d := RecFilters{PriceMax: 30, FloatCapMinYi: 30, FloatCapMaxYi: 200, TurnoverMin: 3, TurnoverMax: 15, MaxGain5dPct: 25, ExcludeLimitUp: true}.Describe()
	joined := strings.Join(d, "|")
	for _, want := range []string{"股价≤30元", "流通市值30~200亿", "换手3%~15%", "排除近5日涨幅>25%", "排除已涨停"} {
		if !strings.Contains(joined, want) {
			t.Errorf("回显应含 %q: %v", want, d)
		}
	}
	if len((RecFilters{}).Describe()) != 0 {
		t.Fatalf("空条件应无回显")
	}
}

// TestNormalizeRecFiltersJSON 偏好落库前校验：空通过、坏格式报错、合法归一化。
func TestNormalizeRecFiltersJSON(t *testing.T) {
	if s, err := normalizeRecFiltersJSON(""); err != nil || s != "" {
		t.Fatalf("空串应通过: %v %q", err, s)
	}
	if _, err := normalizeRecFiltersJSON("{bad"); err == nil {
		t.Fatalf("坏格式应报错")
	}
	s, err := normalizeRecFiltersJSON(`{"price_min":30,"price_max":5,"exclude_limit_up":true}`)
	if err != nil {
		t.Fatalf("合法输入不应报错: %v", err)
	}
	if !strings.Contains(s, `"price_min":5`) || !strings.Contains(s, `"price_max":30`) {
		t.Fatalf("应归一化（区间交换）后存储: %s", s)
	}
}

// TestComposeBatchTitle 标题=类型·策略·关键筛选·数量（落库固化，历史稳定展示）。
func TestComposeBatchTitle(t *testing.T) {
	strat := &strategyTemplate{Key: "momentum", Name: "动量突破"}
	got := composeBatchTitle(model.RecTypeShortTerm, strat, RecFilters{PriceMax: 30}, 3)
	if got != "短线·动量突破 · ≤30元 · 3只" {
		t.Fatalf("标题不符: %q", got)
	}
	// 无价格条件时用市值；都没有则只有类型策略数量。
	got = composeBatchTitle(model.RecTypeLongTerm, &strategyTemplate{Key: "value", Name: "价值低估"}, RecFilters{FloatCapMaxYi: 200}, 5)
	if got != "长线·价值低估 · ≤200亿 · 5只" {
		t.Fatalf("标题不符: %q", got)
	}
	got = composeBatchTitle(model.RecTypeLongTerm, &strategyTemplate{Key: "value", Name: "价值低估"}, RecFilters{}, 4)
	if got != "长线·价值低估 · 4只" {
		t.Fatalf("标题不符: %q", got)
	}
}

// TestStrategyByKeyStrict 空 key 缺省第一个；跨类型 key 报错（旧版静默回退）。
func TestStrategyByKeyStrict(t *testing.T) {
	s, err := strategyByKey(model.RecTypeShortTerm, "")
	if err != nil || s.Key != "momentum" {
		t.Fatalf("空 key 应缺省 momentum: %v %+v", err, s)
	}
	if _, err := strategyByKey(model.RecTypeShortTerm, "value"); err == nil {
		t.Fatalf("短线传长线 key 应报错")
	}
	if s, err := strategyByKey(model.RecTypeLongTerm, "value"); err != nil || s.Name != "价值低估" {
		t.Fatalf("长线 value 应命中: %v", err)
	}
}

// TestPickDailyStrategy 日报按涨跌家数选策略：强势动量/弱势回踩/中性活跃。
func TestPickDailyStrategy(t *testing.T) {
	mk := func(adv, dec int) *reportSnapshot {
		return &reportSnapshot{Market: &reportMarket{Breadth: map[string]any{"advances": adv, "declines": dec}}}
	}
	if got := pickDailyStrategy(mk(3000, 1500)); got != "momentum" {
		t.Fatalf("强势应选 momentum，得到 %s", got)
	}
	if got := pickDailyStrategy(mk(1200, 3300)); got != "pullback" {
		t.Fatalf("弱势应选 pullback，得到 %s", got)
	}
	if got := pickDailyStrategy(mk(2200, 2100)); got != "active" {
		t.Fatalf("中性应选 active，得到 %s", got)
	}
	if got := pickDailyStrategy(nil); got != "momentum" {
		t.Fatalf("无数据应回退 momentum，得到 %s", got)
	}
}

// TestApplyReviews 复核 reject 降级为观察、置信度强制压到 ≤25 并追加风险；复核置信度覆盖。
func TestApplyReviews(t *testing.T) {
	picks := []recPick{
		{Symbol: "600000", Action: model.RecActionBuy, Confidence: 80},
		{Symbol: "000001", Action: model.RecActionBuy, Confidence: 70},
		{Symbol: "600036", Action: model.RecActionBuy, Confidence: 85},
		{Symbol: "601318", Action: model.RecActionBuy, Confidence: 90},
	}
	out := applyReviews(picks, []pickReview{
		{Symbol: "600000", Verdict: "reject", Comment: "证据与数据不符", Confidence: 30},
		{Symbol: "000001", Verdict: "pass", Confidence: 0},
		{Symbol: "600036", Verdict: "reject", Comment: "风险被低估", Confidence: 0}, // 复核未给值也必须压低
		{Symbol: "601318", Verdict: "reject", Comment: "追高", Confidence: 20},    // 复核给的值已 ≤25
	})
	if out[0].Action != model.RecActionWatch {
		t.Fatalf("reject 应降级为 watch，得到 %s", out[0].Action)
	}
	if out[0].Confidence != 25 {
		t.Fatalf("reject 置信度应强制压到 ≤25（复核给 30 也不例外），得到 %d", out[0].Confidence)
	}
	if len(out[0].Risks) == 0 || !strings.Contains(out[0].Risks[len(out[0].Risks)-1], "复核员否决") {
		t.Fatalf("reject 应追加否决说明: %+v", out[0].Risks)
	}
	if out[1].Action != model.RecActionBuy || out[1].Confidence != 70 {
		t.Fatalf("pass 且 confidence=0 不应改动: %+v", out[1])
	}
	if out[1].Review == nil || out[1].Review.Verdict != "pass" {
		t.Fatalf("复核结论应回填: %+v", out[1].Review)
	}
	if out[2].Confidence != 25 || out[2].Action != model.RecActionWatch {
		t.Fatalf("reject+confidence=0 应保底压到 25 并降级（否则「复核否决」与原置信度 85 并存自相矛盾）: %+v", out[2])
	}
	if out[3].Confidence != 20 || out[3].Action != model.RecActionWatch {
		t.Fatalf("reject 且复核给 20（已≤25）应保留 20（压低是上限而非定值）: %+v", out[3])
	}
}
