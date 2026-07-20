package service

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"quantvista/datasource"
	"quantvista/model"
)

// 推荐流水线阶段③：本地量化因子与评分（零 LLM 成本）。
// 因子全部由 90 根前复权日线 + 估值快照派生（Qlib Alpha158 思路的轻量子集），
// 复合分 = 五维技术评分（computeScore，与个股详情/对比同口径）+ 策略加分项（逐条可解释）。
// LLM 在此之后只对排名靠前的候选做「研究员式」解读与否决，不再海选。

// candFactors 单只候选的技术因子快照（全部落库、喂给 LLM、前端可展开）。
type candFactors struct {
	MA5          float64 `json:"ma5,omitempty"`
	MA10         float64 `json:"ma10,omitempty"`
	MA20         float64 `json:"ma20,omitempty"`
	MA60         float64 `json:"ma60,omitempty"`
	Chg5d        float64 `json:"chg_5d"`                  // 近 5 日涨跌 %
	Chg20d       float64 `json:"chg_20d"`                 // 近 20 日涨跌 %
	High20d      bool    `json:"high_20d,omitempty"`      // 收盘创 20 日新高
	BullAlign    bool    `json:"bull_align,omitempty"`    // MA5>MA10>MA20 多头排列
	AboveMA20    bool    `json:"above_ma20,omitempty"`    // 现价站上 MA20
	VolBoost     float64 `json:"vol_boost,omitempty"`     // 今日量 / 前 5 日均量（日线量比近似）
	Vol5v20      float64 `json:"vol_5v20,omitempty"`      // 5 日均量 / 20 日均量
	Volatility20 float64 `json:"volatility_20,omitempty"` // 近 20 日日收益率标准差 %
	Drawdown20   float64 `json:"drawdown_20,omitempty"`   // 近 20 日最大回撤 %（正数）
	Bias20       float64 `json:"bias_20,omitempty"`       // (现价/MA20-1)% 乖离率
	Pos60        float64 `json:"pos_60"`                  // 60 日区间位置 0-100
	BarCount     int     `json:"bar_count"`

	// T1 经典指标（indicator.go 口径：RSI/ATR Wilder 平滑、MACD 柱 2 倍、BOLL 2σ）。
	// 全部 omitempty：样本不足时缺席，LLM/前端见不到即不会引用。
	RSI14     float64 `json:"rsi_14,omitempty"`
	MACDDif   float64 `json:"macd_dif,omitempty"`
	MACDDea   float64 `json:"macd_dea,omitempty"`
	MACDHist  float64 `json:"macd_hist,omitempty"`     // 2×(DIF−DEA) A 股柱口径
	MACDGold  bool    `json:"macd_gold,omitempty"`     // DIF>DEA 多头状态
	MACDXUp   bool    `json:"macd_cross_up,omitempty"` // 近 3 日 DIF 上穿 DEA（金叉）
	BollUp    float64 `json:"boll_up,omitempty"`
	BollMid   float64 `json:"boll_mid,omitempty"`
	BollLow   float64 `json:"boll_low,omitempty"`
	BollPos   float64 `json:"boll_pos,omitempty"` // 带内位置 %（<0 破下轨、>100 破上轨）
	ATR14     float64 `json:"atr_14,omitempty"`
	ATRPct    float64 `json:"atr_pct,omitempty"` // ATR/现价 %

	// T1 筹码分布（chip.go 三角衰减模型，需 210 根日线；ChipBars=0 表示未算/不足）。
	ChipProfit  float64 `json:"chip_profit,omitempty"`   // 获利盘 %（收盘价下方筹码占比）
	ChipAvgCost float64 `json:"chip_avg_cost,omitempty"` // 筹码平均成本
	ChipBars    int     `json:"chip_bars,omitempty"`     // 参与筹码计算的日线根数

	// M3a 主力资金流因子（fund_flow_daily 缓存派生；缺失=资金流数据暂不可得）。
	MainNetDays int     `json:"main_net_days,omitempty"` // 连续净流入天数（负=连续净流出）
	MainNet5dYi float64 `json:"main_net_5d_yi,omitempty"` // 近 5 日主力净额（亿元）

	// M3b 盘中因子（腾讯 5 分钟线盘后聚合；T-1 信号——最近一个已同步交易日的
	// 盘中形态，IntradayDate 声明归属日，缺失=盘中数据暂不可得）。
	IntradayDate string  `json:"intraday_date,omitempty"`  // 因子归属交易日
	Tail30Chg    float64 `json:"tail30_chg,omitempty"`     // 尾盘30分钟涨幅 %
	Tail30VolPct float64 `json:"tail30_vol_pct,omitempty"` // 尾盘30分钟量占全天 %（均匀线 12.5%）
	MorningChg   float64 `json:"morning_chg,omitempty"`    // 早盘1小时涨幅 %
	CloseVsVwap  float64 `json:"close_vs_vwap,omitempty"`  // 收盘 vs 全天VWAP 偏离 %
	PmVwapUp     bool    `json:"pm_vwap_up,omitempty"`     // 下午VWAP>上午（日内重心上移）
}

