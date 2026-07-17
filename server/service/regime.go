package service

import (
	"encoding/json"
	"fmt"
	"math"

	"quantvista/datasource"
	"quantvista/model"
)

// S1-1 大盘闸门：上证 MA20/MA60 位置 + 涨跌家数/涨跌停比 + 两市主力净流向 + 成交额
// 分位，合成 offense/neutral/defense 三档，落库进推荐批次并前端展示。
//
// 影子模式（防「少发 buy 做高表面胜率」，RECOMMENDATION_ACCURACY_PLAN §5 S1-1）：
// regime 照算照落库、前端展示标签与提示，但**不改写 action**；同时把「若强制降级会
// 改写哪些标的」记入 recommendation_candidate_events（gate_type=regime_shadow），其后
// 续影子收益由标签任务结算。转正条件：影子样本表明 defense 档被降级标的的配对收益
// 显著差于保留标的（看 risk-coverage 曲线），才由 feature flag 启用强制 buy→watch。
//
// 三档判定是本项目的新设计（daily_stock_analysis 的护栏依据文本风险标签而非行情
// 计算，只借「程序改写而非 prompt 恳求」的思路），必须先影子验证。

const (
	RegimeOffense = "offense"
	RegimeNeutral = "neutral"
	RegimeDefense = "defense"

	regimeVersion = "rg1"
)

// recRegimeEnforce S1-1 强制降级 feature flag：true 时 defense 档程序改写 buy→watch。
// **默认关闭（影子模式）**——转正须凭影子配对数据评审（gated vs ungated 收益、
// 覆盖率下降上限），不允许看表面胜率拍脑袋打开。包级变量供测试注入。
var recRegimeEnforce = false

// regimeParams rg1 判定参数（配置化；随 RegimeJSON 落批次快照可回溯）。
type regimeParams struct {
	BreadthStrong  float64 `json:"breadth_strong"`  // 涨家占比 ≥ 此值 +1
	BreadthWeak    float64 `json:"breadth_weak"`    // 涨家占比 ≤ 此值 −1
	LimitRatioBull float64 `json:"limit_ratio_bull"` // 涨停/跌停家数比 ≥ 此值 +1
	MainNetWeakYi  float64 `json:"main_net_weak_yi"` // 主力净流出超过此值（亿）−1
	AmtPctHigh     float64 `json:"amt_pct_high"`     // 成交额 120 日分位 ≥ 此值 +1
	AmtPctLow      float64 `json:"amt_pct_low"`      // ≤ 此值 −1
	OffenseMin     int     `json:"offense_min"`      // 合成分 ≥ 此值 → offense
	DefenseMax     int     `json:"defense_max"`      // 合成分 ≤ 此值 → defense
}

func defaultRegimeParams() regimeParams {
	return regimeParams{
		BreadthStrong: 0.55, BreadthWeak: 0.42,
		LimitRatioBull: 3, MainNetWeakYi: -300,
		AmtPctHigh: 0.60, AmtPctLow: 0.25,
		OffenseMin: 3, DefenseMax: 0,
	}
}

// RegimeResult 三档判定结果（RegimeJSON 落库结构）。
type RegimeResult struct {
	Regime  string       `json:"regime"`
	Score   int          `json:"score"`
	Signals []string     `json:"signals"` // 依据明细（人话，前端 tooltip 展示）
	Params  regimeParams `json:"params"`
	Version string       `json:"version"`
	// Sizing S1-2 仓位模型参数快照（computePositionPcts 使用的参数，可回溯）。
	Sizing *positionSizingParams `json:"sizing,omitempty"`
}

