package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// cleanIntradayTables M3b 表清场（内存库 cache=shared 测试间共享，进退场都清）。
func cleanIntradayTables(t *testing.T) {
	t.Helper()
	clean := func() {
		common.DB.Where("1 = 1").Delete(&model.IntradayFactorDaily{})
		common.DB.Where("1 = 1").Delete(&model.MarketSyncState{})
		common.DB.Where("`key` = ?", optIntradayDay).Delete(&model.Option{})
	}
	clean()
	t.Cleanup(clean)
}

// flatMin5 构造 O=H=L=C 的平根（典型价=价格本身，VWAP 手算不涉小数尾差）。
func flatMin5(day, clock string, price float64, vol int64) datasource.Min5Bar {
	return datasource.Min5Bar{Time: day + clock, Open: price, High: price, Low: price, Close: price, Volume: vol}
}

// min5Clocks 一天 48 根的时间戳表（0935~1130 + 1305~1500）。
func min5Clocks() []string {
	var out []string
	push := func(startH, startM, n int) {
		h, m := startH, startM
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("%02d%02d", h, m))
			m += 5
			if m >= 60 {
				m -= 60
				h++
			}
		}
	}
	push(9, 35, 24)  // 0935..1130
	push(13, 5, 24)  // 1305..1500
	return out
}

// fullDay48 全天 48 根：priceAt/volAt 按时钟给值。
func fullDay48(day string, priceAt func(clock string) float64, volAt func(clock string) int64) []datasource.Min5Bar {
	var bars []datasource.Min5Bar
	for _, c := range min5Clocks() {
		bars = append(bars, flatMin5(day, c, priceAt(c), volAt(c)))
	}
	return bars
}

// 场景一手工验算：上午首根 10.00、其余上午 10.30；下午 10.00、末根 9.80，量恒 100。
//   morning = (10.30-10.00)/10.00 = +3.00%
//   tail30  = (9.80-10.00)/10.00 = -2.00%（1430 收盘 10.00 → 1500 收盘 9.80）
//   tail30VolPct = 600/4800 = 12.5%
//   vwapAM = (10.00×100+10.30×2300)/2400 = 10.2875；vwapPM = (10.00×2300+9.80×100)/2400 ≈ 9.9917 → PmVwapUp=false
//   vwap 全天 = 48670/4800 = 10.1396→ closeVsVwap = (9.80-10.1396)/10.1396 = -3.35%
func TestComputeIntradayFactorsManual(t *testing.T) {
	day := "20260708"
	bars := fullDay48(day,
		func(clock string) float64 {
			switch {
			case clock == "0935":
				return 10.00
			case clock <= "1130":
				return 10.30
			case clock == "1500":
				return 9.80
			default:
				return 10.00
			}
		},
		func(string) int64 { return 100 },
	)
	f, ok := computeIntradayFactors(bars)
	if !ok {
		t.Fatal("完整 48 根应可算")
	}
	if f.BarCount != 48 {
		t.Errorf("BarCount = %d", f.BarCount)
	}
	if f.MorningChg != 3.00 {
		t.Errorf("MorningChg = %v, 期望 3.00", f.MorningChg)
	}
	if f.Tail30Chg != -2.00 {
		t.Errorf("Tail30Chg = %v, 期望 -2.00", f.Tail30Chg)
	}
	if f.Tail30VolPct != 12.5 {
		t.Errorf("Tail30VolPct = %v, 期望 12.5", f.Tail30VolPct)
	}
	if f.CloseVsVwap != -3.35 {
		t.Errorf("CloseVsVwap = %v, 期望 -3.35", f.CloseVsVwap)
	}
	if f.PmVwapUp {
		t.Errorf("下午 VWAP 更低，PmVwapUp 应 false")
	}
	if f.Vwap != 10.14 { // 48670/4800 = 10.1396 → round2
		t.Errorf("Vwap = %v, 期望 10.14", f.Vwap)
	}
}

