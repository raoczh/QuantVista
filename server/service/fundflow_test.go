package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

var errTestUpstream = errors.New("假上游故障")

func flowRow(date string, mainNet, mainPct float64) model.FundFlowDaily {
	return model.FundFlowDaily{Symbol: "600519", Market: "cn", TradeDate: date, MainNet: mainNet, MainPct: mainPct}
}

// mainNetStreakDays：连续同号计数、零值截断、方向符号。
func TestMainNetStreakDays(t *testing.T) {
	cases := []struct {
		nets []float64
		want int
	}{
		{[]float64{-1, 2, 3, 4}, 3}, // 末端连续 3 天净流入
		{[]float64{1, -2, -3}, -2},  // 连续 2 天净流出
		{[]float64{5, 0, 3}, 1},     // 零值（停牌）截断
		{[]float64{2, 3, 0}, 0},     // 末日为 0 无方向
		{nil, 0},
		{[]float64{7}, 1},
	}
	for i, c := range cases {
		flows := make([]model.FundFlowDaily, 0, len(c.nets))
		for j, v := range c.nets {
			flows = append(flows, flowRow(time.Date(2026, 7, 1+j, 0, 0, 0, 0, time.Local).Format("2006-01-02"), v, 0))
		}
		if got := mainNetStreakDays(flows); got != c.want {
			t.Errorf("case %d: got %d want %d", i, got, c.want)
		}
	}
}

// flowVolumeScore：净占比均值线性映射（50 中性 / +5% → 90）；样本不足不给分。
func TestFlowVolumeScore(t *testing.T) {
	if _, ok := flowVolumeScore([]model.FundFlowDaily{flowRow("2026-07-01", 1, 5), flowRow("2026-07-02", 1, 5)}); ok {
		t.Error("样本 <3 不应给分")
	}
	flows := []model.FundFlowDaily{}
	for i := 0; i < 5; i++ {
		flows = append(flows, flowRow(time.Date(2026, 7, 1+i, 0, 0, 0, 0, time.Local).Format("2006-01-02"), 1e8, 5))
	}
	fs, ok := flowVolumeScore(flows)
	if !ok || fs != 90 { // 50 + 5*8
		t.Errorf("avg 5%% 应 90，got %v ok=%v", fs, ok)
	}
	// applyFlowScore：量能维 0.6 原 + 0.4 资金分，Total 按权重重算；空序列原样返回。
	base := ScoreResult{Trend: 50, Momentum: 50, Position: 50, Volume: 50, Risk: 50, Total: 50, Label: "中性"}
	out := applyFlowScore(base, flows)
	if out.Volume != 66 { // 0.6*50 + 0.4*90
		t.Errorf("量能维应 66，got %v", out.Volume)
	}
	wantTotal := round2(clamp0100(wTrend*50 + wMomentum*50 + wPosition*50 + wVolume*66 + wRisk*50))
	if out.Total != wantTotal {
		t.Errorf("Total 应 %v，got %v", wantTotal, out.Total)
	}
	same := applyFlowScore(base, nil)
	if same.Total != base.Total || same.Volume != base.Volume {
		t.Error("无资金流数据评分应原样返回")
	}
}

// persistFundFlow：16:00 前丢弃「今天」的行（盘中半截值防残留）、16:00 后允许落。
func TestPersistFundFlowTodayGate(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	today := time.Date(2026, 7, 8, 10, 0, 0, 0, time.Local)
	bars := []datasource.StockFundFlowBar{
		{TradeDate: "2026-07-07", MainNet: 100},
		{TradeDate: "2026-07-08", MainNet: 999}, // 盘中半截值
	}
	persistFundFlow("cn", "600519", bars, today)
	var cnt int64
	common.DB.Model(&model.FundFlowDaily{}).Where("symbol = ?", "600519").Count(&cnt)
	if cnt != 1 {
		t.Fatalf("盘中应只落昨日 1 行，got %d", cnt)
	}
	// 盘后（16:00+）允许落今天，且 upsert 覆盖。
	persistFundFlow("cn", "600519", bars, time.Date(2026, 7, 8, 16, 30, 0, 0, time.Local))
	common.DB.Model(&model.FundFlowDaily{}).Where("symbol = ?", "600519").Count(&cnt)
	if cnt != 2 {
		t.Fatalf("盘后应 2 行，got %d", cnt)
	}
	var row model.FundFlowDaily
	common.DB.Where("symbol = ? AND trade_date = ?", "600519", "2026-07-08").First(&row)
	if row.MainNet != 999 {
		t.Errorf("今日行 MainNet 应 999，got %v", row.MainNet)
	}
}

