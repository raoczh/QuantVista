package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// 数据上下文组装：为每个分析模块采集数据、压缩并分级注入，产出可复现的结构化快照。
// 快照既作为 prompt 的数据部分，又落库供「凭版本号复现」。

// contextBudgetChars 数据快照的软预算（字符数）。中文约 1 字 ≈ 1.5~2 token，
// 8000 字符 ≈ 5k~6k token，给系统提示与输出留足空间。超预算时按分级逐步裁剪。
const contextBudgetChars = 8000

// analysisContext 组装结果。
type analysisContext struct {
	Label    string         // 展示用标签（个股名 / 「全市场」 / 分组名等）
	Snapshot map[string]any // 结构化数据快照
}

// buildContext 按模块采集数据上下文。快照统一带 data_as_of（采集时刻），
// 供 prompt 声明数据时间、避免模型把旧数据当实时数据（PRD 3.5/3.13）。
func (s *AnalysisService) buildContext(ctx context.Context, userID int64, req AnalyzeRequest) (*analysisContext, error) {
	var ac *analysisContext
	var err error
	switch req.Module {
	case model.AnalysisModuleStock:
		if req.AsOf != "" {
			ac, err = s.buildStockContextAsOf(req)
		} else {
			ac, err = s.buildStockContext(ctx, req.Market, req.Symbol)
		}
	case model.AnalysisModuleMarket:
		ac, err = s.buildMarketContext(ctx, req.Market)
	case model.AnalysisModuleSector:
		ac, err = s.buildSectorContext(ctx, req.Market, req.Target)
	case model.AnalysisModuleWatchlist:
		ac, err = s.buildWatchlistContext(ctx, userID, req.Market)
	case model.AnalysisModulePosition:
		ac, err = s.buildPositionContext(ctx, userID)
	default:
		return nil, errors.New("不支持的分析模块")
	}
	if err != nil {
		return nil, err
	}
	if ac.Snapshot != nil {
		ac.Snapshot["data_as_of"] = time.Now().In(time.Local).Format("2006-01-02 15:04:05")
	}
	return ac, nil
}

// --- 个股 ---

func (s *AnalysisService) buildStockContext(ctx context.Context, market, symbol string) (*analysisContext, error) {
	name, snap, err := buildStockSnapshot(ctx, s.market, symbol, market)
	if err != nil {
		return nil, err
	}
	return &analysisContext{Label: name, Snapshot: fitBudget(snap)}, nil
}

