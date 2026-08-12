package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// B6 个人交易复盘统计。
//
// **纪律**（改动前先读）：
//   - 纯读时聚合，零落库、零 LLM 调用。
//   - **不与推荐归因报表混算**：那边（recattribution/reccalib/recjointeval）量的是模型
//     口径——同一套模拟执行、统一拨款、统一持有期；这里量的是**用户执行口径**——用户
//     自己的买卖价、自己的费税、自己的持有时长。两者不可比，混在一起算就是在拿模型的
//     胜率替用户的胜率背书。本文件不 import 任何 recommendation_label 相关查询。
//   - **诚实表达**：无样本就说无样本（不给 0% 胜率）；零亏损时盈亏比无定义（nil，
//     不写 ∞ 也不写 0）；行业未积累时该维如实归「未知」而不是拍一个默认值。

// 统计窗口（按平仓日期过滤；空/all = 全部历史）。
const (
	tradeStatRangeAll = "all"
	tradeStatTopN     = 5
	tradeStatMaxRows  = 5000 // 单用户聚合上限（防超大账本一次性拉爆内存）
)

// tradeStatRangeDays 窗口 → 自然日数（0 = 不限）。
var tradeStatRangeDays = map[string]int{
	"30d":             30,
	"90d":             90,
	"180d":            180,
	"1y":              365,
	tradeStatRangeAll: 0,
	"":                0,
}

// TradeStatBucket 一个分布维度下的单个分组。
type TradeStatBucket struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	Trades       int     `json:"trades"`
	WinCount     int     `json:"win_count"`
	WinRate      float64 `json:"win_rate"`       // %；trades=0 时无意义（前端按 trades 判空）
	RealizedPnl  float64 `json:"realized_pnl"`   // 元
	AvgReturnPct float64 `json:"avg_return_pct"` // 该组平均收益率 %（各笔收益率算术平均）
	// Unknown=true 表示该组是「数据缺失」而不是一个真实取值（如行业快照未覆盖该标的）。
	Unknown bool `json:"unknown,omitempty"`
}

// TradeStatTop 最赚/最亏榜单行。
type TradeStatTop struct {
	PositionID    int64   `json:"position_id"`
	Symbol        string  `json:"symbol"`
	Market        string  `json:"market"`
	Name          string  `json:"name"`
	RealizedPnl   float64 `json:"realized_pnl"`
	ReturnPct     float64 `json:"return_pct"`
	BuyDate       string  `json:"buy_date"`
	SellDate      string  `json:"sell_date"`
	HoldTradeDays int     `json:"hold_trade_days"` // 0 = 日历不可用/日期缺失
	SellPlanned   string  `json:"sell_planned"`
	AiVerdict     string  `json:"ai_verdict"`
}

// TradeStatLesson 复盘教训清单项。
type TradeStatLesson struct {
	PositionID  int64   `json:"position_id"`
	Symbol      string  `json:"symbol"`
	Name        string  `json:"name"`
	SellDate    string  `json:"sell_date"`
	RealizedPnl float64 `json:"realized_pnl"`
	Lesson      string  `json:"lesson"`
	SellPlanned string  `json:"sell_planned"`
	AiVerdict   string  `json:"ai_verdict"`
}

