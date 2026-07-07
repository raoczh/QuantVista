package service

import (
	"context"
	"errors"
	"math"
	"sort"

	"quantvista/datasource"
)

// 筹码分布本地复算（T1）：零上游成本，由日 K + 换手率的三角分布衰减模型累积得出。
//
// 算法（对齐社区通行的东财筹码峰复算口径）：
//   - 价格轴取全窗口 [min(low), max(high)] 等分 150 档；
//   - 每日新增筹码按三角分布落在 [low, high]，峰=(开+高+低+收)/4；
//   - 历史存量按 (1-换手率) 衰减，当日新筹码权重=换手率，总量恒为 1（流通盘）；
//   - 换手率优先用日线自带（东财 f61），缺失根按流通股本（volume×100/floatShares）
//     推断，股本又可从「有换手的根」中位数反推——新浪兜底序列亦可算。
//
// 输入窗口约束：筹码是累积模型，窗口不足会让获利比例/成本区间系统性失真——
// 详情页与推荐按 chipBarLimit=210 根拉取（akshare lmt=210 同口径）；
// <chipHardMinBars 拒算，[chipHardMinBars, 210) 标 data_limited（次新股全历史，如实标注）。
// 注意日线为前复权（fqt=1），与东财 App 展示或有复权口径差异，前端已注明。

const (
	chipBarLimit    = 210  // 标准输入根数（与 akshare 口径对齐）
	chipHardMinBars = 120  // 低于此根数拒算（累积失真过大）
	chipPriceLevels = 150  // 价格档数
	chipTrendDays   = 90   // 输出的获利比例/成本区间逐日序列长度
	chipMaxTurnover = 0.99 // 单日换手钳制上限（新股/妖股 100%+ 换手防清零）
)

// chipDay 单个交易日收盘后的筹码画像。
type chipDay struct {
	Date    string  `json:"date"`
	Profit  float64 `json:"profit"`   // 获利比例 %（收盘价之下的筹码占比）
	AvgCost float64 `json:"avg_cost"` // 平均成本
	C90Low  float64 `json:"c90_low"`  // 90% 筹码成本区间（5%~95% 分位）
	C90High float64 `json:"c90_high"`
	Conc90  float64 `json:"conc_90"` // 90% 集中度 %：(高-低)/(高+低)×100
	C70Low  float64 `json:"c70_low"` // 70% 筹码成本区间（15%~85% 分位）
	C70High float64 `json:"c70_high"`
	Conc70  float64 `json:"conc_70"`
}

// ChipResult 筹码分布计算结果：末日全量画像 + 近 N 日趋势 + 末日分布直方图。
type ChipResult struct {
	chipDay
	Days        []chipDay `json:"days"`   // 近 chipTrendDays 日逐日序列（含末日）
	Prices      []float64 `json:"prices"` // 150 档中心价（升序）
	Chips       []float64 `json:"chips"`  // 各档筹码占比（合计≈1）
	LastClose   float64   `json:"last_close"`
	BarCount    int       `json:"bar_count"`
	DataLimited bool      `json:"data_limited"` // 输入不足 210 根（次新股），精度受限
}

// chipTurnovers 归一化每日换手率（0~1）：自带值优先，缺失根按流通股本推断，
// 股本可由「有换手的根」中位数反推或外部传入（floatShares，股）。
// 返回 nil 表示换手率整体不可得（无法计算筹码）。
func chipTurnovers(bars []datasource.Bar, floatShares float64) []float64 {
	// 从自带换手的根反推流通股本：shares = volume(手)×100 / (turnover/100)。
	var inferred []float64
	for _, b := range bars {
		if b.TurnoverRate > 0 && b.Volume > 0 {
			inferred = append(inferred, float64(b.Volume)*100/(b.TurnoverRate/100))
		}
	}
	shares := floatShares
	if len(inferred) > 0 {
		sort.Float64s(inferred)
		shares = inferred[len(inferred)/2] // 中位数：抗单日字段异常
	}
	out := make([]float64, len(bars))
	for i, b := range bars {
		t := b.TurnoverRate / 100
		if t <= 0 {
			if shares <= 0 {
				return nil // 无自带换手也无股本可推断
			}
			t = float64(b.Volume) * 100 / shares
		}
		if t <= 0 {
			t = 0 // 停牌/零成交：不衰减不新增
		}
		if t > chipMaxTurnover {
			t = chipMaxTurnover
		}
		out[i] = t
	}
	return out
}