// computeRegime 纯函数：由基准日线（升序）、涨跌家数与两市主力净流入（亿元）合成三档。
// 任一输入缺失时对应信号缺席（不臆测）；全部缺失返回 neutral 并声明数据不足。
func computeRegime(benchBars []datasource.Bar, breadth *datasource.Breadth, mainNetYi float64, hasFundFlow bool, p regimeParams) RegimeResult {
	res := RegimeResult{Regime: RegimeNeutral, Version: regimeVersion, Params: p}
	score := 0
	signalCount := 0

	// ① 上证 vs MA20/MA60（趋势位置，各 ±1）。
	if n := len(benchBars); n > 0 {
		closes := make([]float64, n)
		for i, b := range benchBars {
			closes[i] = b.Close
		}
		last := closes[n-1]
		if ma20, ok := movingAverage(closes, 20); ok {
			signalCount++
			if last >= ma20 {
				score++
				res.Signals = append(res.Signals, "上证收于MA20上方(+1)")
			} else {
				score--
				res.Signals = append(res.Signals, "上证收于MA20下方(-1)")
			}
		}
		if ma60, ok := movingAverage(closes, 60); ok {
			signalCount++
			if last >= ma60 {
				score++
				res.Signals = append(res.Signals, "上证收于MA60上方(+1)")
			} else {
				score--
				res.Signals = append(res.Signals, "上证收于MA60下方(-1)")
			}
		}
	}

	// ② 涨跌家数 + 涨跌停比（市场宽度，±1 各一次）。
	if breadth != nil && breadth.Advances+breadth.Declines > 0 {
		signalCount++
		ratio := float64(breadth.Advances) / float64(breadth.Advances+breadth.Declines)
		switch {
		case ratio >= p.BreadthStrong:
			score++
			res.Signals = append(res.Signals, fmt.Sprintf("涨家占比 %.0f%%(+1)", ratio*100))
		case ratio <= p.BreadthWeak:
			score--
			res.Signals = append(res.Signals, fmt.Sprintf("涨家占比 %.0f%%(-1)", ratio*100))
		default:
			res.Signals = append(res.Signals, fmt.Sprintf("涨家占比 %.0f%%(0)", ratio*100))
		}
		if breadth.LimitUp > 0 || breadth.LimitDown > 0 {
			signalCount++
			ld := breadth.LimitDown
			if ld == 0 {
				ld = 1 // 零跌停按 1 计比值，防除零
			}
			lr := float64(breadth.LimitUp) / float64(ld)
			switch {
			case lr >= p.LimitRatioBull:
				score++
				res.Signals = append(res.Signals, fmt.Sprintf("涨停/跌停 %d/%d(+1)", breadth.LimitUp, breadth.LimitDown))
			case breadth.LimitDown > breadth.LimitUp:
				score--
				res.Signals = append(res.Signals, fmt.Sprintf("跌停多于涨停 %d/%d(-1)", breadth.LimitUp, breadth.LimitDown))
			default:
				res.Signals = append(res.Signals, fmt.Sprintf("涨停/跌停 %d/%d(0)", breadth.LimitUp, breadth.LimitDown))
			}
		}
	}

	// ③ 两市主力净流向（资金，±1）：净流入为强信号少见，净流出超阈值才扣分。
	if hasFundFlow {
		signalCount++
		switch {
		case mainNetYi > 0:
			score++
			res.Signals = append(res.Signals, fmt.Sprintf("主力净流入 %.0f 亿(+1)", mainNetYi))
		case mainNetYi <= p.MainNetWeakYi:
			score--
			res.Signals = append(res.Signals, fmt.Sprintf("主力净流出 %.0f 亿(-1)", mainNetYi))
		default:
			res.Signals = append(res.Signals, fmt.Sprintf("主力净流出 %.0f 亿(0)", mainNetYi))
		}
	}

	// ④ 成交额 120 日分位（活跃度，±1）：基准指数日线自带成交额时才参与。
	if pct, ok := amountPercentile(benchBars, 120); ok {
		signalCount++
		switch {
		case pct >= p.AmtPctHigh:
			score++
			res.Signals = append(res.Signals, fmt.Sprintf("成交额120日分位 %.0f%%(+1)", pct*100))
		case pct <= p.AmtPctLow:
			score--
			res.Signals = append(res.Signals, fmt.Sprintf("成交额120日分位 %.0f%%(-1)", pct*100))
		default:
			res.Signals = append(res.Signals, fmt.Sprintf("成交额120日分位 %.0f%%(0)", pct*100))
		}
	}

	res.Score = score
	if signalCount == 0 {
		res.Signals = append(res.Signals, "行情数据不足，默认中性档")
		return res
	}
	switch {
	case score >= p.OffenseMin:
		res.Regime = RegimeOffense
	case score <= p.DefenseMax:
		res.Regime = RegimeDefense
	}
	return res
}

