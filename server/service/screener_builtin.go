package service

import "quantvista/model"

// 内置选股策略（26 个）：只用因子宽表已注册因子（factorDefs），
// 白话讲解 + 适用周期 + 风险等级。参考 StockNova builtin 思路按现有因子重写，
// 阈值沿用项目内已有共识（量比 1.5~5 温和放量、换手 3~15 活跃、RSI 凹形逻辑等）。
//
// 纪律：新增策略只能引用 factorDefs 内因子（validateCondTree 会拦）；实时估值类
//（PE/PB）宽表没有（腾讯估值是实时单只接口，无法全市场普查），别写进内置策略——
// 唯一例外是 C10 的 `div_yield`（来自落库的 corporate_actions 分红方案表，可全市场取），
// 但它对多数股票缺失（NaN），当作硬条件会把没分红数据的票全部筛掉，只宜作可选条件。
// K 线形态因子（C12）是**描述性**的，可进条件树，但不得据此断言买卖方向。

// builtinScreen 内置策略定义。
type builtinScreen struct {
	Key    string
	Name   string
	Desc   string // 白话讲解（这是什么形态、为什么值得看、要注意什么）
	Period string // short / swing / mid
	Risk   string // low / mid / high
	Tree   CondNode
}

// --- 树构造辅助（只在本文件用，保持策略定义一眼可读） ---

func fptr(v float64) *float64 { return &v }

func leafV(factor, op string, v float64) CondNode {
	return CondNode{Factor: factor, Op: op, Value: fptr(v)}
}
func leafBetween(factor string, lo, hi float64) CondNode {
	return CondNode{Factor: factor, Op: "between", Value: fptr(lo), Value2: fptr(hi)}
}
func leafRef(factor, op, ref string) CondNode {
	return CondNode{Factor: factor, Op: op, Ref: ref}
}
func leafTrue(factor string) CondNode  { return CondNode{Factor: factor, Op: "is_true"} }
func leafFalse(factor string) CondNode { return CondNode{Factor: factor, Op: "is_false"} }
func allOf(nodes ...CondNode) CondNode { return CondNode{All: nodes} }

