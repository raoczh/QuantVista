package service

import (
	"context"
	"errors"
	"math"

	"quantvista/datasource"
)

// 经典技术指标纯函数库（T1）：EMA/MACD/BOLL/RSI/ATR。
//
// 口径约定（改动前先读，参考项目的已知偏差不带过来）：
//   - EMA 递推 α=2/(n+1)、seed=首值，等价 pandas ewm(span=n, adjust=False)；
//   - RSI/ATR 用 Wilder 平滑 α=1/n、seed=首值，等价通达信 SMA(X,N,1) 递推——
//     不是参考项目的 SMA(滚动均值) 口径（已识别偏差，会系统性高估超买超卖）；
//   - MACD 柱 = 2×(DIF−DEA)，A 股软件（通达信/东财）统一口径，别改回国际 1 倍；
//   - BOLL 中轨 MA20、带宽 2×样本标准差（n-1，与 pandas rolling.std 默认一致）；
//   - 递推类指标（EMA/RSI/ATR/MACD）的数值受输入窗口起点影响：窗口越长越接近
//     全历史递推。消费方按需加 warmup（indicatorWarmupBars）后截尾。
//
// 序列约定：与输入等长；无法计算的位置填 NaN（仅 BOLL 前 n-1 与 RSI 首位），
// JSON 出口须经 nanToNil 转 null，NaN 直接进 json.Marshal 会报错。

const (
	indicatorWarmupBars = 100 // 递推 warmup：α=1/14 时 (13/14)^100≈0.06%，残差可忽略
	indicatorMaxLimit   = 250
)

// emaSeries 指数移动平均：α=2/(n+1)，seed=xs[0]（pandas ewm adjust=False 口径）。
func emaSeries(xs []float64, n int) []float64 {
	out := make([]float64, len(xs))
	if len(xs) == 0 || n <= 0 {
		return out
	}
	alpha := 2.0 / float64(n+1)
	out[0] = xs[0]
	for i := 1; i < len(xs); i++ {
		out[i] = alpha*xs[i] + (1-alpha)*out[i-1]
	}
	return out
}

// wilderSeries Wilder 平滑（RMA）：y = (x + (n-1)·y') / n，seed=xs[0]。
// 等价通达信 SMA(X,N,1)，RSI/ATR 的标准平滑器。
func wilderSeries(xs []float64, n int) []float64 {
	out := make([]float64, len(xs))
	if len(xs) == 0 || n <= 0 {
		return out
	}
	fn := float64(n)
	out[0] = xs[0]
	for i := 1; i < len(xs); i++ {
		out[i] = (xs[i] + (fn-1)*out[i-1]) / fn
	}
	return out
}

// macdSeriesN 参数化 MACD（快/慢 EMA 与信号线周期可调，供小参数单测手工对拍）。
// 返回 DIF、DEA 与柱（2 倍 A 股口径），全长有值（递推 seed=首值）。
func macdSeriesN(closes []float64, fast, slow, signal int) (dif, dea, hist []float64) {
	emaFast := emaSeries(closes, fast)
	emaSlow := emaSeries(closes, slow)
	dif = make([]float64, len(closes))
	for i := range closes {
		dif[i] = emaFast[i] - emaSlow[i]
	}
	dea = emaSeries(dif, signal)
	hist = make([]float64, len(closes))
	for i := range closes {
		hist[i] = 2 * (dif[i] - dea[i])
	}
	return dif, dea, hist
}

// macdSeries 标准 MACD(12,26,9)。
func macdSeries(closes []float64) (dif, dea, hist []float64) {
	return macdSeriesN(closes, 12, 26, 9)
}

// bollSeries 布林带（中轨 n 日均线 ± k 倍样本标准差）。前 n-1 位无满窗，填 NaN。
func bollSeries(closes []float64, n int, k float64) (up, mid, low []float64) {
	m := len(closes)
	up, mid, low = make([]float64, m), make([]float64, m), make([]float64, m)
	for i := 0; i < m; i++ {
		if i < n-1 {
			up[i], mid[i], low[i] = math.NaN(), math.NaN(), math.NaN()
			continue
		}
		win := closes[i-n+1 : i+1]
		var sum float64
		for _, c := range win {
			sum += c
		}
		avg := sum / float64(n)
		sd := stddev(win) // 样本标准差（n-1），service 包既有实现
		mid[i] = avg
		up[i] = avg + k*sd
		low[i] = avg - k*sd
	}
	return up, mid, low
}