// amountPercentile 末根成交额在近 window 根内的分位（0~1）。成交额缺失（新浪基准
// 兜底无 amount）返回 ok=false，该信号缺席。
func amountPercentile(bars []datasource.Bar, window int) (float64, bool) {
	n := len(bars)
	if n < 20 { // 样本过少分位无意义
		return 0, false
	}
	start := n - window
	if start < 0 {
		start = 0
	}
	amts := make([]float64, 0, n-start)
	for _, b := range bars[start:] {
		if b.Amount > 0 {
			amts = append(amts, b.Amount)
		}
	}
	if len(amts) < 20 || bars[n-1].Amount <= 0 {
		return 0, false
	}
	last := bars[n-1].Amount
	less, equal := 0, 0
	for _, a := range amts {
		switch {
		case a < last:
			less++
		case a == last:
			equal++
		}
	}
	return (float64(less) + 0.5*float64(equal)) / float64(len(amts)), true
}

// regimeCoef 仓位模型的 regime 系数（S1-2 公式项）。
func regimeCoef(regime string) float64 {
	switch regime {
	case RegimeOffense:
		return 1.0
	case RegimeDefense:
		return 0.3
	default:
		return 0.6
	}
}

// regimeBudgetPct 整批总仓位预算（Σposition_pct 上限，%）。
func regimeBudgetPct(regime string) float64 {
	switch regime {
	case RegimeOffense:
		return 100
	case RegimeDefense:
		return 30
	default:
		return 60
	}
}

// regimeLabelCN 前端/日志用中文档位名。
func regimeLabelCN(regime string) string {
	switch regime {
	case RegimeOffense:
		return "进攻"
	case RegimeDefense:
		return "防守"
	case RegimeNeutral:
		return "中性"
	}
	return regime
}

// applyRegimeGate S1-1 大盘闸门（纯函数）：defense 档对每只 buy 条目产出影子门控
// 记录（would_be_action=watch）；enforce=true（feature flag，默认关）才真正改写 action
// 并追加风险说明。offense/neutral 不产生任何门控。
func applyRegimeGate(picks []recPick, regime string, enforce bool) []gateNote {
	if regime != RegimeDefense {
		return nil
	}
	var gates []gateNote
	for i := range picks {
		if picks[i].Action != model.RecActionBuy {
			continue
		}
		reason := "防守档大盘闸门：若强制执行将 buy→watch（当前影子模式，未改写）"
		if enforce {
			reason = "防守档大盘闸门：已强制 buy→watch"
		}
		gates = append(gates, gateNote{
			Symbol: picks[i].Symbol, GateType: model.GateRegimeShadow,
			Reason: reason, WouldBeAction: model.RecActionWatch,
		})
		if enforce {
			picks[i].Action = model.RecActionWatch
			picks[i].Risks = append(picks[i].Risks, "大盘闸门（防守档）强制降级：市场状态判定为防守，buy 已改写为观察")
		}
	}
	return gates
}

// ---------- S1-2 仓位建议（服务端程序计算，非 LLM 输出） ----------

