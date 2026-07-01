package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

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

// buildContext 按模块采集数据上下文。
func (s *AnalysisService) buildContext(ctx context.Context, userID int64, req AnalyzeRequest) (*analysisContext, error) {
	switch req.Module {
	case model.AnalysisModuleStock:
		return s.buildStockContext(ctx, req.Market, req.Symbol)
	case model.AnalysisModuleMarket:
		return s.buildMarketContext(ctx, req.Market)
	case model.AnalysisModuleSector:
		return s.buildSectorContext(ctx, req.Market, req.Target)
	case model.AnalysisModuleWatchlist:
		return s.buildWatchlistContext(ctx, userID, req.Market)
	case model.AnalysisModulePosition:
		return s.buildPositionContext(ctx, userID)
	default:
		return nil, errors.New("不支持的分析模块")
	}
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

	// 日线：取近 60 根算技术指标，注入近 30 根明细。
	bars, berr := market.GetDailyBars(ctx, mkt, symbol, 60)
	if berr == nil && len(bars) > 0 {
		snap["technicals"] = computeTechnicals(bars)
		snap["recent_bars"] = compactBars(bars, 30)
	} else if berr != nil {
		snap["bars_note"] = "日线数据暂不可用"
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
	ma := func(w int) float64 {
		if n < w {
			return 0
		}
		sum := 0.0
		for _, c := range closes[n-w:] {
			sum += c
		}
		return round2(sum / float64(w))
	}
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
	return map[string]any{
		"ma5":            ma(5),
		"ma10":           ma(10),
		"ma20":           ma(20),
		"period_high":    round2(hi),
		"period_low":     round2(lo),
		"change_pct_5d":  changeOver(5),
		"change_pct_20d": changeOver(20),
		"bar_count":      n,
	}
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
	groups, err := s.watchlist.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, 40)
	for _, g := range groups {
		for _, it := range g.Items {
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
				break
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