// buildStockSnapshot 采集单只个股的数据快照（行情 + 技术指标 + 近 30 根日线明细）。
// 供个股分析与个股 AI 问答共用，保证两处口径一致。返回 展示名、快照、错误。
func buildStockSnapshot(ctx context.Context, market *MarketService, symbol, mkt string) (string, map[string]any, error) {
	symbol, mkt, err := normalizeSymbolMarket(symbol, mkt)
	if err != nil {
		return "", nil, err
	}
	q, err := market.GetQuote(ctx, mkt, symbol)
	if err != nil {
		if errors.Is(err, datasource.ErrSymbolInvalid) {
			return "", nil, errors.New("无法识别的股票代码")
		}
		return "", nil, fmt.Errorf("行情数据暂不可用：%w", err)
	}

	snap := map[string]any{
		"symbol": q.Symbol,
		"market": mkt,
		"name":   q.Name,
		"quote": map[string]any{
			"price":      round2(q.Price),
			"change_pct": round2(q.ChangePct),
			"open":       round2(q.Open),
			"high":       round2(q.High),
			"low":        round2(q.Low),
			"prev_close": round2(q.PrevClose),
			"amount":     round2(q.Amount),
			"data_time":  q.DataTime,
			"source":     q.Source,
		},
	}

	// 数据新鲜度：行情快照距采集时刻过久（休市/延迟）时显式标注，
	// 避免模型把旧数据当实时盘面（PRD 数据新鲜度闸门的轻量实现）。
	if !q.DataTime.IsZero() {
		if age := time.Since(q.DataTime); age > 15*time.Minute {
			snap["freshness_note"] = fmt.Sprintf("行情快照为 %d 分钟前的数据（可能处于休市或数据延迟），不代表实时盘面", int(age.Minutes()))
		}
	}

	// 估值/盘面扩展（腾讯免费源 best-effort：拿不到不阻断，提示词已要求缺失时如实说明）。
	// ETF/场内基金无个股估值指标（PE/PB/市值来自个股口径，喂给基金全 0 是噪声），
	// 直接标注资产类型并以说明替代估值段。
	var valuation *datasource.Valuation
	if isCNFund(symbol) {
		snap["asset_type"] = "etf"
		snap["valuation"] = map[string]any{"note": "ETF/基金无个股估值指标（PE/PB/市值不适用）"}
	} else if v, verr := market.GetValuation(ctx, mkt, symbol); verr == nil {
		valuation = v
		val := map[string]any{
			"pe_ttm":        round2(v.PETTM),
			"pe_dynamic":    round2(v.PEDynamic),
			"pb":            round2(v.PB),
			"total_cap":     round2(v.TotalCap),
			"float_cap":     round2(v.FloatCap),
			"turnover_rate": round2(v.TurnoverRate),
			"amplitude":     round2(v.Amplitude),
			"volume_ratio":  round2(v.VolumeRatio),
			"limit_up":      round2(v.LimitUp),
			"limit_down":    round2(v.LimitDown),
		}
		if v.IsST {
			val["is_st"] = true
		}
		snap["valuation"] = val
	}

	// 风险闸门（S1）：ST/退市、一字板、流动性、小市值的程序化前置判定，注入 prompt
	// 且随快照透传前端展示；未接入的数据维度（质押/解禁）恒带「请自行核查」声明。
	snap["risk_gate"] = riskGateBlock(computeRiskGate(q, valuation))

	// 日线：取近 60 根算技术指标，注入近 30 根明细；顺手算五维量化评分
	// （与个股详情页/对比/推荐同一 computeScore 口径），给 LLM 一个确定性的量化锚点，
	// 降低「模型在一坨数字里自由发挥」的空间。
	bars, berr := market.GetDailyBars(ctx, mkt, symbol, 60)
	if berr == nil && len(bars) > 0 {
		snap["technicals"] = computeTechnicals(bars)
		snap["recent_bars"] = compactBars(bars, 30)
		sc := computeScore(q.Price, bars)
		snap["quant_score"] = map[string]any{
			"total": sc.Total, "label": sc.Label,
			"trend": sc.Trend, "momentum": sc.Momentum, "position": sc.Position,
			"volume": sc.Volume, "risk": sc.Risk,
			"note": "纯技术面五维加权评分(0-100)，数据不足维度取中性50，仅供参考锚点，非买卖信号",
		}
		if sc.DataLimited {
			snap["quant_score"].(map[string]any)["data_limited"] = true
		}
	} else if berr != nil {
		snap["bars_note"] = "日线数据暂不可用"
	}

	// N2 舆情段：最近 5 条相关新闻标题+情绪标签；无新闻时注入程序算好的
	// 涨跌五档/量能三档/换手率并明示「暂无直接相关新闻」（fallback 让模型
	// 有确定性的市场信号可依，而不是围绕消息面自由发挥）。仅 A 股 6 位代码口径。
	if mkt == "cn" && len(symbol) == 6 && !isCNFund(symbol) {
		if briefs := latestNewsBriefs(symbol, 5); len(briefs) > 0 {
			snap["news"] = map[string]any{
				"items": briefs,
				"note":  "该股最近相关新闻的标题与情绪标签（利好/利空/中性为程序化预判，仅供参考，不保证完整覆盖）",
			}
		} else {
			turnover := 0.0
			if val, ok := snap["valuation"].(map[string]any); ok {
				if t, ok := val["turnover_rate"].(float64); ok {
					turnover = t
				}
			}
			snap["news"] = map[string]any{
				"note":           "暂无直接相关新闻，请按以下市场信号判断，不得臆测消息面",
				"market_signals": fallbackMarketSignals(q.ChangePct, bars, turnover),
			}
		}
		// F2 财务段：F10 主要财务指标最新一期 + 近 8 期趋势（缓存优先，缺失按需拉一次）。
		// 数值叶子会被 snapshotValueSet 自动并入证据核验值域。无数据不注入，
		// prompt 已声明缺失时如实说明。
		if fin := financeBrief(ctx, symbol); fin != nil {
			snap["finance"] = fin
		}
		// F1 公告段：最近 5 条公告标题+类型+日期（公告 > 新闻报道的证据权重；
		// 覆盖面为已采集库存，best-effort；无公告不注入，prompt 已声明覆盖有限）。
		if anns := latestAnnouncementBriefs(symbol, 5); len(anns) > 0 {
			snap["announcements"] = map[string]any{
				"items": anns,
				"note":  "该股最近的交易所公告（标题与类型；引用只能复述标题，不得臆测公告正文细节）",
			}
		}
		// P3a 机构观点段：研报评级分布/评级变动/目标价偏离/调研密度（缓存优先，
		// 缺失按需拉一次）。数值叶子经 snapshotValueSet 自动进核验值域；无数据不注入。
		if ov := orgViewBrief(ctx, symbol, q.Price); ov != nil {
			snap["org_view"] = ov
		}
	}

	label := q.Name
	if label == "" {
		label = q.Symbol
	}
	return label, snap, nil
}