// positionSizingParams 目标波动模型参数（单位口径显式：年化波动为小数，0.35=35%）。
type positionSizingParams struct {
	TargetVolAnnual float64 `json:"target_vol_annual"` // 目标年化波动（小数）
	// 单票上限按年化波动分档（借 ai-hedge-fund 实测口径）：<15%→25%、15~30%→20%、
	// 30~50%→15%、>50%→10%（均为占总资金 %）。
	CapLowVol  float64 `json:"cap_low_vol"`
	CapMidVol  float64 `json:"cap_mid_vol"`
	CapHighVol float64 `json:"cap_high_vol"`
	CapExtVol  float64 `json:"cap_ext_vol"`
	// 相关性系数分档：与同批次其他入选标的的近 60 日收益相关性 ≥0.8→×0.70、
	// 0.6~0.8→×0.85、其余 ×1.0。
	CorrHigh     float64 `json:"corr_high"`      // 0.8
	CorrHighCoef float64 `json:"corr_high_coef"` // 0.70
	CorrMid      float64 `json:"corr_mid"`       // 0.6
	CorrMidCoef  float64 `json:"corr_mid_coef"`  // 0.85
	Version      string  `json:"version"`
}

func defaultSizingParams() positionSizingParams {
	return positionSizingParams{
		TargetVolAnnual: 0.18,
		CapLowVol:       25, CapMidVol: 20, CapHighVol: 15, CapExtVol: 10,
		CorrHigh: 0.8, CorrHighCoef: 0.70,
		CorrMid: 0.6, CorrMidCoef: 0.85,
		Version: "ps1",
	}
}

// volCapPct 单票上限（%）按年化波动（小数）分档。
func volCapPct(volAnnual float64, p positionSizingParams) float64 {
	switch {
	case volAnnual < 0.15:
		return p.CapLowVol
	case volAnnual < 0.30:
		return p.CapMidVol
	case volAnnual < 0.50:
		return p.CapHighVol
	default:
		return p.CapExtVol
	}
}

// sizingInput 单标的仓位计算输入。
type sizingInput struct {
	Symbol string
	// Vol20Daily 近 20 日日收益率标准差（百分比口径，candFactors.Volatility20；
	// <=0 表示样本不足，仓位建议缺席）。
	Vol20Daily float64
	// MaxCorr 与同批次其他入选标的近 60 日收益相关性的最大值（无对手/序列不足=0）。
	MaxCorr float64
}

// sizingOutput 单标的仓位建议（PositionPct<=0 表示无法给出，Why 说明原因）。
type sizingOutput struct {
	PositionPct float64
	Why         string
}

// computePositionPcts S1-2 目标波动模型（纯函数）：
//
//	position_pct = min(单票上限, target_vol_annual / vol20_annual × 100) × regime系数 × 相关性系数
//	vol20_annual = Vol20Daily/100 × √252（小数口径）
//
// 最后整批归一化：Σposition_pct 超过 regime 总仓位预算时按比例缩。
func computePositionPcts(ins []sizingInput, regime string, p positionSizingParams) []sizingOutput {
	outs := make([]sizingOutput, len(ins))
	rc := regimeCoef(regime)
	var sum float64
	for i, in := range ins {
		if in.Vol20Daily <= 0 {
			outs[i] = sizingOutput{Why: "近20日波动率样本不足，无法给出仓位建议"}
			continue
		}
		volAnnual := in.Vol20Daily / 100 * sqrt252
		cap := volCapPct(volAnnual, p)
		base := p.TargetVolAnnual / volAnnual * 100
		if base > cap {
			base = cap
		}
		corrCoef := 1.0
		corrNote := ""
		switch {
		case in.MaxCorr >= p.CorrHigh:
			corrCoef = p.CorrHighCoef
			corrNote = fmt.Sprintf("×%.2f(批内相关性%.2f)", corrCoef, in.MaxCorr)
		case in.MaxCorr >= p.CorrMid:
			corrCoef = p.CorrMidCoef
			corrNote = fmt.Sprintf("×%.2f(批内相关性%.2f)", corrCoef, in.MaxCorr)
		}
		pct := base * rc * corrCoef
		outs[i] = sizingOutput{
			PositionPct: pct,
			Why: fmt.Sprintf("年化波动 %.0f%%→上限 %.0f%%，目标波动 %.0f%%/个股波动=%.1f%%，×%s系数%.1f%s",
				volAnnual*100, cap, p.TargetVolAnnual*100, base, regimeLabelCN(regime), rc, corrNote),
		}
		sum += pct
	}
	// 整批归一化：总仓位预算按 regime 分档（这是仓位暴露上限；组合风险预算留 S5 后增强）。
	if budget := regimeBudgetPct(regime); sum > budget {
		scale := budget / sum
		for i := range outs {
			if outs[i].PositionPct > 0 {
				outs[i].PositionPct = outs[i].PositionPct * scale
				outs[i].Why += fmt.Sprintf("；整批超预算 %.0f%% 按 %.2f 比例缩", budget, scale)
			}
		}
	}
	for i := range outs {
		outs[i].PositionPct = round2(outs[i].PositionPct)
	}
	return outs
}