// rsiSeries RSI：涨/跌幅分别 Wilder 平滑后 100·up/(up+down)。
// 首位无前收，填 NaN；连续平盘（up+down=0）取中性 50。
func rsiSeries(closes []float64, n int) []float64 {
	m := len(closes)
	out := make([]float64, m)
	if m == 0 {
		return out
	}
	out[0] = math.NaN()
	if m < 2 {
		return out
	}
	ups := make([]float64, m-1)
	downs := make([]float64, m-1)
	for i := 1; i < m; i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			ups[i-1] = d
		} else {
			downs[i-1] = -d
		}
	}
	upAvg := wilderSeries(ups, n)
	downAvg := wilderSeries(downs, n)
	for i := 1; i < m; i++ {
		u, d := upAvg[i-1], downAvg[i-1]
		if u+d <= 0 {
			out[i] = 50
			continue
		}
		out[i] = 100 * u / (u + d)
	}
	return out
}

// atrSeries ATR：真实波幅 TR = max(高-低, |高-前收|, |低-前收|) 的 Wilder 平滑。
// 首位 TR 取当日高-低（无前收），全长有值。
func atrSeries(bars []datasource.Bar, n int) []float64 {
	m := len(bars)
	if m == 0 {
		return nil
	}
	trs := make([]float64, m)
	trs[0] = bars[0].High - bars[0].Low
	for i := 1; i < m; i++ {
		pc := bars[i-1].Close
		tr := bars[i].High - bars[i].Low
		if v := math.Abs(bars[i].High - pc); v > tr {
			tr = v
		}
		if v := math.Abs(bars[i].Low - pc); v > tr {
			tr = v
		}
		trs[i] = tr
	}
	return wilderSeries(trs, n)
}

// indicatorSnapshot 末根时点的指标快照（喂给推荐因子/评分）。
// 各指标按各自最低样本量独立可用，不足留零值（candFactors omitempty 自然缺席）。
type indicatorSnapshot struct {
	RSI14    float64
	MACDDif  float64
	MACDDea  float64
	MACDHist float64
	MACDGold bool // DIF > DEA（多头状态）
	MACDXUp  bool // 近 3 根内 DIF 上穿 DEA（金叉）
	BollUp   float64
	BollMid  float64
	BollLow  float64
	BollPos  float64 // 现价在带内位置 %（<0 破下轨、>100 破上轨）
	ATR14    float64
	ATRPct   float64 // ATR/现价 %
}

// 各指标最低样本量：低于门槛不给值（递推结果失真，宁缺毋滥）。
const (
	rsiMinBars  = 15 // n+1
	atrMinBars  = 15
	bollMinBars = 20
	macdMinBars = 35 // 慢线 26 + 信号 9 的底线
)

// computeIndicatorSnapshot 由升序日线与现价派生末根指标快照。bars 过短返回 nil。
func computeIndicatorSnapshot(price float64, bars []datasource.Bar) *indicatorSnapshot {
	m := len(bars)
	if m < rsiMinBars || price <= 0 {
		return nil
	}
	closes := make([]float64, m)
	for i, b := range bars {
		closes[i] = b.Close
	}
	snap := &indicatorSnapshot{}
	if rsi := rsiSeries(closes, 14); !math.IsNaN(rsi[m-1]) {
		snap.RSI14 = round2(rsi[m-1])
	}
	if m >= atrMinBars {
		atr := atrSeries(bars, 14)
		snap.ATR14 = round2(atr[m-1])
		snap.ATRPct = round2(atr[m-1] / price * 100)
	}
	if m >= bollMinBars {
		up, mid, low := bollSeries(closes, 20, 2)
		snap.BollUp = round2(up[m-1])
		snap.BollMid = round2(mid[m-1])
		snap.BollLow = round2(low[m-1])
		if band := up[m-1] - low[m-1]; band > 0 {
			snap.BollPos = round2((price - low[m-1]) / band * 100)
		}
	}
	if m >= macdMinBars {
		dif, dea, hist := macdSeries(closes)
		snap.MACDDif = round3(dif[m-1])
		snap.MACDDea = round3(dea[m-1])
		snap.MACDHist = round3(hist[m-1])
		snap.MACDGold = dif[m-1] > dea[m-1]
		for j := m - 3; j < m; j++ {
			if j >= 1 && dif[j-1] <= dea[j-1] && dif[j] > dea[j] {
				snap.MACDXUp = true
				break
			}
		}
	}
	return snap
}