// builtinScreens 内置策略清单（顺序即前端策略广场展示序：短线 → 波段 → 中线）。
var builtinScreens = []builtinScreen{
	// ---------- 短线 ----------
	{
		Key:    "vol-break-20d",
		Name:   "放量创20日新高",
		Desc:   "收盘严格超过此前 20 日收盘高点，放量 1.5~5 倍且收在当日较高位置。日涨幅低于所属板块常规涨停幅度的 90%，排除封板信号；突破仍可能失败，执行前另核对延伸距离。",
		Period: "short",
		Risk:   "high",
		Tree: allOf(
			leafTrue("high_20d"),
			leafBetween("vol_boost", 1.5, 5),
			leafV("day_limit_ratio", "<", 0.9),
			leafFalse("limit_up_today"),
			leafV("rq_close_location", ">=", 0.65),
			leafV("amount_yi", ">=", 2),
		),
	},
	{
		Key:    "shrink-pullback-ma20",
		Name:   "强势股缩量回踩MA20",
		Desc:   "近 20 日涨幅至少 10%，近 5 日缩量回落，收盘位于 MA20 上方 1.2 ATR 以内。观察趋势内回踩，量缩不能单独证明抛压减轻；还需确认企稳及支撑是否有效。",
		Period: "short",
		Risk:   "mid",
		Tree: allOf(
			leafV("chg_20d", ">=", 10),
			leafTrue("above_ma20"),
			leafBetween("rq_ma20_dist", 0, 1.2),
			leafBetween("chg_5d", -8, 0),
			leafV("vol_5v20", "<", 0.9),
		),
	},
	{
		Key:    "mild-vol-start",
		Name:   "低位温和放量",
		Desc:   "股价处于 60 日区间下半部，当日放量 1.5~3 倍、上涨 1~6%，观察低位量价变化。低位置不代表低风险，也不能证明资金建仓；需核查下跌原因和后续承接。",
		Period: "short",
		Risk:   "mid",
		Tree: allOf(
			leafV("pos_60", "<", 50),
			leafBetween("vol_boost", 1.5, 3),
			leafBetween("chg_pct", 1, 6),
			leafV("amount_yi", ">=", 1),
		),
	},
	{
		Key:    "bottom-vol-yang",
		Name:   "底部放量长阳",
		Desc:   "60 日区间底部 30% 位置，放量超过 2 倍、涨幅超过 5% 且阳线实体超过 3%。这是低位反弹形态，不能仅凭量价推断利空出尽或资金建仓，仍需核查下跌原因。",
		Period: "short",
		Risk:   "high",
		Tree: allOf(
			leafV("pos_60", "<", 30),
			leafV("chg_pct", ">", 5),
			leafV("body_pct", ">", 3),
			leafFalse("limit_up_today"),
			leafV("vol_boost", ">", 2),
			leafV("amount_yi", ">=", 1),
		),
	},
	{
		Key:    "yang-through-3ma",
		Name:   "一阳穿三线",
		Desc:   "开盘低于 5/10/20 日均线，收盘高于三条均线，且相对昨收涨幅超过 2%。这是短期均线集中收复形态，日涨幅与阳线实体涨幅并不相同；后续仍需确认是否守住突破位置。",
		Period: "short",
		Risk:   "high",
		Tree: allOf(
			leafRef("open", "<", "ma5"),
			leafRef("open", "<", "ma10"),
			leafRef("open", "<", "ma20"),
			leafRef("close", ">", "ma5"),
			leafRef("close", ">", "ma10"),
			leafRef("close", ">", "ma20"),
			leafV("chg_pct", ">", 2),
		),
	},
	{
		Key:    "limit-up-pullback",
		Name:   "涨停后温和回调",
		Desc:   "近 5 日出现过涨停，最新完整日回落 0.5~6% 且收盘守住 MA10。观察强波动后的回调，不能据此判断是洗盘还是出货；A 股 T+1 和跳空也可能使计划止损无法按价成交。",
		Period: "short",
		Risk:   "high",
		Tree: allOf(
			leafV("limit_ups_5d", ">=", 1),
			leafFalse("limit_up_today"),
			leafBetween("chg_pct", -6, -0.5),
			leafRef("close", ">", "ma10"),
		),
	},
	{
		Key:    "strong-consolidation",
		Name:   "强势整理蓄势",
		Desc:   "近 20 日涨超 15%、近 5 日涨跌在 -5%~2%，守住 MA20，且整理振幅收敛、量能没有明显放大。低净涨幅不等于横盘，需同时排除宽幅震荡；整理方向仍待确认。",
		Period: "short",
		Risk:   "mid",
		Tree: allOf(
			leafV("chg_20d", ">", 15),
			leafBetween("chg_5d", -5, 2),
			leafV("rq_compression", "<=", 0.9),
			leafV("vol_5v20", "<=", 1.2),
			leafTrue("above_ma20"),
		),
	},
	{
		Key:    "rsi-strong-zone",
		Name:   "RSI强势区未过热",
		Desc:   "RSI(14) 位于 55~70、均线多头排列且当日涨幅低于 7%，观察趋势动能。70 是常用观察阈值，不能单凭它推断回调概率；入场距离和退出价位另由风险计划核对。",
		Period: "short",
		Risk:   "mid",
		Tree: allOf(
			leafBetween("rsi_14", 55, 70),
			leafTrue("bull_align"),
			leafV("chg_pct", "<", 7),
		),
	},
	// ---------- 波段 ----------
	{
		Key:    "macd-gold-water",
		Name:   "MACD水上金叉",
		Desc:   "近 3 日 DIF 上穿 DEA，当前 DIF 仍高于 DEA 且在零轴上方。过滤金叉后已重新死叉的形态；这是趋势确认线索，不代表已经验证的胜率优势。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafTrue("macd_cross_up"),
			leafTrue("macd_gold"),
			leafV("macd_dif", ">", 0),
		),
	},
	{
		Key:    "macd-gold-under",
		Name:   "MACD水下金叉（超跌反弹）",
		Desc:   "近 3 日 DIF 在零轴下方上穿 DEA，当前仍保持金叉且股价处于 60 日区间下方 40%。观察弱势中的动能修复，金叉不能单独证明趋势反转，仍需确认价格企稳。",
		Period: "swing",
		Risk:   "high",
		Tree: allOf(
			leafTrue("macd_cross_up"),
			leafV("macd_dif", "<", 0),
			leafTrue("macd_gold"),
			leafV("pos_60", "<", 40),
		),
	},
	{
		Key:    "rsi-oversold-up",
		Name:   "RSI超卖回升",
		Desc:   "此前 5 日 RSI(14) 曾低于 30，最新 RSI 回升至 30~45 且收盘上涨。观察超卖后的修复，不能据此证明抛售衰竭；下跌主因和价格企稳仍需核查。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafBetween("rsi_14", 30, 45),
			leafV("rsi_prior5_min", "<", 30),
			leafTrue("rsi_rising"),
			leafV("chg_pct", ">", 0),
		),
	},
	{
		Key:    "boll-lower-bounce",
		Name:   "布林下轨反弹",
		Desc:   "触及昨日已知布林下轨后收复，当前带内位置 0~25% 且收盘上涨。只处于带内低位不算反弹；单边下跌仍可能继续走低，需结合完整日线企稳和市场环境。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafBetween("boll_pos", 0, 25),
			leafTrue("boll_lower_reclaim"),
			leafV("chg_pct", ">", 0.5),
		),
	},
	{
		Key:    "boll-break-up",
		Name:   "放量突破布林上轨",
		Desc:   "收盘位于布林上轨之外，成交量为前 5 日均量的 1.5~6 倍。观察向上的波动扩张；价格可能延续也可能回落，需同时核查突破延伸、承接和近端阻力。",
		Period: "swing",
		Risk:   "high",
		Tree: allOf(
			leafV("boll_pos", ">", 100),
			leafBetween("vol_boost", 1.5, 6),
		),
	},
	{
		Key:    "ma-converge",
		Name:   "均线粘合待变盘",
		Desc:   "5/10/20 日均线之间的价差占比小于 2%，近 20 日波动率低于 3%。观察均线靠拢，不能证明充分换手或预测突破时间；方向未定，需要后续价格与量能确认。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafV("ma_spread_pct", "<", 2),
			leafV("volatility_20", "<", 3),
			leafV("amount_yi", ">=", 1),
		),
	},
	{
		Key:    "new-high-250",
		Name:   "创年内新高",
		Desc:   "至少 250 根完整日线，最新收盘严格创该窗口新高且温和放量。上市几十日的新高不算年内新高；窗口内没有更高收盘并不代表市场不存在套牢盘。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafTrue("high_250d"),
			leafBetween("vol_boost", 1.2, 5),
		),
	},
	// ---------- 中线 ----------
	{
		Key:    "bull-align-trend",
		Name:   "均线多头排列",
		Desc:   "MA5>MA10>MA20 且站上 MA60，观察中期趋势延续。均线反映历史均价，不能推断所有持仓者获利；需核对均线乖离、波动和后续破位风险。",
		Period: "mid",
		Risk:   "mid",
		Tree: allOf(
			leafTrue("bull_align"),
			leafTrue("above_ma60"),
			leafBetween("chg_pct", -2, 6),
		),
	},
	{
		Key:    "year-line-stand",
		Name:   "年线上方企稳",
		Desc:   "股价在 MA250 上方 0~10%、守住 MA20 且最新日线低点与收盘企稳，波动温和。年线反映历史均价，不能单独证明牛熊转换；需至少250根完整日线。",
		Period: "mid",
		Risk:   "low",
		Tree: allOf(
			leafBetween("bias_250", 0, 10),
			leafTrue("rq_stabilized"),
			leafTrue("above_ma20"),
			leafV("volatility_20", "<", 3.5),
		),
	},
	{
		Key:    "steady-uptrend",
		Name:   "稳步上行趋势",
		Desc:   "近 60 日涨幅为 10~40%、近 20 日波动率低于 3%，收盘位于 MA20 和 MA60 上方。观察较平稳的历史上行趋势，不能据形态推断机构持仓、公司质量或未来回撤。",
		Period: "mid",
		Risk:   "low",
		Tree: allOf(
			leafBetween("chg_60d", 10, 40),
			leafV("volatility_20", "<", 3),
			leafTrue("above_ma20"),
			leafTrue("above_ma60"),
		),
	},
	{
		Key:    "calm-consolidation",
		Name:   "缩量横盘蓄势",
		Desc:   "近 20 日波动率低于 2.5%、近 5 日均量低于 20 日均量的 90%，股价处于 60 日区间中部且站上 MA60。观察缩量整理；量缩不能证明浮筹清洗完毕，也不能预测整理方向和时长。",
		Period: "mid",
		Risk:   "low",
		Tree: allOf(
			leafV("volatility_20", "<", 2.5),
			leafV("vol_5v20", "<", 0.9),
			leafBetween("pos_60", 25, 70),
			leafTrue("above_ma60"),
		),
	},
	{
		Key:    "deep-oversold-chip",
		Name:   "超跌低获利筹码观察",
		Desc:   "估算获利筹码不足 10%、股价处 60 日区间底部，且筹码窗口至少 210 日。筹码来自换手衰减估计，不能证明真实持仓或抛压枯竭；未企稳时仅作左侧观察。",
		Period: "mid",
		Risk:   "high",
		Tree: allOf(
			leafV("chip_profit", "<", 10),
			leafV("pos_60", "<", 25),
			leafV("chip_bars", ">=", 210),
		),
	},
	{
		Key:    "low-vol-trend",
		Name:   "低波动趋势股",
		Desc:   "ATR 占现价低于 2.5%、均线多头排列且近 20 日涨幅为正。观察低波动上行形态；历史波动较小不保证未来稳定，仍需检查流动性、入场距离和事件风险。",
		Period: "mid",
		Risk:   "low",
		Tree: allOf(
			leafV("atr_pct", "<", 2.5),
			leafTrue("bull_align"),
			leafV("chg_20d", ">", 0),
		),
	},
	{
		Key: "ma20-cross-ma60", Name: "20/60日均线金叉", Period: "mid", Risk: "mid",
		Desc: "近 3 日 MA20 上穿 MA60，当前仍保持金叉且股价站上两条均线。至少 63 根完整日线，关注中期趋势转换；均线滞后，仍需核对延伸距离。",
		Tree: allOf(leafTrue("ma20_cross_ma60"), leafTrue("above_ma20"), leafTrue("above_ma60"), leafV("amount_yi", ">=", 1)),
	},
	{
		Key: "breakout-retest", Name: "放量突破后回踩确认", Period: "swing", Risk: "mid",
		Desc: "2～10 日前放量突破前 20 日最高价，随后回踩原平台 0.5 ATR 范围，收盘守住且低点企稳。排除突破前的普通回落和中间已失守的平台；至少 31 根日线。",
		Tree: allOf(leafTrue("breakout_retest"), leafV("vol_boost", "<=", 1.5), leafV("amount_yi", ">=", 1)),
	},
	{
		Key: "boll-squeeze-break", Name: "布林收口后放量突破", Period: "swing", Risk: "high",
		Desc: "昨日布林带宽处于前 60 个完整观测的最低 30%，最新收盘突破昨日上轨且放量 1.5~4 倍。先收口再突破，至少 80 根日线；不能将波动扩张等同于持续上涨。",
		Tree: allOf(leafTrue("boll_squeeze_break"), leafBetween("vol_boost", 1.5, 4), leafV("rq_close_location", ">=", 0.65), leafFalse("limit_up_today"), leafV("amount_yi", ">=", 2)),
	},
	{
		Key: "donchian-55", Name: "55日价格通道突破", Period: "mid", Risk: "high",
		Desc: "收盘严格突破此前 55 个完整交易日最高价，配合温和放量和较强收盘位置。这是常见中期趋势跟踪信号；只检查入场形态，退出由统一风险规划管理。",
		Tree: allOf(leafTrue("donchian55_break"), leafBetween("vol_boost", 1.2, 4), leafV("rq_close_location", ">=", 0.6), leafFalse("limit_up_today"), leafV("amount_yi", ">=", 2)),
	},
	{
		Key: "kdj-low-cross", Name: "KDJ低位金叉修复", Period: "short", Risk: "high",
		Desc: "KDJ(9,3,3) 昨日 K、D 均低于 30，最新完整日 K 上穿 D 且收盘上涨，价格处于 60 日区间下半部。需要至少 60 根日线；震荡指标在单边下跌中可能反复失效，需继续确认企稳。",
		Tree: allOf(leafTrue("kdj_low_cross"), leafV("chg_pct", ">", 0), leafV("pos_60", "<", 50), leafV("amount_yi", ">=", 1)),
	},
}

// builtinScreenByKey 按 key 取内置策略。
func builtinScreenByKey(key string) (builtinScreen, bool) {
	for _, b := range builtinScreens {
		if b.Key == key {
			return b, true
		}
	}
	return builtinScreen{}, false
}

// recStrategySignalKey 推荐策略 → 选股信号策略的映射（strategy_signal 进池来源）。
// 与推荐策略意图对齐：动量→突破新高、回踩→缩量回踩、活跃→温和放量、
// 价值→超跌筹码、成长→稳步上行、龙头→多头排列。
func recStrategySignalKey(recType, stratKey string) string {
	if recType == model.RecTypeShortTerm {
		switch stratKey {
		case "pullback":
			return "shrink-pullback-ma20"
		case "active":
			return "mild-vol-start"
		default: // momentum
			return "vol-break-20d"
		}
	}
	switch stratKey {
	case "value":
		return "deep-oversold-chip"
	case "leader":
		return "bull-align-trend"
	default: // growth
		return "steady-uptrend"
	}
}