// scoreDims 五维评分明细（透传 computeScore 结果，前端展示雷达/分项）。
type scoreDims struct {
	Trend    float64 `json:"trend"`
	Momentum float64 `json:"momentum"`
	Position float64 `json:"position"`
	Volume   float64 `json:"volume"`
	Risk     float64 `json:"risk"`
}

// computeCandFactors 由现价与升序日线派生技术因子。bars 为空返回 nil。
func computeCandFactors(price float64, bars []datasource.Bar) *candFactors {
	n := len(bars)
	if n == 0 || price <= 0 {
		return nil
	}
	closes := make([]float64, n)
	vols := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
		vols[i] = float64(b.Volume)
	}
	f := &candFactors{BarCount: n}
	if v, ok := movingAverage(closes, 5); ok {
		f.MA5 = round2(v)
	}
	if v, ok := movingAverage(closes, 10); ok {
		f.MA10 = round2(v)
	}
	if v, ok := movingAverage(closes, 20); ok {
		f.MA20 = round2(v)
	}
	if v, ok := movingAverage(closes, 60); ok {
		f.MA60 = round2(v)
	}
	f.Chg5d = changeOverN(closes, 5)
	f.Chg20d = changeOverN(closes, 20)
	f.BullAlign = f.MA5 > 0 && f.MA10 > 0 && f.MA20 > 0 && f.MA5 > f.MA10 && f.MA10 > f.MA20
	f.AboveMA20 = f.MA20 > 0 && price >= f.MA20
	if f.MA20 > 0 {
		f.Bias20 = round2((price/f.MA20 - 1) * 100)
	}

	// 收盘创 20 日新高：末根收盘 ≥ 前 20 根收盘最大值（收盘口径假突破更少）。
	if n >= 21 {
		maxPrev := closes[n-21]
		for _, c := range closes[n-21 : n-1] {
			if c > maxPrev {
				maxPrev = c
			}
		}
		f.High20d = closes[n-1] >= maxPrev
	}

	// 量能：今日量 / 前 5 日均量（剔除今日，日线口径的量比近似）；5 日均量 / 20 日均量。
	if n >= 6 {
		var prev5 float64
		for _, v := range vols[n-6 : n-1] {
			prev5 += v
		}
		prev5 /= 5
		if prev5 > 0 {
			f.VolBoost = round2(vols[n-1] / prev5)
		}
	}
	avgVol := func(w int) float64 {
		if w > n {
			w = n
		}
		var s float64
		for _, v := range vols[n-w:] {
			s += v
		}
		return s / float64(w)
	}
	if a20 := avgVol(20); a20 > 0 {
		f.Vol5v20 = round2(avgVol(5) / a20)
	}

	// 近 20 日：日收益率标准差（波动率）与最大回撤。
	w := 20
	if w > n {
		w = n
	}
	win := bars[n-w:]
	if len(win) >= 2 {
		var rets []float64
		for i := 1; i < len(win); i++ {
			if win[i-1].Close > 0 {
				rets = append(rets, (win[i].Close-win[i-1].Close)/win[i-1].Close*100)
			}
		}
		f.Volatility20 = round2(stddev(rets))
	}
	peak := win[0].High
	worst := 0.0
	for _, b := range win {
		if b.High > peak {
			peak = b.High
		}
		if peak > 0 {
			if dd := (b.Low - peak) / peak; dd < worst {
				worst = dd
			}
		}
	}
	f.Drawdown20 = round2(-worst * 100)

	// 60 日区间位置（严格取尾部 60 根——bars 总量为 factorBarLimit=90，
	// 全窗遍历会把口径悄悄变成 90 日，而 Pos60 已是高位换手硬排除的判定依据）。
	win60 := bars
	if len(win60) > 60 {
		win60 = win60[len(win60)-60:]
	}
	hi, lo := win60[0].High, win60[0].Low
	for _, b := range win60 {
		if b.High > hi {
			hi = b.High
		}
		if b.Low < lo && b.Low > 0 {
			lo = b.Low
		}
	}
	if hi > lo {
		f.Pos60 = round2((price - lo) / (hi - lo) * 100)
	} else {
		f.Pos60 = 50
	}

	// T1 经典指标快照（各指标按样本量独立可用；筹码字段由调用方另行填充——
	// 它需要 210 根窗口与换手率，见 scorePool）。
	if snap := computeIndicatorSnapshot(price, bars); snap != nil {
		f.RSI14 = snap.RSI14
		f.MACDDif, f.MACDDea, f.MACDHist = snap.MACDDif, snap.MACDDea, snap.MACDHist
		f.MACDGold, f.MACDXUp = snap.MACDGold, snap.MACDXUp
		f.BollUp, f.BollMid, f.BollLow, f.BollPos = snap.BollUp, snap.BollMid, snap.BollLow, snap.BollPos
		f.ATR14, f.ATRPct = snap.ATR14, snap.ATRPct
	}
	return f
}

