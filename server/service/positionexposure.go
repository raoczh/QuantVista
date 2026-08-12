package service

import (
	"context"
	"fmt"
	"sort"

	"quantvista/datasource"
	"quantvista/model"
)

// C13 行业 / 风格暴露（第六十二批）。
//
// 回答「我是不是把钱全压在一个赛道上了」——组合层的第二个风险视角
// （第一个是既有的单一标的集中度 TopWeightPct）。
//
// **纪律（改代码前先读）**：
//   - **缺数据如实缺席**：某一维完全没有数据（行业快照未积累 / 估值源不可用）时
//     `Available=false`，前端整块不渲染——**不拍默认值、不把未知摊派进已知桶**；
//   - **部分缺失显式成桶**：能查到一部分时照常出分布，缺的进「未知」桶且**恒排最后**
//     （沿用 B6 复盘统计「行业未知」的先例：混在中间会被读成一个真实行业）；
//   - **占比基数是「已定价持仓市值合计」**（含未知桶），各桶之和恒为 100%
//     ——未知有多大一块必须看得见；行情 stale/失败的仓既不进市值也不进分布
//     （fail-closed 一以贯之，与 Overview.TotalValue 同口径）；
//   - **覆盖率不足不下结论**：集中度提示只在该维覆盖率 ≥ exposureSignalMinCoverage
//     时给出。只查到 1/5 持仓的行业就断言「某行业占 60%」是拿局部当整体；
//   - **PE==0 是缺失不是亏损**（全项目铁律，见 ROADMAP §3）：只有严格 `PETTM<0`
//     才归「亏损」桶，0 归「未知」。

const (
	// 市值风格分档（元，A 股通行口径）：大盘 ≥500 亿、中盘 100~500 亿、小盘 <100 亿。
	capLargeYuan = 5e10
	capMidYuan   = 1e10

	// 估值风格分档（PE-TTM）：低 ≤15、中 15~30、高 >30；严格负值为亏损。
	peLowMax = 15.0
	peMidMax = 30.0

	// exposureSignalMinCoverage 给出集中度提示所需的最低覆盖率 %。
	// 覆盖率不足时只展示分布并声明覆盖率，不下「集中度偏高」这类结论。
	exposureSignalMinCoverage = 60.0

	// industryConcentrationWarnPct 单一行业占比超过该值提示赛道集中。
	industryConcentrationWarnPct = 50.0
)

// 暴露桶的稳定键（前端配色/排序用，展示名走 Label）。
const (
	exposureUnknownKey = "unknown"

	capLargeKey = "large"
	capMidKey   = "mid"
	capSmallKey = "small"

	peLowKey  = "low"
	peMidKey  = "mid"
	peHighKey = "high"
	peLossKey = "loss"
)

// ExposureBucket 一个暴露分桶。Value 单位=元，WeightPct 为占**已定价持仓市值**的百分比。
type ExposureBucket struct {
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	Value     float64 `json:"value"`
	WeightPct float64 `json:"weight_pct"`
	Count     int     `json:"count"` // 标的数（按 symbol 去重，同标的多笔仓算一只）
	// Unknown 该桶为「数据缺失」桶——前端必须以中性色渲染并**恒排最后**，
	// 不得与真实取值混排（否则会被读成「未知行业赚得最多」那类误解）。
	Unknown bool `json:"unknown,omitempty"`
}

// ExposureDim 一个暴露维度的分布。
type ExposureDim struct {
	// Available=false 表示该维度**一条数据都没有**（快照未积累/数据源不可用），
	// 前端整块缺席——这是「不知道」，不是「分布均匀」。
	Available bool             `json:"available"`
	Buckets   []ExposureBucket `json:"buckets"`
	// KnownPct 有归属的市值占比 %（100−未知桶占比）。
	KnownPct     float64 `json:"known_pct"`
	TopLabel     string  `json:"top_label,omitempty"`
	TopWeightPct float64 `json:"top_weight_pct,omitempty"`
	Note         string  `json:"note,omitempty"`
}

// PortfolioExposure 组合的三维暴露（C13）。
type PortfolioExposure struct {
	// Base 计算基数 = 已定价（fresh 行情）持仓市值合计（元），与 Overview.TotalValue 同口径。
	Base          float64     `json:"base"`
	BaseNote      string      `json:"base_note"`
	Industry      ExposureDim `json:"industry"`
	CapStyle      ExposureDim `json:"cap_style"`
	ValueStyle    ExposureDim `json:"value_style"`
	WindowDays    int         `json:"window_days,omitempty"`
	SampleCount   int         `json:"sample_count,omitempty"`
	AsOf          string      `json:"as_of,omitempty"`
	FactorVersion string      `json:"factor_version,omitempty"`
	DataVersion   string      `json:"data_version,omitempty"`
}

