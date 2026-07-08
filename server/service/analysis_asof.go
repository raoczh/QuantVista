package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// M2 回溯诊断：AI 个股分析加 as_of 参数——日线/指标截断到该日组装 prompt（无未来
// 泄露：对截断序列复用同一套 computeTechnicals/computeScore 因子函数），估值/新闻/
// 公告/财务/实时盘面等无历史快照的证据在快照中如实声明缺失（不硬造）。
// 事后用 Hindsight 端点对照真实走势（+5/10/20/60 日收益、最大涨跌幅、价位首触日、
// 基准 alpha），形成「当时怎么看 → 后来怎么走」的校验闭环。

const (
	asOfBarLimit    = 250 // 截断快照读取的日线根数上限（与全市场地基窗口一致）
	asOfTechBars    = 60  // technicals/quant_score 的计算窗口（与实时链路 GetDailyBars(60) 同口径）
	hindsightWindow = 60  // 事后核验窗口（交易日）
)

// asOfDate 校验回溯日期：YYYY-MM-DD 且不晚于今天。
func asOfDate(s string) (string, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return "", errors.New("回溯日期格式应为 YYYY-MM-DD")
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	d := t.Format("2006-01-02")
	if d >= today {
		return "", errors.New("回溯日期必须早于今天（当天及未来请用实时分析）")
	}
	return d, nil
}