// stddev 样本标准差（n-1）；样本不足返回 0。
func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// strategyAdjust 策略加分/扣分（逐条给中文说明，随候选落库展示——评分可解释是信任的根基）。
// 返回分数增量与说明列表。参数阈值来自社区实战共识（量比 1.5~5 温和/有效放量、
// 换手 3~15% 活跃、BIAS20>12% 超买、近 20 日涨幅 ≥15% 为前期强势 等）。
func strategyAdjust(recType, stratKey string, c candidate, f *candFactors) (float64, []string) {
	if f == nil {
		return 0, nil
	}
	var delta float64
	var notes []string
	add := func(d float64, note string) {
		delta += d
		sign := "+"
		if d < 0 {
			sign = ""
		}
		notes = append(notes, fmt.Sprintf("%s（%s%.0f）", note, sign, d))
	}

	// N2 消息面因子（短线/长线通用）：当日聚合情绪分显著偏向时加/扣分。
	// 利空扣得比利好加得重（消息面上「坏消息更真」的不对称性）；|score|<0.3 视为噪声不动分。
	if c.SentiNews > 0 {
		switch {
		case c.SentiScore >= 0.3:
			add(3, fmt.Sprintf("当日利好情绪 %.2f（%d 条新闻）", c.SentiScore, c.SentiNews))
		case c.SentiScore <= -0.3:
			add(-4, fmt.Sprintf("当日利空情绪 %.2f（%d 条新闻）", c.SentiScore, c.SentiNews))
		}
	}

	// T1 筹码超跌信号（短线/长线通用）：获利盘极低=几乎全员套牢，抛压趋于枯竭的
	// 左侧关注区。仅在筹码窗口完备（210 根，ChipBars 达标）时给分——次新股累积
	// 不足会系统性失真，宁可不加。
	if f.ChipBars >= chipBarLimit && f.ChipProfit < 10 {
		add(4, fmt.Sprintf("获利盘仅 %.1f%% 超跌区（抛压趋于枯竭）", f.ChipProfit))
	}

	// M3a 龙虎榜/机构/人气/主力资金因子（短线/长线通用；全部为「最近有数据交易日」
	// 口径的 T-1 信号，缺失不动分）。机构席位方向权重最高（信息含量>游资席位）；
	// 龙虎榜净卖出与机构净卖出对称扣分（涨停上榜出货是经典陷阱，只加不扣会系统性乐观）。
	if c.OrgNetYi >= 0.1 && c.OrgBuys > 0 {
		add(5, fmt.Sprintf("龙虎榜机构净买入 %.2f 亿（%d 家次买入）", c.OrgNetYi, c.OrgBuys))
	} else if c.OrgNetYi <= -0.1 {
		add(-4, fmt.Sprintf("龙虎榜机构净卖出 %.2f 亿", -c.OrgNetYi))
	} else if c.LhbNetYi >= 0.3 {
		reason := c.LhbReason
		if reason == "" {
			reason = "上榜"
		}
		add(3, fmt.Sprintf("龙虎榜净买入 %.2f 亿（%s）", c.LhbNetYi, reason))
	} else if c.LhbNetYi <= -0.3 {
		add(-3, fmt.Sprintf("龙虎榜净卖出 %.2f 亿", -c.LhbNetYi))
	}
	if c.PopRank > 0 && c.PopRank <= 50 {
		if c.PopNew {
			add(3, fmt.Sprintf("股吧人气榜新上榜第 %d 名（关注度骤升，注意情绪拥挤风险）", c.PopRank))
		} else if c.PopPrev-c.PopRank >= 30 {
			add(2, fmt.Sprintf("股吧人气榜跃升至第 %d 名（昨日第 %d）", c.PopRank, c.PopPrev))
		}
	}
	if f.MainNetDays >= 3 {
		add(4, fmt.Sprintf("主力资金连续净流入 %d 天（近5日 %.2f 亿）", f.MainNetDays, f.MainNet5dYi))
	} else if f.MainNetDays <= -4 {
		add(-3, fmt.Sprintf("主力资金连续净流出 %d 天", -f.MainNetDays))
	}

	if recType == model.RecTypeShortTerm {
		// 短线共用风险扣分。
		if f.Bias20 > 12 {
			add(-6, fmt.Sprintf("MA20乖离 %.1f%% 超买", f.Bias20))
		}
		if f.VolBoost > 5 {
			add(-4, fmt.Sprintf("量比近似 %.1f 爆量警惕", f.VolBoost))
		}
		// M3b 盘中因子（短线专属：日内资金行为对隔日走势的预测力集中在短周期；
		// T-1 信号，IntradayDate 非空才有数据，缺失不动分）。尾盘方向对称加扣
		//（尾盘拉升可能是抢筹也可能是尾盘偷袭出货前的诱多，放量才升档）；
		// 收盘 vs VWAP 是全天买卖力量的总结算，弱于均价的扣分轻于跳水（常态回落）。
		if f.IntradayDate != "" {
			switch {
			case f.Tail30Chg >= 1.5 && f.Tail30VolPct >= 20:
				add(4, fmt.Sprintf("尾盘30分放量拉升 %.1f%%（量占全天 %.0f%%，资金抢筹迹象）", f.Tail30Chg, f.Tail30VolPct))
			case f.Tail30Chg >= 1.5:
				add(3, fmt.Sprintf("尾盘30分拉升 %.1f%%", f.Tail30Chg))
			case f.Tail30Chg <= -1.5 && f.Tail30VolPct >= 20:
				add(-4, fmt.Sprintf("尾盘30分放量跳水 %.1f%%（量占全天 %.0f%%，资金出逃迹象）", f.Tail30Chg, f.Tail30VolPct))
			case f.Tail30Chg <= -1.5:
				add(-3, fmt.Sprintf("尾盘30分跳水 %.1f%%", f.Tail30Chg))
			}
			if f.CloseVsVwap >= 1 {
				add(3, fmt.Sprintf("收盘强于全天均价 %.1f%%（买方主导）", f.CloseVsVwap))
			} else if f.CloseVsVwap <= -1.5 {
				add(-2, fmt.Sprintf("收盘弱于全天均价 %.1f%%", f.CloseVsVwap))
			}
			if f.PmVwapUp && f.MorningChg >= 0 {
				add(2, "午后重心上移（下午VWAP>上午）")
			}
			if f.MorningChg >= 2 && f.Tail30Chg >= 0 {
				add(2, fmt.Sprintf("早盘1小时强势 %.1f%% 且尾盘未回吐", f.MorningChg))
			}
		}
		switch stratKey {
		case "momentum":
			if f.High20d {
				add(6, "收盘创20日新高")
			}
			if f.BullAlign {
				add(5, "MA5>MA10>MA20 多头排列")
			}
			if f.VolBoost >= 1.5 && f.VolBoost <= 5 {
				add(4, fmt.Sprintf("放量健康（%.1f 倍）", f.VolBoost))
			}
			// T1：水上金叉是动量策略最经典的确认信号；RSI 强势区不过热再确认。
			if f.MACDXUp && f.MACDDif > 0 {
				add(4, "MACD 水上金叉（近3日）")
			}
			if f.RSI14 >= 55 && f.RSI14 <= 70 {
				add(3, fmt.Sprintf("RSI %.0f 强势区未过热", f.RSI14))
			}
		case "pullback":
			if f.Chg20d >= 15 {
				add(5, fmt.Sprintf("近20日涨 %.1f%% 前期强势", f.Chg20d))
			}
			if f.AboveMA20 && f.Chg5d <= 0 && f.Chg5d >= -6 {
				add(5, fmt.Sprintf("近5日回调 %.1f%% 未破MA20", f.Chg5d))
			}
			if f.Vol5v20 > 0 && f.Vol5v20 < 0.9 {
				add(3, "回调缩量（供给衰竭）")
			}
			// T1：RSI 回落至中低位=回调充分未超卖；回踩至布林中轨下方（带内下半区）。
			if f.RSI14 >= 30 && f.RSI14 <= 45 {
				add(3, fmt.Sprintf("RSI %.0f 回调充分未超卖", f.RSI14))
			}
			if f.BollMid > 0 && f.BollPos >= 10 && f.BollPos <= 50 {
				add(3, fmt.Sprintf("回踩布林带下半区（带内 %.0f%%）", f.BollPos))
			}
		case "active":
			if c.TurnoverRate >= 5 && c.TurnoverRate <= 15 {
				add(5, fmt.Sprintf("换手 %.1f%% 活跃区间", c.TurnoverRate))
			}
			if f.VolBoost >= 2 {
				add(4, fmt.Sprintf("显著放量（%.1f 倍）", f.VolBoost))
			}
			if c.VolumeRatio >= 1.5 && c.VolumeRatio <= 5 {
				add(3, fmt.Sprintf("实时量比 %.1f 温和放量", c.VolumeRatio))
			}
		}
		return delta, notes
	}

	// 长线：估值与稳定性导向（估值缺失只是不加分，不虚构）。
	// 严格 <0 才判亏损：PETTM==0 是「估值缺失」而非亏损（腾讯估值失败但新浪
	// 榜单 PB 兜底存在时，旧条件 <=0 && PB>0 会把缺失误标成「PE 为负」假证据）。
	if c.PETTM < 0 {
		add(-5, "PE 为负（亏损）")
	}
	// F2 财务因子（有数据才动分，缺失不惩罚——c.Fin=nil 表示无缓存且预算耗尽）。
	// 业绩恶化是长线通用扣分；ROE/增速加分随策略在下方分派。
	if fin := c.Fin; fin != nil && fin.NetProfitYoY <= -30 {
		add(-5, fmt.Sprintf("净利同比 %.1f%% 业绩恶化（%s）", fin.NetProfitYoY, fin.Report))
	}
	switch stratKey {
	case "value":
		switch {
		case c.PETTM > 0 && c.PETTM <= 15:
			add(8, fmt.Sprintf("PE-TTM %.1f 低估区", c.PETTM))
		case c.PETTM > 15 && c.PETTM <= 25:
			add(5, fmt.Sprintf("PE-TTM %.1f 合理区", c.PETTM))
		}
		switch {
		case c.PB > 0 && c.PB <= 1.5:
			add(5, fmt.Sprintf("PB %.2f 破净/低位", c.PB))
		case c.PB > 1.5 && c.PB <= 3:
			add(3, fmt.Sprintf("PB %.2f 不高", c.PB))
		}
		if f.Pos60 <= 50 {
			add(4, fmt.Sprintf("60日区间位置 %.0f%% 未追高", f.Pos60))
		}
		if f.Volatility20 > 0 && f.Volatility20 <= 2 {
			add(3, fmt.Sprintf("波动率 %.1f%% 偏低", f.Volatility20))
		}
		// T1：RSI 超卖区=价值策略的左侧买点参照。
		if f.RSI14 > 0 && f.RSI14 <= 35 {
			add(3, fmt.Sprintf("RSI %.0f 超卖区（左侧）", f.RSI14))
		}
		// F2：低估值 + 高 ROE 是价值策略的黄金组合；净利正增长确认盈利质量。
		if fin := c.Fin; fin != nil {
			switch {
			case fin.ROE >= 12:
				add(5, fmt.Sprintf("ROE %.1f%% 优秀（%s）", fin.ROE, fin.Report))
			case fin.ROE >= 8:
				add(3, fmt.Sprintf("ROE %.1f%% 良好（%s）", fin.ROE, fin.Report))
			}
			if fin.NetProfitYoY >= 10 {
				add(3, fmt.Sprintf("净利同比 +%.1f%%", fin.NetProfitYoY))
			}
		}
	case "growth":
		if f.MA60 > 0 && c.Price > f.MA60 {
			add(5, "站上 MA60 中期趋势向上")
		}
		if f.BullAlign {
			add(4, "均线多头排列")
		}
		if f.Chg20d > 0 && f.Chg20d <= 25 {
			add(4, fmt.Sprintf("近20日涨 %.1f%% 趋势健康", f.Chg20d))
		}
		if f.Bias20 > 20 {
			add(-6, fmt.Sprintf("MA20乖离 %.1f%% 追高风险", f.Bias20))
		}
		// T1：MACD 水上多头=中期趋势的动量确认。
		if f.MACDGold && f.MACDDif > 0 {
			add(3, "MACD 多头（DIF>DEA 且水上）")
		}
		// F2：营收/净利双增速是成长策略的核心证据（高增速给更高档加分）。
		if fin := c.Fin; fin != nil {
			switch {
			case fin.RevenueYoY >= 20:
				add(5, fmt.Sprintf("营收同比 +%.1f%% 高增长（%s）", fin.RevenueYoY, fin.Report))
			case fin.RevenueYoY >= 10:
				add(3, fmt.Sprintf("营收同比 +%.1f%%（%s）", fin.RevenueYoY, fin.Report))
			}
			switch {
			case fin.NetProfitYoY >= 30:
				add(5, fmt.Sprintf("净利同比 +%.1f%% 高增长", fin.NetProfitYoY))
			case fin.NetProfitYoY >= 15:
				add(3, fmt.Sprintf("净利同比 +%.1f%%", fin.NetProfitYoY))
			}
		}
	case "leader":
		if c.TotalCap >= 500e8 {
			add(5, fmt.Sprintf("总市值 %.0f 亿龙头体量", c.TotalCap/1e8))
		}
		if f.MA60 > 0 && c.Price > f.MA60 {
			add(4, "站上 MA60")
		}
		if f.Volatility20 > 0 && f.Volatility20 <= 2.5 {
			add(3, "波动稳定")
		}
		if c.TurnoverRate > 0 && c.TurnoverRate <= 8 {
			add(2, fmt.Sprintf("换手 %.1f%% 不过热", c.TurnoverRate))
		}
		// F2：龙头看盈利质量与业绩稳健。
		if fin := c.Fin; fin != nil {
			if fin.ROE >= 15 {
				add(4, fmt.Sprintf("ROE %.1f%% 盈利质量高（%s）", fin.ROE, fin.Report))
			}
			if fin.NetProfitYoY >= 10 {
				add(2, fmt.Sprintf("净利同比 +%.1f%%", fin.NetProfitYoY))
			}
		}
	}
	return delta, notes
}

