package service

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// ---------- Spearman 秩相关：小样本手工验算 ----------

func TestSpearman(t *testing.T) {
	cases := []struct {
		name string
		xs   []float64
		ys   []float64
		want float64 // NaN=期望 NaN
	}{
		{"完全同向", []float64{1, 2, 3, 4}, []float64{2, 4, 6, 8}, 1},
		{"完全反向", []float64{1, 2, 3, 4}, []float64{8, 6, 4, 2}, -1},
		{"非线性单调仍为1", []float64{1, 2, 3, 4}, []float64{1, 10, 100, 1000}, 1},
		// 含并列手工验算：x=[1,2,2,4] 秩 [1,2.5,2.5,4]；y=[3,1,2,4] 秩 [3,1,2,4]。
		// 均值都是 2.5；cov=(-1.5)(0.5)+0+0+(1.5)(1.5)=1.5；vx=4.5、vy=5.0；
		// r=1.5/√22.5=0.316228。
		{"并列平均秩", []float64{1, 2, 2, 4}, []float64{3, 1, 2, 4}, 0.316228},
		{"零方差NaN", []float64{5, 5, 5, 5}, []float64{1, 2, 3, 4}, math.NaN()},
		{"样本不足NaN", []float64{1, 2}, []float64{2, 1}, math.NaN()},
	}
	for _, c := range cases {
		got := spearman(c.xs, c.ys)
		if math.IsNaN(c.want) {
			if !math.IsNaN(got) {
				t.Fatalf("%s: 应 NaN，得到 %v", c.name, got)
			}
			continue
		}
		if math.Abs(got-c.want) > 1e-5 {
			t.Fatalf("%s: 应 %v，得到 %v", c.name, c.want, got)
		}
	}
}

// TestICAggregate ics=[0.1,0.3,-0.1,0.2]：均值 0.125；样本方差 0.0875/3=0.0291667、
// 标准差 0.170783；ICIR=0.125/0.170783=0.731923；胜率 3/4=75%。
func TestICAggregate(t *testing.T) {
	mean, icir, win, ok := icAggregate([]float64{0.1, 0.3, -0.1, 0.2})
	if !ok {
		t.Fatal("应 ok")
	}
	if math.Abs(mean-0.125) > 1e-9 || math.Abs(icir-0.731923) > 1e-5 || win != 75 {
		t.Fatalf("均值/ICIR/胜率应 0.125/0.731923/75，得到 %v/%v/%v", mean, icir, win)
	}
	if _, _, _, ok := icAggregate(nil); ok {
		t.Fatal("空序列应 !ok")
	}
	// 单样本：胜率可算、ICIR 无标准差为 0。
	mean, icir, win, ok = icAggregate([]float64{-0.2})
	if !ok || mean != -0.2 || icir != 0 || win != 0 {
		t.Fatalf("单样本应 -0.2/0/0，得到 %v/%v/%v", mean, icir, win)
	}
}

// TestRankAvg 并列平均秩：[10,20,20,30] → [1,2.5,2.5,4]。
func TestRankAvg(t *testing.T) {
	got := rankAvg([]float64{10, 20, 20, 30})
	want := []float64{1, 2.5, 2.5, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("秩应 %v，得到 %v", want, got)
		}
	}
}

// ---------- RunFactorIC 端到端（假日线：因子与未来收益完全单调 → IC=1） ----------

// seedICFixture 55 只股 × 60 个交易日：股 k 的日增长率 g_k=0.1%+k×0.05%（严格单调），
// 因此 chg_5d 与未来 5/10/20 日收益的横截面秩完全一致 → 各横截面 RankIC=1。
// 基准不可得 → marketAxis 回退交易日历。
func seedICFixture(t *testing.T) {
	t.Helper()
	common.DB.Exec("DELETE FROM daily_bars")
	common.DB.Exec("DELETE FROM trading_calendars")
	common.DB.Exec("DELETE FROM market_sync_states")
	base := time.Date(2026, 3, 2, 0, 0, 0, 0, time.Local)
	var dates []string
	for d := 0; d < 60; d++ {
		dates = append(dates, base.AddDate(0, 0, d).Format("2006-01-02"))
	}
	for _, d := range dates {
		common.DB.Exec("INSERT INTO trading_calendars (market, trade_date, is_open) VALUES ('cn', ?, 1)", d)
	}
	var bars []model.DailyBar
	for k := 0; k < 55; k++ {
		sym := fmt.Sprintf("600%03d", k+1)
		g := 0.001 + float64(k)*0.0005
		price := 10.0
		for d := 0; d < 60; d++ {
			open := price
			price = price * (1 + g)
			bars = append(bars, model.DailyBar{
				Symbol: sym, Market: "cn", TradeDate: dates[d],
				Open: open, High: price * 1.001, Low: open * 0.999, Close: price,
				Volume: 1_000_000, Amount: price * 1_000_000, Source: "eastmoney",
			})
		}
	}
	if err := common.DB.CreateInBatches(bars, 500).Error; err != nil {
		t.Fatalf("seed bars 失败: %v", err)
	}
	// 复刻生产存量态：turnover_rate 是后加列（AutoMigrate 只加列不回填），此前初始化
	// 的行该列为 NULL——锁定 dailyBarScanCols 的 COALESCE 兜底（裸 float64 扫描遇
	// NULL 会中断整个流式读）。本 fixture 的断言只依赖 chg_5d，与换手无关。
	common.DB.Exec("UPDATE daily_bars SET turnover_rate = NULL")
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM daily_bars")
		common.DB.Exec("DELETE FROM trading_calendars")
	})
}

func TestRunFactorICEndToEnd(t *testing.T) {
	setupTestDB(t)
	seedICFixture(t)
	// 60s 新鲜日期缓存会残留其他测试的值，清掉。
	factorFreshMu.Lock()
	factorFreshVal = ""
	factorFreshMu.Unlock()

	rep, err := RunFactorIC(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunFactorIC 失败: %v", err)
	}
	if rep.Universe != 55 {
		t.Fatalf("宇宙应 55 只，得到 %d", rep.Universe)
	}
	if len(rep.Dates) == 0 {
		t.Fatal("应有采样横截面")
	}
	var chg5 *FactorICStat
	for i := range rep.Stats {
		if rep.Stats[i].Key == "chg_5d" {
			chg5 = &rep.Stats[i]
		}
	}
	if chg5 == nil {
		t.Fatal("排行应含 chg_5d")
	}
	for _, h := range []string{"5", "10", "20"} {
		agg, ok := chg5.Horizons[h]
		if !ok {
			t.Fatalf("chg_5d 应有 %s 日窗口", h)
		}
		if math.Abs(agg.MeanIC-1) > 1e-6 || agg.WinRatePct != 100 {
			t.Fatalf("单调构造下 chg_5d %s 日 IC 应为 1/胜率 100，得到 %v/%v", h, agg.MeanIC, agg.WinRatePct)
		}
		if agg.Days != len(rep.Dates) {
			t.Fatalf("参与横截面数应 %d，得到 %d", len(rep.Dates), agg.Days)
		}
	}
	if CachedFactorICReport() != rep {
		t.Fatal("结果应写入进程内缓存")
	}
}