// ensureStockFundFlow：库存新鲜直接返回（不打上游）；上游失败返回旧库存。
func TestEnsureStockFundFlowCache(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	em := datasource.NewEastMoneyAdapter()
	called := 0
	em.SetFetchForTest(func(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
		called++
		return nil, 0, errTestUpstream
	})
	// 库存末行 = 上一开市日（无日历回退往前工作日）→ 新鲜，不打上游。
	fresh := prevOpenTradeDate(time.Now().Format("2006-01-02"))
	common.DB.Create(&model.FundFlowDaily{Symbol: "000998", Market: "cn", TradeDate: fresh, MainNet: 1})
	flows, ok := ensureStockFundFlow(context.Background(), em, "cn", "000998", nil)
	if !ok || len(flows) != 1 || called != 0 {
		t.Errorf("新鲜库存应直接返回：ok=%v n=%d called=%d", ok, len(flows), called)
	}
	// 陈旧库存 + 上游失败：返回旧库存（stale 也比没有强）。
	common.DB.Create(&model.FundFlowDaily{Symbol: "000999", Market: "cn", TradeDate: "2026-01-05", MainNet: 2})
	flows2, ok2 := ensureStockFundFlow(context.Background(), em, "cn", "000999", nil)
	if ok2 || len(flows2) != 1 || called != 1 {
		t.Errorf("上游失败应返回旧库存：ok=%v n=%d called=%d", ok2, len(flows2), called)
	}
	// 冷却：1h 内同标的不再打上游。
	ensureStockFundFlow(context.Background(), em, "cn", "000999", nil)
	if called != 1 {
		t.Errorf("冷却期内不应再打上游，called=%d", called)
	}
	// 预算耗尽：不打上游。
	budget := 0
	common.DB.Create(&model.FundFlowDaily{Symbol: "000997", Market: "cn", TradeDate: "2026-01-05", MainNet: 3})
	ensureStockFundFlow(context.Background(), em, "cn", "000997", &budget)
	if called != 1 {
		t.Errorf("预算 0 不应打上游，called=%d", called)
	}
}

// inspectStockFundFlow 应截取最新窗口，再恢复成评分消费者需要的日期升序。
// 库存累计超过窗口后，不能一直读到最老 250 行并永久误判 stale。
func TestInspectStockFundFlowUsesLatestWindowAscending(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	now := time.Now().In(time.Local)
	fresh := prevOpenTradeDate(now.Format("2006-01-02"))
	end, err := time.ParseInLocation("2006-01-02", fresh, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	const extra = 5
	rows := make([]model.FundFlowDaily, 0, fflowBarLimit+extra)
	for i := fflowBarLimit + extra - 1; i >= 0; i-- {
		rows = append(rows, model.FundFlowDaily{
			Symbol: "000994", Market: "cn", TradeDate: end.AddDate(0, 0, -i).Format("2006-01-02"),
			MainNet: float64(i),
		})
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	probe := inspectStockFundFlow("cn", "000994", now)
	if len(probe.Rows) != fflowBarLimit {
		t.Fatalf("应只返回最新 %d 行，got %d", fflowBarLimit, len(probe.Rows))
	}
	wantFirst := end.AddDate(0, 0, -(fflowBarLimit - 1)).Format("2006-01-02")
	if probe.Rows[0].TradeDate != wantFirst || probe.Rows[len(probe.Rows)-1].TradeDate != fresh {
		t.Fatalf("最新窗口边界不符：first=%s want=%s last=%s want=%s",
			probe.Rows[0].TradeDate, wantFirst, probe.Rows[len(probe.Rows)-1].TradeDate, fresh)
	}
	for i := 1; i < len(probe.Rows); i++ {
		if probe.Rows[i-1].TradeDate >= probe.Rows[i].TradeDate {
			t.Fatalf("输出必须严格日期升序：i=%d prev=%s curr=%s", i,
				probe.Rows[i-1].TradeDate, probe.Rows[i].TradeDate)
		}
	}
	if !probe.Fresh {
		t.Fatal("最新一行为上一开市日时应判为 fresh")
	}
}

// 推荐评分只能消费新鲜资金流；详情页仍可读取 stale 库存并用 Fresh=false 显式展示。
func TestFundFlowForScoringRejectsStaleRows(t *testing.T) {
	rows := []model.FundFlowDaily{{Symbol: "600519", Market: "cn", TradeDate: "2026-01-05", MainNet: 1}}
	if got := fundFlowForScoring(rows, false); len(got) != 0 {
		t.Fatalf("stale 资金流不得进入评分，got=%+v", got)
	}
	if got := fundFlowForScoring(rows, true); len(got) != 1 || got[0].MainNet != 1 {
		t.Fatalf("fresh 资金流应原样进入评分，got=%+v", got)
	}
}

// 冷却命中没有发生上游调用，不应占掉本批预算并让后续标的因遍历顺序失去补拉机会。
func TestEnsureStockFundFlowCooldownDoesNotConsumeBudget(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	em := datasource.NewEastMoneyAdapter()
	calls := 0
	em.SetFetchForTest(func(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
		calls++
		return nil, 0, errTestUpstream
	})

	common.DB.Create(&model.FundFlowDaily{Symbol: "000996", Market: "cn", TradeDate: "2026-01-05", MainNet: 1})
	common.DB.Create(&model.FundFlowDaily{Symbol: "000995", Market: "cn", TradeDate: "2026-01-05", MainNet: 1})
	fflowTryMu.Lock()
	fflowTry["cn:000996"] = time.Now()
	fflowTryMu.Unlock()

	budget := 1
	ensureStockFundFlow(context.Background(), em, "cn", "000996", &budget)
	if budget != 1 || calls != 0 {
		t.Fatalf("冷却命中不应耗预算或打上游：budget=%d calls=%d", budget, calls)
	}
	ensureStockFundFlow(context.Background(), em, "cn", "000995", &budget)
	if budget != 0 || calls != 1 {
		t.Fatalf("后续标的应仍可使用预算：budget=%d calls=%d", budget, calls)
	}
}
