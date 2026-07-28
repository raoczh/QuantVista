package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"quantvista/datasource"
	"quantvista/model"
)

// 风险闸门（S1）：在分析/问答的个股快照组装阶段，用程序化规则前置识别高风险形态，
// 注入 prompt（约束模型措辞与评级）并透传给前端展示。规则是保守的提示性判断，
// 不阻断分析本身——用户问 ST 持仓照样能得到回答，只是带着强制的风险语境。
//
// 措辞纪律：质押等未接入的数据维度，统一「未接入数据，请自行核查」，不装作查过。
// **限售解禁自 B9 起已接入**（RPT_LIFT_STAGE 每日同步）——但「已接入」不等于「一定有数据」：
// 同步未跑、该股确实无解禁、查询失败三种情形语义完全不同，note 必须按实际可用性动态生成，
// 绝不把「不知道」说成「没有」（riskGateNoteFor 的唯一存在理由）。

// riskGateNoteBase 恒未接入的数据维度（措辞纪律，别改成模糊的「已综合考虑」）。
const riskGateNoteBase = "股权质押、商誉减值等风险数据未接入，本工具无法核查，请自行查证。"

// riskGateNoteLiftUnavailable 解禁维度本次不可用（同步未跑/查询失败）——
// **必须与「无解禁」区分**：说成「无解禁」会让模型给出没有依据的安全结论。
const riskGateNoteLiftUnavailable = "限售解禁数据本次不可用（同步未完成或查询失败），无法判断解禁风险，请自行核查。"

// riskGateNoteLiftNone 解禁数据可用且窗口内确无解禁（这是有依据的结论，可以这么说）。
const riskGateNoteLiftNone = "限售解禁数据已接入，未来 180 天内无解禁安排。"

// riskGateNoteFor 组装风险闸门声明。liftAvailable=解禁数据是否可用，
// liftCount=可用时窗口内的解禁批次数（0=确无解禁）。
func riskGateNoteFor(liftAvailable bool, liftCount int) string {
	switch {
	case !liftAvailable:
		return riskGateNoteBase + riskGateNoteLiftUnavailable
	case liftCount == 0:
		return riskGateNoteBase + riskGateNoteLiftNone
	default:
		// 有解禁：明细已在 corp_events.lifts 段，note 只做指引不重复数字
		//（数字重复出现在两处会让核验值域里同一个数被计两次）。
		return riskGateNoteBase + "限售解禁数据已接入，本次窗口内有解禁安排，详见 corp_events.lifts 段。"
	}
}

// riskFlag 单条风险提示。Level：block（禁止买入建议级）/ warn（显著风险）/ info（提示）。
type riskFlag struct {
	Level string `json:"level"`
	Code  string `json:"code"`
	Text  string `json:"text"`
}

// riskGateNote 未接入数据维度的固定声明（**已废弃，仅供旧测试与旧快照回读参考**）。
// 新代码一律走 riskGateNoteFor：解禁维度已接入，声明必须随实际可用性变化。
const riskGateNote = riskGateNoteBase

// 风险闸门阈值。
const (
	riskLimitBoardPct   = 9.5  // 涨跌幅 ≥9.5% 视为触板
	riskLimitBoardAmpl  = 1.0  // 且振幅 <1% 判一字板
	riskLowLiquidityAmt = 3e7  // 日成交额 <3000 万判流动性不足
	riskSmallCapTotal   = 30e8 // 总市值 <30 亿提示小盘

	// riskLiftRatioWarn 单次解禁占流通股比例 ≥ 该值提示显著抛压（A 股经验阈值）。
	riskLiftRatioWarn = 10.0
	// riskLiftAheadDays 解禁风险闸门的前瞻窗口（自然日）：只有临近的解禁才是当下的决策变量。
	riskLiftAheadDays = 60
)

// computeRiskGate 由行情与估值快照算风险标志（纯函数可测）。v 可为 nil（估值不可得
// 或 ETF），此时仅执行行情侧规则（一字板/流动性）。
func computeRiskGate(q *datasource.Quote, v *datasource.Valuation) []riskFlag {
	flags := []riskFlag{}
	if q == nil {
		return flags
	}

	// ST/退市风险警示：block 级——prompt 会禁止给出买入倾向。
	if v != nil && v.IsST {
		flags = append(flags, riskFlag{
			Level: "block", Code: "st",
			Text: "该股为 ST/风险警示标的，存在退市风险，禁止给出买入建议；评级不得为 bullish。",
		})
	} else if containsDelistMark(q.Name) {
		flags = append(flags, riskFlag{
			Level: "block", Code: "delist",
			Text: "该股名称含「退」，疑似处于退市整理期，流动性与归零风险极高，禁止给出买入建议。",
		})
	}

	// 一字板：涨跌幅 ≥9.5% 且振幅 <1%（全天钉死在涨/跌停价，单边流动性）。
	ampl := 0.0
	if v != nil && v.Amplitude > 0 {
		ampl = v.Amplitude
	} else if q.PrevClose > 0 && q.High >= q.Low && q.High > 0 {
		ampl = (q.High - q.Low) / q.PrevClose * 100
	}
	if abs(q.ChangePct) >= riskLimitBoardPct && ampl < riskLimitBoardAmpl && ampl >= 0 {
		if q.ChangePct > 0 {
			flags = append(flags, riskFlag{
				Level: "warn", Code: "limit_board",
				Text: fmt.Sprintf("涨幅 %.2f%% 且振幅 %.2f%%，疑似一字涨停板：买入大概率无法成交，次日溢价高度不确定，严禁按可正常买入来分析。", q.ChangePct, ampl),
			})
		} else {
			flags = append(flags, riskFlag{
				Level: "warn", Code: "limit_board",
				Text: fmt.Sprintf("跌幅 %.2f%% 且振幅 %.2f%%，疑似一字跌停板：卖出可能无法成交，流动性单边冻结，须提示无法止损的风险。", q.ChangePct, ampl),
			})
		}
	}

	// 流动性：日成交额 <3000 万，进出冲击成本高。
	if q.Amount > 0 && q.Amount < riskLowLiquidityAmt {
		flags = append(flags, riskFlag{
			Level: "warn", Code: "low_liquidity",
			Text: fmt.Sprintf("日成交额仅 %.0f 万元（<3000 万），流动性不足，买卖冲击成本高、可能无法按预期价格成交。", q.Amount/1e4),
		})
	}

	// 小市值提示：<30 亿总市值，波动与操纵风险偏高。
	if v != nil && v.TotalCap > 0 && v.TotalCap < riskSmallCapTotal {
		flags = append(flags, riskFlag{
			Level: "info", Code: "small_cap",
			Text: fmt.Sprintf("总市值约 %.1f 亿元（<30 亿）小盘股，股价波动大、易受资金扰动，结论适用性打折。", v.TotalCap/1e8),
		})
	}
	return flags
}