// TradeStats 个人交易复盘统计结果。
type TradeStats struct {
	Range     string `json:"range"`
	RangeFrom string `json:"range_from,omitempty"` // 窗口起始日（all 时为空）
	FirstSell string `json:"first_sell,omitempty"` // 实际样本的最早/最晚平仓日
	LastSell  string `json:"last_sell,omitempty"`

	Closed           int     `json:"closed"`             // 已平仓笔数（样本量）
	TotalRealizedPnl float64 `json:"total_realized_pnl"` // 总已实现盈亏（元）
	WinCount         int     `json:"win_count"`
	LossCount        int     `json:"loss_count"`
	FlatCount        int     `json:"flat_count"` // 恰好持平（盈亏 0）
	WinRate          float64 `json:"win_rate"`   // %；Closed=0 时不可解读
	AvgWin           float64 `json:"avg_win"`    // 平均每笔盈利（元，仅盈利笔）
	AvgLoss          float64 `json:"avg_loss"`   // 平均每笔亏损（元，正数）
	// ProfitFactor 盈亏比 = 总盈利 / 总亏损。**无亏损笔时分母为 0，此处为 nil**
	//（不是 0 也不是 ∞）——样本还不足以给出这个比值。
	ProfitFactor *float64 `json:"profit_factor"`
	// AvgHoldTradeDays 平均持有交易日；HoldSample 为能算出持有天数的样本数
	//（依赖交易日历与买卖日期，缺任一则该笔不进此项，不用自然日冒充）。
	AvgHoldTradeDays float64 `json:"avg_hold_trade_days"`
	HoldSample       int     `json:"hold_sample"`

	ByIndustry    []TradeStatBucket `json:"by_industry"`
	ByHoldBucket  []TradeStatBucket `json:"by_hold_bucket"`
	ByBuyReason   []TradeStatBucket `json:"by_buy_reason"`
	ByAiVerdict   []TradeStatBucket `json:"by_ai_verdict"`
	BySellPlanned []TradeStatBucket `json:"by_sell_planned"`

	TopWinners []TradeStatTop    `json:"top_winners"`
	TopLosers  []TradeStatTop    `json:"top_losers"`
	Lessons    []TradeStatLesson `json:"lessons"`

	Notes []string `json:"notes"` // 口径与缺口声明
}

// tradeStatRow 单笔已平仓持仓的统计中间态。
type tradeStatRow struct {
	pos         model.Position
	realizedPnl float64
	buyCost     float64
	returnPct   float64
	holdDays    int
	hasHoldDays bool
	industry    string
}

// aiVerdictLabels / sellPlannedLabels 枚举值的中文标签（与 Close 表单同源）。
var aiVerdictLabels = map[string]string{
	"right": "AI 判断正确", "wrong": "AI 判断错误",
	"mixed": "AI 判断部分正确", "unused": "未参考 AI",
}

var sellPlannedLabels = map[string]string{
	"yes": "完全按计划卖出", "no": "未按计划卖出", "partial": "部分按计划卖出",
}

// holdBuckets 持有时长分档（交易日）。上界为闭区间；最后一档无上界。
var holdBuckets = []struct {
	key   string
	label string
	max   int // 0 = 无上界
}{
	{"d1", "1 个交易日内", 1},
	{"d2_5", "2~5 交易日", 5},
	{"d6_20", "6~20 交易日", 20},
	{"d21_60", "21~60 交易日", 60},
	{"d60p", "超过 60 交易日", 0},
}

func holdBucketOf(days int) (string, string) {
	for _, b := range holdBuckets {
		if b.max == 0 || days <= b.max {
			return b.key, b.label
		}
	}
	return holdBuckets[len(holdBuckets)-1].key, holdBuckets[len(holdBuckets)-1].label
}

// normalizeTradeStatRange 归一窗口参数；非法值一律回落 all（不报错——统计是只读视图，
// 参数写错不该让整页打不开，但要在 Notes 里说明实际用的是哪个窗口）。
func normalizeTradeStatRange(raw string) (string, int, bool) {
	r := strings.ToLower(strings.TrimSpace(raw))
	if days, ok := tradeStatRangeDays[r]; ok {
		if r == "" {
			r = tradeStatRangeAll
		}
		return r, days, true
	}
	return tradeStatRangeAll, 0, false
}

// countOpenTradeDaysBetween 统计 (from, to] 的开市日数；日历不可用或日期非法返回 false。
// 与 countOpenTradeDaysAfter 同源口径（交易日历唯一权威，不用自然日近似）。
func countOpenTradeDaysBetween(market, from, to string) (int, bool) {
	if common.DB == nil || from == "" || to == "" || to < from {
		return 0, false
	}
	var total int64
	common.DB.Model(&model.TradingCalendar{}).Where("market = ?", market).Count(&total)
	if total == 0 {
		return 0, false
	}
	var n int64
	common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date > ? AND trade_date <= ?", market, true, from, to).
		Count(&n)
	return int(n), true
}