// triangleDist 单日成交在价格档上的三角分布权重（合计=1）。
// peak=(开+高+低+收)/4 为顶点；一字板（high==low）退化为单档全落。
func triangleDist(prices []float64, level float64, low, high, peak float64) []float64 {
	n := len(prices)
	dist := make([]float64, n)
	if high <= low {
		// 一字板：全部筹码落在最近档。
		idx := nearestLevel(prices, level, low)
		dist[idx] = 1
		return dist
	}
	if peak < low {
		peak = low
	}
	if peak > high {
		peak = high
	}
	var sum float64
	for i, p := range prices {
		if p < low-level/2 || p > high+level/2 {
			continue
		}
		// 三角密度（未归一）：峰处最高，向两端线性衰减；峰贴边时退化为直角三角。
		var w float64
		switch {
		case p <= peak:
			if peak > low {
				w = (p - low) / (peak - low)
			} else {
				w = 1
			}
		default:
			if high > peak {
				w = (high - p) / (high - peak)
			} else {
				w = 1
			}
		}
		if w < 0 {
			w = 0
		}
		dist[i] = w
		sum += w
	}
	if sum <= 0 {
		// 波动区间窄于档宽：全落最近峰档。
		idx := nearestLevel(prices, level, peak)
		dist[idx] = 1
		return dist
	}
	for i := range dist {
		dist[i] /= sum
	}
	return dist
}

// nearestLevel 找价格所属档的下标。
func nearestLevel(prices []float64, level, p float64) int {
	if level <= 0 || len(prices) == 0 {
		return 0
	}
	idx := int((p - (prices[0] - level/2)) / level)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(prices) {
		idx = len(prices) - 1
	}
	return idx
}

// chipQuantile 累积分布的分位价格（档内线性插值）。q∈(0,1)。
func chipQuantile(prices, chips []float64, level, total, q float64) float64 {
	target := total * q
	var cum float64
	for i, c := range chips {
		if cum+c >= target {
			frac := 0.0
			if c > 0 {
				frac = (target - cum) / c
			}
			return prices[i] - level/2 + level*frac
		}
		cum += c
	}
	if len(prices) == 0 {
		return 0
	}
	return prices[len(prices)-1] + level/2
}

// computeChipDistribution 纯函数：由升序日线计算筹码分布。
// floatShares 为流通股本（股，0=未知），仅在日线不自带换手率时作推断兜底。
func computeChipDistribution(bars []datasource.Bar, floatShares float64) (*ChipResult, error) {
	if len(bars) < chipHardMinBars {
		return nil, errors.New("日线历史不足（需至少 120 根），无法计算筹码分布")
	}
	turnovers := chipTurnovers(bars, floatShares)
	if turnovers == nil {
		return nil, errors.New("换手率数据缺失，无法计算筹码分布")
	}

	lo, hi := math.MaxFloat64, -math.MaxFloat64
	for _, b := range bars {
		if b.Low > 0 && b.Low < lo {
			lo = b.Low
		}
		if b.High > hi {
			hi = b.High
		}
	}
	if hi <= 0 || lo >= math.MaxFloat64 {
		return nil, errors.New("日线价格数据异常")
	}
	if hi <= lo { // 全窗一字：人为扩出可用区间
		lo, hi = lo*0.995, hi*1.005
	}
	level := (hi - lo) / float64(chipPriceLevels)
	prices := make([]float64, chipPriceLevels)
	for i := range prices {
		prices[i] = lo + level*(float64(i)+0.5)
	}

	chips := make([]float64, chipPriceLevels)
	trendFrom := len(bars) - chipTrendDays
	if trendFrom < 0 {
		trendFrom = 0
	}
	var days []chipDay
	for i, b := range bars {
		peak := (b.Open + b.High + b.Low + b.Close) / 4
		dist := triangleDist(prices, level, b.Low, b.High, peak)
		if i == 0 {
			copy(chips, dist) // 首日：流通盘全量按首日分布初始化
		} else {
			t := turnovers[i]
			for j := range chips {
				chips[j] = chips[j]*(1-t) + dist[j]*t
			}
		}
		if i >= trendFrom {
			days = append(days, chipDayStats(b, prices, chips, level))
		}
	}

	last := days[len(days)-1]
	out := &ChipResult{
		chipDay:     last,
		Days:        days,
		Prices:      make([]float64, chipPriceLevels),
		Chips:       make([]float64, chipPriceLevels),
		LastClose:   bars[len(bars)-1].Close,
		BarCount:    len(bars),
		DataLimited: len(bars) < chipBarLimit,
	}
	for i := range prices {
		out.Prices[i] = round3(prices[i])
		out.Chips[i] = math.Round(chips[i]*1e6) / 1e6
	}
	return out, nil
}