// --- 程序化证据核验（防幻觉最便宜的一招）---

// evidenceCheck 数值存在性核验结果：AI 文本里引用的数字规范化（单位/方向）后与带路径的
// 数据快照值域比对。Total/Matched 为去重后的可核验数字计数；Items 为逐项明细（新增，旧行无）。
// Matched 是总命中数（legacy 兼容）——ev3 起按值域来源拆分：snapshot_matched 才是
// 「被数据快照佐证」，plan/user/context 命中只是「合法复述」（模型复述自身计划价、
// 用户输入、新闻标题等），不得混称「快照命中」。
type evidenceCheck struct {
	Total          int            `json:"total"`                     // 可核验数字个数（去重后）
	Matched        int            `json:"matched"`                   // 总命中数（legacy：含 plan/user/context 复述命中）
	Unmatched      []string       `json:"unmatched,omitempty"`       // 未吻合数字原样（≤10，legacy 兼容）
	Version        string         `json:"version,omitempty"`         // 核验引擎版本（ev2 起带 items；ev3 起带来源分类计数）
	SkippedCount   int            `json:"skipped_count,omitempty"`   // 跳过的非核验对象个数（小整数/年份/代码）
	UnmatchedTotal int            `json:"unmatched_total,omitempty"` // 未吻合总数（Total-Matched）
	Truncated      bool           `json:"truncated,omitempty"`       // items 是否被截断至 50
	Items          []evidenceItem `json:"items,omitempty"`           // 逐项明细

	SnapshotMatched int `json:"snapshot_matched"`           // 被数据快照佐证的个数（origin 空）
	PlanMatched     int `json:"plan_matched,omitempty"`     // 命中模型自身计划价/公式输出（复述，非快照佐证）
	UserMatched     int `json:"user_matched,omitempty"`     // 命中用户输入/设定阈值（复述，非快照佐证）
	ContextMatched  int `json:"context_matched,omitempty"`  // 命中新闻/公告标题、提醒文案等上下文文本
}

var evidenceNumRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

// 惯用窗口/参数整数中 >99 的部分（MA120、180 日机构观点窗、250 日年线、百分位 100）；
// ≤99 的整数已由 verifyEvidence 的小整数规则整体跳过。
var evidenceNoiseInts = map[float64]bool{
	100: true, 120: true, 180: true, 250: true,
}

// candidateLabeledValues 汇总一只候选可被引用的全部数值（带字段路径，含亿元衍生），供核验比对。
func candidateLabeledValues(c candidate) []labeledValue {
	out := labeledVals("现价", c.Price)
	out = append(out, labeledVals("涨跌幅%", c.ChangePct)...)
	out = append(out, labeledValue{Path: "成交额", Value: c.Amount})
	if math.Abs(c.Amount) >= 1e8 {
		out = append(out, labeledValue{Path: "成交额(亿)", Value: c.Amount / 1e8, Unit: "亿", Derived: true})
	}
	out = append(out, labeledVals("PE-TTM", c.PETTM)...)
	out = append(out, labeledVals("市净率", c.PB)...)
	out = append(out, labeledValue{Path: "总市值", Value: c.TotalCap})
	if math.Abs(c.TotalCap) >= 1e8 {
		out = append(out, labeledValue{Path: "总市值(亿)", Value: c.TotalCap / 1e8, Unit: "亿", Derived: true})
	}
	out = append(out, labeledValue{Path: "流通市值", Value: c.FloatCap})
	if math.Abs(c.FloatCap) >= 1e8 {
		out = append(out, labeledValue{Path: "流通市值(亿)", Value: c.FloatCap / 1e8, Unit: "亿", Derived: true})
	}
	out = append(out, labeledVals("换手率%", c.TurnoverRate)...)
	out = append(out, labeledVals("量比", c.VolumeRatio)...)
	out = append(out, labeledVals("振幅%", c.Amplitude)...)
	out = append(out, labeledVals("量化分", c.Score)...)
	out = append(out, labeledVals("情绪分", c.SentiScore)...)
	out = append(out, labeledVals("龙虎榜净买(亿)", c.LhbNetYi)...)
	out = append(out, labeledVals("机构净买(亿)", c.OrgNetYi)...)
	out = append(out, labeledVals("人气排名", float64(c.PopRank))...)
	out = append(out, labeledVals("人气前值", float64(c.PopPrev))...)
	out = append(out, labeledVals("机构买入家数", float64(c.OrgBuys))...)
	if c.Factors != nil {
		f := c.Factors
		out = append(out, labeledVals("factors.ma5", f.MA5)...)
		out = append(out, labeledVals("factors.ma10", f.MA10)...)
		out = append(out, labeledVals("factors.ma20", f.MA20)...)
		out = append(out, labeledVals("factors.ma60", f.MA60)...)
		out = append(out, labeledVals("factors.chg5d", f.Chg5d)...)
		out = append(out, labeledVals("factors.chg20d", f.Chg20d)...)
		out = append(out, labeledVals("factors.vol_boost", f.VolBoost)...)
		out = append(out, labeledVals("factors.vol5v20", f.Vol5v20)...)
		out = append(out, labeledVals("factors.volatility20", f.Volatility20)...)
		out = append(out, labeledVals("factors.drawdown20", f.Drawdown20)...)
		out = append(out, labeledVals("factors.bias20", f.Bias20)...)
		out = append(out, labeledVals("factors.pos60", f.Pos60)...)
		out = append(out, labeledVals("factors.rsi14", f.RSI14)...)
		out = append(out, labeledVals("factors.macd_dif", f.MACDDif)...)
		out = append(out, labeledVals("factors.macd_dea", f.MACDDea)...)
		out = append(out, labeledVals("factors.macd_hist", f.MACDHist)...)
		out = append(out, labeledVals("factors.boll_up", f.BollUp)...)
		out = append(out, labeledVals("factors.boll_mid", f.BollMid)...)
		out = append(out, labeledVals("factors.boll_low", f.BollLow)...)
		out = append(out, labeledVals("factors.boll_pos", f.BollPos)...)
		out = append(out, labeledVals("factors.atr14", f.ATR14)...)
		out = append(out, labeledVals("factors.atr_pct", f.ATRPct)...)
		out = append(out, labeledVals("factors.chip_profit", f.ChipProfit)...)
		out = append(out, labeledVals("factors.chip_avg_cost", f.ChipAvgCost)...)
		out = append(out, labeledVals("factors.main_net_5d_yi", f.MainNet5dYi)...)
		out = append(out, labeledVals("factors.main_net_days", float64(f.MainNetDays))...)
		out = append(out, labeledVals("factors.tail30_chg", f.Tail30Chg)...)
		out = append(out, labeledVals("factors.tail30_vol_pct", f.Tail30VolPct)...)
		out = append(out, labeledVals("factors.morning_chg", f.MorningChg)...)
		out = append(out, labeledVals("factors.close_vs_vwap", f.CloseVsVwap)...)
	}
	if c.ScoreDims != nil {
		out = append(out, labeledVals("score.trend", c.ScoreDims.Trend)...)
		out = append(out, labeledVals("score.momentum", c.ScoreDims.Momentum)...)
		out = append(out, labeledVals("score.position", c.ScoreDims.Position)...)
		out = append(out, labeledVals("score.volume", c.ScoreDims.Volume)...)
		out = append(out, labeledVals("score.risk", c.ScoreDims.Risk)...)
	}
	if c.Fin != nil {
		out = append(out, labeledVals("fin.roe", c.Fin.ROE)...)
		out = append(out, labeledVals("fin.revenue_yoy", c.Fin.RevenueYoY)...)
		out = append(out, labeledVals("fin.net_profit_yoy", c.Fin.NetProfitYoY)...)
		out = append(out, labeledVals("fin.gross_margin", c.Fin.GrossMargin)...)
		out = append(out, labeledVals("fin.net_margin", c.Fin.NetMargin)...)
		out = append(out, labeledVals("fin.debt_ratio", c.Fin.DebtRatio)...)
	}
	return out
}