const sqrt252 = 15.874507866387544

// ---------- S1-3 组合去相关（纯函数部分） ----------

// pairwiseCorr 两条收盘序列的日收益 Pearson 相关（按尾部对齐，样本 <20 返回 0 不判）。
// 注意：按数组位置对齐仅在两股无停牌错位时成立——生产路径应使用 pairwiseCorrAligned
//（按交易日交集对齐），本函数保留给无日期序列的调用方与既有测试。
func pairwiseCorr(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 21 {
		return 0
	}
	ra := dailyReturns(a[len(a)-n:])
	rb := dailyReturns(b[len(b)-n:])
	return corrOfReturns(ra, rb)
}

// pairwiseCorrAligned 两条带交易日的收盘序列按日期交集对齐后的日收益 Pearson 相关。
// 任一股停牌造成的位置错位在此消除（相邻交集日的收益是跨停牌累计收益，如实）；
// 交集不足 21 个交易日返回 0 不判。dates 与 closes 等长升序。
func pairwiseCorrAligned(datesA []string, a []float64, datesB []string, b []float64) float64 {
	if len(datesA) != len(a) || len(datesB) != len(b) {
		return 0
	}
	byB := make(map[string]float64, len(datesB))
	for i, d := range datesB {
		byB[d] = b[i]
	}
	ca := make([]float64, 0, len(a))
	cb := make([]float64, 0, len(a))
	for i, d := range datesA {
		if vb, ok := byB[d]; ok {
			ca = append(ca, a[i])
			cb = append(cb, vb)
		}
	}
	if len(ca) < 21 {
		return 0
	}
	return corrOfReturns(dailyReturns(ca), dailyReturns(cb))
}

// corrOfReturns 两条日收益序列（尾部截齐）的 Pearson 相关；样本 <20 返回 0。
func corrOfReturns(ra, rb []float64) float64 {
	m := len(ra)
	if len(rb) < m {
		m = len(rb)
	}
	if m < 20 {
		return 0
	}
	ra, rb = ra[len(ra)-m:], rb[len(rb)-m:]
	var sa, sb float64
	for i := 0; i < m; i++ {
		sa += ra[i]
		sb += rb[i]
	}
	ma, mb := sa/float64(m), sb/float64(m)
	var cov, va, vb float64
	for i := 0; i < m; i++ {
		da, db := ra[i]-ma, rb[i]-mb
		cov += da * db
		va += da * da
		vb += db * db
	}
	if va <= 0 || vb <= 0 {
		return 0
	}
	return cov / (math.Sqrt(va) * math.Sqrt(vb))
}

func dailyReturns(closes []float64) []float64 {
	out := make([]float64, 0, len(closes))
	for i := 1; i < len(closes); i++ {
		if closes[i-1] > 0 && closes[i] > 0 {
			out = append(out, closes[i]/closes[i-1]-1)
		}
	}
	return out
}

// marshalRegimeJSON 序列化 RegimeResult（含仓位参数快照）供批次落库。
func marshalRegimeJSON(r RegimeResult) string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}