// exposureInput 单只标的（按 symbol 聚合后）参与暴露计算的输入。
type exposureInput struct {
	Symbol string
	Market string
	Name   string
	Value  float64 // 已定价市值（元）
}

// bucketAcc 分桶累加器。
type bucketAcc struct {
	value float64
	count int
}

// buildExposureDim 把「标的 → 桶键」的归类结果汇总成一个维度。
//
//	order 为固定桶序（风格维用）；为 nil 时按占比降序排（行业维）。
//	labels 给出每个桶键的展示名。未归类（键为空）的进「未知」桶，恒排最后。
func buildExposureDim(items []exposureInput, base float64, classify func(exposureInput) string,
	labels map[string]string, order []string, unknownLabel string) ExposureDim {
	dim := ExposureDim{Buckets: []ExposureBucket{}}
	if len(items) == 0 || base <= 0 {
		return dim
	}
	accs := map[string]*bucketAcc{}
	unknown := bucketAcc{}
	known := 0.0
	for _, it := range items {
		key := classify(it)
		if key == "" {
			unknown.value += it.Value
			unknown.count++
			continue
		}
		known += it.Value
		if accs[key] == nil {
			accs[key] = &bucketAcc{}
		}
		accs[key].value += it.Value
		accs[key].count++
	}
	if len(accs) == 0 {
		return dim // 一条都没归上：该维度不可用（不是「全未知」，是没数据）
	}
	dim.Available = true
	push := func(key, label string, acc bucketAcc, isUnknown bool) {
		if acc.count == 0 {
			return
		}
		dim.Buckets = append(dim.Buckets, ExposureBucket{
			Key: key, Label: label, Value: round2(acc.value),
			WeightPct: round2(acc.value / base * 100), Count: acc.count, Unknown: isUnknown,
		})
	}
	if len(order) > 0 {
		for _, key := range order {
			if acc := accs[key]; acc != nil {
				push(key, labels[key], *acc, false)
			}
		}
	} else {
		keys := make([]string, 0, len(accs))
		for k := range accs {
			keys = append(keys, k)
		}
		// 占比降序；同占比按键名升序保证结果稳定（map 遍历序不定）。
		sort.Slice(keys, func(i, j int) bool {
			if accs[keys[i]].value != accs[keys[j]].value {
				return accs[keys[i]].value > accs[keys[j]].value
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			label := labels[k]
			if label == "" {
				label = k
			}
			push(k, label, *accs[k], false)
		}
	}
	// 未知桶恒排最后。
	push(exposureUnknownKey, unknownLabel, unknown, true)

	dim.KnownPct = round2(known / base * 100)
	for _, b := range dim.Buckets {
		if !b.Unknown && b.WeightPct > dim.TopWeightPct {
			dim.TopWeightPct, dim.TopLabel = b.WeightPct, b.Label
		}
	}
	return dim
}

// capStyleKey 按总市值分档；cap<=0（缺失）返回空串进未知桶。
func capStyleKey(totalCap float64) string {
	switch {
	case totalCap <= 0:
		return ""
	case totalCap >= capLargeYuan:
		return capLargeKey
	case totalCap >= capMidYuan:
		return capMidKey
	default:
		return capSmallKey
	}
}

// valueStyleKey 按 PE-TTM 分档。
// **PE==0 是「估值缺失」不是「亏损」**（全项目铁律）——只有严格负值才归亏损桶。
func valueStyleKey(peTTM float64) string {
	switch {
	case peTTM < 0:
		return peLossKey
	case peTTM == 0:
		return ""
	case peTTM <= peLowMax:
		return peLowKey
	case peTTM <= peMidMax:
		return peMidKey
	default:
		return peHighKey
	}
}

var (
	capStyleLabels = map[string]string{
		capLargeKey: "大盘（≥500亿）", capMidKey: "中盘（100~500亿）", capSmallKey: "小盘（<100亿）",
	}
	capStyleOrder    = []string{capLargeKey, capMidKey, capSmallKey}
	valueStyleLabels = map[string]string{
		peLowKey: "低估值（PE≤15）", peMidKey: "中等（PE 15~30）",
		peHighKey: "高估值（PE>30）", peLossKey: "亏损（PE为负）",
	}
	valueStyleOrder = []string{peLowKey, peMidKey, peHighKey, peLossKey}
)

// computeExposure 由持仓视图算三维暴露。valuations 可为空（该两维退化为不可用）。
// 纯函数（不查 DB / 不发请求），供单测直接手工验算。
func computeExposure(views []PositionView, industries map[string]string,
	valuations map[string]*datasource.Valuation, skipped int) *PortfolioExposure {
	// 按 symbol 聚合已定价市值（同一标的分批多仓算一只）。
	agg := map[string]*exposureInput{}
	order := make([]string, 0, len(views))
	base := 0.0
	for _, v := range views {
		if v.Status != model.PositionStatusHolding || !v.QuoteOK || v.MarketValue <= 0 {
			continue // fail-closed：非 fresh 的仓既不进市值也不进分布
		}
		key := QuoteKey(v.Market, v.Symbol)
		if agg[key] == nil {
			agg[key] = &exposureInput{Symbol: v.Symbol, Market: v.Market, Name: v.Name}
			order = append(order, key)
		}
		agg[key].Value += v.MarketValue
		base += v.MarketValue
	}
	if base <= 0 {
		return nil // 没有可定价的持仓：整块缺席
	}
	items := make([]exposureInput, 0, len(order))
	for _, k := range order {
		items = append(items, *agg[k])
	}

	out := &PortfolioExposure{Base: round2(base)}
	out.BaseNote = "占比基数为「已取到当前有效行情」的持仓市值合计；行情过期或获取失败的持仓不参与分布（与组合市值口径一致）。"
	if skipped > 0 {
		out.BaseNote += fmt.Sprintf("本次有 %d 笔持仓因行情不可用未计入。", skipped)
	}

	out.Industry = buildExposureDim(items, base, func(it exposureInput) string {
		return industries[it.Symbol]
	}, nil, nil, "行业未知")
	if out.Industry.Available {
		out.Industry.Note = "行业归属来自全市场宇宙快照（东财行业板块），非 A 股标的与快照未覆盖的标的进「行业未知」。"
		if out.Industry.KnownPct < exposureSignalMinCoverage {
			out.Industry.Note += fmt.Sprintf("当前仅 %.1f%% 的持仓市值有行业归属，分布仅代表这部分，不足以判断整体赛道集中度。", out.Industry.KnownPct)
		}
	}

	valOf := func(it exposureInput) *datasource.Valuation {
		if valuations == nil {
			return nil
		}
		return valuations[QuoteKey(it.Market, it.Symbol)]
	}
	out.CapStyle = buildExposureDim(items, base, func(it exposureInput) string {
		v := valOf(it)
		if v == nil {
			return ""
		}
		return capStyleKey(v.TotalCap)
	}, capStyleLabels, capStyleOrder, "市值未知")
	if out.CapStyle.Available {
		out.CapStyle.Note = "按总市值分档：大盘 ≥500 亿、中盘 100~500 亿、小盘 <100 亿（元）。估值源取不到市值的标的进「市值未知」。"
	}

	out.ValueStyle = buildExposureDim(items, base, func(it exposureInput) string {
		v := valOf(it)
		if v == nil {
			return ""
		}
		return valueStyleKey(v.PETTM)
	}, valueStyleLabels, valueStyleOrder, "估值未知")
	if out.ValueStyle.Available {
		out.ValueStyle.Note = "按 PE-TTM 分档。**PE 为 0 表示估值数据缺失（进「估值未知」），不是亏损**；只有 PE 为负才是亏损。ETF/基金无个股 PE，天然进未知。"
	}
	return out
}

// exposureSignals 由暴露分布生成组合风控提示（并入 Overview.Signals）。
// **覆盖率不足时不下结论**——只查到一小部分持仓的行业就断言「赛道集中」是拿局部当整体。
func exposureSignals(ex *PortfolioExposure) []string {
	if ex == nil {
		return nil
	}
	var out []string
	ind := ex.Industry
	if ind.Available && ind.KnownPct >= exposureSignalMinCoverage &&
		ind.TopWeightPct > industryConcentrationWarnPct {
		out = append(out, fmt.Sprintf("%s 行业占持仓市值 %.1f%%，单一赛道集中度偏高（行业数据覆盖 %.0f%%）",
			ind.TopLabel, ind.TopWeightPct, ind.KnownPct))
	}
	return out
}

// exposureFor 组装暴露：查行业快照 + 批量取估值，再走纯函数计算。
// 行情不可用的持仓数由调用方传入（进 BaseNote 声明）。
func (s *PositionService) exposureFor(ctx context.Context, views []PositionView, skipped int) *PortfolioExposure {
	symbols := make([]string, 0, len(views))
	refs := make([]QuoteRef, 0, len(views))
	seen := map[string]bool{}
	for _, v := range views {
		if v.Status != model.PositionStatusHolding || !v.QuoteOK || v.MarketValue <= 0 {
			continue
		}
		k := QuoteKey(v.Market, v.Symbol)
		if seen[k] {
			continue
		}
		seen[k] = true
		refs = append(refs, QuoteRef{Market: v.Market, Symbol: v.Symbol})
		if v.Market == "cn" {
			symbols = append(symbols, v.Symbol)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	industries := industriesFor(symbols)
	var valuations map[string]*datasource.Valuation
	if s.market != nil {
		// best-effort：单只失败缺席（进「未知」桶），不阻断整块。
		valuations = s.market.ValuationsFor(ctx, refs)
	}
	return computeExposure(views, industries, valuations, skipped)
}
