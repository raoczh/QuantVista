package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// D16 单测：五类利空事件 → 卖出复核映射、账面影响拼装、幂等与用户隔离。

// TestEvalSellReviewLift 解禁：窗口内取最近一日、同日多批合并、占比 ≥10% 判 high。
func TestEvalSellReviewLift(t *testing.T) {
	today := "2026-07-28"
	rows := []model.RestrictedRelease{
		// 窗口外（第 20 天）——不该被选中。
		{Symbol: "600000", FreeDate: "2026-08-17", FreeShares: 9e8, LiftMarketCap: 9e9, FreeRatio: 50},
		// 第 5 天，两批不同类型同日解禁 → 合并。
		{Symbol: "600000", FreeDate: "2026-08-02", FreeShares: 1e7, LiftMarketCap: 1e8, FreeRatio: 4, FreeType: "首发原股东限售股份"},
		{Symbol: "600000", FreeDate: "2026-08-02", FreeShares: 2e7, LiftMarketCap: 2e8, FreeRatio: 8, FreeType: "定向增发机构配售股份"},
	}
	h := evalSellReviewLift(rows, today)
	if h == nil {
		t.Fatal("窗口内解禁应命中")
	}
	if h.TradeDate != "2026-08-02" {
		t.Fatalf("应取窗口内最近的解禁日，得到 %s", h.TradeDate)
	}
	if h.Severity != model.SellReviewSeverityHigh {
		t.Fatalf("合并后占流通股 12%% 应判 high，得到 %s", h.Severity)
	}
	if !strings.Contains(h.Detail, "3000 万股") || !strings.Contains(h.Detail, "12.00%") {
		t.Fatalf("同日多批必须合并规模与占比（只按日期去重会少算）: %s", h.Detail)
	}
	// 窗口外（>10 天）不命中。
	if h := evalSellReviewLift(rows[:1], today); h != nil {
		t.Fatalf("窗口外解禁不应命中: %+v", h)
	}
}

// TestEvalSellReviewEarnFcst 业绩预告变脸：类型词与幅度双判据，利好不报。
func TestEvalSellReviewEarnFcst(t *testing.T) {
	since := "2026-07-26"
	bad := model.EarningsForecast{NoticeDate: "2026-07-27", PredictType: "预减",
		PredictFinance: "净利润", AmpLower: -60, AmpUpper: -40}
	h := evalSellReviewEarnFcst(&bad, since)
	if h == nil || h.Trigger != model.SellReviewEarnFcst {
		t.Fatal("预减应命中")
	}
	if !strings.Contains(h.Detail, "净利润变动 -60.00%~-40.00%") {
		t.Fatalf("详情应带幅度区间: %s", h.Detail)
	}
	// 首亏判 high。
	if h := evalSellReviewEarnFcst(&model.EarningsForecast{
		NoticeDate: "2026-07-27", PredictType: "首亏"}, since); h == nil || h.Severity != model.SellReviewSeverityHigh {
		t.Fatalf("首亏应判 high: %+v", h)
	}
	// 预增不报（这不是利空）。
	if h := evalSellReviewEarnFcst(&model.EarningsForecast{
		NoticeDate: "2026-07-27", PredictType: "预增", AmpLower: 30, AmpUpper: 60}, since); h != nil {
		t.Fatalf("利好预告不应进卖出复核: %+v", h)
	}
	// 窗口外的旧预告不报（否则每天重复提示同一份）。
	if h := evalSellReviewEarnFcst(&model.EarningsForecast{
		NoticeDate: "2026-07-01", PredictType: "预亏"}, since); h != nil {
		t.Fatalf("窗口外预告不应命中: %+v", h)
	}
	if h := evalSellReviewEarnFcst(nil, since); h != nil {
		t.Fatal("无预告不应命中")
	}
}