// round3 保留 3 位小数（MACD 的 DIF/DEA 常在 ±0.x 量级，2 位会损失分辨率）。
func round3(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*1000) / 1000
}

// --- 详情页副图 API ---

// IndicatorService 个股指标序列（详情页 MACD/BOLL 副图数据）。
type IndicatorService struct {
	market *MarketService
}

func NewIndicatorService(market *MarketService) *IndicatorService {
	return &IndicatorService{market: market}
}

// IndicatorSeriesView 与 K 线对齐的指标序列；null=该位置无值（BOLL 前 19 根等）。
// 后端统一计算保证与推荐因子/单测同一口径，前端不重复实现递推。
type IndicatorSeriesView struct {
	Symbol  string     `json:"symbol"`
	Market  string     `json:"market"`
	Dates   []string   `json:"dates"`
	DIF     []*float64 `json:"dif"`
	DEA     []*float64 `json:"dea"`
	Hist    []*float64 `json:"hist"` // 2×(DIF−DEA)，A 股柱口径
	BollUp  []*float64 `json:"boll_up"`
	BollMid []*float64 `json:"boll_mid"`
	BollLow []*float64 `json:"boll_low"`
	RSI     []*float64 `json:"rsi"`
	ATR     []*float64 `json:"atr"`
}

// nanToNil 序列尾部截取 + NaN→null + 逐值四舍五入。
func nanToNil(xs []float64, tail int, round func(float64) float64) []*float64 {
	if len(xs) > tail {
		xs = xs[len(xs)-tail:]
	}
	out := make([]*float64, len(xs))
	for i, v := range xs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		r := round(v)
		out[i] = &r
	}
	return out
}

// Series 拉日线（多取 warmup 根降低递推 seed 误差）计算指标，截尾 limit 根返回。
func (s *IndicatorService) Series(ctx context.Context, market, symbol string, limit int) (*IndicatorSeriesView, error) {
	symbol, market, err := normalizeSymbolMarket(symbol, market)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > indicatorMaxLimit {
		limit = 120
	}
	bars, err := s.market.GetDailyBars(ctx, market, symbol, limit+indicatorWarmupBars)
	if err != nil {
		return nil, errors.New("日线数据暂不可用")
	}
	if len(bars) == 0 {
		return nil, errors.New("日线数据为空")
	}
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	dif, dea, hist := macdSeries(closes)
	up, mid, low := bollSeries(closes, 20, 2)
	rsi := rsiSeries(closes, 14)
	atr := atrSeries(bars, 14)

	tail := limit
	if len(bars) < tail {
		tail = len(bars)
	}
	dates := make([]string, 0, tail)
	for _, b := range bars[len(bars)-tail:] {
		dates = append(dates, b.TradeDate)
	}
	return &IndicatorSeriesView{
		Symbol: symbol, Market: market, Dates: dates,
		DIF: nanToNil(dif, tail, round3), DEA: nanToNil(dea, tail, round3), Hist: nanToNil(hist, tail, round3),
		BollUp: nanToNil(up, tail, round2), BollMid: nanToNil(mid, tail, round2), BollLow: nanToNil(low, tail, round2),
		RSI: nanToNil(rsi, tail, round2), ATR: nanToNil(atr, tail, round3),
	}, nil
}