// cnBarsUpTo 读单只 A 股 daily_bars 中 trade_date <= asOf 的序列（升序，尾部 limit 根）。
func cnBarsUpTo(symbol, asOf string, limit int) []datasource.Bar {
	var rows []model.DailyBar
	if err := common.DB.Where("market = ? AND symbol = ? AND trade_date <= ?", "cn", symbol, asOf).
		Order("trade_date DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]datasource.Bar, len(rows))
	for i, r := range rows {
		out[len(rows)-1-i] = datasource.Bar{ // 反转为升序
			TradeDate: r.TradeDate, Open: r.Open, High: r.High, Low: r.Low, Close: r.Close,
			Volume: r.Volume, Amount: r.Amount, TurnoverRate: r.TurnoverRate, Source: r.Source,
		}
	}
	return out
}

// cnStockName 标的展示名：宇宙字典优先，stocks 表兜底。
func cnStockName(symbol string) string {
	var st model.MarketSyncState
	if err := common.DB.Select("name").Where("market = ? AND symbol = ?", "cn", symbol).
		First(&st).Error; err == nil && st.Name != "" {
		return st.Name
	}
	var stock model.Stock
	if err := common.DB.Select("name").Where("market = ? AND symbol = ?", "cn", symbol).
		First(&stock).Error; err == nil {
		return stock.Name
	}
	return ""
}

// buildStockContextAsOf 个股模块的回溯分流入口（buildContext 调用）。
func (s *AnalysisService) buildStockContextAsOf(req AnalyzeRequest) (*analysisContext, error) {
	label, snap, err := buildStockSnapshotAsOf(req.Symbol, req.Market, req.AsOf)
	if err != nil {
		return nil, err
	}
	return &analysisContext{Label: label, Snapshot: fitBudget(snap)}, nil
}

// buildStockSnapshotAsOf 组装「截至 as_of」的个股历史快照：仅含日线衍生数据
//（行情由末根 bar 合成、technicals/quant_score/recent_bars 对截断序列复算），
// 估值/新闻/公告/财务/实时盘面在快照中显式声明不可得。仅支持 A 股（全市场日线地基）。
func buildStockSnapshotAsOf(symbol, mkt, asOf string) (string, map[string]any, error) {
	symbol, mkt, err := normalizeSymbolMarket(symbol, mkt)
	if err != nil {
		return "", nil, err
	}
	if mkt != "cn" {
		return "", nil, errors.New("回溯诊断目前仅支持 A 股（依赖全市场日线库存）")
	}
	if isCNFund(symbol) {
		return "", nil, errors.New("回溯诊断暂不支持 ETF/场内基金")
	}
	if common.DB == nil {
		return "", nil, errors.New("数据库不可用")
	}
	bars := cnBarsUpTo(symbol, asOf, asOfBarLimit)
	if len(bars) == 0 {
		return "", nil, errors.New("该标的在回溯日期前没有本地日线数据（可能未初始化全市场历史或日期过早）")
	}
	last := bars[len(bars)-1]
	if last.Close <= 0 {
		return "", nil, errors.New("回溯日期附近的日线数据异常（收盘价缺失）")
	}

	name := cnStockName(symbol)
	label := name
	if label == "" {
		label = symbol
	}

	quote := map[string]any{
		"price":      round2(last.Close),
		"open":       round2(last.Open),
		"high":       round2(last.High),
		"low":        round2(last.Low),
		"amount":     round2(last.Amount),
		"trade_date": last.TradeDate,
		"source":     "daily_bars",
	}
	if len(bars) >= 2 && bars[len(bars)-2].Close > 0 {
		prev := bars[len(bars)-2].Close
		quote["prev_close"] = round2(prev)
		quote["change_pct"] = round2((last.Close/prev - 1) * 100)
	}

	snap := map[string]any{
		"symbol": symbol,
		"market": mkt,
		"name":   name,
		"as_of":  asOf,
		"quote":  quote,
	}
	if last.TradeDate != asOf {
		snap["as_of_note"] = fmt.Sprintf("回溯日期 %s 非交易日或该股停牌，快照采用其前最近交易日 %s 的数据", asOf, last.TradeDate)
	}

	// 与实时链路同口径：尾部 60 根算 technicals 与五维评分（无未来泄露的核心——
	// 复用同一套函数对截断序列复算）。
	tech := bars
	if len(tech) > asOfTechBars {
		tech = tech[len(tech)-asOfTechBars:]
	}
	snap["technicals"] = computeTechnicals(tech)
	snap["recent_bars"] = compactBars(tech, 30)
	sc := computeScore(last.Close, tech)
	qs := map[string]any{
		"total": sc.Total, "label": sc.Label,
		"trend": sc.Trend, "momentum": sc.Momentum, "position": sc.Position,
		"volume": sc.Volume, "risk": sc.Risk,
		"note": "纯技术面五维加权评分(0-100)，对截至 as_of 的日线复算，与实时评分同口径",
	}
	if sc.DataLimited {
		qs["data_limited"] = true
	}
	snap["quant_score"] = qs

	// 诚实声明：该模式下不可得的数据维度（防模型臆测，也防「事后诸葛」）。
	snap["unavailable_note"] = "历史回溯快照：估值(PE/PB/市值)、新闻舆情、公告、财务指标、实时盘面与风险闸门数据在该模式下不可得，均不得臆测；分析只能依据上述日线衍生数据"
	return label, snap, nil
}

// ---------- 事后核验（hindsight） ----------

// HindsightNode 单个时间节点的真实收益。
type HindsightNode struct {
	ReturnPct float64 `json:"return_pct"`
	Date      string  `json:"date"`
}

// HindsightTouch 价位首触信息。
type HindsightTouch struct {
	Price    float64 `json:"price"`
	Date     string  `json:"date"`
	DayIndex int     `json:"day_index"` // as_of 后第几个交易根（1 起）
}

// HindsightView 分析记录的事后核验：as_of（或创建日）之后的真实走势。
type HindsightView struct {
	RecordID  int64  `json:"record_id"`
	Symbol    string `json:"symbol"`
	Name      string `json:"name"`
	AsOf      string `json:"as_of"`      // 基准日（回溯分析=as_of；实时分析=创建日）
	BaseDate  string `json:"base_date"`  // 实际基准根日期（≤as_of 最近交易日）
	BasePrice float64 `json:"base_price"` // 基准根收盘
	Rating    string `json:"rating"`

	ElapsedBars int                       `json:"elapsed_bars"` // 基准日后已有的日线根数
	Returns     map[string]*HindsightNode `json:"returns"`      // d5/d10/d20/d60（未到期为 null）
	MaxGainPct     float64 `json:"max_gain_pct"`     // 窗口内最高价相对基准
	MaxDrawdownPct float64 `json:"max_drawdown_pct"` // 窗口内最低价相对基准（正数）

	BenchReturnPct *float64 `json:"bench_return_pct,omitempty"` // 同窗基准收益（末点=min(60根,现有)）
	AlphaPct       *float64 `json:"alpha_pct,omitempty"`

	TargetTouch *HindsightTouch `json:"target_touch,omitempty"` // 目标价（high 上穿）首触
	StopTouch   *HindsightTouch `json:"stop_touch,omitempty"`   // 止损价（low 下破）首触
	RatingHit   *bool           `json:"rating_hit,omitempty"`   // 评级方向 vs 20 日收益（中性/未到期为 null）
	Note        string          `json:"note"`
}

// Hindsight 对个股分析记录做事后核验。回溯分析用 as_of，普通分析用创建日——
// 所有历史个股分析都能回看「当时的判断后来走成什么样」。
// targetPrice/stopPrice 可选（>0 生效）：用户想验证的价位（分析结果无固定目标价字段）。
func (s *AnalysisService) Hindsight(ctx context.Context, userID, recordID int64, targetPrice, stopPrice float64) (*HindsightView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var rec model.AnalysisRecord
	if err := common.DB.Select("id", "user_id", "module", "market", "symbol", "target", "rating", "as_of", "created_at").
		Where("id = ? AND user_id = ?", recordID, userID).First(&rec).Error; err != nil {
		return nil, errors.New("分析记录不存在")
	}
	if rec.Module != model.AnalysisModuleStock {
		return nil, errors.New("事后核验仅支持个股分析记录")
	}
	if rec.Market != "cn" {
		return nil, errors.New("事后核验目前仅支持 A 股（依赖全市场日线库存）")
	}
	asOf := rec.AsOf
	if asOf == "" {
		asOf = rec.CreatedAt.In(time.Local).Format("2006-01-02")
	}

	base := cnBarsUpTo(rec.Symbol, asOf, 1)
	if len(base) == 0 || base[0].Close <= 0 {
		return nil, errors.New("该标的在基准日期前没有本地日线数据")
	}
	basePrice := base[0].Close

	var rows []model.DailyBar
	if err := common.DB.Where("market = ? AND symbol = ? AND trade_date > ?", "cn", rec.Symbol, asOf).
		Order("trade_date").Limit(hindsightWindow + 20).Find(&rows).Error; err != nil {
		return nil, err
	}
	after := make([]datasource.Bar, len(rows))
	for i, r := range rows {
		after[i] = datasource.Bar{TradeDate: r.TradeDate, Open: r.Open, High: r.High, Low: r.Low, Close: r.Close}
	}

	v := &HindsightView{
		RecordID: rec.ID, Symbol: rec.Symbol, Name: rec.Target,
		AsOf: asOf, BaseDate: base[0].TradeDate, BasePrice: round2(basePrice),
		Rating: rec.Rating, ElapsedBars: len(after),
		Returns: map[string]*HindsightNode{"d5": nil, "d10": nil, "d20": nil, "d60": nil},
	}
	if len(after) == 0 {
		v.Note = "基准日之后暂无日线数据（日期太新或长期停牌），暂无法核验"
		return v, nil
	}

	nodeAt := func(n int) *HindsightNode {
		if len(after) < n || after[n-1].Close <= 0 {
			return nil
		}
		return &HindsightNode{
			ReturnPct: round2((after[n-1].Close/basePrice - 1) * 100),
			Date:      after[n-1].TradeDate,
		}
	}
	v.Returns["d5"] = nodeAt(5)
	v.Returns["d10"] = nodeAt(10)
	v.Returns["d20"] = nodeAt(20)
	v.Returns["d60"] = nodeAt(60)

	// 窗口（≤60 根）内最大涨跌幅与价位首触。
	win := after
	if len(win) > hindsightWindow {
		win = win[:hindsightWindow]
	}
	hi, lo := 0.0, 0.0
	for i, b := range win {
		if b.High > 0 {
			if g := (b.High/basePrice - 1) * 100; g > hi {
				hi = g
			}
			if targetPrice > 0 && v.TargetTouch == nil && b.High >= targetPrice {
				v.TargetTouch = &HindsightTouch{Price: targetPrice, Date: b.TradeDate, DayIndex: i + 1}
			}
		}
		if b.Low > 0 {
			if d := (b.Low/basePrice - 1) * 100; d < lo {
				lo = d
			}
			if stopPrice > 0 && v.StopTouch == nil && b.Low <= stopPrice {
				v.StopTouch = &HindsightTouch{Price: stopPrice, Date: b.TradeDate, DayIndex: i + 1}
			}
		}
	}
	v.MaxGainPct = round2(hi)
	v.MaxDrawdownPct = round2(-lo)

	// 基准同窗 alpha（上证 close→close，与推荐追踪同口径）。
	if s.market != nil {
		if _, bench, err := s.market.GetBenchmarkBars(ctx, "cn", benchBarLimit); err == nil && len(bench) > 0 {
			sort.Slice(bench, func(i, j int) bool { return bench[i].TradeDate < bench[j].TradeDate })
			var b0, b1 float64
			endDate := win[len(win)-1].TradeDate
			for _, b := range bench {
				if b.TradeDate <= asOf && b.Close > 0 {
					b0 = b.Close
				}
				if b.TradeDate <= endDate && b.Close > 0 {
					b1 = b.Close
				}
			}
			if b0 > 0 && b1 > 0 && win[len(win)-1].Close > 0 {
				br := round2((b1 - b0) / b0 * 100)
				stockRet := round2((win[len(win)-1].Close/basePrice - 1) * 100)
				alpha := round2(stockRet - br)
				v.BenchReturnPct = &br
				v.AlphaPct = &alpha
			}
		}
	}

	// 评级方向命中：以 +20 日收益为准（bullish>0 / bearish<0；中性不判）。
	if d20 := v.Returns["d20"]; d20 != nil {
		switch rec.Rating {
		case model.AnalysisRatingBullish:
			hit := d20.ReturnPct > 0
			v.RatingHit = &hit
		case model.AnalysisRatingBearish:
			hit := d20.ReturnPct < 0
			v.RatingHit = &hit
		}
	}
	v.Note = "核验基于前复权日线；评级命中以 +20 交易日收益方向为准，仅供研究参考"
	return v, nil
}