// computeTechnicals 从升序日线（最新在末尾）算常用技术指标。
func computeTechnicals(bars []datasource.Bar) map[string]any {
	n := len(bars)
	closes := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
	}
	last := closes[n-1]
	// 区间高低与阶段涨跌。
	hi, lo := bars[0].High, bars[0].Low
	for _, b := range bars {
		if b.High > hi {
			hi = b.High
		}
		if b.Low < lo && b.Low > 0 {
			lo = b.Low
		}
	}
	changeOver := func(w int) float64 {
		if n <= w {
			return 0
		}
		prev := closes[n-1-w]
		if prev == 0 {
			return 0
		}
		return round2((last - prev) / prev * 100)
	}
	tech := map[string]any{
		"period_high":    round2(hi),
		"period_low":     round2(lo),
		"change_pct_5d":  changeOver(5),
		"change_pct_20d": changeOver(20),
		"bar_count":      n,
	}
	// 均线：数据不足时省略该键而非注入 0——0 会被模型当成真实均价得出
	// 「现价远高于 MA20」类幻觉结论。
	for _, w := range []int{5, 10, 20} {
		if n < w {
			tech["ma_note"] = "日线不足，部分均线缺失"
			continue
		}
		sum := 0.0
		for _, c := range closes[n-w:] {
			sum += c
		}
		tech[fmt.Sprintf("ma%d", w)] = round2(sum / float64(w))
	}
	return tech
}

// compactBars 取末尾 keep 根日线的精简字段（date/o/h/l/c/vol）。
func compactBars(bars []datasource.Bar, keep int) []map[string]any {
	if keep > len(bars) {
		keep = len(bars)
	}
	tail := bars[len(bars)-keep:]
	out := make([]map[string]any, 0, len(tail))
	for _, b := range tail {
		out = append(out, map[string]any{
			"d": b.TradeDate,
			"o": round2(b.Open),
			"h": round2(b.High),
			"l": round2(b.Low),
			"c": round2(b.Close),
			"v": b.Volume,
		})
	}
	return out
}

// --- 全市场 ---

func (s *AnalysisService) buildMarketContext(ctx context.Context, market string) (*analysisContext, error) {
	market = normalizeMarketOnly(market)
	ov := s.market.GetOverview(ctx, market)
	snap := map[string]any{
		"market":    market,
		"indices":   compactIndices(ov.Indices),
		"breadth":   ov.Breadth,
		"fund_flow": ov.FundFlow,
		"gainers":   compactRanks(ov.Gainers, 6),
		"actives":   compactRanks(ov.Actives, 6),
		"sectors":   compactSectors(ov.Sectors, 10),
	}
	// M3a 情绪温度计：涨停池盘后聚合（连板高度分布/炸板率/昨涨停溢价）。
	// 库中无数据（首日部署/采集失败）自然缺席，guidance 已声明按缺失处理。
	if market == "cn" {
		if mood := moodBrief(); mood != nil {
			snap["mood"] = mood
		}
	}
	if len(ov.Errors) > 0 {
		snap["unavailable"] = ov.Errors // 透传缺失块，供模型知悉数据不全
	}
	return &analysisContext{Label: "全市场", Snapshot: fitBudget(snap)}, nil
}

// --- 板块 ---