// TestEvalSellReviewMaBreak 跌破均线：只报「刚跌破」那一天，长期在均线下方不报。
func TestEvalSellReviewMaBreak(t *testing.T) {
	// 造 25 根：前 24 根恒为 10（MA20=10），最后一根跌到 9 → 刚跌破 MA20。
	bars := make([]model.DailyBar, 0, 25)
	for i := 0; i < 24; i++ {
		bars = append(bars, model.DailyBar{Symbol: "600000", Market: "cn",
			TradeDate: "2026-06-" + twoDigit(i+1), Close: 10})
	}
	bars = append(bars, model.DailyBar{Symbol: "600000", Market: "cn", TradeDate: "2026-07-01", Close: 9})
	h := evalSellReviewMaBreak(bars)
	if h == nil || h.Trigger != model.SellReviewMaBreak {
		t.Fatal("刚跌破 MA20 应命中")
	}
	if h.TradeDate != "2026-07-01" {
		t.Fatalf("事件日应为跌破那一天，得到 %s", h.TradeDate)
	}
	if !h.Push {
		t.Fatal("跌破均线是既有 guard 未覆盖的类型，应标记需推送")
	}

	// 长期在均线下方：再加一根 8.5，此时前一根（9）已在均线下方 → 不再报。
	bars = append(bars, model.DailyBar{Symbol: "600000", Market: "cn", TradeDate: "2026-07-02", Close: 8.5})
	if h := evalSellReviewMaBreak(bars); h != nil {
		t.Fatalf("长期在均线下方不应每天重复报: %+v", h)
	}

	// 样本不足：**不判断 ≠ 没跌破**，返回 nil 由调用方按「本轮无结论」处理。
	if h := evalSellReviewMaBreak(bars[:10]); h != nil {
		t.Fatalf("样本不足时不应给出结论: %+v", h)
	}
	// 坏根（收盘 ≤0）整段不判。
	broken := append([]model.DailyBar{}, bars...)
	broken[3].Close = 0
	if h := evalSellReviewMaBreak(broken); h != nil {
		t.Fatal("存在坏根时应整段不判（宁可不报也不错报）")
	}
}

// TestEvalSellReviewLhbSell 龙虎榜净卖出：小额不报、净买入不报、取最近一日。
func TestEvalSellReviewLhbSell(t *testing.T) {
	rows := []model.LhbEntry{
		{Symbol: "600000", TradeDate: "2026-07-20", NetBuy: 3e8},  // 净买入：不报
		{Symbol: "600000", TradeDate: "2026-07-24", NetBuy: -2e6}, // 净卖出 200 万：低于门槛
		{Symbol: "600000", TradeDate: "2026-07-27", NetBuy: -3e7, NetRatio: -8, Reason: "日跌幅偏离值达7%"},
	}
	h := evalSellReviewLhbSell(rows)
	if h == nil {
		t.Fatal("显著净卖出应命中")
	}
	if h.TradeDate != "2026-07-27" || h.Severity != model.SellReviewSeverityHigh {
		t.Fatalf("应取最近一日且占比 -8%% 判 high，得到 %s / %s", h.TradeDate, h.Severity)
	}
	if !strings.Contains(h.Detail, "0.30 亿元") || !strings.Contains(h.Detail, "日跌幅偏离值达7%") {
		t.Fatalf("详情应带净卖出规模与上榜原因: %s", h.Detail)
	}
	if h := evalSellReviewLhbSell(rows[:2]); h != nil {
		t.Fatalf("净买入与小额净卖出都不应命中: %+v", h)
	}
}

// TestComposeSellReviewDetail 账面影响：带成本与浮盈亏；行情不可用时如实声明不猜。
func TestComposeSellReviewDetail(t *testing.T) {
	p := model.Position{BuyPrice: 10, Quantity: 1000}
	detail, pnl := composeSellReviewDetail("2026-08-02 解禁 3000 万股", p, 12, true)
	if pnl != 20 {
		t.Fatalf("浮盈应为 20%%，得到 %v", pnl)
	}
	if !containsAll(detail, "成本 10.00", "1000 股", "现价 12.00", "盈利 20.00%", "2000.00 元") {
		t.Fatalf("详情必须回答「这件事对我这笔持仓意味着什么」: %s", detail)
	}
	// fail-closed：无当前有效行情时不算浮盈亏（绝不用旧价冒充）。
	detail, pnl = composeSellReviewDetail("解禁", p, 12, false)
	if pnl != 0 || !strings.Contains(detail, "当前行情不可用") {
		t.Fatalf("行情不可用时应如实声明且不给浮盈亏: %s / %v", detail, pnl)
	}
	if strings.Contains(detail, "现价") {
		t.Fatalf("行情不可用时不得写现价: %s", detail)
	}
}

