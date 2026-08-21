package service

import (
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func seedBar(t *testing.T, market, symbol, date string) {
	t.Helper()
	if err := common.DB.Create(&model.DailyBar{
		Market: market, Symbol: symbol, TradeDate: date,
		Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000, Amount: 10500,
	}).Error; err != nil {
		t.Fatalf("写入测试日线失败: %v", err)
	}
}

func countBars(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := common.DB.Model(&model.DailyBar{}).Count(&n).Error; err != nil {
		t.Fatalf("统计日线失败: %v", err)
	}
	return n
}

// TestDailyBarRetentionCutoffCovers250TradingDays 保留期必须覆盖系统里全部 250 交易日
// 窗口（ma250 / pos_250 / 年内新高 / 板块估值分位 / 指标与资金流窗口 / 除权重锚重建）。
// 按 A 股每年约 243 个交易日折算，250 个交易日约需 375 自然日——这是本测试的防回归点：
// 谁把 DailyBarRetentionDays 下调到 375 以下，这些因子会在窗口不足时静默按短样本计算，
// 不报错、只是口径变了，靠人工 review 发现不了。
func TestDailyBarRetentionCutoffCovers250TradingDays(t *testing.T) {
	const tradingDaysNeeded = 250
	// 用变量而非常量参与运算：常量表达式算出 375.514，Go 不允许带小数的常量直接转 int。
	tradingDaysPerYear := 243.0
	minCalendarDays := int(float64(tradingDaysNeeded) / tradingDaysPerYear * 365.0) // ≈375
	if model.DailyBarRetentionDays < minCalendarDays {
		t.Fatalf("保留期 %d 天不足以覆盖 %d 个交易日（需至少约 %d 自然日）——下调前必须同步评估 "+
			"ma250/pos_250/年内新高/板块分位/indicatorMaxLimit/wideBarLimit 的窗口口径",
			model.DailyBarRetentionDays, tradingDaysNeeded, minCalendarDays)
	}

	// cutoff 就是「今天往前 DailyBarRetentionDays 天」，且随基准时刻移动。
	base := time.Date(2026, 8, 21, 15, 30, 0, 0, time.Local)
	got := model.DailyBarRetentionCutoffAt(base)
	want := base.AddDate(0, 0, -model.DailyBarRetentionDays).Format("2006-01-02")
	if got != want {
		t.Errorf("cutoff = %s, 期望 %s", got, want)
	}
}

// TestCleanupDailyBarsBefore 清理只删早于 cutoff 的行，cutoff 当日必须保留（边界含当日）。
func TestCleanupDailyBarsBefore(t *testing.T) {
	setupTestDB(t)

	seedBar(t, "cn", "600000", "2024-01-02") // 远早于 cutoff → 删
	seedBar(t, "cn", "600000", "2025-06-30") // 早于 cutoff → 删
	seedBar(t, "cn", "600000", "2026-01-15") // cutoff 当日 → 保留
	seedBar(t, "cn", "600001", "2026-08-20") // 晚于 cutoff → 保留
	seedBar(t, "hk", "00700", "2024-05-06")  // 另一市场的超期行 → 也要删
	seedBar(t, "hk", "00700", "2026-08-20")  // 另一市场的新行 → 保留

	deleted, err := CleanupDailyBarsBefore("2026-01-15")
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if deleted != 3 {
		t.Errorf("应删除 3 行（含 hk 市场那条），实际 %d", deleted)
	}
	if n := countBars(t); n != 3 {
		t.Errorf("应剩余 3 行，实际 %d", n)
	}
	// cutoff 当日那条必须还在：窗口按「>= cutoff」保留，删成 > cutoff 会每天少一根。
	var boundary int64
	common.DB.Model(&model.DailyBar{}).Where("trade_date = ?", "2026-01-15").Count(&boundary)
	if boundary != 1 {
		t.Errorf("cutoff 当日的行不应被删除")
	}
	// 幂等：再跑一次不应再删任何行。
	again, err := CleanupDailyBarsBefore("2026-01-15")
	if err != nil {
		t.Fatalf("重复清理失败: %v", err)
	}
	if again != 0 {
		t.Errorf("幂等性破坏：第二次清理又删了 %d 行", again)
	}
}

// TestCleanupDailyBarsRejectsBadCutoff 非法 cutoff 必须报错而不是执行删除。
// 空串/错格式若被当成有效条件，字符串比较会把几乎所有行判为「早于」而全表清空。
func TestCleanupDailyBarsRejectsBadCutoff(t *testing.T) {
	setupTestDB(t)
	seedBar(t, "cn", "600000", "2020-01-02")
	seedBar(t, "cn", "600000", "2026-08-20")

	for _, bad := range []string{"2026/08/20", "20260820", "not-a-date", "2026-13-45"} {
		if _, err := CleanupDailyBarsBefore(bad); err == nil {
			t.Errorf("非法 cutoff %q 应报错", bad)
		}
	}
	if n := countBars(t); n != 2 {
		t.Fatalf("非法 cutoff 不得删除任何行，实际剩 %d 行", n)
	}

	// 空串走默认保留期：2020 年那条超期应被删，近期那条保留。
	deleted, err := CleanupDailyBarsBefore("")
	if err != nil {
		t.Fatalf("默认 cutoff 清理失败: %v", err)
	}
	if deleted != 1 {
		t.Errorf("空 cutoff 应按默认保留期删除 1 行，实际 %d", deleted)
	}
}

// TestCleanupDailyBarsBatching 跨批删除：行数超过单批上限时必须继续删干净，
// 不能只删掉第一批就返回（分批循环的终止条件写错时正是这个症状）。
func TestCleanupDailyBarsBatching(t *testing.T) {
	setupTestDB(t)

	// 跨两批且末批不满。用「多标的 × 有限日期」凑行数，不能用单标的连续自然日——
	// 5000+ 天会一路排到 2034 年，大半行反而晚于 cutoff（唯一键是 symbol+market+trade_date，
	// 同一标的的日期不能重复）。
	const symbolCount = 60
	const daysPerSymbol = 90 // 60 × 90 = 5400 > barRetentionBatchRows(5000)
	total := symbolCount * daysPerSymbol
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]model.DailyBar, 0, total)
	for s := 0; s < symbolCount; s++ {
		symbol := fmt.Sprintf("60%04d", s)
		for d := 0; d < daysPerSymbol; d++ {
			bars = append(bars, model.DailyBar{
				Market: "cn", Symbol: symbol,
				TradeDate: start.AddDate(0, 0, d).Format("2006-01-02"),
				Close:     10,
			})
		}
	}
	if err := common.DB.CreateInBatches(bars, 1000).Error; err != nil {
		t.Fatalf("批量写入测试日线失败: %v", err)
	}
	seedBar(t, "cn", "600001", "2026-08-20") // 保留行（symbol 不与上面的 60xxxx 冲突）

	deleted, err := CleanupDailyBarsBefore("2026-08-01")
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if deleted != int64(total) {
		t.Errorf("应删除 %d 行，实际 %d（跨批删除未删净）", total, deleted)
	}
	if n := countBars(t); n != 1 {
		t.Errorf("应只剩 1 行，实际 %d", n)
	}
}

// TestGetDailyBarRetentionStat 管理端只读统计口径。
func TestGetDailyBarRetentionStat(t *testing.T) {
	setupTestDB(t)
	seedBar(t, "cn", "600000", "2020-03-04") // 超期
	seedBar(t, "cn", "600000", "2026-08-19")
	seedBar(t, "cn", "600001", "2026-08-20")

	stat, err := GetDailyBarRetentionStat()
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if stat.TotalRows != 3 {
		t.Errorf("总行数应为 3，实际 %d", stat.TotalRows)
	}
	if stat.StaleRows != 1 {
		t.Errorf("超期行数应为 1，实际 %d", stat.StaleRows)
	}
	if stat.MinTradeDate != "2020-03-04" || stat.MaxTradeDate != "2026-08-20" {
		t.Errorf("日期区间不符: %s ~ %s", stat.MinTradeDate, stat.MaxTradeDate)
	}
	if stat.RetentionDays != model.DailyBarRetentionDays {
		t.Errorf("保留天数应回显常量值")
	}
}
