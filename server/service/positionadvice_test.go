package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// D17 单测：结论枚举解析与越界丢弃、证据核验值域、紧急度排序、信号归并。

// TestNormalizePositionVerdict 枚举归一：认识的变体归一，不认识的一律空串。
func TestNormalizePositionVerdict(t *testing.T) {
	cases := map[string]string{
		"hold": PositionVerdictHold, "HOLD": PositionVerdictHold, " 持有 ": PositionVerdictHold,
		"trim": PositionVerdictTrim, "减仓": PositionVerdictTrim, "减持": PositionVerdictTrim,
		"exit": PositionVerdictExit, "清仓": PositionVerdictExit, "卖出": PositionVerdictExit,
		// **不认识的绝不猜默认值**：猜 hold 会把「模型没结论」伪装成「建议继续持有」。
		"buy": "", "加仓": "", "观望": "", "": "", "maybe": "",
	}
	for in, want := range cases {
		if got := normalizePositionVerdict(in); got != want {
			t.Fatalf("normalizePositionVerdict(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// TestFilterPositionAdvices 服务端强校验：越界标的/非法枚举/重复/空理由或失效条件一律丢弃，
// 名称与持仓 id 由服务端回填。
func TestFilterPositionAdvices(t *testing.T) {
	positions := map[int64]positionAdviceRow{
		11: {PositionID: 11, Symbol: "600000", Name: "浦发银行"},
		12: {PositionID: 12, Symbol: "600000", Name: "浦发银行第二笔"},
		22: {PositionID: 22, Symbol: "600519", Name: "贵州茅台"},
	}
	in := []PositionAdvice{
		{PositionID: 11, Symbol: "600000", Verdict: "清仓", Reason: "解禁临近且已破 MA60", Invalidation: "收复 MA60 且解禁落地无抛压"},
		{PositionID: 11, Symbol: "600000", Verdict: "hold", Reason: "重复 ID"}, // 重复：丢弃
		{PositionID: 12, Symbol: "600000", Verdict: "hold", Reason: "第二笔成本不同，继续持有", Invalidation: "跌破 8 元"},
		{PositionID: 22, Symbol: "600000", Verdict: "exit", Reason: "ID 与代码错配"}, // 丢弃
		{PositionID: 999, Symbol: "000001", Verdict: "exit", Reason: "不在持仓名单里"},
		{PositionID: 22, Symbol: "600519", Verdict: "加仓", Reason: "枚举越界"},
		{PositionID: 22, Symbol: "600519", Verdict: "hold", Reason: ""},
		{PositionID: 22, Symbol: "600519", Verdict: "hold", Reason: "理由合法但失效条件为空"},
		{PositionID: 22, Symbol: "600519", Verdict: "trim", Reason: "仓位占比过高", Invalidation: "仓位降至 20%", Name: "模型自填的假名字"},
	}
	out := filterPositionAdvices(in, positions)
	if len(out) != 3 {
		t.Fatalf("应保留同代码的两笔持仓及茅台共 3 条合法结论，得到 %d: %+v", len(out), out)
	}
	if out[0].Verdict != PositionVerdictExit || out[0].PositionID != 11 || out[0].Name != "浦发银行" {
		t.Fatalf("首条应归一为 exit 且服务端回填名称与持仓 id: %+v", out[0])
	}
	if out[1].PositionID != 12 || out[1].Name != "浦发银行第二笔" {
		t.Fatalf("同代码第二笔持仓不得被第一笔吞掉: %+v", out[1])
	}
	if out[2].Name != "贵州茅台" {
		t.Fatalf("模型自填的名称必须被服务端值覆盖: %+v", out[2])
	}
}

// TestVerifyPositionAdviceEvidence 结论里的数字进核验值域：真实值可引用、伪造值被标记。
func TestVerifyPositionAdviceEvidence(t *testing.T) {
	rows := []positionAdviceRow{{
		PositionID: 11, Symbol: "600000", Name: "浦发银行", Cost: 10.5, Price: 8.4, PnlPct: -20,
		HeldDays: 35, Peak: 13.2, PeakDrawdownPct: 36.36, WeightPct: 42.5,
	}}
	real := []PositionAdvice{{
		PositionID: 11, Symbol: "600000", Name: "浦发银行", Verdict: PositionVerdictExit,
		Reason:       "成本 10.5 元，现价 8.4 元，浮亏 20%；自持仓期最高 13.2 已回撤 36.36%，且仓位占比 42.5% 过高",
		Invalidation: "重新站上 10.5 元成本线",
	}}
	check := verifyPositionAdvice(real, rows)
	if check == nil {
		t.Fatal("核验结果不应为 nil")
	}
	if check.UnmatchedTotal != 0 {
		t.Fatalf("忠实引用快照数值不应被判幻觉，未匹配 %d 项: %+v", check.UnmatchedTotal, check.Items)
	}
	// 伪造值必须被抓出来。
	fake := []PositionAdvice{{
		PositionID: 11, Symbol: "600000", Name: "浦发银行", Verdict: PositionVerdictExit,
		Reason: "成本 66.66 元，现价 99.99 元，浮亏 88.88%",
	}}
	fakeCheck := verifyPositionAdvice(fake, rows)
	if fakeCheck.UnmatchedTotal == 0 {
		t.Fatal("编造的数字必须被核验标记（信任层失守）")
	}

	// 同代码第二笔仓位的真实成本也不能替第一笔背书。
	rows = append(rows, positionAdviceRow{
		PositionID: 12, Symbol: "600000", Name: "浦发银行第二笔", Cost: 66.66, Price: 8.4,
	})
	cross := []PositionAdvice{{
		PositionID: 11, Symbol: "600000", Name: "浦发银行", Verdict: PositionVerdictExit,
		Reason: "我的成本是 66.66 元",
	}}
	if crossCheck := verifyPositionAdvice(cross, rows); crossCheck.UnmatchedTotal == 0 {
		t.Fatal("第一笔建议引用第二笔仓位成本必须判为未匹配")
	}
}

// TestSortAdviceRowsByUrgency 紧急度排序：浮亏最深在前，同分按回撤深、再按代码稳定。
func TestSortAdviceRowsByUrgency(t *testing.T) {
	rows := []positionAdviceRow{
		{Symbol: "600004", PnlPct: 12},
		{Symbol: "600002", PnlPct: -30},
		{Symbol: "600003", PnlPct: -5, PeakDrawdownPct: 10},
		{Symbol: "600001", PnlPct: -5, PeakDrawdownPct: 40},
	}
	sortAdviceRowsByUrgency(rows)
	want := []string{"600002", "600001", "600003", "600004"}
	for i, w := range want {
		if rows[i].Symbol != w {
			t.Fatalf("第 %d 位应为 %s，得到 %s（全序：%+v）", i, w, rows[i].Symbol, rows)
		}
	}
}

// TestAttachAdviceSignals 信号按 position_id 归并（同一标的两笔仓位各自的信号不串）。
func TestAttachAdviceSignals(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")

	common.DB.Create(&model.AlertEvent{RuleID: 1, UserID: 1, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindPeakDrawdown, Message: "自持仓期最高 20.00 回撤 40.00%",
		TradeDate: today, PositionID: 101, TriggeredAt: time.Now(), Status: model.AlertEventUnread})
	// 另一笔仓位的信号（不同 position_id）。
	common.DB.Create(&model.AlertEvent{RuleID: 1, UserID: 1, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindCostDrawdown, Message: "相对我的成本已跌 12.00%",
		TradeDate: today, PositionID: 102, TriggeredAt: time.Now(), Status: model.AlertEventUnread})
	// 他人的信号（用户隔离）。
	common.DB.Create(&model.AlertEvent{RuleID: 9, UserID: 2, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindCostDrawdown, Message: "他人的信号",
		TradeDate: today, PositionID: 101, TriggeredAt: time.Now(), Status: model.AlertEventUnread})
	common.DB.Create(&model.SellReview{UserID: 1, PositionID: 101, Symbol: "600000", Market: "cn",
		Trigger: model.SellReviewLift, TradeDate: today, Title: "限售解禁临近",
		Detail: "解禁 3000 万股", Status: model.SellReviewStatusOpen})

	rows := []positionAdviceRow{{Symbol: "600000", PositionID: 101}, {Symbol: "600000", PositionID: 102}}
	attachAdviceSignals(1, rows)
	if len(rows[0].Signals) != 1 || !strings.Contains(rows[0].Signals[0], "回撤 40.00%") {
		t.Fatalf("第一笔仓位应只拿到自己的回撤信号: %+v", rows[0].Signals)
	}
	if len(rows[1].Signals) != 1 || !strings.Contains(rows[1].Signals[0], "跌 12.00%") {
		t.Fatalf("第二笔仓位应只拿到自己的成本信号: %+v", rows[1].Signals)
	}
	if len(rows[0].Events) != 1 || !strings.Contains(rows[0].Events[0], "限售解禁临近") {
		t.Fatalf("第一笔仓位应挂上卖出复核事件: %+v", rows[0].Events)
	}
	if len(rows[1].Events) != 0 {
		t.Fatalf("第二笔仓位没有复核事件: %+v", rows[1].Events)
	}
}

// TestPositionAdviceFailClosed 全部持仓无 fresh 行情时整体拒答（不基于旧价出结论）。
func TestPositionAdviceFailClosed(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	today := time.Now().Format("2006-01-02")
	seedHoldingWithPeak(t, 1, "600000", "浦发银行", 10, 1000, 10, today)

	// PositionService 接一个零适配器的 Manager：行情逐源找不到 → QuoteOK 恒 false，
	// 走的是真实的 fail-closed 分支（不是 mock 出来的假状态）。
	posSvc := NewPositionService(NewMarketService(datasource.NewManagerWithAdapters()))
	svc := NewPositionAdviceService(posSvc, nil)
	_, err := svc.Advise(context.Background(), 1, false, PositionAdviceRequest{})
	if err == nil {
		t.Fatal("无当前有效行情时必须拒答，绝不基于旧价给出割/守/补结论")
	}
	if code := RefusalCodeOf(err); code != RefusalFreshQuotesInsufficient {
		t.Fatalf("拒答码应为 %s，得到 %q（错误：%v）", RefusalFreshQuotesInsufficient, code, err)
	}
}

// TestPositionAdviceModuleBudget 新模块必须在预算表登记（未登记属接线遗漏）。
func TestPositionAdviceModuleBudget(t *testing.T) {
	b, ok := llmModuleBudgets["position_advice"]
	if !ok {
		t.Fatal("position_advice 未在 llmModuleBudgets 登记")
	}
	if b.MaxTokens <= 0 || b.RepairAttempts != 1 {
		t.Fatalf("预算声明异常: %+v", b)
	}
}