// 场景二：尾盘放量拉升（尾盘 6 根 10.50 量 200、其余 10.00 量 100）。
//   tail30 = +5.00%、tail30VolPct = 1200/5400 = 22.22%、PmVwapUp=true
func TestComputeIntradayFactorsTailRush(t *testing.T) {
	day := "20260708"
	bars := fullDay48(day,
		func(clock string) float64 {
			if clock > "1430" {
				return 10.50
			}
			return 10.00
		},
		func(clock string) int64 {
			if clock > "1430" {
				return 200
			}
			return 100
		},
	)
	f, ok := computeIntradayFactors(bars)
	if !ok {
		t.Fatal("应可算")
	}
	if f.Tail30Chg != 5.00 {
		t.Errorf("Tail30Chg = %v, 期望 5.00", f.Tail30Chg)
	}
	if f.Tail30VolPct != 22.22 {
		t.Errorf("Tail30VolPct = %v, 期望 22.22", f.Tail30VolPct)
	}
	if !f.PmVwapUp {
		t.Errorf("下午重心上移应 true")
	}
	if f.MorningChg != 0 {
		t.Errorf("MorningChg = %v, 期望 0", f.MorningChg)
	}
	// vwap = (4200×10.00 + 1200×10.50)/5400 = (42000+12600)/5400 = 10.1111 → 10.11
	if f.Vwap != 10.11 {
		t.Errorf("Vwap = %v, 期望 10.11", f.Vwap)
	}
	// closeVsVwap = (10.50-10.1111)/10.1111 = 3.85%
	if f.CloseVsVwap != 3.85 {
		t.Errorf("CloseVsVwap = %v, 期望 3.85", f.CloseVsVwap)
	}
}

// 完整性门槛与锚点缺失。
func TestComputeIntradayFactorsGuards(t *testing.T) {
	day := "20260708"
	full := fullDay48(day, func(string) float64 { return 10 }, func(string) int64 { return 100 })

	// 根数不足（45 根 < 46）。
	if _, ok := computeIntradayFactors(full[:45]); ok {
		t.Error("45 根应拒算")
	}
	// 47 根但缺 1500 收盘锚点根 → 拒算。
	no1500 := make([]datasource.Min5Bar, 0, 47)
	for _, b := range full {
		if min5Clock(b.Time) != "1500" {
			no1500 = append(no1500, b)
		}
	}
	if _, ok := computeIntradayFactors(no1500); ok {
		t.Error("缺 1500 锚点应拒算")
	}
	// 47 根缺一根非锚点根（1105）→ 可算。
	no1105 := make([]datasource.Min5Bar, 0, 47)
	for _, b := range full {
		if min5Clock(b.Time) != "1105" {
			no1105 = append(no1105, b)
		}
	}
	if f, ok := computeIntradayFactors(no1105); !ok || f.BarCount != 47 {
		t.Errorf("缺非锚点根应可算, ok=%v bars=%d", ok, f.BarCount)
	}
	// 半场零成交 → 拒算。
	amZero := fullDay48(day, func(string) float64 { return 10 }, func(clock string) int64 {
		if clock <= "1130" {
			return 0
		}
		return 100
	})
	if _, ok := computeIntradayFactors(amZero); ok {
		t.Error("上午零成交应拒算")
	}
}

func TestSplitMin5ByDate(t *testing.T) {
	bars := []datasource.Min5Bar{
		flatMin5("20260709", "0940", 10, 1),
		flatMin5("20260708", "1500", 10, 1),
		flatMin5("20260709", "0935", 10, 1), // 乱序输入
		{Time: "bad", Open: 1, High: 1, Low: 1, Close: 1},
	}
	m := splitMin5ByDate(bars)
	if len(m) != 2 {
		t.Fatalf("应分 2 日, got %d", len(m))
	}
	if len(m["2026-07-09"]) != 2 || len(m["2026-07-08"]) != 1 {
		t.Errorf("分组错误: %v", m)
	}
	if m["2026-07-09"][0].Time != "202607090935" {
		t.Errorf("组内应升序")
	}
}

func TestMin5CountForDays(t *testing.T) {
	for _, c := range []struct{ days, want int }{{1, 60}, {2, 108}, {10, 492}, {20, 800}} {
		if got := min5CountForDays(c.days); got != c.want {
			t.Errorf("min5CountForDays(%d) = %d, 期望 %d", c.days, got, c.want)
		}
	}
}