// TradeStats 个人交易复盘统计（仅本人已平仓持仓 + 其流水）。
func (s *PositionService) TradeStats(ctx context.Context, userID int64, rangeKey string) (*TradeStats, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindReal)
	if err != nil {
		return nil, err
	}
	return s.TradeStatsByAccount(ctx, userID, account.ID, rangeKey)
}

// TradeStatsByAccount 只聚合指定真实账户。旧接口由 TradeStats 幂等解析默认账户。
func (s *PositionService) TradeStatsByAccount(ctx context.Context, userID, accountID int64, rangeKey string) (*TradeStats, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if _, err := PortfolioAccountByID(userID, accountID, model.PortfolioKindReal); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, days, valid := normalizeTradeStatRange(rangeKey)
	out := &TradeStats{
		Range: normalized, Notes: []string{},
		ByIndustry: []TradeStatBucket{}, ByHoldBucket: []TradeStatBucket{},
		ByBuyReason: []TradeStatBucket{}, ByAiVerdict: []TradeStatBucket{},
		BySellPlanned: []TradeStatBucket{},
		TopWinners:    []TradeStatTop{}, TopLosers: []TradeStatTop{}, Lessons: []TradeStatLesson{},
	}
	if !valid {
		out.Notes = append(out.Notes, fmt.Sprintf("未识别的时间范围 %q，已回落为全部历史", rangeKey))
	}
	q := common.DB.WithContext(ctx).Where("user_id = ? AND account_id = ? AND status = ?", userID, accountID, model.PositionStatusClosed)
	if days > 0 {
		from := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
		out.RangeFrom = from
		// 无卖出日期的老记录在限定窗口下无法归期：如实排除并在 Notes 说明。
		q = q.Where("sell_date >= ?", from)
	}
	var positions []model.Position
	if err := q.Order("sell_date DESC, id DESC").Limit(tradeStatMaxRows).Find(&positions).Error; err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		out.Notes = append(out.Notes, tradeStatBaseNotes(normalized)...)
		out.Notes = append(out.Notes, "当前窗口内没有已平仓记录，各项指标暂无法计算（不是 0%，是没有样本）")
		return out, nil
	}
	// 补齐账本：老持仓无流水时 TotalBuyCost=0，先补建再统计，否则收益率分母为 0。
	backfillPositionLedgers(userID, positions)
	// 重读补建后的汇总列。user_id 条件不可省（全链路隔离铁律，即使 ids 已来自本人）。
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND account_id = ? AND id IN ?", userID, accountID, positionIDs(positions)).
		Order("sell_date DESC, id DESC").Find(&positions).Error; err != nil {
		return nil, err
	}

	symbols := make([]string, 0, len(positions))
	for _, p := range positions {
		symbols = append(symbols, p.Symbol)
	}
	industries := industriesFor(symbols)

	rows := make([]tradeStatRow, 0, len(positions))
	missingIndustry, missingSellDate := 0, 0
	for _, p := range positions {
		r := tradeStatRow{pos: p, industry: industries[p.Symbol]}
		if r.industry == "" {
			missingIndustry++
		}
		// 账本口径优先；旧记录（补建失败等）回退原算式，绝不因缺列少算一笔。
		if p.TotalBuyCost > 0 {
			r.realizedPnl, r.buyCost = p.RealizedPnl, p.TotalBuyCost
		} else {
			r.buyCost = round4(p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax)
			r.realizedPnl = round4(p.SellPrice*p.Quantity - p.SellFee - p.SellTax - r.buyCost)
		}
		if r.buyCost > 0 {
			r.returnPct = round2(r.realizedPnl / r.buyCost * 100)
		}
		if p.SellDate == "" {
			missingSellDate++
		}
		if d, ok := countOpenTradeDaysBetween(p.Market, p.BuyDate, p.SellDate); ok && p.BuyDate != "" && p.SellDate != "" {
			r.holdDays, r.hasHoldDays = d, true
		}
		rows = append(rows, r)
	}

	fillTradeStatTotals(out, rows)
	out.ByIndustry = bucketBy(rows, func(r tradeStatRow) (string, string, bool) {
		if r.industry == "" {
			return "unknown", "行业未知（宇宙快照未覆盖该标的）", true
		}
		return r.industry, r.industry, false
	})
	out.ByHoldBucket = bucketBy(rows, func(r tradeStatRow) (string, string, bool) {
		if !r.hasHoldDays {
			return "unknown", "持有时长未知（缺交易日历或买卖日期）", true
		}
		k, l := holdBucketOf(r.holdDays)
		return k, l, false
	})
	out.ByBuyReason = bucketBy(rows, func(r tradeStatRow) (string, string, bool) {
		reason := truncateRunes(strings.TrimSpace(r.pos.BuyReason), 24)
		if reason == "" {
			return "unknown", "未填买入理由", true
		}
		return reason, reason, false
	})
	out.ByAiVerdict = bucketBy(rows, func(r tradeStatRow) (string, string, bool) {
		if label, ok := aiVerdictLabels[r.pos.AiVerdict]; ok {
			return r.pos.AiVerdict, label, false
		}
		return "unknown", "未填 AI 判断", true
	})
	out.BySellPlanned = bucketBy(rows, func(r tradeStatRow) (string, string, bool) {
		if label, ok := sellPlannedLabels[r.pos.SellPlanned]; ok {
			return r.pos.SellPlanned, label, false
		}
		return "unknown", "未填是否按计划", true
	})
	out.TopWinners, out.TopLosers = tradeStatTops(rows)
	out.Lessons = tradeStatLessons(rows)

	out.Notes = append(out.Notes, tradeStatBaseNotes(normalized)...)
	if out.ProfitFactor == nil {
		out.Notes = append(out.Notes, "窗口内没有亏损交易，盈亏比无定义（分母为 0）——不是「无穷大」，是样本不足以给出该比值")
	}
	if out.HoldSample < out.Closed {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%d/%d 笔缺买卖日期或所在市场无交易日历，未计入平均持有交易日", out.Closed-out.HoldSample, out.Closed))
	}
	if missingIndustry > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%d 笔标的行业未知（宇宙快照未覆盖），已单列「行业未知」不摊派到其它行业", missingIndustry))
	}
	if missingSellDate > 0 && days > 0 {
		out.Notes = append(out.Notes, "无卖出日期的历史记录无法归入时间窗口，仅在「全部历史」下可见")
	}
	if len(positions) >= tradeStatMaxRows {
		out.Notes = append(out.Notes, fmt.Sprintf("样本超过 %d 笔上限，仅统计最近的部分", tradeStatMaxRows))
	}
	return out, nil
}

