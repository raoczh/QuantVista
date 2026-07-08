package service

import "quantvista/model"

// M1 内置选股策略（约 20 个白话策略）：只用因子宽表已有因子（factorDefs），
// 白话讲解 + 适用周期 + 风险等级。参考 StockNova builtin 思路按现有因子重写，
// 阈值沿用项目内已有共识（量比 1.5~5 温和放量、换手 3~15 活跃、RSI 凹形逻辑等）。
//
// 纪律：新增策略只能引用 factorDefs 内因子（validateCondTree 会拦）；估值类
//（PE/PB）宽表没有（腾讯估值是实时单只接口，无法全市场普查），别写进内置策略。

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
		Desc:   "收盘价刷新近 20 日高点且成交量温和放大（1.5~5 倍），突破有量能确认。当日涨幅限制在 9% 内，排除已涨停买不进的。追突破需设好止损，假突破回落要果断离场。",
		Period: "short",
		Risk:   "high",
		Tree: allOf(
			leafTrue("high_20d"),
			leafBetween("vol_boost", 1.5, 5),
			leafV("chg_pct", "<", 9),
			leafV("amount_yi", ">=", 2),
		),
	},
	{
		Key:    "shrink-pullback-ma20",
		Name:   "强势股缩量回踩MA20",
		Desc:   "近 20 日涨过 10% 的强势股，近几日缩量回调但没跌破 20 日均线——主升浪中的技术性回踩，缩量说明抛压不重。若放量跌破 MA20 则形态破坏。",
		Period: "short",
		Risk:   "mid",
		Tree: allOf(
			leafV("chg_20d", ">=", 10),
			leafTrue("above_ma20"),
			leafBetween("chg_5d", -8, 0),
			leafV("vol_5v20", "<", 0.9),
		),
	},
	{
		Key:    "mild-vol-start",
		Name:   "低位温和放量",
		Desc:   "股价处 60 日区间下半场，当日温和放量（1.5~3 倍）小幅上涨 1~6%——可能是资金试探性建仓的启动迹象。位置低风险相对可控，但也可能只是一日游，看后续量能是否持续。",
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
		Desc:   "60 日区间底部 30% 位置，当日放量（2 倍以上）拉出 5% 以上长阳——超跌后的强反弹信号，常见于利空出尽或资金抄底。底部第一根长阳后常有回踩，不急于全仓。",
		Period: "short",
		Risk:   "high",
		Tree: allOf(
			leafV("pos_60", "<", 30),
			leafV("chg_pct", ">", 5),
			leafV("vol_boost", ">", 2),
			leafV("amount_yi", ">=", 1),
		),
	},
	{
		Key:    "yang-through-3ma",
		Name:   "一阳穿三线",
		Desc:   "开盘在 5/10/20 日均线下方，收盘一根阳线全部收复——短期均线的集中突破，多头一次性夺回主动权。要求涨幅 >2% 保证是实体阳线。次日若不回落确认，动能较强。",
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
		Desc:   "近 5 日有过涨停的活跃股，今日温和回调 0.5~6% 且守住 10 日线——涨停打开空间后的洗盘形态。妖股逻辑，波动极大，只适合能盯盘的短线玩家，严格止损。",
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
		Desc:   "近 20 日涨超 15% 的强势股，近 5 日横盘整理（-5%~2%）且未破 20 日线——强势股的中继形态，整理后常有第二波。若整理时间过长或放量下跌则动能衰竭。",
		Period: "short",
		Risk:   "mid",
		Tree: allOf(
			leafV("chg_20d", ">", 15),
			leafBetween("chg_5d", -5, 2),
			leafTrue("above_ma20"),
		),
	},
	{
		Key:    "rsi-strong-zone",
		Name:   "RSI强势区未过热",
		Desc:   "RSI(14) 处 55~70 强势区间且均线多头排列——趋势健康、动能充足但还没到超买（≥70 追高风险陡增）。顺势而为的标准姿势，跌破 20 日线离场。",
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
		Desc:   "DIF 在零轴上方金叉 DEA（近 3 日内）——多头趋势中的二次启动信号，比零轴下金叉可靠得多，是 MACD 最经典的买点形态。配合放量确认更佳。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafTrue("macd_cross_up"),
			leafV("macd_dif", ">", 0),
		),
	},
	{
		Key:    "macd-gold-under",
		Name:   "MACD水下金叉（超跌反弹）",
		Desc:   "DIF 在零轴下方金叉 DEA 且股价处 60 日区间下 40%——超跌后的反弹尝试信号。零轴下金叉失败率不低，只做反弹不做反转，目标位不宜贪。",
		Period: "swing",
		Risk:   "high",
		Tree: allOf(
			leafTrue("macd_cross_up"),
			leafV("macd_dif", "<", 0),
			leafV("pos_60", "<", 40),
		),
	},
	{
		Key:    "rsi-oversold-up",
		Name:   "RSI超卖回升",
		Desc:   "RSI(14) 从超卖区回升至 30~45 且当日收红——恐慌抛售衰竭后的修复启动。左侧偏右的买点，比在 RSI<30 时硬接刀子安全，但仍需确认下跌主因是否消除。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafBetween("rsi_14", 30, 45),
			leafV("chg_pct", ">", 0),
		),
	},
	{
		Key:    "boll-lower-bounce",
		Name:   "布林下轨反弹",
		Desc:   "股价触及布林带下轨附近（带内位置 0~25%）后收红——统计意义上的超卖修复。震荡市胜率较高，单边下跌市会沿下轨阴跌（「骑轨」），需结合大盘环境。",
		Period: "swing",
		Risk:   "mid",
		Tree: allOf(
			leafBetween("boll_pos", 0, 25),
			leafV("chg_pct", ">", 0.5),
		),
	},
	{
		Key:    "boll-break-up",
		Name:   "放量突破布林上轨",
		Desc:   "收盘突破布林上轨且放量 1.5 倍以上——波动率扩张的强势信号，常是主升浪起点。但上轨外无法长留，要么快速拉升要么回带内，节奏快，适合激进风格。",
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
		Desc:   "5/10/20 日均线彼此偏差 <2% 且波动率收敛——多空充分换手后的平衡态，变盘窗口临近。方向未定：向上放量突破跟进，向下破位回避。这是「等信号」的池子而非买入清单。",
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
		Desc:   "收盘创近一年新高且量能温和放大——上方无套牢盘，「新高之上皆坦途」的动量逻辑。A 股新高股的延续性两极分化，需甄别是业绩驱动还是纯情绪炒作。",
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
		Desc:   "MA5>MA10>MA20 且站上 60 日线——教科书式的多头趋势结构，各周期持仓者全部获利、抛压小。趋势跟踪的基本盘，均线拐头前持有，适合不盯盘的波段/中线仓。",
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
		Desc:   "股价站在年线（250 日均线）附近 -3%~10% 区间且波动温和——牛熊分界线上的蓄势区。年线是长线资金的成本锚，其上企稳说明中期趋势由弱转强。",
		Period: "mid",
		Risk:   "low",
		Tree: allOf(
			leafBetween("bias_250", -3, 10),
			leafTrue("above_ma20"),
			leafV("volatility_20", "<", 3.5),
		),
	},
	{
		Key:    "steady-uptrend",
		Name:   "稳步上行趋势",
		Desc:   "近 60 日涨 10~40%、波动率低于 3%、站稳 20/60 日线——不急不躁的慢牛形态，常见于基本面扎实的机构票。涨幅上限 40% 排除已经涨疯的，回撤风险相对小。",
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
		Desc:   "波动率 <2.5%、量能萎缩、60 日区间中部横盘且守住 60 日线——无人问津的安静票，浮筹清洗充分。适合左侧埋伏等催化，缺点是不知道要横多久。",
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
		Name:   "超跌抛压枯竭（获利盘<10%）",
		Desc:   "获利盘不足 10%（几乎全员套牢）且股价处 60 日区间底部——想卖的早就卖了，抛压趋于枯竭的左侧关注区。要求筹码窗口完整（210 日）排除次新股失真。左侧策略需耐心与分批。",
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
		Desc:   "日均波幅（ATR 占比）<2.5% 且均线多头、近 20 日为正收益——波动小、趋势稳的「安稳票」，拿得住是这类票最大的优势，适合底仓配置。",
		Period: "mid",
		Risk:   "low",
		Tree: allOf(
			leafV("atr_pct", "<", 2.5),
			leafTrue("bull_align"),
			leafV("chg_20d", ">", 0),
		),
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