// TestRunSellReviewsEndToEnd 端到端：多类事件落待办、幂等、用户隔离、已处理不被拉回。
func TestRunSellReviewsEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	since := now.AddDate(0, 0, -sellReviewWindowDays).Format("2006-01-02")

	p1 := seedHoldingWithPeak(t, 1, "600000", "浦发银行", 10, 1000, 10, today)
	// 他人持仓同标的（用户隔离）。
	p2 := seedHoldingWithPeak(t, 2, "600000", "浦发银行", 10, 500, 10, today)

	// 解禁（5 天后）+ 业绩预告变脸（昨日发布）。
	common.DB.Create(&model.RestrictedRelease{Symbol: "600000", Market: "cn", Name: "浦发银行",
		FreeDate:   now.AddDate(0, 0, 5).Format("2006-01-02"),
		FreeShares: 3e7, LiftMarketCap: 3e8, FreeRatio: 12, FreeType: "首发原股东限售股份"})
	common.DB.Create(&model.EarningsForecast{Symbol: "600000", Market: "cn", ReportDate: "2026-06-30",
		NoticeDate: now.AddDate(0, 0, -1).Format("2006-01-02"), PredictType: "预亏",
		PredictFinance: "净利润", AmpLower: -120, AmpUpper: -80})

	// market/notify 均为 nil：验的是「无行情时仍生成待办且如实声明浮盈亏未知」这条
	// fail-closed 分支（行情富化另有 TestComposeSellReviewDetail 覆盖）。
	svc := &SellReviewService{}

	n := svc.EvaluateSellReviewsForUser(context.Background(), 1, today, since)
	if n != 2 {
		t.Fatalf("解禁 + 业绩变脸应生成 2 条复核，得到 %d", n)
	}
	rows, err := ListSellReviews(1, model.SellReviewStatusOpen)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("待办应有 2 条，得到 %d", len(rows))
	}
	for _, r := range rows {
		if r.PositionID != p1.ID || r.UserID != 1 {
			t.Fatalf("复核必须绑定到本人的那一笔持仓: %+v", r)
		}
		if !strings.Contains(r.Detail, "成本 10.00") {
			t.Fatalf("详情必须带我的成本: %s", r.Detail)
		}
		if r.QuoteOK {
			t.Fatalf("无行情时 QuoteOK 必须为 false: %+v", r)
		}
	}

	// 幂等：重跑不新增。
	if n := svc.EvaluateSellReviewsForUser(context.Background(), 1, today, since); n != 0 {
		t.Fatalf("重跑不应新增，得到 %d", n)
	}

	// 用户隔离：用户 2 各自生成自己的。
	if n := svc.EvaluateSellReviewsForUser(context.Background(), 2, today, since); n != 2 {
		t.Fatalf("用户 2 应各自生成 2 条，得到 %d", n)
	}
	rows2, _ := ListSellReviews(2, model.SellReviewStatusOpen)
	for _, r := range rows2 {
		if r.PositionID != p2.ID {
			t.Fatalf("用户 2 的复核应绑自己的持仓: %+v", r)
		}
	}
	if list1, _ := ListSellReviews(1, model.SellReviewStatusOpen); len(list1) != 2 {
		t.Fatalf("用户 1 的清单不应被用户 2 的评估污染，得到 %d", len(list1))
	}

	// **已处理的绝不能被下一轮拉回 open**（同 B8 除权建议的先例）。
	if _, err := SetSellReviewStatus(1, rows[0].ID, model.SellReviewStatusResolved); err != nil {
		t.Fatalf("标记已复核失败: %v", err)
	}
	svc.EvaluateSellReviewsForUser(context.Background(), 1, today, since)
	var back model.SellReview
	common.DB.First(&back, rows[0].ID)
	if back.Status != model.SellReviewStatusResolved {
		t.Fatalf("已复核的条目被重新扫描拉回了 %s——用户会被反复要求处理同一件事", back.Status)
	}
	// 越权改状态被拒。
	if _, err := SetSellReviewStatus(2, rows[0].ID, model.SellReviewStatusDismissed); err == nil {
		t.Fatal("他人不得修改本用户的卖出复核状态")
	}
}

// TestSellReviewIntoTodo 卖出复核进今日待办，且属于 ledger 范围（默认可见）。
func TestSellReviewIntoTodo(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	common.DB.Create(&model.SellReview{
		UserID: 1, PositionID: 7, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Trigger: model.SellReviewLift, TradeDate: today, Severity: model.SellReviewSeverityHigh,
		Title: "限售解禁临近", Detail: "解禁 3000 万股。我的持仓：成本 10.00 元 × 1000 股",
		Status: model.SellReviewStatusOpen,
	})
	svc := NewTodoService(&AlertService{}, &PositionService{market: nil}, nil)
	res, err := svc.Build(context.Background(), 1, TodoScopeLedger)
	if err != nil {
		t.Fatalf("待办聚合失败: %v", err)
	}
	var found bool
	for _, it := range res.Items {
		if it.Kind == TodoKindSellReview {
			found = true
			if it.Scope != TodoScopeLedger {
				t.Fatalf("卖出复核必须属账本范围: %+v", it)
			}
			if it.Priority != 1 {
				t.Fatalf("high 严重度应与止损同为优先级 1: %+v", it)
			}
			if !strings.Contains(it.Detail, "成本 10.00") {
				t.Fatalf("待办详情应带我的成本: %s", it.Detail)
			}
		}
	}
	if !found {
		t.Fatal("卖出复核未进今日待办")
	}
	if res.Reviews < 1 {
		t.Fatalf("卖出复核应计入待复盘计数，得到 %d", res.Reviews)
	}
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
