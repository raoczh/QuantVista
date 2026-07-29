package service

import (
	"testing"

	"quantvista/datasource"
	"quantvista/model"
)

// C13 行业 / 风格暴露测试（第六十二批）。手工验算 + 三条诚实性反例：
// 缺数据整维缺席、未知桶恒排最后、覆盖率不足不下集中度结论。

// posView 造一条持仓视图（market_value 直接给定，绕开行情）。
func posView(symbol, name string, value float64, quoteOK bool) PositionView {
	v := PositionView{}
	v.Symbol, v.Market, v.Name = symbol, "cn", name
	v.Status = model.PositionStatusHolding
	v.QuoteOK = quoteOK
	v.MarketValue = value
	return v
}

func bucketOf(dim ExposureDim, key string) (ExposureBucket, bool) {
	for _, b := range dim.Buckets {
		if b.Key == key {
			return b, true
		}
	}
	return ExposureBucket{}, false
}

// TestComputeExposureManual 手工验算：三只票 60000/30000/10000（基数 100000）。
func TestComputeExposureManual(t *testing.T) {
	views := []PositionView{
		posView("600000", "浦发银行", 60000, true),
		posView("601398", "工商银行", 30000, true),
		posView("300750", "宁德时代", 10000, true),
		// 行情不可用的仓：**既不进市值也不进分布**（fail-closed）。
		posView("600519", "贵州茅台", 999999, false),
		// 已平仓：不进当前暴露。
		func() PositionView {
			v := posView("000001", "平安银行", 50000, true)
			v.Status = model.PositionStatusClosed
			return v
		}(),
	}
	industries := map[string]string{"600000": "银行", "601398": "银行"} // 300750 无行业归属
	valuations := map[string]*datasource.Valuation{
		"cn:600000": {TotalCap: 6e11, PETTM: 6.5}, // 大盘 + 低估值
		"cn:601398": {TotalCap: 2e12, PETTM: 20},  // 大盘 + 中等
		"cn:300750": {TotalCap: 8e9, PETTM: 0},    // 小盘 + 估值缺失（**不是亏损**）
	}

	ex := computeExposure(views, industries, valuations, 1)
	if ex == nil {
		t.Fatal("应产出暴露")
	}
	if ex.Base != 100000 {
		t.Fatalf("基数应为已定价市值合计 100000，got %v", ex.Base)
	}
	if !contains(ex.BaseNote, "1 笔") {
		t.Fatalf("行情不可用的笔数须在口径说明里，got %q", ex.BaseNote)
	}

	// 行业维：银行 90%（60000+30000），未知 10%。
	ind := ex.Industry
	if !ind.Available {
		t.Fatal("行业维应可用")
	}
	if len(ind.Buckets) != 2 {
		t.Fatalf("应为「银行 + 行业未知」两桶，got %+v", ind.Buckets)
	}
	if ind.Buckets[0].Label != "银行" || ind.Buckets[0].WeightPct != 90 || ind.Buckets[0].Count != 2 {
		t.Fatalf("银行桶应为 90%% / 2 只，got %+v", ind.Buckets[0])
	}
	last := ind.Buckets[len(ind.Buckets)-1]
	if !last.Unknown || last.WeightPct != 10 {
		t.Fatalf("未知桶应恒排最后且为 10%%，got %+v", last)
	}
	if ind.KnownPct != 90 || ind.TopLabel != "银行" || ind.TopWeightPct != 90 {
		t.Fatalf("行业覆盖率/最大桶不对: %+v", ind)
	}
	// 覆盖率 90% ≥ 60% 且占比 90% > 50% → 给出赛道集中提示。
	sigs := exposureSignals(ex)
	if len(sigs) != 1 || !contains(sigs[0], "银行") {
		t.Fatalf("应给出行业集中度提示，got %v", sigs)
	}

	// 市值风格：大盘 90%、小盘 10%，固定桶序（大→中→小→未知）。
	cap := ex.CapStyle
	if !cap.Available {
		t.Fatal("市值风格维应可用")
	}
	large, ok := bucketOf(cap, capLargeKey)
	if !ok || large.WeightPct != 90 {
		t.Fatalf("大盘桶应为 90%%，got %+v", large)
	}
	small, ok := bucketOf(cap, capSmallKey)
	if !ok || small.WeightPct != 10 {
		t.Fatalf("小盘桶应为 10%%，got %+v", small)
	}
	if _, ok := bucketOf(cap, exposureUnknownKey); ok {
		t.Fatal("三只票都有市值，不应出现未知桶")
	}
	if cap.Buckets[0].Key != capLargeKey {
		t.Fatalf("市值风格应按固定档序，got %+v", cap.Buckets)
	}

	// 估值风格：低估值 60%、中等 30%、**PE=0 的进「估值未知」而不是亏损** 10%。
	val := ex.ValueStyle
	low, _ := bucketOf(val, peLowKey)
	mid, _ := bucketOf(val, peMidKey)
	if low.WeightPct != 60 || mid.WeightPct != 30 {
		t.Fatalf("估值分档不对: %+v", val.Buckets)
	}
	if _, ok := bucketOf(val, peLossKey); ok {
		t.Fatal("PE=0 是估值缺失，绝不能归入「亏损」桶")
	}
	unk, ok := bucketOf(val, exposureUnknownKey)
	if !ok || unk.WeightPct != 10 || !unk.Unknown {
		t.Fatalf("PE=0 应进估值未知桶（10%%），got %+v", unk)
	}
	if val.Buckets[len(val.Buckets)-1].Key != exposureUnknownKey {
		t.Fatal("未知桶必须排最后")
	}
}