// chipDayStats 单日收盘后的画像：获利比例、平均成本、90%/70% 成本区间与集中度。
func chipDayStats(b datasource.Bar, prices, chips []float64, level float64) chipDay {
	var total, below, costSum float64
	for i, c := range chips {
		total += c
		costSum += prices[i] * c
		if prices[i] <= b.Close {
			below += c
		}
	}
	d := chipDay{Date: b.TradeDate}
	if total <= 0 {
		return d
	}
	d.Profit = round2(below / total * 100)
	d.AvgCost = round2(costSum / total)
	d.C90Low = round2(chipQuantile(prices, chips, level, total, 0.05))
	d.C90High = round2(chipQuantile(prices, chips, level, total, 0.95))
	d.C70Low = round2(chipQuantile(prices, chips, level, total, 0.15))
	d.C70High = round2(chipQuantile(prices, chips, level, total, 0.85))
	if s := d.C90High + d.C90Low; s > 0 {
		d.Conc90 = round2((d.C90High - d.C90Low) / s * 100)
	}
	if s := d.C70High + d.C70Low; s > 0 {
		d.Conc70 = round2((d.C70High - d.C70Low) / s * 100)
	}
	return d
}

// --- 详情页 API ---

// ChipService 个股筹码分布（详情页筹码峰）。
type ChipService struct {
	market *MarketService
}

func NewChipService(market *MarketService) *ChipService {
	return &ChipService{market: market}
}

// ChipView 筹码分布 + 标的信息。
type ChipView struct {
	Symbol string `json:"symbol"`
	Market string `json:"market"`
	*ChipResult
}

// Distribution 拉 210 根日线本地复算筹码分布。
// 日线不自带换手率（新浪兜底）时，尝试用腾讯估值的流通市值/现价推流通股本。
func (s *ChipService) Distribution(ctx context.Context, market, symbol string) (*ChipView, error) {
	symbol, market, err := normalizeSymbolMarket(symbol, market)
	if err != nil {
		return nil, err
	}
	bars, err := s.market.GetDailyBars(ctx, market, symbol, chipBarLimit)
	if err != nil {
		return nil, errors.New("日线数据暂不可用")
	}
	var floatShares float64
	hasTurnover := false
	for _, b := range bars {
		if b.TurnoverRate > 0 {
			hasTurnover = true
			break
		}
	}
	if !hasTurnover {
		if v, err := s.market.GetValuation(ctx, market, symbol); err == nil && v.FloatCap > 0 {
			if last := bars[len(bars)-1].Close; last > 0 {
				floatShares = v.FloatCap / last
			}
		}
	}
	res, err := computeChipDistribution(bars, floatShares)
	if err != nil {
		return nil, err
	}
	return &ChipView{Symbol: symbol, Market: market, ChipResult: res}, nil
}