// 端到端：假 fetch 两日数据 → 同步落库 → 幂等重跑 → 消费查询取 MAX(trade_date)。
func TestSyncIntradayFactorsE2E(t *testing.T) {
	setupTestDB(t)
	cleanIntradayTables(t)
	// 宇宙 3 只：000001/600519 正常，300750 上游无数据（ErrNoData 静默跳过）。
	for _, sym := range []string{"000001", "600519", "300750"} {
		common.DB.Create(&model.MarketSyncState{Symbol: sym, Market: "cn", InitStatus: "done"})
	}
	mk := func(day string, base float64) []datasource.Min5Bar {
		return fullDay48(day, func(clock string) float64 {
			if clock > "1430" {
				return base + 0.5
			}
			return base
		}, func(string) int64 { return 100 })
	}
	svc := &IntradayService{fetchMin5: func(ctx context.Context, market, symbol string, count int) ([]datasource.Min5Bar, error) {
		switch symbol {
		case "000001":
			return append(mk("20260707", 10), mk("20260708", 10)...), nil
		case "600519":
			return mk("20260708", 1000), nil // 0707 停牌：上游缺该日根
		default:
			return nil, datasource.ErrNoData
		}
	}}
	dates := []string{"2026-07-07", "2026-07-08"}
	n, err := svc.SyncIntradayFactors(context.Background(), dates)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if n != 3 { // 000001 两日 + 600519 一日
		t.Fatalf("应落 3 行, got %d", n)
	}
	var rows []model.IntradayFactorDaily
	common.DB.Where("symbol = ? AND market = ?", "000001", "cn").Order("trade_date").Find(&rows)
	if len(rows) != 2 || rows[0].TradeDate != "2026-07-07" || rows[1].TradeDate != "2026-07-08" {
		t.Fatalf("000001 应两日: %+v", rows)
	}
	if rows[1].Tail30Chg != 5.00 || !rows[1].PmVwapUp {
		t.Errorf("尾盘拉升因子错误: %+v", rows[1])
	}

	// 幂等：重跑同 dates，行数不翻倍（先删后插）。
	if _, err := svc.SyncIntradayFactors(context.Background(), dates); err != nil {
		t.Fatalf("重跑失败: %v", err)
	}
	var total int64
	common.DB.Model(&model.IntradayFactorDaily{}).Count(&total)
	if total != 3 {
		t.Fatalf("重跑后应仍 3 行, got %d", total)
	}

	// 消费查询：MAX(trade_date)=0708 的行。
	sigs := intradaySignalsFor([]string{"000001", "600519", "300750"})
	if len(sigs) != 2 {
		t.Fatalf("信号应 2 只, got %d", len(sigs))
	}
	if sigs["000001"].TradeDate != "2026-07-08" {
		t.Errorf("应取最近日, got %s", sigs["000001"].TradeDate)
	}
	if IntradayAccumulatedDays() != 2 {
		t.Errorf("累计交易日应 2, got %d", IntradayAccumulatedDays())
	}
}

// 源故障熔断：连续网络类失败达阈值中止且不落库。
func TestSyncIntradayAbortOnSourceFailure(t *testing.T) {
	setupTestDB(t)
	cleanIntradayTables(t)
	for i := 0; i < intradayAbortStreak+10; i++ {
		common.DB.Create(&model.MarketSyncState{Symbol: fmt.Sprintf("%06d", 100000+i), Market: "cn", InitStatus: "done"})
	}
	calls := 0
	svc := &IntradayService{fetchMin5: func(ctx context.Context, market, symbol string, count int) ([]datasource.Min5Bar, error) {
		calls++
		return nil, errors.New("connection reset")
	}}
	_, err := svc.SyncIntradayFactors(context.Background(), []string{"2026-07-08"})
	if err == nil || errors.Is(err, ErrSyncInProgress) {
		t.Fatalf("源故障应报错, got %v", err)
	}
	var total int64
	common.DB.Model(&model.IntradayFactorDaily{}).Count(&total)
	if total != 0 {
		t.Errorf("中止后不应落库, got %d", total)
	}
}

// 游标补跑日期窗口。
func TestIntradayPendingDates(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	t.Cleanup(func() { common.DB.Where("1 = 1").Delete(&model.TradingCalendar{}) })
	for _, d := range []string{"2026-07-01", "2026-07-02", "2026-07-03", "2026-07-06", "2026-07-07", "2026-07-08"} {
		common.DB.Select("Market", "TradeDate", "IsOpen").Create(&model.TradingCalendar{Market: "cn", TradeDate: d, IsOpen: true})
	}
	// 游标已到位 → 空。
	if got := intradayPendingDates("2026-07-08", "2026-07-08"); got != nil {
		t.Errorf("游标到位应 nil, got %v", got)
	}
	// 首轮（游标空）→ 只目标日。
	if got := intradayPendingDates("", "2026-07-08"); len(got) != 1 || got[0] != "2026-07-08" {
		t.Errorf("首轮应只目标日, got %v", got)
	}
	// 游标落后：0703 → (0703, 0708] 开市日 = 0706/0707/0708。
	got := intradayPendingDates("2026-07-03", "2026-07-08")
	if len(got) != 3 || got[0] != "2026-07-06" || got[2] != "2026-07-08" {
		t.Errorf("补跑窗口错误: %v", got)
	}
}