// TestExposureDimUnavailable 缺数据如实缺席：一条都查不到的维度 Available=false，
// **不是「分布均匀」也不是「全未知」**——前端据此整块不渲染。
func TestExposureDimUnavailable(t *testing.T) {
	views := []PositionView{posView("600000", "浦发银行", 10000, true)}

	// 行业快照未积累 + 估值源不可用 → 三维全不可用，但 Base 仍成立。
	ex := computeExposure(views, map[string]string{}, nil, 0)
	if ex == nil || ex.Base != 10000 {
		t.Fatalf("基数应仍成立: %+v", ex)
	}
	if ex.Industry.Available || ex.CapStyle.Available || ex.ValueStyle.Available {
		t.Fatalf("无数据时三维都应不可用: %+v", ex)
	}
	if len(exposureSignals(ex)) != 0 {
		t.Fatal("没有数据时不得给出任何集中度结论")
	}

	// 全部持仓行情不可用 → 整块缺席（nil）。
	if ex := computeExposure([]PositionView{posView("600000", "浦发银行", 10000, false)},
		map[string]string{"600000": "银行"}, nil, 1); ex != nil {
		t.Fatalf("无可定价持仓时应整块缺席，got %+v", ex)
	}
}

// TestExposureCoverageGate 覆盖率不足时**不下集中度结论**：
// 只查到一半持仓的行业，就算这一半全是同一个行业也不能说「赛道集中」。
func TestExposureCoverageGate(t *testing.T) {
	views := []PositionView{
		posView("600000", "浦发银行", 55000, true),
		posView("300750", "宁德时代", 45000, true), // 无行业归属
	}
	ex := computeExposure(views, map[string]string{"600000": "银行"}, nil, 0)
	if ex.Industry.KnownPct != 55 {
		t.Fatalf("覆盖率应为 55%%，got %v", ex.Industry.KnownPct)
	}
	if ex.Industry.TopWeightPct != 55 {
		t.Fatalf("最大桶应为 55%%，got %v", ex.Industry.TopWeightPct)
	}
	if sigs := exposureSignals(ex); len(sigs) != 0 {
		t.Fatalf("覆盖率 55%% < 60%% 时不得下集中度结论，got %v", sigs)
	}
	if !contains(ex.Industry.Note, "不足以判断") {
		t.Fatalf("覆盖率不足须在 note 里如实声明，got %q", ex.Industry.Note)
	}
}

// TestExposureSameSymbolMultiPosition 同一标的分批多仓：市值累加、只算一只。
func TestExposureSameSymbolMultiPosition(t *testing.T) {
	views := []PositionView{
		posView("600000", "浦发银行", 30000, true),
		posView("600000", "浦发银行", 20000, true), // 同标的第二笔
		posView("300750", "宁德时代", 50000, true),
	}
	ex := computeExposure(views, map[string]string{"600000": "银行", "300750": "电池"}, nil, 0)
	bank, ok := bucketOf(ex.Industry, "银行")
	if !ok || bank.WeightPct != 50 || bank.Count != 1 {
		t.Fatalf("同标的两笔应合并为一只、市值累加，got %+v", bank)
	}
}

// TestValueStyleKeyBoundaries 估值分档边界手工验算（PE=0 的语义是本组重点）。
func TestValueStyleKeyBoundaries(t *testing.T) {
	cases := []struct {
		pe   float64
		want string
	}{
		{-3.2, peLossKey}, // 亏损
		{0, ""},           // **缺失**，不是亏损
		{0.5, peLowKey},
		{15, peLowKey}, // 边界含等号
		{15.01, peMidKey},
		{30, peMidKey},
		{30.01, peHighKey},
	}
	for _, c := range cases {
		if got := valueStyleKey(c.pe); got != c.want {
			t.Fatalf("PE=%v 应归 %q，got %q", c.pe, c.want, got)
		}
	}
	capCases := []struct {
		cap  float64
		want string
	}{
		{0, ""}, {-1, ""},
		{5e10, capLargeKey}, {4.99e10, capMidKey},
		{1e10, capMidKey}, {9.99e9, capSmallKey},
	}
	for _, c := range capCases {
		if got := capStyleKey(c.cap); got != c.want {
			t.Fatalf("市值=%v 应归 %q，got %q", c.cap, c.want, got)
		}
	}
}