func (s *AnalysisService) buildSectorContext(ctx context.Context, market, target string) (*analysisContext, error) {
	market = normalizeMarketOnly(market)
	ov := s.market.GetOverview(ctx, market)
	label := strings.TrimSpace(target)
	if label == "" {
		label = "板块概览"
	}
	snap := map[string]any{
		"market":  market,
		"focus":   label,
		"sectors": compactSectors(ov.Sectors, 20),
		"indices": compactIndices(ov.Indices),
		"breadth": ov.Breadth,
	}
	if len(ov.Sectors) == 0 {
		return nil, errors.New("板块数据暂不可用（数据源繁忙），请稍后重试")
	}
	return &analysisContext{Label: label, Snapshot: fitBudget(snap)}, nil
}

// --- 自选股 ---

func (s *AnalysisService) buildWatchlistContext(ctx context.Context, userID int64, market string) (*analysisContext, error) {
	market = normalizeMarketOnly(market)
	groups, err := s.watchlist.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, 40)
outer:
	for _, g := range groups {
		for _, it := range g.Items {
			if it.Market != market {
				continue // 记录声明了 market，快照按同口径过滤
			}
			row := map[string]any{
				"group":     g.Name,
				"name":      it.Name,
				"symbol":    it.Symbol,
				"market":    it.Market,
				"is_pinned": it.IsPinned,
			}
			if it.QuoteOK {
				row["price"] = round2(it.Price)
				row["change_pct"] = round2(it.ChangePct)
			}
			if it.FocusReason != "" {
				row["focus_reason"] = it.FocusReason
			}
			if it.Note != "" {
				row["note"] = it.Note
			}
			items = append(items, row)
			if len(items) >= 60 {
				break outer
			}
		}
	}
	if len(items) == 0 {
		return nil, errors.New("自选股为空，请先添加自选后再分析")
	}
	snap := map[string]any{"count": len(items), "items": items}
	return &analysisContext{Label: "自选股", Snapshot: fitBudget(snap)}, nil
}

// --- 持仓 ---

func (s *AnalysisService) buildPositionContext(ctx context.Context, userID int64) (*analysisContext, error) {
	views, err := s.position.List(ctx, userID, "all")
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, errors.New("暂无持仓，请先记录持仓后再分析")
	}
	rows := make([]map[string]any, 0, len(views))
	var totalCost, totalMV, totalPnL float64
	for _, v := range views {
		row := map[string]any{
			"name":      v.Name,
			"symbol":    v.Symbol,
			"market":    v.Market,
			"type":      v.PositionType,
			"status":    v.Status,
			"buy_price": round2(v.BuyPrice),
			"quantity":  round2(v.Quantity),
			"cost":      round2(v.Cost),
		}
		if v.QuoteOK {
			row["current_price"] = round2(v.CurrentPrice)
			row["profit_amount"] = round2(v.ProfitAmount)
			row["profit_pct"] = round2(v.ProfitPct)
		}
		if v.BuyReason != "" {
			row["buy_reason"] = v.BuyReason
		}
		rows = append(rows, row)
		if v.Status == model.PositionStatusHolding && v.QuoteOK {
			totalCost += v.Cost
			totalMV += v.MarketValue
			totalPnL += v.ProfitAmount
		}
		if len(rows) >= 60 {
			break
		}
	}
	pnlPct := 0.0
	if totalCost > 0 {
		pnlPct = totalPnL / totalCost * 100
	}
	snap := map[string]any{
		"count": len(rows),
		"holding_summary": map[string]any{
			"total_cost":   round2(totalCost),
			"market_value": round2(totalMV),
			"profit":       round2(totalPnL),
			"profit_pct":   round2(pnlPct),
		},
		"positions": rows,
	}
	// 资金上下文（S1）：用户在设置里填了总投资资金才注入——持仓占总资金的比例
	// 决定「割/守/补」的容错空间（满仓亏损与三成仓亏损是两种处境）。
	var pref model.UserPreference
	if err := common.DB.Where("user_id = ?", userID).First(&pref).Error; err == nil && pref.TotalCapital > 0 {
		ratio := 0.0
		if pref.TotalCapital > 0 {
			ratio = totalMV / pref.TotalCapital * 100
		}
		snap["capital_context"] = map[string]any{
			"total_capital":     round2(pref.TotalCapital),
			"holding_ratio_pct": round2(ratio),
			"note":              "total_capital 为用户设定的总投资资金（元）；holding_ratio_pct 为当前持仓市值占总资金比例(%)，反映仓位水平与补仓余地",
		}
	}
	return &analysisContext{Label: "持仓", Snapshot: fitBudget(snap)}, nil
}