// 盘中因子短线加分项：命中/方向对称/长线不参与/无数据不动分。
func TestStrategyAdjustIntraday(t *testing.T) {
	// 全强势形态：尾盘放量拉升 +4、收盘强于均价 +3、午后重心上移 +2、早盘强势 +2 = +11。
	strong := &candFactors{
		IntradayDate: "2026-07-08",
		Tail30Chg:    2.1, Tail30VolPct: 25,
		CloseVsVwap: 1.5, PmVwapUp: true, MorningChg: 2.5,
	}
	delta, notes := strategyAdjust(model.RecTypeShortTerm, "momentum", candidate{}, strong)
	if delta != 11 {
		t.Errorf("全强势盘中形态应 +11, got %v (%v)", delta, notes)
	}
	joined := strings.Join(notes, "|")
	for _, kw := range []string{"尾盘30分放量拉升", "收盘强于全天均价", "午后重心上移", "早盘1小时强势"} {
		if !strings.Contains(joined, kw) {
			t.Errorf("notes 缺少 %q: %v", kw, notes)
		}
	}

	// 尾盘放量跳水 -4 + 收盘弱于均价 -2 = -6。
	weak := &candFactors{IntradayDate: "2026-07-08", Tail30Chg: -2, Tail30VolPct: 30, CloseVsVwap: -2}
	delta2, notes2 := strategyAdjust(model.RecTypeShortTerm, "momentum", candidate{}, weak)
	if delta2 != -6 {
		t.Errorf("尾盘出逃形态应 -6, got %v (%v)", delta2, notes2)
	}

	// 无放量的尾盘拉升降档 +3；量占比 <20 不触发放量文案。
	mild := &candFactors{IntradayDate: "2026-07-08", Tail30Chg: 1.8, Tail30VolPct: 15}
	delta3, notes3 := strategyAdjust(model.RecTypeShortTerm, "momentum", candidate{}, mild)
	if delta3 != 3 || strings.Contains(strings.Join(notes3, "|"), "放量") {
		t.Errorf("无放量拉升应 +3 且不带放量文案, got %v (%v)", delta3, notes3)
	}

	// IntradayDate 为空（无盘中数据）不动分。
	empty := &candFactors{Tail30Chg: 5, Tail30VolPct: 30, CloseVsVwap: 3}
	if d, n := strategyAdjust(model.RecTypeShortTerm, "momentum", candidate{}, empty); d != 0 {
		t.Errorf("无归属日不应动分, got %v (%v)", d, n)
	}

	// 长线不参与盘中加分（日内噪声对长线无意义；Pos60 等长线自有项照常）。
	_, nLong := strategyAdjust(model.RecTypeLongTerm, "value", candidate{}, strong)
	if s := strings.Join(nLong, "|"); strings.Contains(s, "尾盘") || strings.Contains(s, "均价") || strings.Contains(s, "重心") {
		t.Errorf("长线不应出现盘中加分文案: %v", nLong)
	}
}

// 值域同步铁律：盘中数值因子必须进 candidateLabeledValues（LLM 忠实引用不被误报幻觉）。
func TestCandidateValueSetIntraday(t *testing.T) {
	c := candidate{Factors: &candFactors{
		IntradayDate: "2026-07-08",
		Tail30Chg:    2.1, Tail30VolPct: 25.3, MorningChg: 1.7, CloseVsVwap: -1.2,
	}}
	vals := candidateLabeledValues(c)
	for _, want := range []float64{2.1, 25.3, 1.7, -1.2} {
		if !labeledHas(vals, want) {
			t.Errorf("值域缺少盘中因子 %v", want)
		}
	}
}

// TestLiveIntradayFactors 真实数据端到端（LIVE_INTRADAY=1 门控）：
// 拉平安银行 5 分钟线，对最近一个完整交易日算五件因子。
func TestLiveIntradayFactors(t *testing.T) {
	if os.Getenv("LIVE_INTRADAY") == "" {
		t.Skip("需要 LIVE_INTRADAY=1")
	}
	ta := datasource.NewTencentAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bars, err := ta.GetMin5Bars(ctx, "cn", "000001", 108)
	if err != nil {
		t.Fatalf("真实拉取失败: %v", err)
	}
	byDate := splitMin5ByDate(bars)
	computed := 0
	for d, dayBars := range byDate {
		f, ok := computeIntradayFactors(dayBars)
		if !ok {
			t.Logf("%s: %d 根不完整（盘中当日/半日停牌），跳过", d, len(dayBars))
			continue
		}
		computed++
		if f.Vwap <= 0 || f.BarCount < intradayMinBars {
			t.Errorf("%s 因子异常: %+v", d, f)
		}
		if f.Tail30VolPct <= 0 || f.Tail30VolPct >= 100 {
			t.Errorf("%s 尾盘量占比越界: %v", d, f.Tail30VolPct)
		}
		t.Logf("%s: tail30=%v%% tail30Vol=%v%% morning=%v%% closeVsVwap=%v%% pmUp=%v vwap=%v bars=%d",
			d, f.Tail30Chg, f.Tail30VolPct, f.MorningChg, f.CloseVsVwap, f.PmVwapUp, f.Vwap, f.BarCount)
	}
	if computed == 0 {
		t.Fatal("108 根应至少覆盖一个完整交易日")
	}
}
