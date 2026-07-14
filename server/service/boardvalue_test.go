package service

import (
	"context"
	"errors"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// cleanBoardValTables 清场（内存库 cache=shared 测试间共享，进退场都清）。
func cleanBoardValTables(t *testing.T) {
	t.Helper()
	clean := func() {
		common.DB.Exec("DELETE FROM board_valuation_dailies")
	}
	clean()
	t.Cleanup(clean)
}

// fakeBoardLister 聚合链路的假清单源。
type fakeBoardLister struct {
	boards []datasource.BoardListItem
	err    error
	calls  int
}

func (f *fakeBoardLister) GetBoardList(ctx context.Context, kind string) ([]datasource.BoardListItem, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.boards, nil
}

func TestPercentileRank(t *testing.T) {
	cases := []struct {
		name string
		all  []float64
		v    float64
		want float64
	}{
		{"空集", nil, 10, 0},
		{"单样本对称取 50", []float64{10}, 10, 50},
		{"最小值", []float64{10, 20, 30, 40}, 10, 12.5}, // (0+0.5)/4
		{"最大值", []float64{10, 20, 30, 40}, 40, 87.5}, // (3+0.5)/4
		{"中间值", []float64{10, 20, 30, 40}, 30, 62.5}, // (2+0.5)/4
		{"重复值", []float64{10, 20, 20, 40}, 20, 50},   // (1+0.5*2)/4
	}
	for _, c := range cases {
		if got := percentileRank(c.all, c.v); got != c.want {
			t.Errorf("%s: percentileRank=%v, want %v", c.name, got, c.want)
		}
	}
}

// 手工验算：半导体 PE 正样本 {30,50,70} 中位 50、PB {5,7} 中位 6（偶数取均值）；
// 银行 PE 只有 {6}（亏损 -8 与停牌 0 被过滤）；空行业名（退市壳票）整行跳过。
func TestAggregateSpotByIndustry(t *testing.T) {
	rows := []datasource.SpotRow{
		{Symbol: "A1", Industry: "半导体", PETTM: 30, PB: 5},
		{Symbol: "A2", Industry: "半导体", PETTM: 70, PB: 7},
		{Symbol: "A3", Industry: "半导体", PETTM: 50, PB: 0},
		{Symbol: "B1", Industry: "银行", PETTM: 6, PB: 0.6},
		{Symbol: "B2", Industry: "银行", PETTM: -8, PB: 0.5},
		{Symbol: "B3", Industry: "银行", PETTM: 0, PB: 0},
		{Symbol: "C1", Industry: "", PETTM: 99, PB: 9}, // 退市壳票
	}
	aggs := aggregateSpotByIndustry(rows)
	if len(aggs) != 2 {
		t.Fatalf("行业数 = %d, want 2: %+v", len(aggs), aggs)
	}
	// sort.Strings 序：银行 < 半导体 取决于 Unicode 码点；按名取用。
	byName := map[string]boardValAgg{}
	for _, a := range aggs {
		byName[a.Industry] = a
	}
	semi := byName["半导体"]
	if semi.MedianPETTM != 50 || semi.MedianPB != 6 || semi.PosPECount != 3 || semi.StockCount != 3 {
		t.Fatalf("半导体聚合错误: %+v", semi)
	}
	bank := byName["银行"]
	if bank.MedianPETTM != 6 || round2(bank.MedianPB) != 0.55 || bank.PosPECount != 1 || bank.StockCount != 3 {
		t.Fatalf("银行聚合错误: %+v", bank)
	}
}

func TestFillCrossSectionRank(t *testing.T) {
	recs := []model.BoardValuationDaily{
		{BoardCode: "BK1", MedianPETTM: 10, PosPECount: 1},
		{BoardCode: "BK2", MedianPETTM: 30, PosPECount: 2},
		{BoardCode: "BK3", MedianPETTM: 50, PosPECount: 3},
		{BoardCode: "BK4", MedianPETTM: 0, PosPECount: 0}, // 算不出
	}
	fillCrossSectionRank(recs)
	if recs[0].PctRank != round2(100*0.5/3) || recs[1].PctRank != 50 || recs[2].PctRank != round2(100*2.5/3) {
		t.Fatalf("横截面分位错误: %+v", recs)
	}
	if recs[3].PctRank != -1 {
		t.Fatalf("无样本板块应记 -1: %+v", recs[3])
	}
}

// 聚合端到端：落库 + 幂等（二跑行数不变、改值覆盖）+ 未匹配行业跳过 + 清单失败不落库。
func TestAggregateBoardValuationIdempotent(t *testing.T) {
	setupTestDB(t)
	cleanBoardValTables(t)
	lister := &fakeBoardLister{boards: []datasource.BoardListItem{
		{Code: "BK1036", Name: "半导体"},
		{Code: "BK0447", Name: "银行"},
	}}
	rows := []datasource.SpotRow{
		{Symbol: "A1", Industry: "半导体", PETTM: 40, PB: 4},
		{Symbol: "B1", Industry: "银行", PETTM: 6, PB: 0.6},
		{Symbol: "X1", Industry: "神秘行业", PETTM: 20, PB: 2}, // 清单里没有 → 跳过
	}
	if err := aggregateBoardValuation(context.Background(), lister, rows, "2026-07-14"); err != nil {
		t.Fatalf("聚合: %v", err)
	}
	var cnt int64
	common.DB.Model(&model.BoardValuationDaily{}).Count(&cnt)
	if cnt != 2 {
		t.Fatalf("落库行数 = %d, want 2（未匹配行业跳过）", cnt)
	}
	// 幂等：改值重跑同日，行数不变、值被覆盖。
	rows[0].PETTM = 60
	if err := aggregateBoardValuation(context.Background(), lister, rows, "2026-07-14"); err != nil {
		t.Fatalf("二跑: %v", err)
	}
	common.DB.Model(&model.BoardValuationDaily{}).Count(&cnt)
	if cnt != 2 {
		t.Fatalf("二跑行数 = %d, want 2（upsert 幂等）", cnt)
	}
	var semi model.BoardValuationDaily
	if err := common.DB.Where("board_code = ? AND trade_date = ?", "BK1036", "2026-07-14").First(&semi).Error; err != nil {
		t.Fatalf("查半导体: %v", err)
	}
	if semi.MedianPETTM != 60 || semi.Kind != "industry" || semi.BoardName != "半导体" {
		t.Fatalf("覆盖失败: %+v", semi)
	}
	// 横截面分位：半导体 60 贵于银行 6。
	if semi.PctRank != 75 { // (1+0.5)/2
		t.Fatalf("半导体横截面分位 = %v, want 75", semi.PctRank)
	}
	// 清单失败：整轮不落库（次日增量再来）。
	failLister := &fakeBoardLister{err: errors.New("限流")}
	if err := aggregateBoardValuation(context.Background(), failLister, rows, "2026-07-15"); err == nil {
		t.Fatal("清单失败应返回错误")
	}
	common.DB.Model(&model.BoardValuationDaily{}).Where("trade_date = ?", "2026-07-15").Count(&cnt)
	if cnt != 0 {
		t.Fatalf("清单失败不应落库, got %d", cnt)
	}
}

// 查询侧：最新行 + 时序分位与积累天数；按名匹配；无数据返回 nil。
func TestBoardValuationForAndByName(t *testing.T) {
	setupTestDB(t)
	cleanBoardValTables(t)
	seed := []model.BoardValuationDaily{
		{Kind: "industry", BoardCode: "BK1036", BoardName: "半导体", TradeDate: "2026-07-10", MedianPETTM: 40, MedianPB: 4, PosPECount: 100, StockCount: 120, PctRank: 80},
		{Kind: "industry", BoardCode: "BK1036", BoardName: "半导体", TradeDate: "2026-07-11", MedianPETTM: 44, MedianPB: 4.2, PosPECount: 101, StockCount: 120, PctRank: 82},
		{Kind: "industry", BoardCode: "BK1036", BoardName: "半导体", TradeDate: "2026-07-14", MedianPETTM: 42, MedianPB: 4.1, PosPECount: 99, StockCount: 120, PctRank: 81},
	}
	if err := common.DB.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	v := boardValuationFor("BK1036")
	if v == nil {
		t.Fatal("应取到估值视图")
	}
	if v.TradeDate != "2026-07-14" || v.MedianPETTM != 42 || v.HistDays != 3 {
		t.Fatalf("最新行/积累天数错误: %+v", v)
	}
	// 时序分位：42 在 {40,44,42} 中 = (1+0.5)/3 ≈ 50。
	if v.HistPctRank != 50 {
		t.Fatalf("时序分位 = %v, want 50", v.HistPctRank)
	}
	bv, code := boardValuationByName("半导体")
	if bv == nil || code != "BK1036" || bv.TradeDate != "2026-07-14" {
		t.Fatalf("按名匹配失败: %+v code=%s", bv, code)
	}
	if bv2, code2 := boardValuationByName("不存在的板块"); bv2 != nil || code2 != "" {
		t.Fatalf("未匹配应返回 nil: %+v", bv2)
	}
	if boardValuationFor("BK9999") != nil {
		t.Fatal("无数据板块应返回 nil")
	}
}