// --- 压缩辅助 ---

func compactIndices(idx []datasource.Index) []map[string]any {
	out := make([]map[string]any, 0, len(idx))
	for _, i := range idx {
		out = append(out, map[string]any{
			"name": i.Name, "price": round2(i.Price), "change_pct": round2(i.ChangePct),
		})
	}
	return out
}

func compactRanks(rs []datasource.StockRank, keep int) []map[string]any {
	if keep > len(rs) {
		keep = len(rs)
	}
	out := make([]map[string]any, 0, keep)
	for _, r := range rs[:keep] {
		out = append(out, map[string]any{
			"name": r.Name, "symbol": r.Symbol, "price": round2(r.Price), "change_pct": round2(r.ChangePct),
		})
	}
	return out
}

func compactSectors(ss []datasource.SectorRank, keep int) []map[string]any {
	if keep > len(ss) {
		keep = len(ss)
	}
	out := make([]map[string]any, 0, keep)
	for _, s := range ss[:keep] {
		out = append(out, map[string]any{
			"name": s.Name, "change_pct": round2(s.ChangePct), "leader": s.Leader,
		})
	}
	return out
}

// normalizeMarketOnly 仅规整市场码（默认 cn）。
func normalizeMarketOnly(market string) string {
	market = strings.ToLower(strings.TrimSpace(market))
	if !validPortfolioMarket[market] {
		return "cn"
	}
	return market
}

// round2 保留两位小数（NaN/Inf 归零，避免 JSON 序列化失败）。
func round2(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*100) / 100
}

// fitBudget 按软预算裁剪快照：超预算时优先丢明细日线，再截断列表，保证核心字段始终保留。
func fitBudget(snap map[string]any) map[string]any {
	if snapSize(snap) <= contextBudgetChars {
		return snap
	}
	// 一级裁剪：去掉个股近日明细（技术指标已含摘要）。
	if _, ok := snap["recent_bars"]; ok {
		delete(snap, "recent_bars")
		snap["bars_note"] = "为控制上下文预算，已省略逐日明细，仅保留技术指标摘要"
		if snapSize(snap) <= contextBudgetChars {
			return snap
		}
	}
	// 二级裁剪：逐步截断大列表。
	for _, key := range []string{"positions", "items", "sectors", "actives", "gainers"} {
		if arr, ok := snap[key].([]map[string]any); ok && len(arr) > 10 {
			snap[key] = arr[:10]
			snap[key+"_truncated"] = true
			if snapSize(snap) <= contextBudgetChars {
				return snap
			}
		}
	}
	return snap
}

// snapSize 估算快照序列化后的字符数。
func snapSize(snap map[string]any) int {
	b, err := json.Marshal(snap)
	if err != nil {
		return 0
	}
	return len([]rune(string(b)))
}

// fallbackMarketSignals 无新闻时的程序化市场信号（N2 舆情段 fallback，纯函数可测）：
// 涨跌五档 + 量能三档（今日量/前5日均量）+ 换手率。分档标签为定性文字，
// 数值字段留给 snapshotValueSet 自动进核验值域。
func fallbackMarketSignals(changePct float64, bars []datasource.Bar, turnoverRate float64) map[string]any {
	band := "平稳(±1%)"
	switch {
	case changePct >= 5:
		band = "大涨(≥5%)"
	case changePct >= 1:
		band = "上涨(1%~5%)"
	case changePct <= -5:
		band = "大跌(≤-5%)"
	case changePct <= -1:
		band = "下跌(-5%~-1%)"
	}
	out := map[string]any{"change_band": band}

	// 量能三档：今日量 / 前 5 日均量（与 recfactor 的 VolBoost 同口径）。
	if n := len(bars); n >= 6 {
		var prev5 float64
		for _, b := range bars[n-6 : n-1] {
			prev5 += float64(b.Volume)
		}
		prev5 /= 5
		if prev5 > 0 {
			ratio := round2(float64(bars[n-1].Volume) / prev5)
			vb := "平量"
			switch {
			case ratio >= 1.5:
				vb = "放量"
			case ratio < 0.7:
				vb = "缩量"
			}
			out["volume_band"] = vb
			out["volume_vs_5d_avg"] = ratio
		}
	}
	if turnoverRate > 0 {
		out["turnover_rate"] = round2(turnoverRate)
	}
	return out
}