// containsDelistMark 名称是否带退市标记（「退」字：*ST 已由 IsST 覆盖，这里抓退市整理期）。
func containsDelistMark(name string) bool {
	for _, r := range name {
		if r == '退' {
			return true
		}
	}
	return false
}

// riskGateBlock 组装进个股快照的 risk_gate 块。flags 为空也注入 note（措辞纪律恒在）。
// note 按解禁数据的**实际可用性**动态生成：liftAvailable=false 时明确声明「无法判断」，
// 绝不因为查不到就让模型以为「没有解禁」。
func riskGateBlock(flags []riskFlag, liftAvailable bool, liftCount int) map[string]any {
	return map[string]any{
		"flags": flags,
		"note":  riskGateNoteFor(liftAvailable, liftCount),
	}
}

// evalLiftRiskFlags 由临近解禁批次生成风险标志（纯函数可测）。
// today 为判定日；只看未来 riskLiftAheadDays 内的解禁——半年后的解禁不是今天的决策变量。
// 比例 ≥riskLiftRatioWarn 记 warn，否则 info；同一日多批合并成一条（口径与 guard 一致）。
func evalLiftRiskFlags(lifts []model.RestrictedRelease, today string) []riskFlag {
	td, err := time.ParseInLocation("2006-01-02", today, time.Local)
	if err != nil {
		return nil
	}
	type agg struct {
		shares, cap, ratio float64
		days               int
	}
	byDate := map[string]*agg{}
	var dates []string
	for _, l := range lifts {
		fd, perr := time.ParseInLocation("2006-01-02", l.FreeDate, time.Local)
		if perr != nil {
			continue
		}
		days := int(fd.Sub(td).Hours() / 24)
		if days < 0 || days > riskLiftAheadDays {
			continue
		}
		a, ok := byDate[l.FreeDate]
		if !ok {
			a = &agg{days: days}
			byDate[l.FreeDate] = a
			dates = append(dates, l.FreeDate)
		}
		a.shares += l.FreeShares
		a.cap += l.LiftMarketCap
		a.ratio += l.FreeRatio
	}
	sort.Strings(dates)
	var flags []riskFlag
	for _, d := range dates {
		a := byDate[d]
		if a.shares <= 0 {
			continue
		}
		level := "info"
		if a.ratio >= riskLiftRatioWarn {
			level = "warn"
		}
		flags = append(flags, riskFlag{
			Level: level, Code: "lift_release",
			Text: fmt.Sprintf("%s（%d 天后）限售解禁 %.0f 万股、市值约 %.2f 亿元，占流通股 %.2f%%，须评估抛压与股价压制风险。",
				d, a.days, a.shares/1e4, a.cap/1e8, a.ratio),
		})
	}
	return flags
}

// riskGateTexts 从快照 risk_gate 块提取提示文本（信任层：文本里的阈值数字如 9.5/3000
// 是喂给模型的合法来源，须并入核验值域，否则忠实复述会被误报幻觉——同新闻标题前例）。
// 兼容原生 []riskFlag 与 JSON 反序列化后的 []any/map 两种形态（问答复用落库快照）。
func riskGateTexts(snapshot map[string]any) []string {
	blk, ok := snapshot["risk_gate"].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	switch fl := blk["flags"].(type) {
	case []riskFlag:
		for _, f := range fl {
			out = append(out, f.Text)
		}
	case []any:
		for _, v := range fl {
			if m, ok := v.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// parseRiskFlags 从落库快照 JSON 里解析 risk_flags（回传前端展示用）。
func parseRiskFlagsFromSnapshot(snapshotJSON string) []riskFlag {
	if snapshotJSON == "" {
		return nil
	}
	var wrap struct {
		RiskGate struct {
			Flags []riskFlag `json:"flags"`
		} `json:"risk_gate"`
	}
	if err := json.Unmarshal([]byte(snapshotJSON), &wrap); err != nil {
		return nil
	}
	return wrap.RiskGate.Flags
}