func positionIDs(positions []model.Position) []int64 {
	ids := make([]int64, 0, len(positions))
	for _, p := range positions {
		ids = append(ids, p.ID)
	}
	return ids
}

func tradeStatBaseNotes(rangeKey string) []string {
	scope := "全部历史"
	if rangeKey != tradeStatRangeAll {
		scope = "按平仓日期筛选的窗口 " + rangeKey
	}
	return []string{
		"统计口径 = **用户执行口径**：你自己的买卖价、费税与持有时长，样本为" + scope + "内的已平仓持仓",
		"与「推荐追踪 / 校准报表 / 联合评估」不可比也不混算——那些是模型口径（统一模拟入场、统一拨款、固定持有期）",
		"盈亏均为已扣买卖费税的净额；收益率分母为该笔累计买入成本（含买入费税）",
	}
}

// fillTradeStatTotals 总量指标（胜率/盈亏比/平均持有）。
func fillTradeStatTotals(out *TradeStats, rows []tradeStatRow) {
	var sumWin, sumLoss, sumHold float64
	for _, r := range rows {
		out.Closed++
		out.TotalRealizedPnl = round4(out.TotalRealizedPnl + r.realizedPnl)
		switch {
		case r.realizedPnl > 0:
			out.WinCount++
			sumWin += r.realizedPnl
		case r.realizedPnl < 0:
			out.LossCount++
			sumLoss += -r.realizedPnl
		default:
			out.FlatCount++
		}
		if r.hasHoldDays {
			out.HoldSample++
			sumHold += float64(r.holdDays)
		}
		if d := r.pos.SellDate; d != "" {
			if out.FirstSell == "" || d < out.FirstSell {
				out.FirstSell = d
			}
			if d > out.LastSell {
				out.LastSell = d
			}
		}
	}
	if out.Closed > 0 {
		out.WinRate = round2(float64(out.WinCount) / float64(out.Closed) * 100)
	}
	if out.WinCount > 0 {
		out.AvgWin = round2(sumWin / float64(out.WinCount))
	}
	if out.LossCount > 0 {
		out.AvgLoss = round2(sumLoss / float64(out.LossCount))
		pf := round2(sumWin / sumLoss)
		out.ProfitFactor = &pf
	}
	if out.HoldSample > 0 {
		out.AvgHoldTradeDays = round2(sumHold / float64(out.HoldSample))
	}
}