// labeledVals 构造带同一路径的值域项，剔除零值（0 多为缺失，纳入会造假证据）。
func labeledVals(path string, vs ...float64) []labeledValue {
	var out []labeledValue
	for _, v := range vs {
		if v != 0 {
			out = append(out, labeledValue{Path: path, Value: v})
		}
	}
	return out
}

// verifyEvidence 核验一条推荐的 evidence 数字与快照的吻合度。数字提取/单位规范化/方向规则/
// 去重计数统一在 trust.go 的 verifyEvidenceLabeled（全模块共用口径）。
// extra 为快照之外的合法引用值域（模型自身计划价、用户筛选阈值），由调用方按上下文传入。
func verifyEvidence(evidence []string, c candidate, extra ...labeledValue) *evidenceCheck {
	vals := candidateLabeledValues(c)
	vals = append(vals, extra...)
	sections := make([]evidenceSection, 0, len(evidence))
	for _, e := range evidence {
		sections = append(sections, evidenceSection{Module: "推荐依据", Text: e})
	}
	return verifyEvidenceLabeled(sections, vals)
}

// systemConfidence 程序合成置信度（高/中/低三档）：LLM 口头置信度系统性过度自信，
// 不能单独作为信任依据；这里由 量化排名分位 × 证据核验 × 数据完备度 合成，
// 与 LLM 的 confidence 并排展示。
func systemConfidence(c candidate, ev *evidenceCheck, poolSize int) (string, string) {
	var reasons []string
	level := 1 // 0=低 1=中 2=高

	if poolSize > 0 && c.Rank > 0 {
		pct := float64(c.Rank) / float64(poolSize)
		switch {
		case pct <= 0.25:
			level++
			reasons = append(reasons, fmt.Sprintf("量化分池内前 25%%（第 %d/%d）", c.Rank, poolSize))
		case pct > 0.6:
			level--
			reasons = append(reasons, fmt.Sprintf("量化分池内靠后（第 %d/%d）", c.Rank, poolSize))
		default:
			reasons = append(reasons, fmt.Sprintf("量化分池内第 %d/%d", c.Rank, poolSize))
		}
	}
	if ev != nil && ev.Total > 0 {
		// ev3：升档只认快照佐证（复述命中剔出分母）、降档看总命中率——口径在
		// evidenceConfidenceSignal（与分析 analysisSystemConfidence 共用）。
		delta, reason := evidenceConfidenceSignal(ev)
		level += delta
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if c.Factors == nil || c.Factors.BarCount < 20 {
		level--
		reasons = append(reasons, "日线历史不足，技术因子精度受限")
	}
	if c.PETTM == 0 && c.PB == 0 && c.FloatCap == 0 {
		level--
		reasons = append(reasons, "估值快照缺失")
	}

	if level < 0 {
		level = 0
	}
	if level > 2 {
		level = 2
	}
	labels := [3]string{"low", "medium", "high"}
	return labels[level], strings.Join(reasons, "；")
}