// bucketBy 按 keyOf 归组。结果按已实现盈亏降序，「未知」组恒排最后（它不是一个取值，
// 是一处缺口，混在真实取值中间排序会读成「未知行业赚得最多」）。
func bucketBy(rows []tradeStatRow, keyOf func(tradeStatRow) (string, string, bool)) []TradeStatBucket {
	idx := map[string]int{}
	out := []TradeStatBucket{}
	sumPct := map[string]float64{}
	for _, r := range rows {
		key, label, unknown := keyOf(r)
		i, ok := idx[key]
		if !ok {
			out = append(out, TradeStatBucket{Key: key, Label: label, Unknown: unknown})
			i = len(out) - 1
			idx[key] = i
		}
		out[i].Trades++
		out[i].RealizedPnl = round4(out[i].RealizedPnl + r.realizedPnl)
		if r.realizedPnl > 0 {
			out[i].WinCount++
		}
		sumPct[key] += r.returnPct
	}
	for i := range out {
		if out[i].Trades > 0 {
			out[i].WinRate = round2(float64(out[i].WinCount) / float64(out[i].Trades) * 100)
			out[i].AvgReturnPct = round2(sumPct[out[i].Key] / float64(out[i].Trades))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Unknown != out[j].Unknown {
			return !out[i].Unknown
		}
		if out[i].RealizedPnl != out[j].RealizedPnl {
			return out[i].RealizedPnl > out[j].RealizedPnl
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// tradeStatTops 最赚/最亏 Top5。盈亏恰好为 0 的不进任何一榜。
func tradeStatTops(rows []tradeStatRow) (winners, losers []TradeStatTop) {
	sorted := append([]tradeStatRow{}, rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].realizedPnl > sorted[j].realizedPnl })
	winners, losers = []TradeStatTop{}, []TradeStatTop{}
	for _, r := range sorted {
		if r.realizedPnl > 0 && len(winners) < tradeStatTopN {
			winners = append(winners, topOf(r))
		}
	}
	for i := len(sorted) - 1; i >= 0; i-- {
		if sorted[i].realizedPnl < 0 && len(losers) < tradeStatTopN {
			losers = append(losers, topOf(sorted[i]))
		}
	}
	return winners, losers
}

func topOf(r tradeStatRow) TradeStatTop {
	return TradeStatTop{
		PositionID: r.pos.ID, Symbol: r.pos.Symbol, Market: r.pos.Market, Name: r.pos.Name,
		RealizedPnl: round2(r.realizedPnl), ReturnPct: r.returnPct,
		BuyDate: r.pos.BuyDate, SellDate: r.pos.SellDate, HoldTradeDays: r.holdDays,
		SellPlanned: r.pos.SellPlanned, AiVerdict: r.pos.AiVerdict,
	}
}

// tradeStatLessons 复盘教训清单（有 LessonLearned 的按平仓日倒序）。
func tradeStatLessons(rows []tradeStatRow) []TradeStatLesson {
	out := []TradeStatLesson{}
	for _, r := range rows {
		lesson := strings.TrimSpace(r.pos.LessonLearned)
		if lesson == "" {
			continue
		}
		out = append(out, TradeStatLesson{
			PositionID: r.pos.ID, Symbol: r.pos.Symbol, Name: r.pos.Name,
			SellDate: r.pos.SellDate, RealizedPnl: round2(r.realizedPnl), Lesson: lesson,
			SellPlanned: r.pos.SellPlanned, AiVerdict: r.pos.AiVerdict,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SellDate != out[j].SellDate {
			return out[i].SellDate > out[j].SellDate
		}
		return out[i].PositionID > out[j].PositionID
	})
	return out
}
